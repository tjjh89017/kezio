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

package ingest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tjjh89017/kezio/internal/store"
)

// writePublishedFixture writes a content directory already in the
// published, nested form Run's finalizeContent leaves behind (extent
// files under content/, not flat alongside torrent.info): what
// RunPublish always receives as a PublishPartition.SourceDir in
// production.
func writePublishedFixture(t *testing.T, dir string, content []byte) {
	t.Helper()
	writeFixtureContentDir(t, dir, content)
	info, err := store.LoadContentDirTorrentInfo(dir)
	if err != nil {
		t.Fatalf("LoadContentDirTorrentInfo: %v", err)
	}
	if err := store.NestExtentFiles(dir, info); err != nil {
		t.Fatalf("NestExtentFiles: %v", err)
	}
}

func TestRunPublish_CopiesValidatesAndBuildsTorrent(t *testing.T) {
	sourceDir := t.TempDir()
	writePublishedFixture(t, sourceDir, []byte("payload"))
	destDir := filepath.Join(t.TempDir(), "content-dest")

	result := RunPublish(PublishConfig{
		TrackerURL: "http://tracker.example.com:6969/announce",
		Partitions: []PublishPartition{{Number: 1, SourceDir: sourceDir, DestDir: destDir}},
	})
	if !result.Success {
		t.Fatalf("RunPublish: Success=false, Error=%q", result.Error)
	}

	// ValidateContentDir is not re-checked here: it requires an exact
	// directory listing, and destDir also holds the .torrent this run
	// just wrote alongside torrent.info (see publishPartition, which
	// validates before writing the .torrent).
	extentPath := filepath.Join(store.ContentDataDir(destDir), store.ExtentFileName(0))
	extentData, err := os.ReadFile(extentPath) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("read copied extent: %v", err)
	}
	if len(extentData) != len("payload") {
		t.Errorf("len(extentData) = %d, want %d", len(extentData), len("payload"))
	}

	torrentPath := store.ContentTorrentPath(destDir)
	data, err := os.ReadFile(torrentPath) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("read %s: %v", torrentPath, err)
	}
	if len(data) == 0 {
		t.Error("written .torrent file is empty")
	}
}

func TestRunPublish_MultiplePartitions(t *testing.T) {
	source1 := t.TempDir()
	writePublishedFixture(t, source1, []byte("payload-one"))
	source2 := t.TempDir()
	writePublishedFixture(t, source2, []byte("payload-two-longer"))

	dest1 := filepath.Join(t.TempDir(), "p1")
	dest2 := filepath.Join(t.TempDir(), "p3")

	result := RunPublish(PublishConfig{
		TrackerURL: "http://tracker.example.com:6969/announce",
		Partitions: []PublishPartition{
			{Number: 1, SourceDir: source1, DestDir: dest1},
			{Number: 3, SourceDir: source2, DestDir: dest2},
		},
	})
	if !result.Success {
		t.Fatalf("RunPublish: Success=false, Error=%q", result.Error)
	}

	for _, dest := range []string{dest1, dest2} {
		if _, err := os.Stat(store.ContentTorrentPath(dest)); err != nil {
			t.Errorf("missing .torrent at %s: %v", dest, err)
		}
	}
}

func TestRunPublish_EmptyTrackerURLFails(t *testing.T) {
	sourceDir := t.TempDir()
	writeFixtureContentDir(t, sourceDir, []byte("payload"))
	destDir := filepath.Join(t.TempDir(), "content-dest")

	result := RunPublish(PublishConfig{
		Partitions: []PublishPartition{{Number: 1, SourceDir: sourceDir, DestDir: destDir}},
	})
	if result.Success {
		t.Fatal("RunPublish: expected failure when TrackerURL is empty")
	}
	if result.Error == "" {
		t.Error("result.Error is empty, want a message naming the missing tracker URL")
	}
	if _, err := os.Stat(destDir); !os.IsNotExist(err) {
		t.Errorf("expected no content to be copied when publish fails before writing anything, stat err = %v", err)
	}
}

func TestRunPublish_MissingSourceContentIsAFailure(t *testing.T) {
	result := RunPublish(PublishConfig{
		TrackerURL: "http://tracker.example.com:6969/announce",
		Partitions: []PublishPartition{{Number: 1, SourceDir: filepath.Join(t.TempDir(), "missing"), DestDir: t.TempDir()}},
	})
	if result.Success {
		t.Fatal("RunPublish: Success=true, want a failure for a source content dir that does not exist")
	}
	if result.Error == "" {
		t.Error("result.Error is empty, want a message naming the missing content")
	}
}

func TestContentMountPath(t *testing.T) {
	var hash store.InfoHash
	for i := range hash {
		hash[i] = 0x42
	}
	want := ContentMountRoot + "/" + store.PVCName(hash)
	if got := ContentMountPath(hash); got != want {
		t.Errorf("ContentMountPath = %q, want %q", got, want)
	}
}
