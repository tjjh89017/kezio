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

package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestComputeIngestScratchSizeBytes(t *testing.T) {
	const floor = 16 * gibiByte

	cases := []struct {
		name       string
		source     int64
		known      bool
		wantScaled bool // wantScaled: expect 3x source rounded up to GiB; otherwise expect floor
	}{
		{name: "unknown size falls back to floor", source: 0, known: false, wantScaled: false},
		{name: "zero size falls back to floor even if reported known", source: 0, known: true, wantScaled: false},
		{name: "negative size falls back to floor", source: -1, known: true, wantScaled: false},
		{name: "small known source stays at the floor", source: 1 * gibiByte, known: true, wantScaled: false},
		{name: "large known source scales past the floor", source: 20 * gibiByte, known: true, wantScaled: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeIngestScratchSizeBytes(floor, tc.source, tc.known, scratchSizeSourceFactor)
			if tc.wantScaled {
				want := roundUpGiB(tc.source * scratchSizeSourceFactor)
				if got != want {
					t.Errorf("computeIngestScratchSizeBytes(%d, %d, %v) = %d, want %d", floor, tc.source, tc.known, got, want)
				}
				if got <= floor {
					t.Errorf("computeIngestScratchSizeBytes(%d, %d, %v) = %d, want it to exceed the floor", floor, tc.source, tc.known, got)
				}
			} else if got != floor {
				t.Errorf("computeIngestScratchSizeBytes(%d, %d, %v) = %d, want floor %d", floor, tc.source, tc.known, got, floor)
			}
		})
	}
}

// TestComputeIngestScratchSizeBytes_RawFactorIsSmaller checks that a raw
// source's factor produces a smaller scratch request than a converted
// source's for the same discovered size, matching the smaller peak-usage
// layout a raw source needs (see scratchSizeSourceFactorRaw's doc
// comment).
func TestComputeIngestScratchSizeBytes_RawFactorIsSmaller(t *testing.T) {
	const floor = 1 * gibiByte
	const source = 20 * gibiByte

	rawSize := computeIngestScratchSizeBytes(floor, source, true, scratchSizeSourceFactorRaw)
	convertedSize := computeIngestScratchSizeBytes(floor, source, true, scratchSizeSourceFactor)
	if rawSize >= convertedSize {
		t.Errorf("raw-factor size %d, want it smaller than the converted-factor size %d", rawSize, convertedSize)
	}
	if want := roundUpGiB(source * scratchSizeSourceFactorRaw); rawSize != want {
		t.Errorf("computeIngestScratchSizeBytes(raw factor) = %d, want %d", rawSize, want)
	}
}

func TestRoundUpGiB(t *testing.T) {
	cases := []struct {
		name string
		n    int64
		want int64
	}{
		{name: "already aligned", n: 2 * gibiByte, want: 2 * gibiByte},
		{name: "rounds up", n: 2*gibiByte + 1, want: 3 * gibiByte},
		{name: "zero stays zero", n: 0, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := roundUpGiB(tc.n); got != tc.want {
				t.Errorf("roundUpGiB(%d) = %d, want %d", tc.n, got, tc.want)
			}
		})
	}
}

func TestDiscoverSourceSizeBytesHTTPHead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("unexpected method %s, want HEAD", r.Method)
		}
		w.Header().Set("Content-Length", "12345")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	size, known := discoverSourceSizeBytes(context.Background(), ImageIngestConfig{}, srv.URL+"/disk.img")
	if !known {
		t.Fatal("expected size to be known")
	}
	if size != 12345 {
		t.Errorf("size = %d, want 12345", size)
	}
}

func TestDiscoverSourceSizeBytesHTTPRangeFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			// A server that refuses HEAD outright, forcing the Range
			// fallback.
			w.WriteHeader(http.StatusMethodNotAllowed)
		case http.MethodGet:
			if r.Header.Get("Range") != "bytes=0-0" {
				t.Errorf("Range header = %q, want bytes=0-0", r.Header.Get("Range"))
			}
			w.Header().Set("Content-Range", "bytes 0-0/98765")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte{0})
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}))
	defer srv.Close()

	size, known := discoverSourceSizeBytes(context.Background(), ImageIngestConfig{}, srv.URL+"/disk.img")
	if !known {
		t.Fatal("expected size to be known")
	}
	if size != 98765 {
		t.Errorf("size = %d, want 98765", size)
	}
}

func TestDiscoverSourceSizeBytesHTTPUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Neither HEAD nor the Range fallback yields a usable size.
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	defer srv.Close()

	_, known := discoverSourceSizeBytes(context.Background(), ImageIngestConfig{}, srv.URL+"/disk.img")
	if known {
		t.Fatal("expected size to be unknown")
	}
}

func TestDiscoverSourceSizeBytesStaged(t *testing.T) {
	const token = "test-token"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("unexpected method %s, want HEAD", r.Method)
		}
		if r.URL.Path != "/uploads/golden-1" {
			t.Errorf("path = %q, want /uploads/golden-1", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("Authorization = %q, want Bearer %s", got, token)
		}
		w.Header().Set("Content-Length", "55555")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := ImageIngestConfig{ImageServiceURL: srv.URL, ImageServiceToken: token}
	size, known := discoverSourceSizeBytes(context.Background(), cfg, "kezio-staged://golden-1")
	if !known {
		t.Fatal("expected size to be known")
	}
	if size != 55555 {
		t.Errorf("size = %d, want 55555", size)
	}
}

func TestDiscoverSourceSizeBytesStagedNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	cfg := ImageIngestConfig{ImageServiceURL: srv.URL, ImageServiceToken: "irrelevant"}
	_, known := discoverSourceSizeBytes(context.Background(), cfg, "kezio-staged://missing")
	if known {
		t.Fatal("expected size to be unknown")
	}
}

func TestDiscoverSourceSizeBytesStagedNoImageServiceConfigured(t *testing.T) {
	_, known := discoverSourceSizeBytes(context.Background(), ImageIngestConfig{}, "kezio-staged://golden-1")
	if known {
		t.Fatal("expected size to be unknown when no ImageServiceURL is configured")
	}
}

func TestDiscoverSourceSizeBytesUnsupportedScheme(t *testing.T) {
	_, known := discoverSourceSizeBytes(context.Background(), ImageIngestConfig{}, "ftp://example.test/disk.img")
	if known {
		t.Fatal("expected size to be unknown for an unsupported scheme")
	}
}

// TestScratchSizeUnknownEventMessageNamesSource checks
// scratchSizeUnknownEventMessage mentions the source URL it was given,
// since callers rely on that for a useful Event.
func TestScratchSizeUnknownEventMessageNamesSource(t *testing.T) {
	msg := scratchSizeUnknownEventMessage("https://example.test/disk.img")
	if !strings.Contains(msg, "https://example.test/disk.img") {
		t.Errorf("message %q does not mention the source URL", msg)
	}
}
