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

package imageservice

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
)

// ChecksumHeader is the request header a client sets to assert the
// checksum of the bytes it is uploading, in "sha256:<hex digest>" form.
// It is optional; the service always computes the checksum of the bytes
// it actually receives regardless, so this header only lets a bad upload
// be caught without needing to wait for a later ingest-time check.
const ChecksumHeader = "X-Kezio-Checksum"

// DefaultMaxUploadBytes bounds a single upload when the deployment does
// not set a tighter limit. It is large because source disk images are
// large (see Image.spec.source.format: raw/qcow2/vmdk); it exists to stop
// an unbounded request from filling the staging volume, not to bound
// normal use.
const DefaultMaxUploadBytes = 1 << 40 // 1 TiB

// Server is the HTTP handler for the image upload service: the
// server-side target of `kezioctl image upload`. It streams a request
// body straight to the staging volume (never buffering a whole upload in
// memory) and returns a staged reference; it does not create or touch any
// Kubernetes object. The uploading client creates the Image CR itself
// once the upload it names in spec.source.url has succeeded.
type Server struct {
	staging        *Staging
	auth           *Authenticator
	maxUploadBytes int64
	logger         *slog.Logger
}

// NewServer builds a Server. maxUploadBytes <= 0 means DefaultMaxUploadBytes.
// logger nil means slog.Default().
func NewServer(staging *Staging, auth *Authenticator, maxUploadBytes int64, logger *slog.Logger) *Server {
	if maxUploadBytes <= 0 {
		maxUploadBytes = DefaultMaxUploadBytes
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{staging: staging, auth: auth, maxUploadBytes: maxUploadBytes, logger: logger}
}

// Handler returns the routed http.Handler. Every route but /healthz
// requires the bearer token (see Authenticator); /healthz is unauthenticated
// so it works as a Kubernetes liveness/readiness probe target.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("PUT /uploads/{name}", requireAuth(s.auth, s.handleUpload))
	return mux
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

// uploadResponse is the JSON body returned for a successful upload.
type uploadResponse struct {
	Name       string `json:"name"`
	URL        string `json:"url"`
	SizeBytes  int64  `json:"sizeBytes"`
	Checksum   string `json:"checksum"`
	Idempotent bool   `json:"idempotent"`
}

// handleUpload implements PUT /uploads/{name}: stream the request body to
// the staging area under name, verify checksums, and report the staged
// reference. See Staging.Receive for the idempotency and conflict rules.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	// A streamed upload must declare its length: it lets this handler
	// reject an oversize request before reading a single body byte
	// (never buffering it first), and rules out unbounded
	// chunked-transfer request bodies for this endpoint. MaxBytesReader
	// below is the second, independent layer that still applies even if
	// this check were ever bypassed (for example a client that lies
	// about Content-Length).
	if r.ContentLength < 0 {
		http.Error(w, "Content-Length is required", http.StatusLengthRequired)
		return
	}
	if r.ContentLength > s.maxUploadBytes {
		http.Error(w, "upload exceeds the maximum allowed size", http.StatusRequestEntityTooLarge)
		return
	}

	body := http.MaxBytesReader(w, r.Body, s.maxUploadBytes)
	checksum := r.Header.Get(ChecksumHeader)

	result, err := s.staging.Receive(name, body, checksum)
	if err != nil {
		s.writeUploadError(w, r, name, err)
		return
	}

	status := http.StatusCreated
	if result.Idempotent {
		status = http.StatusOK
	}
	s.writeJSON(w, status, uploadResponse{
		Name:       result.Name,
		URL:        UploadedRef(result.Name),
		SizeBytes:  result.SizeBytes,
		Checksum:   result.Checksum,
		Idempotent: result.Idempotent,
	})
}

// writeUploadError maps a Staging.Receive error to a response status. It
// never includes err's text for classes of failure that could leak
// filesystem layout details (anything not explicitly matched below comes
// back as an opaque 500); the real error is logged server-side instead.
func (s *Server) writeUploadError(w http.ResponseWriter, r *http.Request, name string, err error) {
	switch {
	case errors.Is(err, ErrInvalidName):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, ErrChecksumMismatch):
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
	case errors.Is(err, ErrNameConflict):
		http.Error(w, err.Error(), http.StatusConflict)
	case isMaxBytesError(err):
		// Belt-and-braces: the explicit Content-Length check in
		// handleUpload rejects an oversize declared length before any
		// read; this catches the remaining case of a client that
		// declares a small Content-Length but then streams more bytes
		// than it claimed.
		http.Error(w, "upload exceeds the maximum allowed size", http.StatusRequestEntityTooLarge)
	default:
		s.logger.Error("upload failed", "name", name, "remote", r.RemoteAddr, "err", err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
	}
}

// isMaxBytesError reports whether err was produced by http.MaxBytesReader
// tripping its limit.
func isMaxBytesError(err error) bool {
	var maxBytesErr *http.MaxBytesError
	return errors.As(err, &maxBytesErr)
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		s.logger.Error("write response body", "err", err)
	}
}
