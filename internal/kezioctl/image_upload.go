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
	// ImageImport's name.
	Name string
	// Namespace is the namespace the ImageImport is created in.
	Namespace string
	// ImageName is the name of the Image the import will create. Empty
	// defaults to Name.
	ImageName string
	// ContentPrefix names the PartitionContent objects the import will
	// capture ("<prefix>-p<partition number>"). Empty defaults to Name.
	ContentPrefix string
	// OSFamily is copied onto the import's spec.osFamily. Empty leaves the
	// CRD default in place.
	OSFamily string
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
	Import *keziov1alpha2.ImageImport
}

// ImageUpload implements `kezioctl image upload`: it streams opts.File to
// the image service, then creates the ImageImport CR once the checksum of
// the uploaded content is known.
//
// It creates no Image and names no partition content. Both are the
// import's own output: the partition table, every partition's role and
// file system, and every content's size only exist once partclone has
// run, and that run happens exactly once, in the cluster. This command
// therefore needs no qemu, no sfdisk, no partclone, and no advance
// knowledge of what is inside the file it uploads.
//
// The upload comes first because ImageImportSpec carries the checksum and
// is immutable once created (see its doc comment), so there is no
// create-then-patch fallback if the upload fails partway.
func ImageUpload(ctx context.Context, httpClient *http.Client, k8sClient client.Client, opts ImageUploadOptions) (ImageUploadResult, error) {
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

	imageName := opts.ImageName
	if imageName == "" {
		imageName = opts.Name
	}
	contentPrefix := opts.ContentPrefix
	if contentPrefix == "" {
		contentPrefix = opts.Name
	}

	imp := &keziov1alpha2.ImageImport{
		ObjectMeta: metav1.ObjectMeta{
			Name:      opts.Name,
			Namespace: opts.Namespace,
		},
		Spec: keziov1alpha2.ImageImportSpec{
			Source: keziov1alpha2.ImportSource{
				URL:      uploadResp.URL,
				Checksum: uploadResp.Checksum,
			},
			ImageName:     imageName,
			ContentPrefix: contentPrefix,
			OSFamily:      opts.OSFamily,
		},
	}
	if err := k8sClient.Create(ctx, imp); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return ImageUploadResult{}, fmt.Errorf(
				"upload %s succeeded (checksum %s) but ImageImport %s/%s already exists: "+
					"delete it first, or use a different --name",
				opts.Name, uploadResp.Checksum, opts.Namespace, opts.Name)
		}
		return ImageUploadResult{}, fmt.Errorf(
			"upload %s succeeded (checksum %s) but creating the ImageImport failed: %w",
			opts.Name, uploadResp.Checksum, err)
	}

	return ImageUploadResult{Upload: uploadResp, Import: imp}, nil
}
