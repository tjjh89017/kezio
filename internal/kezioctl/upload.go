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

package kezioctl

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// UploadResponse mirrors the JSON body the image service returns for a
// successful upload (see internal/imageservice's uploadResponse). It is
// redeclared here, rather than imported, because kezioctl is a
// client-only binary: it must never need the imageservice package's
// server-side dependencies (staging directories, and so on) to build or
// run.
type UploadResponse struct {
	Name       string `json:"name"`
	URL        string `json:"url"`
	SizeBytes  int64  `json:"sizeBytes"`
	Checksum   string `json:"checksum"`
	Idempotent bool   `json:"idempotent"`
}

// UploadOptions configures a call to Upload.
type UploadOptions struct {
	// ServerURL is the image service's base URL, for example
	// "http://kezio-image-service.kezio-system.svc:8080".
	ServerURL string
	// Token is the bearer token sent as "Authorization: Bearer <Token>".
	Token string
	// Name is the upload name; the server stores it at PUT
	// /uploads/{name} and the created Image's spec.source.url points at
	// it (see imageservice.UploadedRef).
	Name string
	// ProgressInterval is the minimum time between progress lines written
	// to Progress. The zero value uses a 1-second default.
	ProgressInterval time.Duration
	// Progress, when non-nil, receives simple byte-count progress lines
	// while the upload streams. Intended for os.Stderr; nil disables
	// progress reporting.
	Progress io.Writer
}

// Upload streams size bytes read from body to the image service's PUT
// /uploads/{name} endpoint and returns the parsed response. The caller
// must know size upfront (the server requires a declared Content-Length,
// see imageservice's handleUpload) and body must yield exactly that many
// bytes.
//
// The sha256 of the bytes actually streamed is computed in the same pass
// as the upload (a TeeReader ahead of the request body, never a second
// read of body) and compared against the server's response checksum
// before Upload returns success: the server already computes and returns
// its own hash of what it received, so this is a cheap end-to-end check
// that the bytes the server stored are the bytes this process actually
// read, catching a client-side read error or transport corruption that
// happens not to trip the HTTP layer.
//
// Errors returned by the server are translated into messages a CLI user
// can act on: which flag to check for a 401, that the name is taken for a
// 409, and so on. Any other non-2xx status is reported with the server's
// response body, truncated, since imageservice only ever sends
// http.Error-shaped plain text for those.
func Upload(client *http.Client, opts UploadOptions, body io.Reader, size int64) (UploadResponse, error) {
	if opts.ServerURL == "" {
		return UploadResponse{}, errors.New("upload server URL is empty")
	}
	if opts.Name == "" {
		return UploadResponse{}, errors.New("upload name is empty")
	}

	url := trimRightSlash(opts.ServerURL) + "/uploads/" + opts.Name

	hasher := sha256.New()
	reqBody := io.TeeReader(body, hasher)
	if opts.Progress != nil {
		reqBody = &progressReader{
			r:        reqBody,
			total:    size,
			w:        opts.Progress,
			interval: opts.ProgressInterval,
		}
	}

	req, err := http.NewRequest(http.MethodPut, url, reqBody)
	if err != nil {
		return UploadResponse{}, fmt.Errorf("build upload request: %w", err)
	}
	req.ContentLength = size
	req.Header.Set("Authorization", "Bearer "+opts.Token)

	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return UploadResponse{}, fmt.Errorf("upload to %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return UploadResponse{}, uploadError(resp)
	}

	var out UploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return UploadResponse{}, fmt.Errorf("decode upload response: %w", err)
	}

	if out.Checksum == "" {
		return UploadResponse{}, errors.New(
			"upload integrity check failed: server response carried no checksum; cannot verify upload integrity")
	}
	streamedChecksum := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	if streamedChecksum != out.Checksum {
		return UploadResponse{}, fmt.Errorf(
			"upload integrity check failed: streamed content hashed to %s but the server reports %s",
			streamedChecksum, out.Checksum)
	}

	if opts.Progress != nil && size > 0 {
		_, _ = fmt.Fprintf(opts.Progress, "uploaded %d/%d bytes (100%%)\n", size, size)
	}
	return out, nil
}

// uploadError turns a non-2xx upload response into an actionable error.
// The status codes matched here are exactly the ones imageservice's
// writeUploadError can produce.
func uploadError(resp *http.Response) error {
	const maxBodyPreview = 4 << 10 // more than enough for imageservice's plain-text errors
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyPreview))
	msg := trimNewline(string(body))

	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return fmt.Errorf("upload rejected: not authorized (check --token/--token-file or KEZIOCTL_TOKEN): %s", msg)
	case http.StatusConflict:
		return fmt.Errorf("upload rejected: name already in use with different content; choose a different --name or delete the existing Image first: %s", msg)
	case http.StatusUnprocessableEntity:
		return fmt.Errorf("upload rejected: checksum mismatch, the bytes received did not match: %s", msg)
	case http.StatusRequestEntityTooLarge:
		return fmt.Errorf("upload rejected: file exceeds the server's maximum upload size: %s", msg)
	case http.StatusLengthRequired:
		return fmt.Errorf("upload rejected: server requires a known content length: %s", msg)
	case http.StatusInsufficientStorage:
		return fmt.Errorf("upload rejected: image service has insufficient staging space or quota: %s", msg)
	default:
		return fmt.Errorf("upload failed with status %s: %s", resp.Status, msg)
	}
}

func trimNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

// trimRightSlash trims exactly one trailing "/" from s, so a --server
// value given with or without a trailing slash produces the same request
// URL.
func trimRightSlash(s string) string {
	if len(s) > 0 && s[len(s)-1] == '/' {
		return s[:len(s)-1]
	}
	return s
}

// progressReader wraps an io.Reader and writes simple byte-count progress
// to w as it is read, no more often than interval. It never buffers the
// underlying data; it exists purely to give large-file uploads visible
// progress on stderr, per kezioctl's no-fancy-TUI requirement.
type progressReader struct {
	r        io.Reader
	total    int64 // 0 (or unknown) when the total size is not meaningful to report
	w        io.Writer
	interval time.Duration

	read     int64
	lastShow time.Time
}

func (p *progressReader) Read(buf []byte) (int, error) {
	n, err := p.r.Read(buf)
	if n > 0 {
		p.read += int64(n)
		p.maybeReport()
	}
	return n, err
}

func (p *progressReader) maybeReport() {
	interval := p.interval
	if interval <= 0 {
		interval = time.Second
	}
	now := time.Now()
	if !p.lastShow.IsZero() && now.Sub(p.lastShow) < interval {
		return
	}
	p.lastShow = now
	if p.total > 0 {
		pct := float64(p.read) / float64(p.total) * 100
		_, _ = fmt.Fprintf(p.w, "uploaded %d/%d bytes (%.0f%%)\n", p.read, p.total, pct)
	} else {
		_, _ = fmt.Fprintf(p.w, "uploaded %d bytes\n", p.read)
	}
}
