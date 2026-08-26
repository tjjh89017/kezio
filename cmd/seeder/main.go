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

// Command kezio-seeder-register hands a per-content seeder's own content
// to the ezio daemon running beside it in the same pod.
//
// A content's own PVC (see internal/ingest.ContentMountPath) holds
// torrent.info but no .torrent: publishing never has a Site in scope, so
// it has no announce URL to bake into one. This process builds the
// .torrent itself, from that torrent.info plus this pod's own Site's
// tracker URL (TRACKER_URL) - the same bytes it then both registers with
// ezio and serves over HTTP, so a leecher's info hash always matches
// what ezio actually seeds.
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

	"github.com/tjjh89017/kezio/internal/ingest"
	"github.com/tjjh89017/kezio/internal/seeder"
	"github.com/tjjh89017/kezio/internal/seederdeploy"
	"github.com/tjjh89017/kezio/internal/store"
)

// defaultEzioTarget is the ezio container's gRPC listener. It is always
// pod-local: this process and ezio share a network namespace.
const defaultEzioTarget = "127.0.0.1:50051"

// defaultInterval is how often the registration set is reconciled.
const defaultInterval = 30 * time.Second

// dialTimeout bounds one reconcile pass's RPCs.
const dialTimeout = 30 * time.Second

// defaultHTTPAddr is where the .torrent HTTP server listens, matching
// seederdeploy.TorrentHTTPPort.
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
			// The readiness probe (seederdeploy.TorrentHealthzPath, on
			// this same server) is what tells Kubernetes this pod is fit
			// to receive .torrent fetches; once the server that probe
			// depends on is dead, silently logging leaves the pod Ready
			// forever with nothing actually listening. Exiting lets the
			// container restart - the honest recovery - instead of
			// lying to consumers.
			log.Fatalf("torrent http server: %v", err)
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

// config is the process configuration, all of it from the environment
// the seeder Deployment sets on this container.
type config struct {
	ContentRoot    string
	EzioTarget     string
	TrackerURL     string
	Interval       time.Duration
	MaxUploads     int32
	MaxConnections int32
}

func configFromEnv() (config, error) {
	cfg := config{
		ContentRoot:    envOr("CONTENT_ROOT", ingest.ContentMountRoot),
		EzioTarget:     envOr("EZIO_TARGET", defaultEzioTarget),
		TrackerURL:     os.Getenv("TRACKER_URL"),
		Interval:       defaultInterval,
		MaxUploads:     seeder.DefaultMaxUploads,
		MaxConnections: seeder.DefaultSeederMaxConnections,
	}
	if cfg.TrackerURL == "" {
		// image_seeder_placement.go's seederRegisterEnv only sets this
		// Deployment up at all once its Site has a tracker URL to carry
		// (see Site's webhook rules); an empty value here means this
		// process was started with no Site behind it, which it cannot
		// build a correct .torrent for.
		return config{}, errors.New("TRACKER_URL is required")
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

// contentEntry is one mounted content directory's identity: the info
// hash it registers under and the exact .torrent bytes built for it
// (store.BuildTorrentFile against this pod's own TrackerURL). Computed
// once per reconcile pass and shared between AddTorrent registration and
// the torrentIndex the HTTP server reads from - the same bytes go to
// both, which is what keeps what ezio seeds and what a leecher fetches
// in agreement.
type contentEntry struct {
	dir          string
	hash         string
	torrentBytes []byte
}

// loadContentEntries reads dirs' torrent.info files and builds each
// one's .torrent against trackerURL, returning one contentEntry per
// directory that parses and builds cleanly; a directory that fails is
// reported as an error alongside the entries that did succeed, rather
// than aborting the whole pass - a single damaged directory should not
// stop every other content from being registered or served.
func loadContentEntries(dirs []string, trackerURL string) ([]contentEntry, []error) {
	entries := make([]contentEntry, 0, len(dirs))
	var errs []error
	for _, dir := range dirs {
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
		// BuildTorrentFile bencodes {"announce": trackerURL, "info":
		// BuildInfoDict(info)} - the same info dict ComputeInfoHash just
		// hashed, so this .torrent's info hash equals hash above
		// regardless of which tracker built it.
		torrentBytes, err := store.BuildTorrentFile(info, trackerURL)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: build torrent file: %w", dir, err))
			continue
		}
		entries = append(entries, contentEntry{
			dir:          dir,
			hash:         hash.String(),
			torrentBytes: torrentBytes,
		})
	}
	return entries, errs
}

// reconcile refreshes idx from every mounted content directory, then
// adds to ezio whichever content it does not already hold. Content
// already present is left alone: AddTorrent is not idempotent in ezio,
// and re-adding a seeding torrent is not a no-op.
//
// idx is only ever updated to entries this pass confirmed ezio already
// holds or just registered successfully - never from the raw scan of
// cfg.ContentRoot. The HTTP index otherwise would serve a .torrent for
// content ezio does not yet hold: leech.FetchTorrent (internal/leech)
// does a single GET with no retry, so a premature index entry is a hard
// failure downstream, not a transient one. On a dial or list error, idx
// is left untouched rather than cleared, so already-confirmed entries
// stay served through a transient error.
func reconcile(ctx context.Context, cfg config, idx *torrentIndex) error {
	dirs, err := contentDirs(cfg.ContentRoot)
	if err != nil {
		return err
	}
	if len(dirs) == 0 {
		return nil
	}

	entries, errs := loadContentEntries(dirs, cfg.TrackerURL)

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

	served := make([]contentEntry, 0, len(entries))
	for _, entry := range entries {
		added, err := addContentDir(ctx, c, cfg, entry, existing)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", entry.dir, err))
			continue
		}
		if added {
			log.Printf("added content from %s", entry.dir)
		}
		served = append(served, entry)
	}
	idx.set(served)
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

	// seedMode: the content was hash-verified when the publish step copied
	// it into this PVC (see internal/ingest.publishPartition), so ezio
	// re-hashing every piece on start would only repeat that work.
	if err := c.AddTorrent(ctx, entry.torrentBytes, entry.dir, true, cfg.MaxUploads, cfg.MaxConnections); err != nil {
		return false, fmt.Errorf("add torrent %s: %w", entry.hash, err)
	}
	return true, nil
}

// torrentIndex maps a content's info hash to its exact .torrent bytes,
// kept current by reconcile on every pass and read by the HTTP handler
// below. A separate type (rather than a bare map behind a mutex) so the
// zero-downtime swap-the-whole-map-on-each-pass pattern - never mutating
// the map a concurrent request might be reading - is enforced in one
// place.
type torrentIndex struct {
	mu     sync.RWMutex
	byHash map[string][]byte
}

func newTorrentIndex() *torrentIndex {
	return &torrentIndex{byHash: map[string][]byte{}}
}

// set replaces the whole index from entries, keyed by hash.
func (idx *torrentIndex) set(entries []contentEntry) {
	byHash := make(map[string][]byte, len(entries))
	for _, e := range entries {
		byHash[e.hash] = e.torrentBytes
	}
	idx.mu.Lock()
	idx.byHash = byHash
	idx.mu.Unlock()
}

// bytes returns the .torrent bytes registered for hash, if any - the
// same bytes addContentDir handed to AddTorrent for that hash.
func (idx *torrentIndex) bytes(hash string) ([]byte, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	b, ok := idx.byHash[hash]
	return b, ok
}

// torrentsPathPrefix is the URL path prefix the HTTP handler below
// serves from; the info hash is everything after it.
const torrentsPathPrefix = "/torrents/"

// torrentMux builds the HTTP handler serving each indexed content's
// .torrent bytes by info hash. It never touches the filesystem to answer
// a request: the only bytes ever served are ones idx itself already
// built and holds in memory, keyed by an exact map lookup on the hash
// the request names - the same bytes reconcile registered with ezio for
// that hash.
func torrentMux(idx *torrentIndex) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(seederdeploy.TorrentHealthzPath, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc(torrentsPathPrefix, func(w http.ResponseWriter, r *http.Request) {
		hash := r.URL.Path[len(torrentsPathPrefix):]
		torrentBytes, ok := idx.bytes(hash)
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/x-bittorrent")
		_, _ = w.Write(torrentBytes)
	})
	return mux
}

// contentDirs lists the mounted content directories under root that
// carry a torrent.info. A directory without one is skipped rather than
// reported: it means the publish step has not finished copying that
// content into its PVC yet, which is a valid transient state, not a
// fault here. One seeder Deployment mounts exactly one content's PVC
// today, but this scans root rather than assuming a single fixed
// subdirectory name, so it keeps working unchanged if that ever loosens.
func contentDirs(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read content root %s: %w", root, err)
	}

	dirs := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if _, err := os.Stat(store.ContentDirTorrentInfoPath(dir)); err != nil {
			continue
		}
		dirs = append(dirs, dir)
	}
	return dirs, nil
}
