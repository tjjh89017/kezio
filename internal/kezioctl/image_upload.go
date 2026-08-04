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

package kezioctl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
)

// StdinArg is the file argument that selects stdin, matching the common
// "-" convention used by curl, tar, and kubectl.
const StdinArg = "-"

// ImageUploadOptions configures ImageUpload. It corresponds directly to
// `kezioctl image upload`'s flags.
type ImageUploadOptions struct {
	// File is the local path to upload, or StdinArg to read from Stdin.
	File string
	// Name is both the upload name at the image service and the created
	// Image's name.
	Name string
	// Namespace is the namespace the Image is created in.
	Namespace string
	// Format is the source image format (raw/qcow2/vmdk). Empty means
	// "detect from File's extension"; detection is skipped (and Format
	// must be set) when File is StdinArg, since stdin has no filename to
	// infer one from.
	Format string
	// Server is the image service base URL.
	Server string
	// Token authenticates the upload with the image service.
	Token string
	// Progress, when non-nil, receives upload progress lines. Intended
	// for os.Stderr.
	Progress io.Writer
	// Stdin is read from when File == StdinArg. Intended for os.Stdin;
	// overridable so tests do not depend on process stdin.
	Stdin io.Reader
}

// ImageUploadResult reports what ImageUpload created.
type ImageUploadResult struct {
	Upload UploadResponse
	Image  *keziov1alpha1.Image
}

// ImageUpload implements `kezioctl image upload`: it streams opts.File
// (or stdin) to the image service, then creates the Image CR once the
// checksum of the uploaded content is known.
//
// This order is load-bearing: ImageSpec.Source (which carries the
// checksum) is immutable once the Image exists (see ImageSpec's doc
// comment), so the checksum must come back from the upload before the
// Image is created — there is no create-then-patch fallback available if
// the upload fails partway or its checksum does not match what a caller
// might have guessed ahead of time.
func ImageUpload(ctx context.Context, httpClient *http.Client, k8sClient client.Client, opts ImageUploadOptions) (ImageUploadResult, error) {
	format := opts.Format
	if format == "" {
		if opts.File == StdinArg {
			return ImageUploadResult{}, errors.New("--format is required when reading from stdin (its extension cannot be detected)")
		}
		detected, ok := DetectFormatFromFilename(opts.File)
		if !ok {
			return ImageUploadResult{}, fmt.Errorf("cannot detect format from %q; pass --format explicitly (raw, qcow2, or vmdk)", opts.File)
		}
		format = detected
	}

	body, size, cleanup, err := openUploadSource(opts.File, opts.Stdin)
	if err != nil {
		return ImageUploadResult{}, err
	}
	defer cleanup()

	uploadResp, err := Upload(httpClient, UploadOptions{
		ServerURL: opts.Server,
		Token:     opts.Token,
		Name:      opts.Name,
		Progress:  opts.Progress,
	}, body, size)
	if err != nil {
		return ImageUploadResult{}, err
	}

	image := &keziov1alpha1.Image{
		ObjectMeta: metav1.ObjectMeta{
			Name:      opts.Name,
			Namespace: opts.Namespace,
		},
		Spec: keziov1alpha1.ImageSpec{
			Source: keziov1alpha1.ImageSource{
				URL:      uploadResp.URL,
				Format:   format,
				Checksum: uploadResp.Checksum,
			},
		},
	}
	if err := k8sClient.Create(ctx, image); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return ImageUploadResult{}, fmt.Errorf(
				"upload %s succeeded (checksum %s) but Image %s/%s already exists: "+
					"delete it first, or use a different --name",
				opts.Name, uploadResp.Checksum, opts.Namespace, opts.Name)
		}
		return ImageUploadResult{}, fmt.Errorf(
			"upload %s succeeded (checksum %s) but creating the Image failed: %w",
			opts.Name, uploadResp.Checksum, err)
	}

	return ImageUploadResult{Upload: uploadResp, Image: image}, nil
}

// openUploadSource resolves an ImageUploadOptions.File value into a
// ready-to-stream body and its exact size. The image service requires a
// declared Content-Length (see imageservice's handleUpload), so a stdin
// source — whose size is not known upfront — is first drained into a
// temp file to learn its size; that temp file is what actually gets
// streamed. cleanup removes the temp file, if one was created, and must
// always be called.
func openUploadSource(file string, stdin io.Reader) (body io.ReadCloser, size int64, cleanup func(), err error) {
	if file != StdinArg {
		f, err := os.Open(file) //nolint:gosec // file is a CLI-supplied path, the whole point of this command
		if err != nil {
			return nil, 0, func() {}, fmt.Errorf("open %s: %w", file, err)
		}
		info, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return nil, 0, func() {}, fmt.Errorf("stat %s: %w", file, err)
		}
		return f, info.Size(), func() { _ = f.Close() }, nil
	}

	if stdin == nil {
		stdin = os.Stdin
	}
	tmp, err := os.CreateTemp("", "kezioctl-upload-*")
	if err != nil {
		return nil, 0, func() {}, fmt.Errorf("buffer stdin: %w", err)
	}
	cleanup = func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}
	size, err = io.Copy(tmp, stdin)
	if err != nil {
		cleanup()
		return nil, 0, func() {}, fmt.Errorf("read stdin: %w", err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, 0, func() {}, fmt.Errorf("rewind buffered stdin: %w", err)
	}
	return tmp, size, cleanup, nil
}
