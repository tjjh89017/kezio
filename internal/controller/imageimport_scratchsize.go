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
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tjjh89017/kezio/internal/ingest"
)

// gibiByte is 1 GiB in bytes - the unit computeIngestScratchSizeBytes
// rounds its result up to, matching how a PVC's storage request is
// ordinarily expressed.
const gibiByte = 1024 * 1024 * 1024

// scratchSizeSourceFactor is the multiplier computeIngestScratchSizeBytes
// applies to a discovered source size when the source is not already raw
// (see scratchSizeSourceFactorRaw for that case). It covers the ingest
// scratch PVC's peak usage as internal/ingest/orchestrator.go's run() lays
// it out for a source that needs converting: source.img and its raw
// conversion disk.raw briefly sit on the volume together (source.img is
// deleted the moment disk.raw exists), after which disk.raw, at most one
// partition's part-N.raw slice, and that slice's cloned content-N output
// share it. Sizing from the (possibly compressed) source rather than the
// inflated raw disk is an approximation - 3x keeps enough headroom for
// source.img plus disk.raw's own inflation without trying to model a
// specific partition layout.
const scratchSizeSourceFactor = 3

// scratchSizeSourceFactorRaw is scratchSizeSourceFactor's counterpart for
// a source already declared raw (ImageIngestConfig.SourceFormat ==
// "raw"): run() skips the conversion copy entirely and slices partitions
// straight out of the downloaded source, so the volume's peak usage is
// only the source itself plus one partition's slice and its cloned
// content - roughly 2x the source size, not 3x.
const scratchSizeSourceFactorRaw = 2

// scratchSizeHTTPTimeout bounds one size-discovery HTTP call (image-service
// or an http(s):// source), so a reconcile never blocks indefinitely on an
// unreachable or slow peer.
const scratchSizeHTTPTimeout = 15 * time.Second

// computeIngestScratchSizeBytes returns the ingest scratch PVC size to
// request: floorBytes, or factor*sourceSizeBytes rounded up to the next
// GiB if that is larger. factor is scratchSizeSourceFactor for a
// converted source or scratchSizeSourceFactorRaw for one already raw -
// the caller picks based on the configured source format. sourceSizeKnown
// false (size could not be discovered) or a non-positive sourceSizeBytes
// both fall back to floorBytes untouched.
func computeIngestScratchSizeBytes(floorBytes, sourceSizeBytes int64, sourceSizeKnown bool, factor int64) int64 {
	if !sourceSizeKnown || sourceSizeBytes <= 0 {
		return floorBytes
	}
	computed := roundUpGiB(sourceSizeBytes * factor)
	if computed > floorBytes {
		return computed
	}
	return floorBytes
}

// roundUpGiB rounds n up to the next whole gibibyte.
func roundUpGiB(n int64) int64 {
	if rem := n % gibiByte; rem != 0 {
		return n + (gibiByte - rem)
	}
	return n
}

// discoverSourceSizeBytes tries to learn sourceURL's byte size without
// downloading it, for sizing the ingest scratch PVC ahead of running
// ingest. known is false whenever the size could not be determined - an
// unsupported scheme, no ImageServiceURL configured for a kezio-staged://
// source, a request error, a non-2xx response, or a response that carries
// no usable size - and the caller is expected to fall back to its own
// floor rather than treat this as fatal: ingest itself still discovers the
// real size regardless of what the scratch PVC was sized from.
func discoverSourceSizeBytes(ctx context.Context, cfg ImageIngestConfig, sourceURL string) (sizeBytes int64, known bool) {
	ctx, cancel := context.WithTimeout(ctx, scratchSizeHTTPTimeout)
	defer cancel()

	switch {
	case strings.HasPrefix(sourceURL, ingest.StagedURLScheme+"://"):
		if cfg.ImageServiceURL == "" {
			return 0, false
		}
		name := strings.TrimPrefix(sourceURL, ingest.StagedURLScheme+"://")
		return discoverStagedSourceSizeBytes(ctx, cfg.ImageServiceURL, cfg.ImageServiceToken, name)

	case strings.HasPrefix(sourceURL, "http://"), strings.HasPrefix(sourceURL, "https://"):
		return discoverHTTPSourceSizeBytes(ctx, sourceURL)

	default:
		return 0, false
	}
}

// discoverStagedSourceSizeBytes asks the image-service fronting a staging
// volume for a staged upload's size via HEAD /uploads/{name} (see
// imageservice.Server.handleUploadStat), reading its Content-Length.
func discoverStagedSourceSizeBytes(ctx context.Context, baseURL, token, name string) (int64, bool) {
	url := strings.TrimSuffix(baseURL, "/") + "/uploads/" + name
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return 0, false
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, false
	}
	return contentLengthOf(resp)
}

// discoverHTTPSourceSizeBytes learns an http(s):// source's size via a
// HEAD request's Content-Length. Some servers refuse HEAD (405/501) or
// omit Content-Length on it; when that happens this falls back to a GET
// with "Range: bytes=0-0" and reads the total size back out of the
// Content-Range response header, without reading more than the response
// headers - the body (whether the server honored the range or sent the
// whole object) is discarded unread.
func discoverHTTPSourceSizeBytes(ctx context.Context, sourceURL string) (int64, bool) {
	if size, ok := headContentLength(ctx, sourceURL); ok {
		return size, true
	}
	return rangeContentLength(ctx, sourceURL)
}

func headContentLength(ctx context.Context, sourceURL string) (int64, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, sourceURL, nil)
	if err != nil {
		return 0, false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, false
	}
	return contentLengthOf(resp)
}

func rangeContentLength(ctx context.Context, sourceURL string) (int64, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return 0, false
	}
	req.Header.Set("Range", "bytes=0-0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, false
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusPartialContent {
		return 0, false
	}
	return parseContentRangeTotal(resp.Header.Get("Content-Range"))
}

// contentLengthOf reads resp's Content-Length header. Go's http.Response
// already parses it into resp.ContentLength, but that field is -1 when the
// header is absent or malformed - the same "unknown" outcome this package
// treats identically everywhere else.
func contentLengthOf(resp *http.Response) (int64, bool) {
	if resp.ContentLength < 0 {
		return 0, false
	}
	return resp.ContentLength, true
}

// parseContentRangeTotal extracts the total size from a "Content-Range:
// bytes 0-0/<total>" header value. A "*" total (server does not know the
// full size) is reported as unknown, matching every other undiscoverable
// case here.
func parseContentRangeTotal(value string) (int64, bool) {
	_, total, ok := strings.Cut(value, "/")
	if !ok || total == "*" {
		return 0, false
	}
	n, err := strconv.ParseInt(total, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// scratchSizeUnknownEventReason names the Event this package's callers
// record when an import's scratch PVC could not be sized from its source
// and fell back to the configured floor.
const scratchSizeUnknownEventReason = "ScratchSizeUnknown"

// scratchSizeUnknownEventMessage is that Event's message, parametrized by
// the source URL that could not be sized.
func scratchSizeUnknownEventMessage(sourceURL string) string {
	return fmt.Sprintf("could not discover the size of source %q; the ingest scratch volume was sized from the configured floor instead", sourceURL)
}
