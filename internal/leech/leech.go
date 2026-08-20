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

// Package leech drives one ezio daemon through a single leech: fetch a
// content's .torrent over HTTP, add it in non-seeding mode, wait for the
// download to finish, and reconstruct the original partition bytes from
// the downloaded extent files. cmd/leechctl is a thin CLI wrapper around
// Run; the logic lives here so it is unit testable without a real ezio
// daemon, cluster, or partclone binary.
package leech

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/tjjh89017/kezio/internal/seeder"
	"github.com/tjjh89017/kezio/internal/store"
)

// EzioClient is the subset of *seeder.Client's API Run needs, narrowed so
// tests can drive it without a real ezio daemon.
type EzioClient interface {
	AddTorrent(ctx context.Context, torrent []byte, savePath string, seedMode bool, maxUploads, maxConnections int32) error
	GetTorrentStatus(ctx context.Context, hashes []string) (map[string]seeder.Torrent, error)
}

// FetchTorrent GETs a .torrent file's bytes from url - the content's own
// seeder-register container serves it at
// http://<seeder>:seederdeploy.TorrentHTTPPort/torrents/<hash>.
func FetchTorrent(ctx context.Context, httpClient *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request for %s: %w", url, err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: unexpected status %s", url, resp.Status)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body from %s: %w", url, err)
	}
	return data, nil
}

// WaitFinished polls checker for hash's status every pollInterval until it
// reports IsFinished or ctx is done, whichever comes first. Callers bound
// the wait by giving ctx a deadline (context.WithTimeout).
func WaitFinished(ctx context.Context, checker EzioClient, hash string, pollInterval time.Duration) error {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		statuses, err := checker.GetTorrentStatus(ctx, []string{hash})
		if err != nil {
			return fmt.Errorf("get torrent status for %s: %w", hash, err)
		}
		if t, ok := statuses[hash]; ok && t.IsFinished {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("torrent %s did not finish: %w", hash, ctx.Err())
		case <-ticker.C:
		}
	}
}

// Reconstruct writes outPath as a partitionSizeBytes-long file with every
// extent file in extentsDir placed at the partition offset its name
// encodes (store.ParseExtentFileName) and every other byte zero. This is
// the leecher-side inverse of what partclone -T/--btfiles extracted at
// ingest time (see store.Extent's doc comment): BitTorrent only ever
// transports the extent files themselves, never the original partition
// bytes directly, so this is the one place they are put back at their
// real offsets for a byte-level comparison against the source partition.
func Reconstruct(extentsDir, outPath string, partitionSizeBytes int64) (err error) {
	out, err := os.Create(outPath) //nolint:gosec // outPath is caller-controlled, not user input
	if err != nil {
		return fmt.Errorf("create %s: %w", outPath, err)
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}()

	if err := out.Truncate(partitionSizeBytes); err != nil {
		return fmt.Errorf("truncate %s to %d bytes: %w", outPath, partitionSizeBytes, err)
	}

	entries, err := os.ReadDir(extentsDir)
	if err != nil {
		return fmt.Errorf("read extents dir %s: %w", extentsDir, err)
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		offset, perr := store.ParseExtentFileName(e.Name())
		if perr != nil {
			return fmt.Errorf("extents dir %s: %w", extentsDir, perr)
		}
		data, rerr := os.ReadFile(filepath.Join(extentsDir, e.Name())) //nolint:gosec // e.Name() came from ReadDir on extentsDir itself
		if rerr != nil {
			return fmt.Errorf("read extent file %s: %w", e.Name(), rerr)
		}
		end := int64(offset) + int64(len(data)) //nolint:gosec // offset/len(data) are file-derived, not attacker-controlled beyond this process's own inputs
		if end > partitionSizeBytes {
			return fmt.Errorf("extent file %s: offset+length %d exceeds partition size %d", e.Name(), end, partitionSizeBytes)
		}
		if _, werr := out.WriteAt(data, int64(offset)); werr != nil { //nolint:gosec // see above
			return fmt.Errorf("write extent file %s at offset %#x: %w", e.Name(), offset, werr)
		}
	}
	return nil
}

// SHA256File returns the lowercase hex sha256 digest of path's contents.
func SHA256File(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // path is caller-controlled, not user input
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Options configures Run. SavePath is the directory ezio downloads into;
// the extent files land at store.ContentDataDir(SavePath) once the
// torrent finishes.
type Options struct {
	TorrentURL         string
	InfoHash           string
	SavePath           string
	OutPath            string
	PartitionSizeBytes int64
	MaxUploads         int32
	MaxConnections     int32
	PollInterval       time.Duration
	// WantSHA256, when non-empty, makes Run return an error if the
	// reconstructed partition's digest does not match it.
	WantSHA256 string
}

// Result is what Run produced.
type Result struct {
	// SHA256 is the reconstructed partition file's digest.
	SHA256 string
}

// Run fetches opts.TorrentURL, adds it to client in leech mode
// (seedMode=false - unlike the seeder's own AddTorrent calls, this
// content is not yet trusted, so ezio verifies every piece against the
// torrent's own piece hashes as it downloads), waits for it to finish,
// reconstructs the partition at opts.OutPath, and hashes it. If
// opts.WantSHA256 is set and does not match, Run returns an error - the
// download still succeeded and reconstructed, but the byte-compare this
// exists for failed.
func Run(ctx context.Context, client EzioClient, httpClient *http.Client, opts Options) (Result, error) {
	torrent, err := FetchTorrent(ctx, httpClient, opts.TorrentURL)
	if err != nil {
		return Result{}, err
	}

	if err := client.AddTorrent(ctx, torrent, opts.SavePath, false, opts.MaxUploads, opts.MaxConnections); err != nil {
		return Result{}, fmt.Errorf("add torrent %s: %w", opts.InfoHash, err)
	}

	if err := WaitFinished(ctx, client, opts.InfoHash, opts.PollInterval); err != nil {
		return Result{}, err
	}

	extentsDir := store.ContentDataDir(opts.SavePath)
	if err := Reconstruct(extentsDir, opts.OutPath, opts.PartitionSizeBytes); err != nil {
		return Result{}, fmt.Errorf("reconstruct partition from %s: %w", extentsDir, err)
	}

	digest, err := SHA256File(opts.OutPath)
	if err != nil {
		return Result{}, err
	}

	if opts.WantSHA256 != "" && digest != opts.WantSHA256 {
		return Result{SHA256: digest}, fmt.Errorf("reconstructed partition sha256 %s does not match expected %s", digest, opts.WantSHA256)
	}
	return Result{SHA256: digest}, nil
}
