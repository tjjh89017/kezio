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
	"context"
	"fmt"
	"io"
	"net/http"
	"os"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

// ImageUploadOptions configures ImageUpload. It corresponds directly to
// `kezioctl image upload`'s flags.
type ImageUploadOptions struct {
	// File is the local path to upload.
	File string
	// Name is both the upload name at the image service and the created
	// Image's name.
	Name string
	// Namespace is the namespace the Image is created in.
	Namespace string
	// LayoutFile is the path to a layout file (see LoadLayout) describing
	// ImageSpec.Layout. Required: ImageSpec.Layout has no zero value that
	// passes CRD validation.
	LayoutFile string
	// Server is the image service base URL.
	Server string
	// Token authenticates the upload with the image service.
	Token string
	// Progress, when non-nil, receives upload progress lines. Intended
	// for os.Stderr.
	Progress io.Writer
}

// ImageUploadResult reports what ImageUpload created.
type ImageUploadResult struct {
	Upload UploadResponse
	Image  *keziov1alpha2.Image
}

// ImageUpload implements `kezioctl image upload`: it streams opts.File to
// the image service, then creates the Image CR once the checksum of the
// uploaded content is known.
//
// This order is load-bearing: ImageSpec (which carries Source.Checksum
// and the immutable Layout) cannot be patched after the Image exists (see
// ImageSpec's doc comment), so the checksum must come back from the
// upload before the Image is created - there is no create-then-patch
// fallback available if the upload fails partway or its checksum does
// not match what a caller might have guessed ahead of time.
func ImageUpload(ctx context.Context, httpClient *http.Client, k8sClient client.Client, opts ImageUploadOptions) (ImageUploadResult, error) {
	layout, err := LoadLayout(opts.LayoutFile)
	if err != nil {
		return ImageUploadResult{}, err
	}

	f, err := os.Open(opts.File) //nolint:gosec // opts.File is a CLI-supplied path, the whole point of this command
	if err != nil {
		return ImageUploadResult{}, fmt.Errorf("open %s: %w", opts.File, err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return ImageUploadResult{}, fmt.Errorf("stat %s: %w", opts.File, err)
	}

	uploadResp, err := Upload(httpClient, UploadOptions{
		ServerURL: opts.Server,
		Token:     opts.Token,
		Name:      opts.Name,
		Progress:  opts.Progress,
	}, f, info.Size())
	if err != nil {
		return ImageUploadResult{}, err
	}

	image := &keziov1alpha2.Image{
		ObjectMeta: metav1.ObjectMeta{
			Name:      opts.Name,
			Namespace: opts.Namespace,
		},
		Spec: keziov1alpha2.ImageSpec{
			Layout: layout,
			Source: &keziov1alpha2.ImageSource{
				URL:      uploadResp.URL,
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
