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
	"crypto/sha1" //nolint:gosec // SHA-1 is BitTorrent v1's mandated piece/info-hash algorithm, not used for any security property here
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
)

// InfoHashSize is the length in bytes of a BitTorrent v1 info hash.
const InfoHashSize = sha1.Size

// InfoHash identifies a partition content by the SHA-1 hash of its
// BitTorrent v1 bencoded info dict. It is also the identity a
// PartitionContent object's name is derived from (see ObjectName).
type InfoHash [InfoHashSize]byte

// contentName is the fixed "name" field of every content torrent this
// package builds. Content is addressed purely by info hash (extent
// layout + piece hashes, see BuildInfoDict), so a constant name keeps
// hash computation deterministic without a caller-supplied naming
// scheme. ezio never reads this field: a consuming client recovers a
// file's partition offset from its own leaf name (hex), not the
// directory name.
const contentName = "content"

// BuildInfoDict bencodes info's BitTorrent v1 info dict: one file per
// extent (named by ExtentFileName, so ezio can recover each file's
// partition offset), a fixed piece length of PieceSize, and the piece
// hashes taken verbatim from torrent.info — mirroring how ezio's own
// torrent-building tooling assembles the same info dict from the same
// three streams.
//
// Extents are sorted by Offset before encoding, so the result depends only
// on the set of (offset, length) extents and the piece hash sequence, not
// on the order torrent.info happened to list them in.
func BuildInfoDict(info *TorrentInfo) ([]byte, error) {
	if len(info.Extents) == 0 {
		return nil, errors.New("build info dict: no extents")
	}
	if len(info.PieceHashes) == 0 {
		return nil, errors.New("build info dict: no piece hashes")
	}

	extents := make([]Extent, len(info.Extents))
	copy(extents, info.Extents)
	sort.Slice(extents, func(i, j int) bool { return extents[i].Offset < extents[j].Offset })

	var buf bytes.Buffer
	buf.WriteByte('d')

	bencodeString(&buf, []byte("files"))
	buf.WriteByte('l')
	for _, e := range extents {
		bencodeExtentFile(&buf, ExtentFileName(e.Offset), e.Length)
	}
	buf.WriteByte('e')

	bencodeString(&buf, []byte("name"))
	bencodeString(&buf, []byte(contentName))

	bencodeString(&buf, []byte("piece length"))
	bencodeInt(&buf, PieceSize)

	pieces := make([]byte, 0, len(info.PieceHashes)*SHA1Size)
	for _, h := range info.PieceHashes {
		pieces = append(pieces, h[:]...)
	}
	bencodeString(&buf, []byte("pieces"))
	bencodeString(&buf, pieces)

	buf.WriteByte('e')
	return buf.Bytes(), nil
}

// ComputeInfoHash returns the BitTorrent v1 info hash for info: the SHA-1
// of its bencoded info dict (BuildInfoDict). Two TorrentInfo values that
// describe the same content (same extents, same piece hashes) always
// produce the same InfoHash, regardless of Extents order, which is what
// makes it a valid content address.
func ComputeInfoHash(info *TorrentInfo) (InfoHash, error) {
	dict, err := BuildInfoDict(info)
	if err != nil {
		return InfoHash{}, err
	}
	return sha1.Sum(dict), nil //nolint:gosec // see the sha1 import comment
}

// BuildTorrentFile bencodes a complete single-tracker .torrent file for
// info: {"announce": trackerURL, "info": <BuildInfoDict(info)>}.
func BuildTorrentFile(info *TorrentInfo, trackerURL string) ([]byte, error) {
	if trackerURL == "" {
		return nil, errors.New("build torrent file: empty tracker URL")
	}
	dict, err := BuildInfoDict(info)
	if err != nil {
		return nil, fmt.Errorf("build torrent file: %w", err)
	}

	var buf bytes.Buffer
	buf.WriteByte('d')
	bencodeString(&buf, []byte("announce"))
	bencodeString(&buf, []byte(trackerURL))
	bencodeString(&buf, []byte("info"))
	buf.Write(dict)
	buf.WriteByte('e')
	return buf.Bytes(), nil
}

// ParseInfoHash parses s, the lowercase-hex form InfoHash.String() and a
// PartitionContent's name (minus its "pc-" prefix) both use, back into an
// InfoHash. It is the inverse of String.
func ParseInfoHash(s string) (InfoHash, error) {
	var h InfoHash
	if len(s) != 2*InfoHashSize {
		return h, fmt.Errorf("parse info hash: %q has %d hex chars, want %d", s, len(s), 2*InfoHashSize)
	}
	if _, err := hex.Decode(h[:], []byte(s)); err != nil {
		return h, fmt.Errorf("parse info hash %q: %w", s, err)
	}
	return h, nil
}

// String renders the info hash as lowercase hex, the form used in store
// paths, object names, and log messages.
func (h InfoHash) String() string {
	const hexDigits = "0123456789abcdef"
	out := make([]byte, 2*InfoHashSize)
	for i, b := range h {
		out[2*i] = hexDigits[b>>4]
		out[2*i+1] = hexDigits[b&0x0f]
	}
	return string(out)
}
