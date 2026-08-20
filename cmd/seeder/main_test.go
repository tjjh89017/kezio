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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjjh89017/kezio/internal/ingest"
	"github.com/tjjh89017/kezio/internal/seeder"
	"github.com/tjjh89017/kezio/internal/seederdeploy"
	"github.com/tjjh89017/kezio/internal/store"
)

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
// everything loadContentEntries needs from a content directory.
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

func TestConfigFromEnv_InvalidRegisterInterval(t *testing.T) {
	t.Setenv("REGISTER_INTERVAL", "not-a-duration")

	if _, err := configFromEnv(); err == nil {
		t.Fatal("configFromEnv: want error for unparsable REGISTER_INTERVAL, got nil")
	}
}

func TestConfigFromEnv_NegativeRegisterInterval(t *testing.T) {
	t.Setenv("REGISTER_INTERVAL", "-5s")

	if _, err := configFromEnv(); err == nil {
		t.Fatal("configFromEnv: want error for negative REGISTER_INTERVAL, got nil")
	}
}

func TestConfigFromEnv_ZeroRegisterInterval(t *testing.T) {
	t.Setenv("REGISTER_INTERVAL", "0s")

	if _, err := configFromEnv(); err == nil {
		t.Fatal("configFromEnv: want error for zero REGISTER_INTERVAL, got nil")
	}
}

func TestConfigFromEnv_InvalidMaxUploads(t *testing.T) {
	t.Setenv("EZIO_MAX_UPLOADS", "not-an-int")

	if _, err := configFromEnv(); err == nil {
		t.Fatal("configFromEnv: want error for unparsable EZIO_MAX_UPLOADS, got nil")
	}
}

func TestConfigFromEnv_InvalidMaxConnections(t *testing.T) {
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

	entries, errs := loadContentEntries([]string{goodA, goodB, damaged})

	if len(entries) != 2 {
		t.Fatalf("loadContentEntries: got %d entries, want 2 (the undamaged directories): %+v", len(entries), entries)
	}
	gotDirs := map[string]bool{}
	for _, e := range entries {
		gotDirs[e.dir] = true
		if e.hash == "" {
			t.Errorf("entry for %s has empty hash", e.dir)
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

	entries, errs := loadContentEntries([]string{dirA, dirB})

	if errs != nil {
		t.Fatalf("loadContentEntries: unexpected errors %v", errs)
	}
	if len(entries) != 2 {
		t.Fatalf("loadContentEntries: got %d entries, want 2", len(entries))
	}
}

func TestTorrentIndex_ConcurrentSetIsRaceFree(t *testing.T) {
	idx := newTorrentIndex()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func(n int) {
			defer wg.Done()
			idx.set([]contentEntry{{dir: "d", hash: "h", torrentPath: filepath.Join("p", string(rune('a'+n%26)))}})
		}(i)
		go func() {
			defer wg.Done()
			idx.path("h")
		}()
	}
	wg.Wait()

	// The last set to actually land must be readable back: whichever one
	// that was, path("h") must resolve to a torrentPath this loop wrote,
	// never to a torn or missing value.
	path, ok := idx.path("h")
	if !ok {
		t.Fatal("path(\"h\"): not found after concurrent sets, want the last set's entry")
	}
	if filepath.Dir(path) != "p" {
		t.Errorf("path(%q): not one of the values this test wrote", path)
	}
}

func TestTorrentIndex_PathMissReportsNotOK(t *testing.T) {
	idx := newTorrentIndex()

	if _, ok := idx.path("unknown"); ok {
		t.Error("path(\"unknown\"): ok = true on an empty index, want false")
	}
}

func TestTorrentMux_ServesRegisteredHashAndHealthzAnd404sUnknown(t *testing.T) {
	dir := t.TempDir()
	torrentPath := filepath.Join(dir, "content.torrent")
	const body = "fake bencoded torrent bytes"
	if err := os.WriteFile(torrentPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture torrent file: %v", err)
	}

	idx := newTorrentIndex()
	idx.set([]contentEntry{{dir: dir, hash: "deadbeef", torrentPath: torrentPath}})

	srv := httptest.NewServer(torrentMux(idx))
	defer srv.Close()

	t.Run("registered hash serves the .torrent bytes", func(t *testing.T) {
		resp, err := http.Get(srv.URL + torrentsPathPrefix + "deadbeef")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close() //nolint:errcheck // test HTTP client, nothing actionable on close failure
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
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
