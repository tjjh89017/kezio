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
	"reflect"
	"strings"
	"testing"
)

// fixtureTorrentInfo returns a small, deterministic TorrentInfo used across
// this package's tests: three extents and two piece hashes. The hash bytes
// are not real SHA-1 digests of anything; struct-level tests only need
// stable, distinguishable 20-byte values.
func fixtureTorrentInfo() *TorrentInfo {
	return &TorrentInfo{
		BlockSize:   4096,
		BlocksTotal: 100,
		Extents: []Extent{
			{Offset: 0x1000, Length: 8},
			{Offset: 0x2000, Length: 3},
			{Offset: 0x3000, Length: 5},
		},
		PieceHashes: []PieceHash{
			repeatByte(0x11),
			repeatByte(0x22),
		},
	}
}

func repeatByte(b byte) PieceHash {
	var h PieceHash
	for i := range h {
		h[i] = b
	}
	return h
}

func TestWriteThenParseTorrentInfo_RoundTrips(t *testing.T) {
	want := fixtureTorrentInfo()

	var buf bytes.Buffer
	if err := WriteTorrentInfo(&buf, want); err != nil {
		t.Fatalf("WriteTorrentInfo: %v", err)
	}

	got, err := ParseTorrentInfo(&buf)
	if err != nil {
		t.Fatalf("ParseTorrentInfo: %v", err)
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip mismatch:\n got=%+v\nwant=%+v", got, want)
	}
}

// hexPad renders v as lowercase hex, zero-padded to 32 digits, built
// independently of the production ExtentFileName/WriteTorrentInfo helpers
// (via strings.Repeat rather than a shared fmt verb) so this test exercises
// the field width partclone actually uses rather than whatever width the
// production code happens to format with.
func hexPad(hexDigits string) string {
	return strings.Repeat("0", 32-len(hexDigits)) + hexDigits
}

// TestParseTorrentInfo_MatchesPartcloneLayout hand-builds a torrent.info
// text that mirrors partclone's actual writer:
//   - block_size / blocks_total as decimal
//   - offset / length as 32-digit zero-padded lowercase hex
//   - sha1 as 40 lowercase hex digits, interleaved with the offset/length
//     pairs rather than following each one 1:1 (partclone only emits a
//     sha1 line when a 16 MiB piece boundary is crossed, so here the
//     first sha1 follows the first extent, then two more extents pass
//     before the next one).
func TestParseTorrentInfo_MatchesPartcloneLayout(t *testing.T) {
	hash1 := strings.Repeat("11", 20)
	hash2 := strings.Repeat("22", 20)

	text := "" +
		"block_size: 4096\n" +
		"blocks_total: 100\n" +
		"offset: " + hexPad("1000") + "\n" +
		"length: " + hexPad("8") + "\n" +
		"sha1: " + hash1 + "\n" +
		"offset: " + hexPad("2000") + "\n" +
		"length: " + hexPad("3") + "\n" +
		"offset: " + hexPad("3000") + "\n" +
		"length: " + hexPad("5") + "\n" +
		"sha1: " + hash2 + "\n"

	got, err := ParseTorrentInfo(strings.NewReader(text))
	if err != nil {
		t.Fatalf("ParseTorrentInfo: %v", err)
	}

	want := fixtureTorrentInfo()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parsed mismatch:\n got=%+v\nwant=%+v", got, want)
	}
}

func TestExtentFileName_MatchesOffsetFieldWidth(t *testing.T) {
	got := ExtentFileName(0x1000)
	want := hexPad("1000")
	if got != want {
		t.Fatalf("ExtentFileName(0x1000) = %q, want %q", got, want)
	}
	if len(got) != 32 {
		t.Fatalf("ExtentFileName length = %d, want 32", len(got))
	}
}

func TestParseTorrentInfo_Errors(t *testing.T) {
	cases := map[string]string{
		"missing block_size": "" +
			"blocks_total: 100\n" +
			"offset: " + hexPad("0") + "\n" +
			"length: " + hexPad("1") + "\n",
		"missing blocks_total": "" +
			"block_size: 4096\n" +
			"offset: " + hexPad("0") + "\n" +
			"length: " + hexPad("1") + "\n",
		"duplicate offset without length": "" +
			"block_size: 4096\n" +
			"blocks_total: 100\n" +
			"offset: " + hexPad("0") + "\n" +
			"offset: " + hexPad("1") + "\n",
		"length without offset": "" +
			"block_size: 4096\n" +
			"blocks_total: 100\n" +
			"length: " + hexPad("1") + "\n",
		"trailing dangling offset": "" +
			"block_size: 4096\n" +
			"blocks_total: 100\n" +
			"offset: " + hexPad("0") + "\n",
		"bad offset hex": "" +
			"block_size: 4096\n" +
			"blocks_total: 100\n" +
			"offset: not-hex\n" +
			"length: " + hexPad("1") + "\n",
		"sha1 wrong length": "" +
			"block_size: 4096\n" +
			"blocks_total: 100\n" +
			"sha1: deadbeef\n",
		"unrecognized line": "" +
			"block_size: 4096\n" +
			"blocks_total: 100\n" +
			"garbage line\n",
	}

	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseTorrentInfo(strings.NewReader(text)); err == nil {
				t.Fatalf("ParseTorrentInfo(%q): got nil error, want an error", text)
			}
		})
	}
}
