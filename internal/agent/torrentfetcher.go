/*
Copyright 2026.

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

package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/tjjh89017/kezio/internal/agent/deploy"
)

// DefaultTorrentFetchRetries is how many times HTTPTorrentFetcher retries
// a failed fetch before giving up, when Retries is zero. The per-Image
// seeder pod a torrent URL points at may still be coming up when a
// deploy reaches that slot (it has no ordering dependency on any
// machine's deploy), so a handful of retries covers that startup race
// without hanging indefinitely on a genuinely unreachable seeder.
const DefaultTorrentFetchRetries = 5

// DefaultTorrentFetchRetryInterval is the wait between
// HTTPTorrentFetcher retries, when RetryInterval is zero.
const DefaultTorrentFetchRetryInterval = 2 * time.Second

// HTTPTorrentFetcher is the production deploy.TorrentFetcher: it fetches
// a .torrent file over plain HTTP GET, retrying transiently on failure.
type HTTPTorrentFetcher struct {
	// Client performs the request. Nil means http.DefaultClient.
	Client *http.Client
	// Retries is how many attempts HTTPTorrentFetcher makes before
	// giving up. Zero means DefaultTorrentFetchRetries.
	Retries int
	// RetryInterval is how long to wait between attempts. Zero means
	// DefaultTorrentFetchRetryInterval.
	RetryInterval time.Duration
}

var _ deploy.TorrentFetcher = HTTPTorrentFetcher{}

// FetchTorrent implements deploy.TorrentFetcher.
func (f HTTPTorrentFetcher) FetchTorrent(ctx context.Context, url string) ([]byte, error) {
	retries := f.Retries
	if retries <= 0 {
		retries = DefaultTorrentFetchRetries
	}
	interval := f.RetryInterval
	if interval <= 0 {
		interval = DefaultTorrentFetchRetryInterval
	}
	client := f.Client
	if client == nil {
		client = http.DefaultClient
	}

	var lastErr error
	for attempt := 0; attempt < retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(interval):
			}
		}
		body, err := fetchOnce(ctx, client, url)
		if err == nil {
			return body, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("fetching %s after %d attempts: %w", url, retries, lastErr)
}

func fetchOnce(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}
	return io.ReadAll(resp.Body)
}
