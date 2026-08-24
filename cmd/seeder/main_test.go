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

package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/tjjh89017/kezio/internal/ingest"
	"github.com/tjjh89017/kezio/internal/seeder"
	"github.com/tjjh89017/kezio/internal/seeder/ezioapi"
	"github.com/tjjh89017/kezio/internal/seederdeploy"
	"github.com/tjjh89017/kezio/internal/store"
)

// testTrackerURL is the announce URL fixtures build .torrent bytes
// against, standing in for a Site's tracker.
const testTrackerURL = "http://tracker.example.test:6969/announce"

// fixtureTorrentInfo returns a small, deterministic TorrentInfo, distinct
// per seed so callers writing several torrent.info fixtures get distinct
// info hashes.
func fixtureTorrentInfo(seed uint64) *store.TorrentInfo {
	return &store.TorrentInfo{
		BlockSize:   4096,
		BlocksTotal: 100 + seed,
		Extents: []store.Extent{
			{Offset: seed, Length: 8},
		},
		PieceHashes: []store.PieceHash{{byte(seed), 0x22}},
	}
}

// writeContentDir creates dir with a torrent.info parsing to info -
// everything loadContentEntries and contentDirs need from a content
// directory (torrent.info alone, since a content's own PVC never holds
// a pre-built .torrent - see contentDirs' doc comment).
func writeContentDir(t *testing.T, dir string, info *store.TorrentInfo) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	f, err := os.Create(store.ContentDirTorrentInfoPath(dir))
	if err != nil {
		t.Fatalf("create torrent.info in %s: %v", dir, err)
	}
	defer f.Close() //nolint:errcheck // test fixture, nothing actionable on close failure
	if err := store.WriteTorrentInfo(f, info); err != nil {
		t.Fatalf("write torrent.info in %s: %v", dir, err)
	}
}

// writeReconcileContentDir creates dir with a torrent.info parsing to
// info - exactly what contentDirs requires to include dir in a reconcile
// pass. It returns the info hash reconcile will compute for dir, so a
// test can key its fake ezio server and its index assertions off the
// same value reconcile uses.
func writeReconcileContentDir(t *testing.T, dir string, info *store.TorrentInfo) string {
	t.Helper()
	writeContentDir(t, dir, info)
	hash, err := store.ComputeInfoHash(info)
	if err != nil {
		t.Fatalf("compute info hash for %s: %v", dir, err)
	}
	return hash.String()
}

// fakeReconcileEZIO is a minimal in-memory ezioapi.EZIOServer for driving
// reconcile end to end: it tracks which save_paths ezio has registered,
// the exact bytes each was registered with, and can be told to fail
// AddTorrent for specific ones, so a test can assert what reconcile does
// with a mixed success/failure pass and that the registered bytes are
// what it expects.
type fakeReconcileEZIO struct {
	ezioapi.UnimplementedEZIOServer

	mu             sync.Mutex
	hashBySavePath map[string]string
	registered     map[string]bool
	registeredData map[string][]byte
	failSavePaths  map[string]bool
}

func newFakeReconcileEZIO(hashBySavePath map[string]string) *fakeReconcileEZIO {
	return &fakeReconcileEZIO{
		hashBySavePath: hashBySavePath,
		registered:     make(map[string]bool),
		registeredData: make(map[string][]byte),
		failSavePaths:  make(map[string]bool),
	}
}

func (f *fakeReconcileEZIO) AddTorrent(_ context.Context, req *ezioapi.AddRequest) (*ezioapi.AddResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failSavePaths[req.GetSavePath()] {
		return &ezioapi.AddResponse{Result: false}, nil
	}
	f.registered[req.GetSavePath()] = true
	f.registeredData[req.GetSavePath()] = append([]byte(nil), req.GetTorrent()...)
	return &ezioapi.AddResponse{Result: true}, nil
}

func (f *fakeReconcileEZIO) GetTorrentStatus(
	_ context.Context, _ *ezioapi.UpdateRequest,
) (*ezioapi.UpdateStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := &ezioapi.UpdateStatus{Torrents: make(map[string]*ezioapi.Torrent)}
	for savePath := range f.registered {
		hash := f.hashBySavePath[savePath]
		out.Torrents[hash] = &ezioapi.Torrent{Hash: hash}
	}
	return out, nil
}

// startFakeReconcileEZIO starts fake behind a real TCP listener (reconcile
// dials cfg.EzioTarget with seeder.Dial, which needs a real address, not
// an in-memory bufconn) and returns the address to put in cfg.EzioTarget.
// The server stops when the test's Cleanup runs.
func startFakeReconcileEZIO(t *testing.T, fake *fakeReconcileEZIO) string {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	ezioapi.RegisterEZIOServer(srv, fake)
	go func() {
		_ = srv.Serve(lis)
	}()
	t.Cleanup(srv.Stop)

	return lis.Addr().String()
}

// TestReconcile_IndexServesOnlySuccessfullyRegisteredEntries is the
// fix for the index-stale-before-registration race: the HTTP index must
// serve a content only once ezio has actually confirmed it, never merely
// because reconcile found it on disk.
func TestReconcile_IndexServesOnlySuccessfullyRegisteredEntries(t *testing.T) {
	root := t.TempDir()
	goodDir := filepath.Join(root, "good")
	badDir := filepath.Join(root, "bad")
	goodHash := writeReconcileContentDir(t, goodDir, fixtureTorrentInfo(1))
	badHash := writeReconcileContentDir(t, badDir, fixtureTorrentInfo(2))

	fake := newFakeReconcileEZIO(map[string]string{
		goodDir: goodHash,
		badDir:  badHash,
	})
	fake.failSavePaths[badDir] = true
	addr := startFakeReconcileEZIO(t, fake)

	cfg := config{
		ContentRoot:    root,
		EzioTarget:     addr,
		TrackerURL:     testTrackerURL,
		MaxUploads:     seeder.DefaultMaxUploads,
		MaxConnections: seeder.DefaultMaxConnections,
	}
	idx := newTorrentIndex()

	err := reconcile(context.Background(), cfg, idx)
	if err == nil {
		t.Fatal("reconcile: got nil error, want one reporting the bad entry's AddTorrent failure")
	}
	if !strings.Contains(err.Error(), badDir) {
		t.Errorf("reconcile error %q does not name the failed directory %s", err, badDir)
	}

	if _, ok := idx.bytes(goodHash); !ok {
		t.Errorf("index does not serve %s, want it served after successful registration", goodHash)
	}
	if _, ok := idx.bytes(badHash); ok {
		t.Errorf("index serves %s, want it withheld since AddTorrent failed for it", badHash)
	}
}

// TestReconcile_KeepsPriorlyRegisteredEntriesServedAcrossReconciles
// covers the ordering fix's other half: content ezio already holds from
// an earlier reconcile pass must stay in the index on a later pass, even
// though that later pass never calls AddTorrent for it again (AddTorrent
// is not idempotent - see reconcile's doc comment).
func TestReconcile_KeepsPriorlyRegisteredEntriesServedAcrossReconciles(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "content")
	hash := writeReconcileContentDir(t, dir, fixtureTorrentInfo(1))

	fake := newFakeReconcileEZIO(map[string]string{dir: hash})
	addr := startFakeReconcileEZIO(t, fake)

	cfg := config{
		ContentRoot:    root,
		EzioTarget:     addr,
		TrackerURL:     testTrackerURL,
		MaxUploads:     seeder.DefaultMaxUploads,
		MaxConnections: seeder.DefaultMaxConnections,
	}
	idx := newTorrentIndex()

	if err := reconcile(context.Background(), cfg, idx); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if _, ok := idx.bytes(hash); !ok {
		t.Fatalf("index does not serve %s after the first reconcile", hash)
	}

	// A second pass must not re-add (AddTorrent is not idempotent); if
	// it tried, this would fail the pass since the fake only accepts
	// each save_path's first AddTorrent as a success-causing registration
	// and reconcile would skip calling AddTorrent again because the hash
	// is already in existing - the entry must still be served regardless.
	if err := reconcile(context.Background(), cfg, idx); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if _, ok := idx.bytes(hash); !ok {
		t.Errorf("index no longer serves %s after the second reconcile, want it to stay served", hash)
	}
}

// TestReconcile_RegisteredBytesMatchServedBytesAndAnnounceTracker is the
// property the whole per-Site design rests on (see internal/store's
// BuildTorrentFile): the .torrent reconcile hands to AddTorrent and the
// .torrent an agent later fetches over HTTP must be byte-identical, and
// its announce must be this pod's own Site's tracker URL.
func TestReconcile_RegisteredBytesMatchServedBytesAndAnnounceTracker(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "content")
	info := fixtureTorrentInfo(1)
	hash := writeReconcileContentDir(t, dir, info)

	fake := newFakeReconcileEZIO(map[string]string{dir: hash})
	addr := startFakeReconcileEZIO(t, fake)

	cfg := config{
		ContentRoot:    root,
		EzioTarget:     addr,
		TrackerURL:     testTrackerURL,
		MaxUploads:     seeder.DefaultMaxUploads,
		MaxConnections: seeder.DefaultMaxConnections,
	}
	idx := newTorrentIndex()

	if err := reconcile(context.Background(), cfg, idx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	servedBytes, ok := idx.bytes(hash)
	if !ok {
		t.Fatalf("index does not serve %s after reconcile", hash)
	}

	fake.mu.Lock()
	registeredBytes := fake.registeredData[dir]
	fake.mu.Unlock()
	if registeredBytes == nil {
		t.Fatalf("ezio was never registered for save_path %s", dir)
	}

	if !bytes.Equal(servedBytes, registeredBytes) {
		t.Fatalf("served .torrent bytes differ from registered bytes:\n served=%x\nregistered=%x",
			servedBytes, registeredBytes)
	}

	// The info hash inside the built .torrent must still be the
	// content's own hash - the whole point of announce sitting outside
	// the info dict (see store.BuildTorrentFile).
	wantHash, err := store.ComputeInfoHash(info)
	if err != nil {
		t.Fatalf("ComputeInfoHash: %v", err)
	}
	if wantHash.String() != hash {
		t.Fatalf("fixture hash mismatch: %s != %s", wantHash, hash)
	}

	if !bytes.Contains(servedBytes, []byte(testTrackerURL)) {
		t.Fatalf(".torrent bytes %x do not contain the configured tracker URL %q", servedBytes, testTrackerURL)
	}
}

func TestConfigFromEnv_Defaults(t *testing.T) {
	// configFromEnv treats an empty value the same as unset (envOr, and
	// the explicit != "" checks below it) - t.Setenv("") pins that state
	// for the test without disturbing anything else in the environment.
	envKeys := []string{
		"CONTENT_ROOT", "EZIO_TARGET", "REGISTER_INTERVAL",
		"EZIO_MAX_UPLOADS", "EZIO_MAX_CONNECTIONS",
	}
	for _, k := range envKeys {
		t.Setenv(k, "")
	}
	t.Setenv("TRACKER_URL", testTrackerURL)

	cfg, err := configFromEnv()
	if err != nil {
		t.Fatalf("configFromEnv: %v", err)
	}
	if cfg.ContentRoot != ingest.ContentMountRoot {
		t.Errorf("ContentRoot = %q, want %q", cfg.ContentRoot, ingest.ContentMountRoot)
	}
	if cfg.EzioTarget != defaultEzioTarget {
		t.Errorf("EzioTarget = %q, want %q", cfg.EzioTarget, defaultEzioTarget)
	}
	if cfg.TrackerURL != testTrackerURL {
		t.Errorf("TrackerURL = %q, want %q", cfg.TrackerURL, testTrackerURL)
	}
	if cfg.Interval != defaultInterval {
		t.Errorf("Interval = %v, want %v", cfg.Interval, defaultInterval)
	}
	if cfg.MaxUploads != seeder.DefaultMaxUploads {
		t.Errorf("MaxUploads = %d, want %d", cfg.MaxUploads, seeder.DefaultMaxUploads)
	}
	if cfg.MaxConnections != seeder.DefaultMaxConnections {
		t.Errorf("MaxConnections = %d, want %d", cfg.MaxConnections, seeder.DefaultMaxConnections)
	}
}

func TestConfigFromEnv_ValidOverrides(t *testing.T) {
	t.Setenv("CONTENT_ROOT", "/custom/root")
	t.Setenv("EZIO_TARGET", "10.0.0.1:50051")
	t.Setenv("TRACKER_URL", testTrackerURL)
	t.Setenv("REGISTER_INTERVAL", "5s")
	t.Setenv("EZIO_MAX_UPLOADS", "7")
	t.Setenv("EZIO_MAX_CONNECTIONS", "42")

	cfg, err := configFromEnv()
	if err != nil {
		t.Fatalf("configFromEnv: %v", err)
	}
	if cfg.ContentRoot != "/custom/root" {
		t.Errorf("ContentRoot = %q, want /custom/root", cfg.ContentRoot)
	}
	if cfg.EzioTarget != "10.0.0.1:50051" {
		t.Errorf("EzioTarget = %q, want 10.0.0.1:50051", cfg.EzioTarget)
	}
	if cfg.Interval != 5*time.Second {
		t.Errorf("Interval = %v, want 5s", cfg.Interval)
	}
	if cfg.MaxUploads != 7 {
		t.Errorf("MaxUploads = %d, want 7", cfg.MaxUploads)
	}
	if cfg.MaxConnections != 42 {
		t.Errorf("MaxConnections = %d, want 42", cfg.MaxConnections)
	}
}

// TestConfigFromEnv_MissingTrackerURLFails proves cmd/seeder cannot
// start with no announce URl configured: a fallback here would be a
// cluster-wide tracker under another name.
func TestConfigFromEnv_MissingTrackerURLFails(t *testing.T) {
	t.Setenv("TRACKER_URL", "")

	if _, err := configFromEnv(); err == nil {
		t.Fatal("configFromEnv: want error when TRACKER_URL is unset, got nil")
	}
}

func TestConfigFromEnv_InvalidRegisterInterval(t *testing.T) {
	t.Setenv("TRACKER_URL", testTrackerURL)
	t.Setenv("REGISTER_INTERVAL", "not-a-duration")

	if _, err := configFromEnv(); err == nil {
		t.Fatal("configFromEnv: want error for unparsable REGISTER_INTERVAL, got nil")
	}
}

func TestConfigFromEnv_NegativeRegisterInterval(t *testing.T) {
	t.Setenv("TRACKER_URL", testTrackerURL)
	t.Setenv("REGISTER_INTERVAL", "-5s")

	if _, err := configFromEnv(); err == nil {
		t.Fatal("configFromEnv: want error for negative REGISTER_INTERVAL, got nil")
	}
}

func TestConfigFromEnv_ZeroRegisterInterval(t *testing.T) {
	t.Setenv("TRACKER_URL", testTrackerURL)
	t.Setenv("REGISTER_INTERVAL", "0s")

	if _, err := configFromEnv(); err == nil {
		t.Fatal("configFromEnv: want error for zero REGISTER_INTERVAL, got nil")
	}
}

func TestConfigFromEnv_InvalidMaxUploads(t *testing.T) {
	t.Setenv("TRACKER_URL", testTrackerURL)
	t.Setenv("EZIO_MAX_UPLOADS", "not-an-int")

	if _, err := configFromEnv(); err == nil {
		t.Fatal("configFromEnv: want error for unparsable EZIO_MAX_UPLOADS, got nil")
	}
}

func TestConfigFromEnv_InvalidMaxConnections(t *testing.T) {
	t.Setenv("TRACKER_URL", testTrackerURL)
	t.Setenv("EZIO_MAX_CONNECTIONS", "not-an-int")

	if _, err := configFromEnv(); err == nil {
		t.Fatal("configFromEnv: want error for unparsable EZIO_MAX_CONNECTIONS, got nil")
	}
}

// TestLoadContentEntries_DamagedDirectoryDoesNotStopOthers is the
// documented partial-failure contract (see loadContentEntries's doc
// comment): one directory with a damaged/missing torrent.info must not
// prevent every other content directory from registering.
func TestLoadContentEntries_DamagedDirectoryDoesNotStopOthers(t *testing.T) {
	root := t.TempDir()

	goodA := filepath.Join(root, "good-a")
	goodB := filepath.Join(root, "good-b")
	damaged := filepath.Join(root, "damaged")

	writeContentDir(t, goodA, fixtureTorrentInfo(1))
	writeContentDir(t, goodB, fixtureTorrentInfo(2))
	// damaged has no torrent.info at all - LoadContentDirTorrentInfo will
	// fail to open it.
	if err := os.MkdirAll(damaged, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", damaged, err)
	}

	entries, errs := loadContentEntries([]string{goodA, goodB, damaged}, testTrackerURL)

	if len(entries) != 2 {
		t.Fatalf("loadContentEntries: got %d entries, want 2 (the undamaged directories): %+v", len(entries), entries)
	}
	gotDirs := map[string]bool{}
	for _, e := range entries {
		gotDirs[e.dir] = true
		if e.hash == "" {
			t.Errorf("entry for %s has empty hash", e.dir)
		}
		if len(e.torrentBytes) == 0 {
			t.Errorf("entry for %s has no torrent bytes", e.dir)
		}
	}
	if !gotDirs[goodA] || !gotDirs[goodB] {
		t.Errorf("loadContentEntries: entries = %+v, want one for each of %s and %s", entries, goodA, goodB)
	}

	if len(errs) != 1 {
		t.Fatalf("loadContentEntries: got %d errors, want 1 (for the damaged directory): %v", len(errs), errs)
	}
	if !strings.Contains(errs[0].Error(), damaged) {
		t.Errorf("loadContentEntries: error %q does not name the damaged directory %s", errs[0], damaged)
	}
}

func TestLoadContentEntries_AllValid(t *testing.T) {
	root := t.TempDir()
	dirA := filepath.Join(root, "a")
	dirB := filepath.Join(root, "b")
	writeContentDir(t, dirA, fixtureTorrentInfo(1))
	writeContentDir(t, dirB, fixtureTorrentInfo(2))

	entries, errs := loadContentEntries([]string{dirA, dirB}, testTrackerURL)

	if errs != nil {
		t.Fatalf("loadContentEntries: unexpected errors %v", errs)
	}
	if len(entries) != 2 {
		t.Fatalf("loadContentEntries: got %d entries, want 2", len(entries))
	}
}

// TestLoadContentEntries_SameInfoDifferentTrackerSameHashDifferentAnnounce
// is the property section 4.1 rests on: two different tracker URLs over
// the same torrent.info produce .torrent bytes with the same info hash
// (the content address does not move) and a different announce.
func TestLoadContentEntries_SameInfoDifferentTrackerSameHashDifferentAnnounce(t *testing.T) {
	root := t.TempDir()
	dirA := filepath.Join(root, "site-a")
	dirB := filepath.Join(root, "site-b")
	info := fixtureTorrentInfo(1)
	writeContentDir(t, dirA, info)
	writeContentDir(t, dirB, info)

	const trackerA = "http://tracker-a.example.test:6969/announce"
	const trackerB = "http://tracker-b.example.test:6969/announce"

	entriesA, errs := loadContentEntries([]string{dirA}, trackerA)
	if errs != nil {
		t.Fatalf("loadContentEntries(trackerA): unexpected errors %v", errs)
	}
	entriesB, errs := loadContentEntries([]string{dirB}, trackerB)
	if errs != nil {
		t.Fatalf("loadContentEntries(trackerB): unexpected errors %v", errs)
	}

	if entriesA[0].hash != entriesB[0].hash {
		t.Fatalf("info hash differs across trackers: %s != %s", entriesA[0].hash, entriesB[0].hash)
	}
	if bytes.Equal(entriesA[0].torrentBytes, entriesB[0].torrentBytes) {
		t.Fatal(".torrent bytes are identical across two different tracker URLs, want them to differ")
	}
	if !bytes.Contains(entriesA[0].torrentBytes, []byte(trackerA)) {
		t.Errorf(".torrent for trackerA does not contain %q", trackerA)
	}
	if !bytes.Contains(entriesB[0].torrentBytes, []byte(trackerB)) {
		t.Errorf(".torrent for trackerB does not contain %q", trackerB)
	}
}

func TestTorrentIndex_ConcurrentSetIsRaceFree(t *testing.T) {
	idx := newTorrentIndex()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func(n int) {
			defer wg.Done()
			idx.set([]contentEntry{{dir: "d", hash: "h", torrentBytes: []byte{byte(n)}}})
		}(i)
		go func() {
			defer wg.Done()
			idx.bytes("h")
		}()
	}
	wg.Wait()

	// The last set to actually land must be readable back: whichever one
	// that was, bytes("h") must resolve to a value this loop wrote, never
	// to a torn or missing value.
	got, ok := idx.bytes("h")
	if !ok {
		t.Fatal("bytes(\"h\"): not found after concurrent sets, want the last set's entry")
	}
	if len(got) != 1 {
		t.Errorf("bytes(%x): not one of the single-byte values this test wrote", got)
	}
}

func TestTorrentIndex_BytesMissReportsNotOK(t *testing.T) {
	idx := newTorrentIndex()

	if _, ok := idx.bytes("unknown"); ok {
		t.Error("bytes(\"unknown\"): ok = true on an empty index, want false")
	}
}

func TestTorrentMux_ServesRegisteredHashAndHealthzAnd404sUnknown(t *testing.T) {
	const body = "fake bencoded torrent bytes"

	idx := newTorrentIndex()
	idx.set([]contentEntry{{dir: t.TempDir(), hash: "deadbeef", torrentBytes: []byte(body)}})

	srv := httptest.NewServer(torrentMux(idx))
	defer srv.Close()

	t.Run("registered hash serves the exact .torrent bytes", func(t *testing.T) {
		resp, err := http.Get(srv.URL + torrentsPathPrefix + "deadbeef")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close() //nolint:errcheck // test HTTP client, nothing actionable on close failure
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		got := make([]byte, len(body))
		if _, err := resp.Body.Read(got); err != nil && err.Error() != "EOF" {
			t.Fatalf("read body: %v", err)
		}
		if string(got) != body {
			t.Errorf("served body = %q, want %q", got, body)
		}
	})

	t.Run("unknown hash 404s", func(t *testing.T) {
		resp, err := http.Get(srv.URL + torrentsPathPrefix + "unknownhash")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close() //nolint:errcheck // test HTTP client, nothing actionable on close failure
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
	})

	t.Run("healthz reports 200 independent of registration", func(t *testing.T) {
		resp, err := http.Get(srv.URL + seederdeploy.TorrentHealthzPath)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close() //nolint:errcheck // test HTTP client, nothing actionable on close failure
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
	})
}
