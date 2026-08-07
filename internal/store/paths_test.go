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
	"path/filepath"
	"testing"
)

func TestContentPaths(t *testing.T) {
	root := "/store"
	var hash InfoHash
	for i := range hash {
		hash[i] = byte(i)
	}

	dir := ContentDir(root, hash)
	wantDir := filepath.Join(root, "contents", hash.String())
	if dir != wantDir {
		t.Errorf("ContentDir = %q, want %q", dir, wantDir)
	}

	if got, want := ContentTorrentInfoPath(root, hash), filepath.Join(dir, TorrentInfoFileName); got != want {
		t.Errorf("ContentTorrentInfoPath = %q, want %q", got, want)
	}
	if got, want := ContentTorrentPath(root, hash), filepath.Join(dir, ContentTorrentFileName); got != want {
		t.Errorf("ContentTorrentPath = %q, want %q", got, want)
	}
	if got, want := ContentExtentPath(root, hash, 0x1000), filepath.Join(dir, contentName, ExtentFileName(0x1000)); got != want {
		t.Errorf("ContentExtentPath = %q, want %q", got, want)
	}
	if got, want := ContentDataDir(dir), filepath.Join(dir, contentName); got != want {
		t.Errorf("ContentDataDir = %q, want %q", got, want)
	}
}

func TestContentDir_SameHashSamePath(t *testing.T) {
	root := "/store"
	var h1, h2 InfoHash
	for i := range h1 {
		h1[i] = 0x42
		h2[i] = 0x42
	}

	if ContentDir(root, h1) != ContentDir(root, h2) {
		t.Fatal("ContentDir differs for equal info hashes")
	}
}

func TestContentDir_DifferentHashDifferentPath(t *testing.T) {
	root := "/store"
	var h1, h2 InfoHash
	h2[0] = 0x01

	if ContentDir(root, h1) == ContentDir(root, h2) {
		t.Fatal("ContentDir collides for different info hashes")
	}
}

func TestImagePaths(t *testing.T) {
	root := "/store"
	imageName := "ubuntu-24-04"

	dir := ImageDir(root, imageName)
	wantDir := filepath.Join(root, "images", imageName)
	if dir != wantDir {
		t.Errorf("ImageDir = %q, want %q", dir, wantDir)
	}

	if got, want := ImageLayoutPath(root, imageName), filepath.Join(dir, ImageLayoutFileName); got != want {
		t.Errorf("ImageLayoutPath = %q, want %q", got, want)
	}
}

func TestIngestScratchDir(t *testing.T) {
	root := "/store"
	imageName := "ubuntu-24-04"

	dir := IngestScratchDir(root, imageName)
	wantDir := filepath.Join(root, ".ingest", imageName)
	if dir != wantDir {
		t.Errorf("IngestScratchDir = %q, want %q", dir, wantDir)
	}

	// Must live directly under root (not under contents/ or images/) so
	// a rename into contents/ is guaranteed same-device.
	if filepath.Dir(filepath.Dir(dir)) != root {
		t.Errorf("IngestScratchDir %q is not two levels below root %q", dir, root)
	}
}

func TestIngestScratchDir_DifferentImagesDoNotCollide(t *testing.T) {
	root := "/store"
	if IngestScratchDir(root, "image-a") == IngestScratchDir(root, "image-b") {
		t.Fatal("IngestScratchDir collides for different image names")
	}
}
