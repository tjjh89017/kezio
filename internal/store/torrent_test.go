/*
Copyright 2026 Date Huang.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package store

import (
	"bytes"
	"crypto/sha1" //nolint:gosec // test-only verification of the production package's own SHA-1 usage
	"encoding/hex"
	"fmt"
	"testing"
)

// decodeBencode is a minimal, test-only bencode decoder (BEP3) used to
// verify BuildTorrentFile's output structurally, independent of the
// encoder in bencode.go/torrent.go. It returns one of: int64, []byte,
// []any, or map[string]any.
func decodeBencode(b []byte) (any, int, error) {
	if len(b) == 0 {
		return nil, 0, fmt.Errorf("empty input")
	}
	switch {
	case b[0] == 'i':
		end := indexByte(b, 'e')
		if end < 0 {
			return nil, 0, fmt.Errorf("unterminated integer")
		}
		var n int64
		if _, err := fmt.Sscanf(string(b[1:end]), "%d", &n); err != nil {
			return nil, 0, fmt.Errorf("bad integer %q: %w", b[1:end], err)
		}
		return n, end + 1, nil
	case b[0] == 'l':
		i := 1
		var out []any
		for b[i] != 'e' {
			v, n, err := decodeBencode(b[i:])
			if err != nil {
				return nil, 0, err
			}
			out = append(out, v)
			i += n
		}
		return out, i + 1, nil
	case b[0] == 'd':
		i := 1
		out := map[string]any{}
		for b[i] != 'e' {
			k, n, err := decodeBencode(b[i:])
			if err != nil {
				return nil, 0, err
			}
			i += n
			key, ok := k.([]byte)
			if !ok {
				return nil, 0, fmt.Errorf("dict key is not a string")
			}
			v, n2, err := decodeBencode(b[i:])
			if err != nil {
				return nil, 0, err
			}
			i += n2
			out[string(key)] = v
		}
		return out, i + 1, nil
	case b[0] >= '0' && b[0] <= '9':
		colon := indexByte(b, ':')
		if colon < 0 {
			return nil, 0, fmt.Errorf("malformed string length")
		}
		var length int
		if _, err := fmt.Sscanf(string(b[:colon]), "%d", &length); err != nil {
			return nil, 0, fmt.Errorf("bad string length %q: %w", b[:colon], err)
		}
		start := colon + 1
		end := start + length
		if end > len(b) {
			return nil, 0, fmt.Errorf("string length %d exceeds input", length)
		}
		return b[start:end], end, nil
	default:
		return nil, 0, fmt.Errorf("unrecognized bencode tag %q", b[0])
	}
}

func indexByte(b []byte, c byte) int {
	for i, v := range b {
		if v == c {
			return i
		}
	}
	return -1
}

func TestBuildInfoDict_Structure(t *testing.T) {
	info := fixtureTorrentInfo()

	dict, err := BuildInfoDict(info)
	if err != nil {
		t.Fatalf("BuildInfoDict: %v", err)
	}

	decoded, n, err := decodeBencode(dict)
	if err != nil {
		t.Fatalf("decodeBencode: %v", err)
	}
	if n != len(dict) {
		t.Fatalf("decodeBencode consumed %d of %d bytes", n, len(dict))
	}

	m, ok := decoded.(map[string]any)
	if !ok {
		t.Fatalf("decoded info dict is not a dict: %T", decoded)
	}

	if got, want := string(m["name"].([]byte)), contentName; got != want {
		t.Errorf("name = %q, want %q", got, want)
	}
	if got, want := m["piece length"].(int64), int64(PieceSize); got != want {
		t.Errorf("piece length = %d, want %d", got, want)
	}

	pieces := m["pieces"].([]byte)
	if got, want := len(pieces), len(info.PieceHashes)*SHA1Size; got != want {
		t.Errorf("pieces length = %d, want %d", got, want)
	}

	files, ok := m["files"].([]any)
	if !ok || len(files) != len(info.Extents) {
		t.Fatalf("files = %#v, want %d entries", m["files"], len(info.Extents))
	}
	for i, f := range files {
		fm := f.(map[string]any)
		wantExtent := info.Extents[i] // BuildInfoDict sorts by offset; fixture is already ascending
		if got := fm["length"].(int64); got != int64(wantExtent.Length) {
			t.Errorf("files[%d].length = %d, want %d", i, got, wantExtent.Length)
		}
		path := fm["path"].([]any)
		if len(path) != 1 {
			t.Fatalf("files[%d].path = %#v, want a single segment", i, path)
		}
		if got, want := string(path[0].([]byte)), ExtentFileName(wantExtent.Offset); got != want {
			t.Errorf("files[%d].path[0] = %q, want %q", i, got, want)
		}
	}
}

func TestComputeInfoHash_DeterministicRegardlessOfExtentOrder(t *testing.T) {
	info := fixtureTorrentInfo()

	reordered := &TorrentInfo{
		BlockSize:   info.BlockSize,
		BlocksTotal: info.BlocksTotal,
		Extents:     []Extent{info.Extents[2], info.Extents[0], info.Extents[1]},
		PieceHashes: info.PieceHashes,
	}

	h1, err := ComputeInfoHash(info)
	if err != nil {
		t.Fatalf("ComputeInfoHash(info): %v", err)
	}
	h2, err := ComputeInfoHash(reordered)
	if err != nil {
		t.Fatalf("ComputeInfoHash(reordered): %v", err)
	}

	if h1 != h2 {
		t.Fatalf("info hash depends on Extents order: %s != %s", h1, h2)
	}
}

func TestComputeInfoHash_DiffersForDifferentContent(t *testing.T) {
	info := fixtureTorrentInfo()
	h1, err := ComputeInfoHash(info)
	if err != nil {
		t.Fatalf("ComputeInfoHash: %v", err)
	}

	changed := fixtureTorrentInfo()
	changed.Extents[0].Length++
	h2, err := ComputeInfoHash(changed)
	if err != nil {
		t.Fatalf("ComputeInfoHash(changed): %v", err)
	}

	if h1 == h2 {
		t.Fatal("ComputeInfoHash did not change when extent content changed")
	}
}

func TestComputeInfoHash_DiffersForDifferentPieceHashes(t *testing.T) {
	info := fixtureTorrentInfo()
	h1, err := ComputeInfoHash(info)
	if err != nil {
		t.Fatalf("ComputeInfoHash: %v", err)
	}

	changed := fixtureTorrentInfo()
	changed.PieceHashes[0][0]++
	h2, err := ComputeInfoHash(changed)
	if err != nil {
		t.Fatalf("ComputeInfoHash(changed): %v", err)
	}

	if h1 == h2 {
		t.Fatal("ComputeInfoHash did not change when piece hashes changed")
	}
}

func TestComputeInfoHash_MatchesSHA1OfInfoDict(t *testing.T) {
	info := fixtureTorrentInfo()

	dict, err := BuildInfoDict(info)
	if err != nil {
		t.Fatalf("BuildInfoDict: %v", err)
	}
	want := sha1.Sum(dict) //nolint:gosec // see the sha1 import comment

	got, err := ComputeInfoHash(info)
	if err != nil {
		t.Fatalf("ComputeInfoHash: %v", err)
	}

	if InfoHash(want) != got {
		t.Fatalf("ComputeInfoHash = %s, want sha1(BuildInfoDict(info)) = %s", got, InfoHash(want))
	}
}

func TestBuildTorrentFile(t *testing.T) {
	info := fixtureTorrentInfo()
	const tracker = "http://tracker.example.invalid:6969/announce"

	raw, err := BuildTorrentFile(info, tracker)
	if err != nil {
		t.Fatalf("BuildTorrentFile: %v", err)
	}

	decoded, n, err := decodeBencode(raw)
	if err != nil {
		t.Fatalf("decodeBencode: %v", err)
	}
	if n != len(raw) {
		t.Fatalf("decodeBencode consumed %d of %d bytes", n, len(raw))
	}

	m := decoded.(map[string]any)
	if got := string(m["announce"].([]byte)); got != tracker {
		t.Errorf("announce = %q, want %q", got, tracker)
	}
	if _, ok := m["info"].(map[string]any); !ok {
		t.Fatalf("info = %#v, want a dict", m["info"])
	}
}

// TestBuildTorrentFile_SameInfoDifferentTrackerSameHashDifferentAnnounce
// is the property section 4.1 of the design rests on: the info hash
// covers only the info dict, so two .torrent files built from the same
// TorrentInfo with different announce URLs carry the same info hash and
// a different announce - two Sites can serve the same content with
// their own tracker and it stays the same content.
func TestBuildTorrentFile_SameInfoDifferentTrackerSameHashDifferentAnnounce(t *testing.T) {
	info := fixtureTorrentInfo()
	const trackerA = "http://tracker-a.example.invalid:6969/announce"
	const trackerB = "http://tracker-b.example.invalid:6969/announce"

	rawA, err := BuildTorrentFile(info, trackerA)
	if err != nil {
		t.Fatalf("BuildTorrentFile(trackerA): %v", err)
	}
	rawB, err := BuildTorrentFile(info, trackerB)
	if err != nil {
		t.Fatalf("BuildTorrentFile(trackerB): %v", err)
	}

	if bytes.Equal(rawA, rawB) {
		t.Fatal("BuildTorrentFile produced identical bytes for two different tracker URLs")
	}

	decodedA, _, err := decodeBencode(rawA)
	if err != nil {
		t.Fatalf("decodeBencode(rawA): %v", err)
	}
	decodedB, _, err := decodeBencode(rawB)
	if err != nil {
		t.Fatalf("decodeBencode(rawB): %v", err)
	}
	mA := decodedA.(map[string]any)
	mB := decodedB.(map[string]any)

	if got := string(mA["announce"].([]byte)); got != trackerA {
		t.Errorf("announce(A) = %q, want %q", got, trackerA)
	}
	if got := string(mB["announce"].([]byte)); got != trackerB {
		t.Errorf("announce(B) = %q, want %q", got, trackerB)
	}

	// The info dicts themselves - and therefore ComputeInfoHash - must be
	// byte-identical: the announce URL sits outside the info dict, so it
	// cannot move the content address.
	infoBytesA, err := BuildInfoDict(info)
	if err != nil {
		t.Fatalf("BuildInfoDict: %v", err)
	}
	hashA, err := ComputeInfoHash(info)
	if err != nil {
		t.Fatalf("ComputeInfoHash: %v", err)
	}
	if !bytes.Contains(rawA, infoBytesA) || !bytes.Contains(rawB, infoBytesA) {
		t.Fatal("built .torrent bytes do not contain the same info dict for both trackers")
	}
	hashB, err := ComputeInfoHash(info)
	if err != nil {
		t.Fatalf("ComputeInfoHash: %v", err)
	}
	if hashA != hashB {
		t.Fatalf("info hash differs across trackers: %s != %s", hashA, hashB)
	}
}

func TestBuildTorrentFile_EmptyTracker(t *testing.T) {
	info := fixtureTorrentInfo()
	if _, err := BuildTorrentFile(info, ""); err == nil {
		t.Fatal("BuildTorrentFile: got nil error for empty tracker URL, want an error")
	}
}

func TestParseInfoHash_RoundTrips(t *testing.T) {
	var h InfoHash
	for i := range h {
		h[i] = byte(i)
	}

	got, err := ParseInfoHash(h.String())
	if err != nil {
		t.Fatalf("ParseInfoHash: %v", err)
	}
	if got != h {
		t.Errorf("ParseInfoHash(%q) = %x, want %x", h.String(), got, h)
	}
}

func TestParseInfoHash_Errors(t *testing.T) {
	cases := map[string]string{
		"too short":     "abcd",
		"too long":      fmt.Sprintf("%0*x", 2*InfoHashSize+2, 0),
		"non-hex chars": fmt.Sprintf("%0*s", 2*InfoHashSize, "zz"),
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseInfoHash(s); err == nil {
				t.Fatalf("ParseInfoHash(%q): got nil error, want one", s)
			}
		})
	}
}

// goldenInfoDictHex is the exact bencoded byte sequence BuildInfoDict
// produces for fixtureTorrentInfo(), captured once from the current
// implementation. BuildInfoDict's key order (files, name, piece length,
// pieces) is part of the content identity contract (see ComputeInfoHash):
// any byte-layout change here forks the info hash of every existing
// content. If this test fails, a change altered that layout and must not
// ship without a deliberate identity migration.
const goldenInfoDictHex = "64353a66696c65736c64363a6c656e677468693865343a706174686c33323a3030" +
	"303030303030303030303030303030303030303030303030303031303030656564" +
	"363a6c656e677468693365343a706174686c33323a303030303030303030303030" +
	"3030303030303030303030303030303032303030656564363a6c656e6774686935" +
	"65343a706174686c33323a30303030303030303030303030303030303030303030" +
	"30303030303033303030656565343a6e616d65373a636f6e74656e7431323a7069" +
	"656365206c656e67746869313637373732313665363a70696563657334303a1111" +
	"111111111111111111111111111111111111222222222222222222222222222222" +
	"222222222265"

// goldenInfoHashHex is the SHA-1 info hash of goldenInfoDictHex — the
// content identity ComputeInfoHash(fixtureTorrentInfo()) must keep
// producing. Pinned the same way and for the same reason as
// goldenInfoDictHex: a mismatch forks every existing content's identity.
const goldenInfoHashHex = "93aa15c7f3f0566a920616027c87c37cd44287d6"

func TestBuildInfoDict_GoldenBytes(t *testing.T) {
	info := fixtureTorrentInfo()

	dict, err := BuildInfoDict(info)
	if err != nil {
		t.Fatalf("BuildInfoDict: %v", err)
	}

	want, err := hex.DecodeString(goldenInfoDictHex)
	if err != nil {
		t.Fatalf("decode golden hex: %v", err)
	}

	if !bytes.Equal(dict, want) {
		t.Fatalf("BuildInfoDict byte layout changed:\n got  %x\n want %x\n"+
			"this forks the info hash of every existing content; see goldenInfoDictHex's doc comment",
			dict, want)
	}
}

func TestComputeInfoHash_GoldenIdentity(t *testing.T) {
	info := fixtureTorrentInfo()

	got, err := ComputeInfoHash(info)
	if err != nil {
		t.Fatalf("ComputeInfoHash: %v", err)
	}

	if got.String() != goldenInfoHashHex {
		t.Fatalf("ComputeInfoHash = %s, want golden %s; "+
			"this forks the info hash of every existing content; see goldenInfoHashHex's doc comment",
			got, goldenInfoHashHex)
	}
}

func TestBuildInfoDict_NoExtentsOrHashes(t *testing.T) {
	if _, err := BuildInfoDict(&TorrentInfo{PieceHashes: []PieceHash{{}}}); err == nil {
		t.Fatal("BuildInfoDict: got nil error for no extents, want an error")
	}
	if _, err := BuildInfoDict(&TorrentInfo{Extents: []Extent{{Offset: 0, Length: 1}}}); err == nil {
		t.Fatal("BuildInfoDict: got nil error for no piece hashes, want an error")
	}
}
