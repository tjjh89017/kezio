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

package ingest

import (
	"context"
	"fmt"
	"strings"

	"github.com/tjjh89017/kezio/internal/imageservice"
)

// Downloader fetches an http(s):// URL to a local file. The exec-backed
// production implementation lives in cmd/ingest; tests use a fake.
type Downloader interface {
	Download(ctx context.Context, url, destPath string) error
}

// StagingResolver resolves a kezio-staged:// reference to a local file
// path on a mounted staging volume, and removes it once ingest has
// consumed it. *imageservice.Staging implements this directly.
type StagingResolver interface {
	ResolveUpload(name string) (string, error)
}

// ResolveSource turns an Image's spec.source.url into a local file path
// holding the source image bytes:
//   - an http:// or https:// URL is downloaded to destPath via downloader.
//   - a kezio-staged://<name> URL is resolved against staging (the same
//     staging volume the image-service writes to) and used in place,
//     with no copy: the returned path points directly at the staged
//     file, and staged reports true so the caller knows to remove it
//     from staging once ingest has safely stored its content (see
//     CleanupStagedSource).
//
// Any other scheme is rejected: ingest only knows how to fetch these two
// kinds of source.
func ResolveSource(ctx context.Context, sourceURL string, downloader Downloader, staging StagingResolver, destPath string) (path string, staged bool, err error) {
	switch {
	case strings.HasPrefix(sourceURL, "http://"), strings.HasPrefix(sourceURL, "https://"):
		if err := downloader.Download(ctx, sourceURL, destPath); err != nil {
			return "", false, fmt.Errorf("download source %s: %w", sourceURL, err)
		}
		return destPath, false, nil

	case strings.HasPrefix(sourceURL, imageservice.StagedURLScheme+"://"):
		if staging == nil {
			// Staging is an optional Dependency (see Dependencies' doc
			// comment): cmd/ingest only wires it when STAGING_ROOT is
			// set, which in turn only happens when the Image controller
			// was configured with a staging volume (IngestConfig.
			// StagingVolume / INGEST_STAGING_PVC). A kezio-staged://
			// source with no staging resolver wired is a deployment
			// configuration gap, not a bug in this Image - fail with an
			// actionable message instead of dereferencing a nil
			// interface, since this becomes the Job pod's termination
			// message.
			return "", false, fmt.Errorf(
				"source %s requires a staging resolver, but none is configured: "+
					"set STAGING_ROOT on the ingest Job (INGEST_STAGING_PVC on the controller-manager)",
				sourceURL)
		}
		name := strings.TrimPrefix(sourceURL, imageservice.StagedURLScheme+"://")
		p, err := staging.ResolveUpload(name)
		if err != nil {
			return "", false, fmt.Errorf("resolve staged source %s: %w", sourceURL, err)
		}
		return p, true, nil

	default:
		return "", false, fmt.Errorf("unsupported source URL scheme in %q: expected http://, https://, or %s://", sourceURL, imageservice.StagedURLScheme)
	}
}

// StagedRemover removes a completed upload from staging once ingest no
// longer needs it. *imageservice.Staging implements this directly.
type StagedRemover interface {
	RemoveUpload(name string) error
}

// CleanupStagedSource removes name's staged upload, logging is left to
// the caller. It is best-effort by design: a failure here must not turn
// an otherwise-successful ingest into a failure, since the content is
// already safely stored. Callers should log and continue past a non-nil
// error.
func CleanupStagedSource(remover StagedRemover, name string) error {
	if err := remover.RemoveUpload(name); err != nil {
		return fmt.Errorf("remove staged upload %s: %w", name, err)
	}
	return nil
}

// StagedNameFromURL extracts the upload name from a kezio-staged:// URL.
// The caller is expected to have already confirmed the scheme (see
// ResolveSource).
func StagedNameFromURL(sourceURL string) string {
	return strings.TrimPrefix(sourceURL, imageservice.StagedURLScheme+"://")
}
