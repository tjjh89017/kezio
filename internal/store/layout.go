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

// ValidateContentDir checks that dir holds exactly the extent files
// info describes: every Extent has a file named ExtentFileName(Offset)
// whose size equals Length, and dir has no other regular files besides
// torrent.info itself. It does not read file contents (piece hashes are
// taken on trust from torrent.info, per the design: no tool re-hashes the
// data).
func ValidateContentDir(dir string, info *TorrentInfo) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read content dir %s: %w", dir, err)
	}

	present := make(map[string]os.DirEntry, len(entries))
	for _, e := range entries {
		present[e.Name()] = e
	}

	expected := make(map[string]struct{}, len(info.Extents))
	for _, ext := range info.Extents {
		name := ExtentFileName(ext.Offset)
		expected[name] = struct{}{}

		entry, ok := present[name]
		if !ok {
			return fmt.Errorf("content dir %s: missing extent file %s (offset %#x, length %#x)", dir, name, ext.Offset, ext.Length)
		}
		if entry.IsDir() {
			return fmt.Errorf("content dir %s: extent file %s is a directory", dir, name)
		}
		fi, err := entry.Info()
		if err != nil {
			return fmt.Errorf("content dir %s: stat extent file %s: %w", dir, name, err)
		}
		if got := uint64(fi.Size()); got != ext.Length { //nolint:gosec // file sizes are non-negative
			return fmt.Errorf("content dir %s: extent file %s has size %#x, want %#x", dir, name, got, ext.Length)
		}
	}

	for name, entry := range present {
		if name == TorrentInfoFileName {
			continue
		}
		if _, ok := expected[name]; !ok {
			if entry.IsDir() {
				return fmt.Errorf("content dir %s: unexpected directory %s", dir, name)
			}
			return fmt.Errorf("content dir %s: unexpected file %s not listed in torrent.info", dir, name)
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
