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

// writeFixtureContentDir materializes fixtureTorrentInfo() as a content
// directory: torrent.info plus one extent file per Extent, each filled
// with arbitrary bytes of the right length. It returns the directory path.
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

	missing := filepath.Join(dir, ExtentFileName(info.Extents[0].Offset))
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

	corrupt := filepath.Join(dir, ExtentFileName(info.Extents[1].Offset))
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

	stray := filepath.Join(dir, ExtentFileName(0xdead))
	if err := os.WriteFile(stray, []byte{0x01}, 0o644); err != nil {
		t.Fatalf("write stray file: %v", err)
	}

	err := ValidateContentDir(dir, info)
	if err == nil {
		t.Fatal("ValidateContentDir: got nil error, want an unexpected-file error")
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
