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
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// writeFixtureContentDir materializes fixtureTorrentInfo() as a published
// content directory: torrent.info directly inside dir, plus one extent
// file per Extent nested under dir's content/ data subdirectory (see
// ContentDataDir) - the layout ValidateContentDir expects after
// NestExtentFiles has run, i.e. what a real publishContent leaves behind.
// Each extent file is filled with arbitrary bytes of the right length. It
// returns the directory path.
func writeFixtureContentDir(t *testing.T, info *TorrentInfo) string {
	t.Helper()
	dir := t.TempDir()

	var buf bytes.Buffer
	if err := WriteTorrentInfo(&buf, info); err != nil {
		t.Fatalf("WriteTorrentInfo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, TorrentInfoFileName), buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write torrent.info: %v", err)
	}

	dataDir := ContentDataDir(dir)
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		t.Fatalf("create content data dir: %v", err)
	}
	for _, e := range info.Extents {
		data := bytes.Repeat([]byte{0xAB}, int(e.Length))
		path := filepath.Join(dataDir, ExtentFileName(e.Offset))
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatalf("write extent file %s: %v", path, err)
		}
	}
	return dir
}

// writeFlatFixtureContentDir materializes fixtureTorrentInfo() the way
// partclone -T actually writes it: torrent.info and every extent file
// flat, directly inside dir, with no content/ data subdirectory yet -
// the shape NestExtentFiles takes as input.
func writeFlatFixtureContentDir(t *testing.T, info *TorrentInfo) string {
	t.Helper()
	dir := t.TempDir()

	var buf bytes.Buffer
	if err := WriteTorrentInfo(&buf, info); err != nil {
		t.Fatalf("WriteTorrentInfo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, TorrentInfoFileName), buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write torrent.info: %v", err)
	}

	for _, e := range info.Extents {
		data := bytes.Repeat([]byte{0xAB}, int(e.Length))
		path := filepath.Join(dir, ExtentFileName(e.Offset))
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatalf("write extent file %s: %v", path, err)
		}
	}
	return dir
}

func TestValidateContentDir_Valid(t *testing.T) {
	info := fixtureTorrentInfo()
	dir := writeFixtureContentDir(t, info)

	if err := ValidateContentDir(dir, info); err != nil {
		t.Fatalf("ValidateContentDir: %v", err)
	}
}

func TestValidateContentDir_MissingFile(t *testing.T) {
	info := fixtureTorrentInfo()
	dir := writeFixtureContentDir(t, info)

	missing := filepath.Join(ContentDataDir(dir), ExtentFileName(info.Extents[0].Offset))
	if err := os.Remove(missing); err != nil {
		t.Fatalf("remove fixture extent file: %v", err)
	}

	err := ValidateContentDir(dir, info)
	if err == nil {
		t.Fatal("ValidateContentDir: got nil error, want a missing-file error")
	}
}

func TestValidateContentDir_SizeMismatch(t *testing.T) {
	info := fixtureTorrentInfo()
	dir := writeFixtureContentDir(t, info)

	corrupt := filepath.Join(ContentDataDir(dir), ExtentFileName(info.Extents[1].Offset))
	if err := os.WriteFile(corrupt, []byte{0x00}, 0o644); err != nil {
		t.Fatalf("corrupt fixture extent file: %v", err)
	}

	err := ValidateContentDir(dir, info)
	if err == nil {
		t.Fatal("ValidateContentDir: got nil error, want a size-mismatch error")
	}
}

func TestValidateContentDir_UnexpectedFile(t *testing.T) {
	info := fixtureTorrentInfo()
	dir := writeFixtureContentDir(t, info)

	stray := filepath.Join(ContentDataDir(dir), ExtentFileName(0xdead))
	if err := os.WriteFile(stray, []byte{0x01}, 0o644); err != nil {
		t.Fatalf("write stray file: %v", err)
	}

	err := ValidateContentDir(dir, info)
	if err == nil {
		t.Fatal("ValidateContentDir: got nil error, want an unexpected-file error")
	}
}

// TestValidateContentDir_UnexpectedTopLevelEntry pins the structural check
// that dir itself may only hold torrent.info and the content/ data
// subdirectory - this is what would have caught the extent-files-left-flat
// layout bug (see NestExtentFiles's doc comment): a stray file sitting
// directly in dir, outside content/, must fail loudly instead of being
// silently ignored.
func TestValidateContentDir_UnexpectedTopLevelEntry(t *testing.T) {
	info := fixtureTorrentInfo()
	dir := writeFixtureContentDir(t, info)

	stray := filepath.Join(dir, ExtentFileName(info.Extents[0].Offset))
	if err := os.WriteFile(stray, []byte{0x01}, 0o644); err != nil {
		t.Fatalf("write stray top-level file: %v", err)
	}

	err := ValidateContentDir(dir, info)
	if err == nil {
		t.Fatal("ValidateContentDir: got nil error, want an unexpected-top-level-entry error")
	}
}

// TestValidateContentDir_DataDirIsFile pins that content/ must be a
// directory: a regular file at that path (e.g. from a corrupted publish)
// must fail instead of ValidateContentDir trying to os.ReadDir a file.
func TestValidateContentDir_DataDirIsFile(t *testing.T) {
	info := fixtureTorrentInfo()
	dir := writeFixtureContentDir(t, info)

	dataDir := ContentDataDir(dir)
	if err := os.RemoveAll(dataDir); err != nil {
		t.Fatalf("remove content data dir: %v", err)
	}
	if err := os.WriteFile(dataDir, []byte{0x01}, 0o644); err != nil {
		t.Fatalf("write file in place of content data dir: %v", err)
	}

	err := ValidateContentDir(dir, info)
	if err == nil {
		t.Fatal("ValidateContentDir: got nil error, want a content-is-not-a-directory error")
	}
}

func TestNestExtentFiles(t *testing.T) {
	info := fixtureTorrentInfo()
	dir := writeFlatFixtureContentDir(t, info)

	if err := NestExtentFiles(dir, info); err != nil {
		t.Fatalf("NestExtentFiles: %v", err)
	}

	// Every extent file moved into the content/ data subdirectory...
	for _, e := range info.Extents {
		name := ExtentFileName(e.Offset)
		if _, err := os.Stat(filepath.Join(ContentDataDir(dir), name)); err != nil {
			t.Errorf("extent file %s missing from content data dir: %v", name, err)
		}
		// ...and no longer sits flat in dir.
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("extent file %s still present flat in dir, stat err = %v", name, err)
		}
	}

	// torrent.info is untouched: it stays directly in dir, not nested.
	if _, err := os.Stat(filepath.Join(dir, TorrentInfoFileName)); err != nil {
		t.Errorf("torrent.info missing from dir after NestExtentFiles: %v", err)
	}

	// The result is exactly what ValidateContentDir expects.
	if err := ValidateContentDir(dir, info); err != nil {
		t.Errorf("ValidateContentDir after NestExtentFiles: %v", err)
	}
}

func TestLoadContentDirTorrentInfo(t *testing.T) {
	info := fixtureTorrentInfo()
	dir := writeFixtureContentDir(t, info)

	got, err := LoadContentDirTorrentInfo(dir)
	if err != nil {
		t.Fatalf("LoadContentDirTorrentInfo: %v", err)
	}
	if !reflect.DeepEqual(got, info) {
		t.Fatalf("LoadContentDirTorrentInfo mismatch:\n got=%+v\nwant=%+v", got, info)
	}
}
