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
	"fmt"
	"os"
	"path/filepath"
)

// TorrentInfoFileName is the name partclone -T (--btfiles) gives its
// metadata file inside the target directory.
const TorrentInfoFileName = "torrent.info"

// ValidateContentDir checks that dir holds exactly torrent.info and a
// content/ data subdirectory (see ContentDataDir) containing exactly the
// extent files info describes: every Extent has a file named
// ExtentFileName(Offset) whose size equals Length, and the data
// subdirectory has no other regular files. It does not read file
// contents (piece hashes are taken on trust from torrent.info, per the
// design: no tool re-hashes the data).
func ValidateContentDir(dir string, info *TorrentInfo) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read content dir %s: %w", dir, err)
	}
	for _, e := range entries {
		switch e.Name() {
		case TorrentInfoFileName:
			if e.IsDir() {
				return fmt.Errorf("content dir %s: %s is a directory, want a regular file", dir, TorrentInfoFileName)
			}
		case contentName:
			if !e.IsDir() {
				return fmt.Errorf("content dir %s: %s is a regular file, want a directory", dir, contentName)
			}
		default:
			return fmt.Errorf("content dir %s: unexpected entry %s", dir, e.Name())
		}
	}

	dataDir := ContentDataDir(dir)
	dataEntries, err := os.ReadDir(dataDir)
	if err != nil {
		return fmt.Errorf("read content data dir %s: %w", dataDir, err)
	}

	present := make(map[string]os.DirEntry, len(dataEntries))
	for _, e := range dataEntries {
		present[e.Name()] = e
	}

	expected := make(map[string]struct{}, len(info.Extents))
	for _, ext := range info.Extents {
		name := ExtentFileName(ext.Offset)
		expected[name] = struct{}{}

		entry, ok := present[name]
		if !ok {
			return fmt.Errorf("content data dir %s: missing extent file %s (offset %#x, length %#x)", dataDir, name, ext.Offset, ext.Length)
		}
		if entry.IsDir() {
			return fmt.Errorf("content data dir %s: extent file %s is a directory", dataDir, name)
		}
		fi, err := entry.Info()
		if err != nil {
			return fmt.Errorf("content data dir %s: stat extent file %s: %w", dataDir, name, err)
		}
		if got := uint64(fi.Size()); got != ext.Length { //nolint:gosec // file sizes are non-negative
			return fmt.Errorf("content data dir %s: extent file %s has size %#x, want %#x", dataDir, name, got, ext.Length)
		}
	}

	for name, entry := range present {
		if _, ok := expected[name]; !ok {
			if entry.IsDir() {
				return fmt.Errorf("content data dir %s: unexpected directory %s", dataDir, name)
			}
			return fmt.Errorf("content data dir %s: unexpected file %s not listed in torrent.info", dataDir, name)
		}
	}

	return nil
}

// ContentDataDir returns dir's extent-file data subdirectory: the
// directory a BitTorrent v1 multi-file client resolves a file entry's
// path against once save_path is dir - i.e. dir/contentName, since
// BuildInfoDict's info dict always sets "name" to contentName. dir is
// typically a content directory (ContentDir's return value) but may be
// any save_path a client is pointed at (e.g. a leecher's empty target
// directory), which is why this takes a plain dir rather than an
// InfoHash.
func ContentDataDir(dir string) string {
	return filepath.Join(dir, contentName)
}

// NestExtentFiles moves each of info's extent files from directly inside
// dir - where partclone -T (--btfiles) writes them, flat alongside
// torrent.info, since partclone has no option to nest its output - into
// dir's content/ data subdirectory (ContentDataDir), so the on-disk
// layout matches what the torrent's own file entries already imply (see
// ContentDataDir's doc comment). Call this once, right after partclone
// finishes and before ValidateContentDir, on every content directory
// this package's caller publishes.
func NestExtentFiles(dir string, info *TorrentInfo) error {
	dataDir := ContentDataDir(dir)
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return fmt.Errorf("create content data dir %s: %w", dataDir, err)
	}
	for _, ext := range info.Extents {
		name := ExtentFileName(ext.Offset)
		if err := os.Rename(filepath.Join(dir, name), filepath.Join(dataDir, name)); err != nil {
			return fmt.Errorf("nest extent file %s into %s: %w", name, dataDir, err)
		}
	}
	return nil
}

// ContentDirTorrentInfoPath returns the torrent.info path inside a content
// directory.
func ContentDirTorrentInfoPath(dir string) string {
	return filepath.Join(dir, TorrentInfoFileName)
}

// LoadContentDirTorrentInfo reads and parses the torrent.info file inside a
// content directory.
func LoadContentDirTorrentInfo(dir string) (*TorrentInfo, error) {
	f, err := os.Open(ContentDirTorrentInfoPath(dir)) //nolint:gosec // dir is an operator-controlled store path, not user input
	if err != nil {
		return nil, fmt.Errorf("open torrent.info in %s: %w", dir, err)
	}
	defer f.Close() //nolint:errcheck // read-only fd close, nothing actionable on failure

	info, err := ParseTorrentInfo(f)
	if err != nil {
		return nil, fmt.Errorf("content dir %s: %w", dir, err)
	}
	return info, nil
}
