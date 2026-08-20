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

package leech

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tjjh89017/kezio/internal/seeder"
	"github.com/tjjh89017/kezio/internal/store"
)

func TestFetchTorrent_ReturnsBody(t *testing.T) {
	want := []byte("d8:announce...e")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(want)
	}))
	defer srv.Close()

	got, err := FetchTorrent(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("FetchTorrent: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("FetchTorrent body = %q, want %q", got, want)
	}
}

func TestFetchTorrent_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	if _, err := FetchTorrent(context.Background(), srv.Client(), srv.URL); err == nil {
		t.Fatal("FetchTorrent: got nil error, want an error for a 404")
	}
}

// fakeChecker is a minimal EzioClient stub driving WaitFinished/Run
// without a real ezio daemon.
type fakeChecker struct {
	mu     sync.Mutex
	addErr error
	added  []struct {
		torrent  []byte
		savePath string
		seedMode bool
	}
	finishedAt  int
	statusCalls int
	statusErr   error
}

func (f *fakeChecker) AddTorrent(_ context.Context, torrent []byte, savePath string, seedMode bool, _, _ int32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.addErr != nil {
		return f.addErr
	}
	f.added = append(f.added, struct {
		torrent  []byte
		savePath string
		seedMode bool
	}{torrent, savePath, seedMode})
	return nil
}

func (f *fakeChecker) GetTorrentStatus(_ context.Context, hashes []string) (map[string]seeder.Torrent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	f.statusCalls++
	finished := f.statusCalls > f.finishedAt
	out := map[string]seeder.Torrent{}
	for _, h := range hashes {
		out[h] = seeder.Torrent{Hash: h, IsFinished: finished}
	}
	return out, nil
}

func TestWaitFinished_ReturnsOnceFinished(t *testing.T) {
	checker := &fakeChecker{finishedAt: 2}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := WaitFinished(ctx, checker, "abc", time.Millisecond); err != nil {
		t.Fatalf("WaitFinished: %v", err)
	}
}

func TestWaitFinished_TimesOut(t *testing.T) {
	checker := &fakeChecker{finishedAt: 1_000_000}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	if err := WaitFinished(ctx, checker, "abc", time.Millisecond); err == nil {
		t.Fatal("WaitFinished: got nil error, want a timeout error")
	}
}

func TestWaitFinished_PropagatesStatusError(t *testing.T) {
	checker := &fakeChecker{statusErr: errors.New("boom")}
	if err := WaitFinished(context.Background(), checker, "abc", time.Millisecond); err == nil {
		t.Fatal("WaitFinished: got nil error, want the status error")
	}
}

func TestReconstruct_PlacesExtentsAtOffsetsAndZerosTheRest(t *testing.T) {
	dir := t.TempDir()
	extentsDir := filepath.Join(dir, "extents")
	if err := os.MkdirAll(extentsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	const partitionSize = 64
	writeExtent := func(offset uint64, data []byte) {
		name := store.ExtentFileName(offset)
		if err := os.WriteFile(filepath.Join(extentsDir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeExtent(0, []byte("HEAD"))
	writeExtent(32, []byte("TAIL"))

	outPath := filepath.Join(dir, "out.raw")
	if err := Reconstruct(extentsDir, outPath, partitionSize); err != nil {
		t.Fatalf("Reconstruct: %v", err)
	}

	got, err := os.ReadFile(outPath) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != partitionSize {
		t.Fatalf("reconstructed file size = %d, want %d", len(got), partitionSize)
	}
	want := make([]byte, partitionSize)
	copy(want[0:], "HEAD")
	copy(want[32:], "TAIL")
	if string(got) != string(want) {
		t.Fatalf("reconstructed bytes = %q, want %q", got, want)
	}
}

func TestReconstruct_RejectsExtentPastPartitionSize(t *testing.T) {
	dir := t.TempDir()
	extentsDir := filepath.Join(dir, "extents")
	if err := os.MkdirAll(extentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := store.ExtentFileName(60)
	if err := os.WriteFile(filepath.Join(extentsDir, name), []byte("too-long-for-the-partition"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Reconstruct(extentsDir, filepath.Join(dir, "out.raw"), 64); err == nil {
		t.Fatal("Reconstruct: got nil error, want an out-of-range error")
	}
}

func TestSHA256File_MatchesKnownDigest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	content := []byte("hello, kezio")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(content)

	got, err := SHA256File(path)
	if err != nil {
		t.Fatalf("SHA256File: %v", err)
	}
	if got != hex.EncodeToString(want[:]) {
		t.Fatalf("SHA256File = %s, want %s", got, hex.EncodeToString(want[:]))
	}
}

func TestRun_EndToEnd_MatchingDigest(t *testing.T) {
	dir := t.TempDir()
	torrentBytes := []byte("fake-torrent-bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(torrentBytes)
	}))
	defer srv.Close()

	savePath := filepath.Join(dir, "save")
	extentsDir := store.ContentDataDir(savePath)
	if err := os.MkdirAll(extentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extentsDir, store.ExtentFileName(0)), []byte("DATA"), 0o644); err != nil {
		t.Fatal(err)
	}

	checker := &fakeChecker{finishedAt: 0}
	outPath := filepath.Join(dir, "out.raw")
	opts := Options{
		TorrentURL:         srv.URL,
		InfoHash:           "deadbeef",
		SavePath:           savePath,
		OutPath:            outPath,
		PartitionSizeBytes: 8,
		PollInterval:       time.Millisecond,
	}

	want := make([]byte, 8)
	copy(want, "DATA")
	sum := sha256.Sum256(want)
	opts.WantSHA256 = hex.EncodeToString(sum[:])

	result, err := Run(context.Background(), checker, srv.Client(), opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.SHA256 != opts.WantSHA256 {
		t.Fatalf("Run result.SHA256 = %s, want %s", result.SHA256, opts.WantSHA256)
	}
	if len(checker.added) != 1 {
		t.Fatalf("AddTorrent calls = %d, want 1", len(checker.added))
	}
	if checker.added[0].seedMode {
		t.Fatal("Run added the torrent in seedMode=true, want false for a leech")
	}
}

func TestRun_MismatchedDigestIsAnError(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("torrent"))
	}))
	defer srv.Close()

	savePath := filepath.Join(dir, "save")
	extentsDir := store.ContentDataDir(savePath)
	if err := os.MkdirAll(extentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extentsDir, store.ExtentFileName(0)), []byte("DATA"), 0o644); err != nil {
		t.Fatal(err)
	}

	checker := &fakeChecker{finishedAt: 0}
	opts := Options{
		TorrentURL:         srv.URL,
		InfoHash:           "deadbeef",
		SavePath:           savePath,
		OutPath:            filepath.Join(dir, "out.raw"),
		PartitionSizeBytes: 8,
		PollInterval:       time.Millisecond,
		WantSHA256:         "0000000000000000000000000000000000000000000000000000000000000000",
	}

	if _, err := Run(context.Background(), checker, srv.Client(), opts); err == nil {
		t.Fatal("Run: got nil error, want a digest-mismatch error")
	}
}
