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

// Command kezio-seeder-register hands a per-Image seeder's own content to
// the ezio daemon running beside it in the same pod.
//
// It exists because the .torrent for a partition lives in that partition's
// PVC, which only this pod mounts. The Image reconciler has no such mount,
// so it cannot build or read one; registering from inside the pod is the
// only place the bytes are actually reachable. See
// internal/ingest.publishPartition for the writer of these files.
//
// The loop reconciles rather than running once: the ezio container can
// restart independently of this one, taking its torrent list with it.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/tjjh89017/kezio/internal/seeder"
	"github.com/tjjh89017/kezio/internal/store"
)

// defaultContentRoot is the parent of every per-partition mount the
// seeder Deployment creates (see internal/ingest.PartitionMountPath).
const defaultContentRoot = "/content"

// defaultEzioTarget is the ezio container's gRPC listener. It is always
// pod-local: this process and ezio share a network namespace.
const defaultEzioTarget = "127.0.0.1:50051"

// defaultInterval is how often the registration set is reconciled.
const defaultInterval = 30 * time.Second

// dialTimeout bounds one reconcile pass's RPCs.
const dialTimeout = 30 * time.Second

// defaultHTTPAddr is where the per-partition .torrent HTTP server
// listens, matching the seeder-register container's "torrent" port in
// internal/controller's buildSeederDeployment.
const defaultHTTPAddr = ":8080"

func main() {
	cfg, err := configFromEnv()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Printf("starting seeder registration {contentRoot:%s ezio:%s interval:%s}",
		cfg.ContentRoot, cfg.EzioTarget, cfg.Interval)

	idx := newTorrentIndex()
	httpAddr := envOr("HTTP_ADDR", defaultHTTPAddr)
	httpSrv := &http.Server{Addr: httpAddr, Handler: torrentMux(idx)}
	go func() {
		log.Printf("serving .torrent files on %s", httpAddr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("torrent http server: %v", err)
		}
	}()
	defer func() { _ = httpSrv.Close() }()

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		if err := reconcile(ctx, cfg, idx); err != nil {
			// ezio not up yet, or a transient RPC failure: the next tick
			// retries. Exiting would restart-loop the container without
			// making progress ezio has not already made.
			log.Printf("reconcile: %v", err)
		}
		select {
		case <-ctx.Done():
			log.Print("shutting down")
			return
		case <-ticker.C:
		}
	}
}

// config is the process configuration, all of it from the environment the
// Image reconciler sets on this container.
type config struct {
	ContentRoot    string
	EzioTarget     string
	Interval       time.Duration
	MaxUploads     int32
	MaxConnections int32
}

func configFromEnv() (config, error) {
	cfg := config{
		ContentRoot:    envOr("CONTENT_ROOT", defaultContentRoot),
		EzioTarget:     envOr("EZIO_TARGET", defaultEzioTarget),
		Interval:       defaultInterval,
		MaxUploads:     seeder.DefaultMaxUploads,
		MaxConnections: seeder.DefaultMaxConnections,
	}

	if v := os.Getenv("REGISTER_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return config{}, fmt.Errorf("parse REGISTER_INTERVAL %q: %w", v, err)
		}
		if d <= 0 {
			return config{}, fmt.Errorf("REGISTER_INTERVAL must be positive, got %q", v)
		}
		cfg.Interval = d
	}
	if v := os.Getenv("EZIO_MAX_UPLOADS"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			return config{}, fmt.Errorf("parse EZIO_MAX_UPLOADS %q: %w", v, err)
		}
		cfg.MaxUploads = int32(n)
	}
	if v := os.Getenv("EZIO_MAX_CONNECTIONS"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			return config{}, fmt.Errorf("parse EZIO_MAX_CONNECTIONS %q: %w", v, err)
		}
		cfg.MaxConnections = int32(n)
	}
	return cfg, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// contentEntry is one mounted partition directory's content identity: the
// info hash it registers under and the path to its already-built
// .torrent file (see store.ContentTorrentFileName). Computed once per
// reconcile pass and shared between AddTorrent registration and the
// torrentIndex the HTTP server reads from.
type contentEntry struct {
	dir         string
	hash        string
	torrentPath string
}

// loadContentEntries reads dirs' torrent.info files and returns one
// contentEntry per directory that parses cleanly; a directory that fails
// to load is reported as an error alongside the entries that did
// succeed, rather than aborting the whole pass - a single damaged
// directory should not stop every other partition's content from being
// registered or served.
func loadContentEntries(dirs []string) ([]contentEntry, []error) {
	entries := make([]contentEntry, 0, len(dirs))
	var errs []error
	for _, dir := range dirs {
		// The info hash comes from torrent.info rather than the .torrent
		// so this agrees with the store's own content addressing by
		// construction (see store.ComputeInfoHash) instead of
		// re-deriving it from the bencoded file the publish step
		// happened to write.
		info, err := store.LoadContentDirTorrentInfo(dir)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: load torrent.info: %w", dir, err))
			continue
		}
		hash, err := store.ComputeInfoHash(info)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: compute info hash: %w", dir, err))
			continue
		}
		entries = append(entries, contentEntry{
			dir:         dir,
			hash:        hash.String(),
			torrentPath: filepath.Join(dir, store.ContentTorrentFileName),
		})
	}
	return entries, errs
}

// reconcile refreshes idx from every mounted partition directory, then
// adds to ezio whichever content it does not already hold. Content
// already present is left alone: AddTorrent is not idempotent in ezio,
// and re-adding a seeding torrent is not a no-op.
func reconcile(ctx context.Context, cfg config, idx *torrentIndex) error {
	dirs, err := contentDirs(cfg.ContentRoot)
	if err != nil {
		return err
	}
	if len(dirs) == 0 {
		return nil
	}

	entries, errs := loadContentEntries(dirs)
	idx.set(entries)

	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	c, err := seeder.Dial(cfg.EzioTarget)
	if err != nil {
		errs = append(errs, fmt.Errorf("dial ezio at %s: %w", cfg.EzioTarget, err))
		return errors.Join(errs...)
	}
	defer func() { _ = c.Close() }()

	existing, err := c.GetTorrentStatus(ctx, nil)
	if err != nil {
		errs = append(errs, fmt.Errorf("get torrent status: %w", err))
		return errors.Join(errs...)
	}

	for _, entry := range entries {
		added, err := addContentDir(ctx, c, cfg, entry, existing)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", entry.dir, err))
			continue
		}
		if added {
			log.Printf("added content from %s", entry.dir)
		}
	}
	return errors.Join(errs...)
}

// addContentDir registers entry's .torrent with ezio unless its info hash
// is already known, reporting whether it added anything.
func addContentDir(
	ctx context.Context, c *seeder.Client, cfg config, entry contentEntry,
	existing map[string]seeder.Torrent,
) (bool, error) {
	if _, ok := existing[entry.hash]; ok {
		return false, nil
	}

	torrentBytes, err := os.ReadFile(entry.torrentPath) //nolint:gosec // mount path this pod owns, not user input
	if err != nil {
		return false, fmt.Errorf("read %s: %w", store.ContentTorrentFileName, err)
	}

	// seedMode: the content was hash-verified when the publish step copied
	// it into this PVC (see internal/ingest.publishPartition), so ezio
	// re-hashing every piece on start would only repeat that work.
	if err := c.AddTorrent(ctx, torrentBytes, entry.dir, true, cfg.MaxUploads, cfg.MaxConnections); err != nil {
		return false, fmt.Errorf("add torrent %s: %w", entry.hash, err)
	}
	return true, nil
}

// torrentIndex maps a content's info hash to its .torrent file path, kept
// current by reconcile on every pass and read by the HTTP handler below.
// A separate type (rather than a bare map behind a mutex) so the
// zero-downtime swap-the-whole-map-on-each-pass pattern - never mutating
// the map a concurrent request might be reading - is enforced in one
// place.
type torrentIndex struct {
	mu     sync.RWMutex
	byHash map[string]string
}

func newTorrentIndex() *torrentIndex {
	return &torrentIndex{byHash: map[string]string{}}
}

// set replaces the whole index from entries, keyed by hash.
func (idx *torrentIndex) set(entries []contentEntry) {
	byHash := make(map[string]string, len(entries))
	for _, e := range entries {
		byHash[e.hash] = e.torrentPath
	}
	idx.mu.Lock()
	idx.byHash = byHash
	idx.mu.Unlock()
}

// path returns the .torrent file path registered for hash, if any.
func (idx *torrentIndex) path(hash string) (string, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	path, ok := idx.byHash[hash]
	return path, ok
}

// torrentsPathPrefix is the URL path prefix the HTTP handler below
// serves from; the info hash is everything after it.
const torrentsPathPrefix = "/torrents/"

// torrentMux builds the HTTP handler serving each indexed content's
// .torrent file by info hash. It never joins request input into a
// filesystem path or lists a directory: the only paths ever served are
// ones idx itself already resolved from scanning cfg.ContentRoot, keyed
// by an exact map lookup on the hash the request names.
func torrentMux(idx *torrentIndex) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(torrentsPathPrefix, func(w http.ResponseWriter, r *http.Request) {
		hash := r.URL.Path[len(torrentsPathPrefix):]
		path, ok := idx.path(hash)
		if !ok {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, path)
	})
	return mux
}

// contentDirs lists the mounted partition directories under root that
// carry a built .torrent. A directory without one is skipped rather than
// reported: the publish step writes the .torrent only when a tracker is
// configured, so its absence is a valid state, not a fault here.
func contentDirs(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read content root %s: %w", root, err)
	}

	var dirs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if _, err := os.Stat(filepath.Join(dir, store.ContentTorrentFileName)); err != nil {
			continue
		}
		dirs = append(dirs, dir)
	}
	return dirs, nil
}
