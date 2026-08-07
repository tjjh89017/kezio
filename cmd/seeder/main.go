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
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
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

func main() {
	cfg, err := configFromEnv()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Printf("starting seeder registration {contentRoot:%s ezio:%s interval:%s}",
		cfg.ContentRoot, cfg.EzioTarget, cfg.Interval)

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		if err := reconcile(ctx, cfg); err != nil {
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

// reconcile adds every mounted partition's content that ezio does not
// already hold. Content already present is left alone: AddTorrent is not
// idempotent in ezio, and re-adding a seeding torrent is not a no-op.
func reconcile(ctx context.Context, cfg config) error {
	dirs, err := contentDirs(cfg.ContentRoot)
	if err != nil {
		return err
	}
	if len(dirs) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	c, err := seeder.Dial(cfg.EzioTarget)
	if err != nil {
		return fmt.Errorf("dial ezio at %s: %w", cfg.EzioTarget, err)
	}
	defer func() { _ = c.Close() }()

	existing, err := c.GetTorrentStatus(ctx, nil)
	if err != nil {
		return fmt.Errorf("get torrent status: %w", err)
	}

	var errs []error
	for _, dir := range dirs {
		added, err := addContentDir(ctx, c, cfg, dir, existing)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", dir, err))
			continue
		}
		if added {
			log.Printf("added content from %s", dir)
		}
	}
	return errors.Join(errs...)
}

// addContentDir registers dir's .torrent with ezio unless its info hash is
// already known, reporting whether it added anything.
func addContentDir(
	ctx context.Context, c *seeder.Client, cfg config, dir string,
	existing map[string]seeder.Torrent,
) (bool, error) {
	// The info hash comes from torrent.info rather than the .torrent so
	// this agrees with the store's own content addressing by construction
	// (see store.ComputeInfoHash) instead of re-deriving it from the
	// bencoded file the publish step happened to write.
	info, err := store.LoadContentDirTorrentInfo(dir)
	if err != nil {
		return false, fmt.Errorf("load torrent.info: %w", err)
	}
	hash, err := store.ComputeInfoHash(info)
	if err != nil {
		return false, fmt.Errorf("compute info hash: %w", err)
	}
	if _, ok := existing[hash.String()]; ok {
		return false, nil
	}

	torrentPath := filepath.Join(dir, store.ContentTorrentFileName)
	torrentBytes, err := os.ReadFile(torrentPath) //nolint:gosec // mount path this pod owns, not user input
	if err != nil {
		return false, fmt.Errorf("read %s: %w", store.ContentTorrentFileName, err)
	}

	// seedMode: the content was hash-verified when the publish step copied
	// it into this PVC (see internal/ingest.publishPartition), so ezio
	// re-hashing every piece on start would only repeat that work.
	if err := c.AddTorrent(ctx, torrentBytes, dir, true, cfg.MaxUploads, cfg.MaxConnections); err != nil {
		return false, fmt.Errorf("add torrent %s: %w", hash, err)
	}
	return true, nil
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
