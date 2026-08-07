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
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// torrentFetchTimeout bounds a single GET attempt for one content
// partition's .torrent file - generous relative to the file's own tiny
// size (it scales with piece count, not with the partition's own data),
// since a slow attempt here should time out and retry rather than stall
// the whole deploy.
const torrentFetchTimeout = 30 * time.Second

// torrentFetchAttempts is how many times httpTorrentFetcher tries a GET
// before giving up. The per-Image seeder pod this URL points at may
// still be starting (no ordering dependency ties a Machine's deploy to
// its seeder Deployment becoming Ready), so a handful of retries with
// backoff covers that startup window without hanging indefinitely on a
// URL that is simply wrong.
const torrentFetchAttempts = 5

// torrentFetchBackoff is the delay between retries, doubling on each
// attempt (see httpTorrentFetcher.FetchTorrent).
const torrentFetchBackoff = 2 * time.Second

// httpTorrentFetcher is the production deploy.TorrentFetcher: it
// HTTP-GETs a PlanPartition.TorrentURL from the per-Image seeder pod
// agentserver resolved it to.
type httpTorrentFetcher struct {
	Client *http.Client
}

// FetchTorrent implements deploy.TorrentFetcher.
func (f httpTorrentFetcher) FetchTorrent(ctx context.Context, url string) ([]byte, error) {
	client := f.Client
	if client == nil {
		client = http.DefaultClient
	}

	var lastErr error
	for attempt := range torrentFetchAttempts {
		if attempt > 0 {
			backoff := torrentFetchBackoff * time.Duration(attempt)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
		}

		body, err := fetchOnce(ctx, client, url)
		if err == nil {
			return body, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("fetch %s: %d attempts failed, last error: %w", url, torrentFetchAttempts, lastErr)
}

// fetchOnce makes one bounded-timeout GET attempt against url.
func fetchOnce(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, torrentFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	return body, nil
}
