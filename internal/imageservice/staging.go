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

// Package imageservice implements the server-side ingest endpoint of
// `kezioctl image upload`: an HTTP service that receives a streamed image
// upload, stores it into a staging area, and returns a staged reference an
// Image's spec.source.url can point at. It does not create the Image CR;
// the uploading client does that once the upload completes.
package imageservice

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/util/validation"
)

// StagedURLScheme is the URL scheme a staged upload's reference uses. An
// Image's spec.source.url set to StagedURLScheme+"://"+<name> tells the
// ingest job to resolve the source from the staging volume instead of
// fetching it over the network.
const StagedURLScheme = "kezio-staged"

// uploadFileName is the name of the uploaded file inside its upload
// directory.
const uploadFileName = "upload.bin"

// uploadMetaFileName holds the sidecar metadata (checksum, size) recorded
// once an upload completes successfully. Its presence marks the upload as
// complete; a directory holding only a partial temp file has no meta file.
const uploadMetaFileName = "upload.sha256"

// ChecksumAlgorithm is the only checksum algorithm this service verifies.
// It matches the "<algorithm>:<hex digest>" convention used by
// Image.spec.source.checksum.
const ChecksumAlgorithm = "sha256"

// ErrInvalidName reports an upload name that fails validation: not a valid
// DNS-1123 label, or otherwise unsafe as a single path component.
var ErrInvalidName = errors.New("invalid upload name")

// ErrChecksumMismatch reports that the bytes actually received do not
// match a checksum the caller asserted (either the client-supplied
// checksum for this request, or the checksum already recorded for a prior
// upload under the same name).
var ErrChecksumMismatch = errors.New("checksum mismatch")

// ErrNameConflict reports that an upload name is already in use with
// different content, so this request cannot be treated as an idempotent
// retry.
var ErrNameConflict = errors.New("upload name already in use with different content")

// Staging manages the on-disk layout of the staging area:
//
//	<root>/
//	  uploads/
//	    <name>/
//	      upload.bin       # the received bytes, present only once complete
//	      upload.sha256    # sidecar: hex sha256 of upload.bin, completion marker
//	    .tmp/
//	      <random>         # partial uploads in progress; never a staged reference
//
// The root is expected to be a single mounted volume (a PVC or
// equivalent); this type only manipulates paths under it and never
// crosses that boundary.
type Staging struct {
	root string

	// finalizeMu serializes the check-existing/rename/writeMeta sequence
	// in Receive, so two concurrent uploads of the same name can never
	// interleave a rename from one with the metadata write from the
	// other. It guards metadata-sized I/O only - body streaming and
	// hashing happen before it is taken, so large uploads to different
	// names are not serialized by it.
	finalizeMu sync.Mutex
}

// NewStaging returns a Staging rooted at root. root must already exist;
// NewStaging creates the uploads/ and uploads/.tmp/ subdirectories under
// it if they are missing.
func NewStaging(root string) (*Staging, error) {
	s := &Staging{root: root}
	if err := os.MkdirAll(s.tmpDir(), 0o750); err != nil {
		return nil, fmt.Errorf("create staging tmp dir: %w", err)
	}
	return s, nil
}

func (s *Staging) uploadsDir() string { return filepath.Join(s.root, "uploads") }
func (s *Staging) tmpDir() string     { return filepath.Join(s.uploadsDir(), ".tmp") }

// ValidateName reports whether name is safe to use as a single path
// component under the staging root. It requires a valid DNS-1123 label
// (lowercase alphanumeric and '-', 1-63 chars) — the same shape kezioctl
// is expected to derive an upload name from (typically the target Image
// name) — so a name can never contain a path separator, "..", or other
// traversal payload regardless of how it reaches this function.
func ValidateName(name string) error {
	if msgs := validation.IsDNS1123Label(name); len(msgs) > 0 {
		return fmt.Errorf("%w: %s", ErrInvalidName, strings.Join(msgs, "; "))
	}
	return nil
}

// uploadDir returns the directory holding name's upload, after validating
// name. Callers must not build this path any other way: joining an
// unvalidated name into a filesystem path is exactly the path-traversal
// hazard ValidateName exists to rule out.
func (s *Staging) uploadDir(name string) (string, error) {
	if err := ValidateName(name); err != nil {
		return "", err
	}
	dir := filepath.Join(s.uploadsDir(), name)
	// Defense in depth: even though ValidateName already rejects any
	// input that could escape uploadsDir(), confirm the joined path is
	// still exactly one path component below it before it is ever used
	// for I/O.
	if filepath.Dir(dir) != s.uploadsDir() {
		return "", fmt.Errorf("%w: resolves outside the uploads directory", ErrInvalidName)
	}
	return dir, nil
}

// UploadedRef returns the staged reference URL for a completed upload
// named name. This is the value an Image's spec.source.url is set to.
func UploadedRef(name string) string {
	return StagedURLScheme + "://" + name
}

// ResolveUpload returns the local path of a completed upload named name,
// after validating name and confirming the upload actually completed (its
// meta file is present). This is how a consumer that mounts the same
// staging volume (the ingest job, resolving a kezio-staged:// source URL)
// turns a staged reference back into a file it can read; it never reads
// through this package's HTTP server.
func (s *Staging) ResolveUpload(name string) (string, error) {
	dir, err := s.uploadDir(name)
	if err != nil {
		return "", err
	}
	if _, err := readMeta(dir); err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("upload %q has not completed", name)
		}
		return "", fmt.Errorf("check upload %q: %w", name, err)
	}
	return filepath.Join(dir, uploadFileName), nil
}

// RemoveUpload deletes name's upload directory (the data file and its
// completion marker). It is used once ingest has safely stored a staged
// upload's content, so the staging volume does not keep an extra copy of
// every ingested image forever. Removing an upload that does not exist is
// not an error: it makes this idempotent, which matters if a retry runs
// this step twice.
func (s *Staging) RemoveUpload(name string) error {
	dir, err := s.uploadDir(name)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove upload %q: %w", name, err)
	}
	return nil
}

// UsedBytes returns the total size of every completed upload on the
// staging volume. An in-progress upload (.tmp file, or a directory with
// no completion marker yet) is not counted here - it's accounted for
// through Admission's in-flight reservation ledger instead. This is the
// "already staged" term in Admission's logical quota check.
func (s *Staging) UsedBytes() (int64, error) {
	entries, err := os.ReadDir(s.uploadsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read uploads directory: %w", err)
	}

	var total int64
	for _, e := range entries {
		if !e.IsDir() || e.Name() == filepath.Base(s.tmpDir()) {
			continue
		}
		meta, err := readMeta(filepath.Join(s.uploadsDir(), e.Name()))
		if err != nil {
			if os.IsNotExist(err) {
				continue // upload directory exists but has not completed yet
			}
			return 0, fmt.Errorf("read upload metadata for %q: %w", e.Name(), err)
		}
		total += meta.SizeBytes
	}
	return total, nil
}

// UploadResult describes a stored upload.
type UploadResult struct {
	// Name is the upload's name, as given by the client.
	Name string
	// Path is the absolute path of the stored file on the staging
	// volume. It is only meaningful to a process that mounts the same
	// volume (the ingest job); it is not returned to remote clients.
	Path string
	// SizeBytes is the size of the stored file.
	SizeBytes int64
	// Checksum is the sha256 checksum of the stored file, in
	// "sha256:<hex>" form.
	Checksum string
	// Idempotent is true when this call found a prior completed upload
	// with matching content and did not write anything new.
	Idempotent bool
}

// Receive writes body to a temp file, then atomically renames it into
// place as the named upload once fully received. On any error the temp
// file is removed and the final path is left exactly as before the call
// - never a partially written file at the final path.
//
// wantChecksum, when non-empty, is "sha256:<hex>"; a mismatch against the
// bytes actually read is ErrChecksumMismatch with nothing written.
//
// If a completed upload already exists under name, Receive still reads
// and hashes the full body (never trusting a client-supplied checksum
// over verifying the bytes sent), then compares against what's stored:
// equal is an idempotent retry (existing UploadResult, Idempotent set,
// nothing written); different is ErrNameConflict (a name is never
// silently repointed at different content).
func (s *Staging) Receive(name string, body io.Reader, wantChecksum string) (UploadResult, error) {
	dir, err := s.uploadDir(name)
	if err != nil {
		return UploadResult{}, err
	}

	if wantChecksum != "" {
		if _, _, err := parseChecksum(wantChecksum); err != nil {
			return UploadResult{}, err
		}
	}

	tmp, err := os.CreateTemp(s.tmpDir(), "upload-*")
	if err != nil {
		return UploadResult{}, fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	// Any return path other than the final, successful rename must clean
	// up the temp file: it must never be left behind, and it must never
	// be mistaken for a completed upload (it never lives at dir).
	succeeded := false
	defer func() {
		if !succeeded {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	h := sha256.New()
	size, err := io.Copy(tmp, io.TeeReader(body, h))
	if err != nil {
		return UploadResult{}, fmt.Errorf("write upload: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return UploadResult{}, fmt.Errorf("close upload temp file: %w", err)
	}

	got := ChecksumAlgorithm + ":" + hex.EncodeToString(h.Sum(nil))
	if wantChecksum != "" && !strings.EqualFold(got, wantChecksum) {
		return UploadResult{}, fmt.Errorf("%w: computed %s, expected %s", ErrChecksumMismatch, got, wantChecksum)
	}

	// From here on this call decides whether name is already staged with
	// this content, a conflicting name, or a brand new upload, and acts on
	// that decision (rename + writeMeta). That check-then-act sequence
	// must run without interleaving another Receive for the same name,
	// or one call's rename can land after another's writeMeta and leave
	// upload.bin and upload.sha256 pointing at different content.
	s.finalizeMu.Lock()
	defer s.finalizeMu.Unlock()

	if existing, err := readMeta(dir); err == nil {
		if strings.EqualFold(existing.Checksum, got) {
			succeeded = true // no new write; nothing to clean up beyond the temp file above
			return UploadResult{
				Name:       name,
				Path:       filepath.Join(dir, uploadFileName),
				SizeBytes:  existing.SizeBytes,
				Checksum:   existing.Checksum,
				Idempotent: true,
			}, nil
		}
		return UploadResult{}, fmt.Errorf("%w: existing checksum %s, new upload computed %s", ErrNameConflict, existing.Checksum, got)
	} else if !os.IsNotExist(err) {
		return UploadResult{}, fmt.Errorf("check existing upload: %w", err)
	}

	if err := os.MkdirAll(dir, 0o750); err != nil {
		return UploadResult{}, fmt.Errorf("create upload dir: %w", err)
	}
	finalPath := filepath.Join(dir, uploadFileName)
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return UploadResult{}, fmt.Errorf("finalize upload: %w", err)
	}
	if err := writeMeta(dir, uploadMeta{SizeBytes: size, Checksum: got}); err != nil {
		// The data file is in place but unmarked as complete; remove it
		// rather than leave an unverifiable file a later reconcile might
		// pick up. The caller sees an error either way.
		_ = os.Remove(finalPath)
		return UploadResult{}, fmt.Errorf("record upload metadata: %w", err)
	}

	succeeded = true
	return UploadResult{
		Name:      name,
		Path:      finalPath,
		SizeBytes: size,
		Checksum:  got,
	}, nil
}

// uploadMeta is the sidecar metadata recorded for a completed upload.
type uploadMeta struct {
	SizeBytes int64
	Checksum  string
}

// readMeta reads dir's completion marker. It returns an error satisfying
// os.IsNotExist when the upload has not completed (or does not exist).
func readMeta(dir string) (uploadMeta, error) {
	data, err := os.ReadFile(filepath.Join(dir, uploadMetaFileName)) //nolint:gosec // dir is built only via Staging.uploadDir, which validates the name component
	if err != nil {
		return uploadMeta{}, err
	}
	fields := strings.SplitN(strings.TrimSpace(string(data)), " ", 2)
	if len(fields) != 2 {
		return uploadMeta{}, fmt.Errorf("malformed upload metadata in %s", dir)
	}
	var size int64
	if _, err := fmt.Sscanf(fields[0], "%d", &size); err != nil {
		return uploadMeta{}, fmt.Errorf("malformed upload metadata size in %s: %w", dir, err)
	}
	return uploadMeta{SizeBytes: size, Checksum: fields[1]}, nil
}

// writeMeta writes dir's completion marker.
func writeMeta(dir string, m uploadMeta) error {
	content := fmt.Sprintf("%d %s\n", m.SizeBytes, m.Checksum)
	return os.WriteFile(filepath.Join(dir, uploadMetaFileName), []byte(content), 0o640) //nolint:gosec // dir is built only via Staging.uploadDir, which validates the name component
}

// parseChecksum splits a "<algorithm>:<hex digest>" checksum string and
// validates that the algorithm is the one this service supports.
func parseChecksum(checksum string) (algorithm, digest string, err error) {
	algorithm, digest, ok := strings.Cut(checksum, ":")
	if !ok || algorithm == "" || digest == "" {
		return "", "", fmt.Errorf("checksum %q is not in \"<algorithm>:<hex digest>\" form", checksum)
	}
	if !strings.EqualFold(algorithm, ChecksumAlgorithm) {
		return "", "", fmt.Errorf("unsupported checksum algorithm %q, only %q is supported", algorithm, ChecksumAlgorithm)
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return "", "", fmt.Errorf("checksum digest %q is not valid hex: %w", digest, err)
	}
	return algorithm, digest, nil
}
