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

// Package store models the on-disk layout partclone and ezio agree on for a
// single partition's content: a directory of extent files plus a
// torrent.info metadata file, addressed by its BitTorrent v1 info hash. It
// parses and writes torrent.info, validates an extent-file directory
// against it, and builds the .torrent file the seeder and leechers use.
//
// A "content" here is the immutable, position-independent payload of one
// partition backup. Where that content is placed on a target disk (which
// partition slot) is Image-level concern and out of scope for this package.
package store

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// PieceSize is the BitTorrent v1 piece length ezio expects, in bytes: 16
// MiB. This value is fixed on both sides of the exchange - ezio's own
// torrent-building tooling hard-codes it, and it is the same default
// piece boundary partclone cuts on while writing torrent.info.
const PieceSize = 16 * 1024 * 1024

// SHA1Size is the length in bytes of one piece hash.
const SHA1Size = 20

// Extent is one used-block run partclone wrote as its own file: Offset is
// the byte offset into the partition, Length is the byte length. partclone
// -T (--btfiles) writes one such file per extent, named after Offset in the
// same lowercase, zero-padded 32-digit hex format as the torrent.info
// "offset:" line.
type Extent struct {
	Offset uint64
	Length uint64
}

// PieceHash is one BitTorrent v1 piece's SHA-1 hash, taken verbatim from a
// torrent.info "sha1:" line. Piece boundaries are cut on the concatenated
// extent data stream every PieceSize bytes, independent of extent
// boundaries: partclone emits a "sha1:" line whenever the running piece
// length reaches the piece size, then flushes a trailing partial piece at
// the end. Because of this, the number of PieceHash entries is not tied
// to the number of Extents.
type PieceHash [SHA1Size]byte

// TorrentInfo is the parsed content of a partclone torrent.info file.
type TorrentInfo struct {
	// BlockSize is the filesystem block size partclone used, in bytes.
	BlockSize uint32
	// BlocksTotal is the total block count of the source filesystem.
	BlocksTotal uint64
	// Extents lists the used-block runs, in the order torrent.info listed
	// them (partclone writes them in ascending offset order).
	Extents []Extent
	// PieceHashes lists the BitTorrent v1 piece hashes, in piece order.
	PieceHashes []PieceHash
}

// torrent.info line prefixes, exactly as partclone writes them.
// block_size and blocks_total are decimal; offset and length are
// lowercase hex, zero-padded to 32 digits; sha1 is 40 lowercase hex
// digits (20 bytes, two hex digits per byte).
const (
	prefixBlockSize   = "block_size: "
	prefixBlocksTotal = "blocks_total: "
	prefixOffset      = "offset: "
	prefixLength      = "length: "
	prefixSha1        = "sha1: "
)

// hexFieldWidth is the zero-padded width of the offset/length hex
// fields, matching the extent file name width.
const hexFieldWidth = 32

// ParseTorrentInfo parses a partclone torrent.info stream.
//
// torrent.info is a line-oriented text format: a "block_size:" line, a
// "blocks_total:" line, then a sequence of "offset:"/"length:" line pairs
// (one pair per extent) interleaved with "sha1:" lines (one per completed
// piece, emitted wherever the running piece boundary falls, so a "sha1:"
// line does not necessarily follow every extent pair). This parser collects
// offset/length into paired Extents and sha1 lines into an independently
// ordered PieceHashes list. This mirrors how ezio's own torrent-building
// tooling treats the file: offset, length, and sha1 are read as three
// separate ordered streams, not as one interleaved record type, which is
// why grouping them here loses no information (see WriteTorrentInfo).
// ParseTorrentInfoBytes parses a torrent.info file already held in
// memory - the shape ImagePartitionStatus.TorrentInfo carries it in,
// once ingest has copied it into status - rather than a torrent.info
// path on a mounted store.
func ParseTorrentInfoBytes(data []byte) (*TorrentInfo, error) {
	return ParseTorrentInfo(bytes.NewReader(data))
}

func ParseTorrentInfo(r io.Reader) (*TorrentInfo, error) {
	info := &TorrentInfo{}
	haveBlockSize := false
	haveBlocksTotal := false
	havePendingOffset := false
	var pendingOffset uint64

	scanner := bufio.NewScanner(r)
	// torrent.info lines are short (a header or one hex field); raise the
	// scanner's buffer only if a real file ever needs it is unnecessary,
	// the default 64KiB token limit is far larger than any single line.
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		switch {
		case cutPrefix(line, prefixBlockSize) != "":
			if haveBlockSize {
				return nil, fmt.Errorf("torrent.info:%d: duplicate block_size line", lineNo)
			}
			v, err := strconv.ParseUint(cutPrefix(line, prefixBlockSize), 10, 32)
			if err != nil {
				return nil, fmt.Errorf("torrent.info:%d: invalid block_size: %w", lineNo, err)
			}
			info.BlockSize = uint32(v)
			haveBlockSize = true
		case cutPrefix(line, prefixBlocksTotal) != "":
			if haveBlocksTotal {
				return nil, fmt.Errorf("torrent.info:%d: duplicate blocks_total line", lineNo)
			}
			v, err := strconv.ParseUint(cutPrefix(line, prefixBlocksTotal), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("torrent.info:%d: invalid blocks_total: %w", lineNo, err)
			}
			info.BlocksTotal = v
			haveBlocksTotal = true
		case cutPrefix(line, prefixOffset) != "":
			if havePendingOffset {
				return nil, fmt.Errorf("torrent.info:%d: offset line follows another offset line with no length line between them", lineNo)
			}
			v, err := strconv.ParseUint(cutPrefix(line, prefixOffset), 16, 64)
			if err != nil {
				return nil, fmt.Errorf("torrent.info:%d: invalid offset: %w", lineNo, err)
			}
			pendingOffset = v
			havePendingOffset = true
		case cutPrefix(line, prefixLength) != "":
			if !havePendingOffset {
				return nil, fmt.Errorf("torrent.info:%d: length line without a preceding offset", lineNo)
			}
			v, err := strconv.ParseUint(cutPrefix(line, prefixLength), 16, 64)
			if err != nil {
				return nil, fmt.Errorf("torrent.info:%d: invalid length: %w", lineNo, err)
			}
			info.Extents = append(info.Extents, Extent{Offset: pendingOffset, Length: v})
			havePendingOffset = false
		case cutPrefix(line, prefixSha1) != "":
			hexHash := cutPrefix(line, prefixSha1)
			if len(hexHash) != SHA1Size*2 {
				return nil, fmt.Errorf("torrent.info:%d: sha1 line has %d hex chars, want %d", lineNo, len(hexHash), SHA1Size*2)
			}
			var h PieceHash
			if _, err := hex.Decode(h[:], []byte(hexHash)); err != nil {
				return nil, fmt.Errorf("torrent.info:%d: invalid sha1: %w", lineNo, err)
			}
			info.PieceHashes = append(info.PieceHashes, h)
		case line == "":
			// tolerate a trailing blank line
		default:
			return nil, fmt.Errorf("torrent.info:%d: unrecognized line %q", lineNo, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("torrent.info: %w", err)
	}
	if havePendingOffset {
		return nil, errors.New("torrent.info: trailing offset line without a matching length")
	}
	if !haveBlockSize {
		return nil, errors.New("torrent.info: missing block_size line")
	}
	if !haveBlocksTotal {
		return nil, errors.New("torrent.info: missing blocks_total line")
	}
	return info, nil
}

// cutPrefix returns the remainder of line after prefix, or "" if line does
// not start with prefix. It is unambiguous here because every field this
// parser reads is non-empty (a zero-padded hex value or a decimal number),
// so an empty remainder can never be a valid match.
func cutPrefix(line, prefix string) string {
	if len(line) <= len(prefix) || line[:len(prefix)] != prefix {
		return ""
	}
	return line[len(prefix):]
}

// WriteTorrentInfo serializes info back into torrent.info form: the
// block_size/blocks_total header, then every Extent as an offset/length
// pair, then every PieceHash as a sha1 line.
//
// This grouped layout differs from partclone's own output, which
// interleaves offset/length pairs with sha1 lines as pieces complete
// mid-stream (see ParseTorrentInfo's doc comment). The two are
// semantically equivalent: every reader, including ezio's own tooling,
// treats offset/length and sha1 as independent ordered streams, so
// grouping them changes nothing a reader observes.
func WriteTorrentInfo(w io.Writer, info *TorrentInfo) error {
	bw := bufio.NewWriter(w)
	if _, err := fmt.Fprintf(bw, "%s%d\n", prefixBlockSize, info.BlockSize); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(bw, "%s%d\n", prefixBlocksTotal, info.BlocksTotal); err != nil {
		return err
	}
	for _, e := range info.Extents {
		if _, err := fmt.Fprintf(bw, "%s%0*x\n", prefixOffset, hexFieldWidth, e.Offset); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(bw, "%s%0*x\n", prefixLength, hexFieldWidth, e.Length); err != nil {
			return err
		}
	}
	for _, h := range info.PieceHashes {
		if _, err := fmt.Fprintf(bw, "%s%s\n", prefixSha1, hex.EncodeToString(h[:])); err != nil {
			return err
		}
	}
	return bw.Flush()
}

// ExtentFileName returns the extent file name partclone -T (--btfiles)
// uses for offset: a lowercase hex value zero-padded to 32 digits.
func ExtentFileName(offset uint64) string {
	return fmt.Sprintf("%0*x", hexFieldWidth, offset)
}
