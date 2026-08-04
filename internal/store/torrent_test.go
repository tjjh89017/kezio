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
	"crypto/sha1" //nolint:gosec // test-only verification of the production package's own SHA-1 usage
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

func TestBuildInfoDict_NoExtentsOrHashes(t *testing.T) {
	if _, err := BuildInfoDict(&TorrentInfo{PieceHashes: []PieceHash{{}}}); err == nil {
		t.Fatal("BuildInfoDict: got nil error for no extents, want an error")
	}
	if _, err := BuildInfoDict(&TorrentInfo{Extents: []Extent{{Offset: 0, Length: 1}}}); err == nil {
		t.Fatal("BuildInfoDict: got nil error for no piece hashes, want an error")
	}
}
