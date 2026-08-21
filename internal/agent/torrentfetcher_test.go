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
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPTorrentFetcher_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("torrent-bytes"))
	}))
	defer server.Close()

	f := HTTPTorrentFetcher{RetryInterval: time.Millisecond}
	got, err := f.FetchTorrent(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("FetchTorrent: %v", err)
	}
	if string(got) != "torrent-bytes" {
		t.Errorf("got %q, want %q", got, "torrent-bytes")
	}
}

func TestHTTPTorrentFetcher_RetriesThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	f := HTTPTorrentFetcher{RetryInterval: time.Millisecond}
	got, err := f.FetchTorrent(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("FetchTorrent: %v", err)
	}
	if string(got) != "ok" {
		t.Errorf("got %q, want %q", got, "ok")
	}
	if calls.Load() != 3 {
		t.Errorf("server received %d calls, want 3 (2 failures + 1 success)", calls.Load())
	}
}

func TestHTTPTorrentFetcher_GivesUpAfterRetries(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	f := HTTPTorrentFetcher{Retries: 2, RetryInterval: time.Millisecond}
	if _, err := f.FetchTorrent(context.Background(), server.URL); err == nil {
		t.Fatal("FetchTorrent: want an error once every retry is exhausted")
	}
}
