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
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

// imageImportWaitPollInterval is how often ImageUpload's --wait re-checks
// the ImageImport and the Image it creates. Deliberately slower than the
// other kezioctl waits: ingest runs partclone over a whole disk image and
// takes minutes, so a tighter poll would only add noise.
const imageImportWaitPollInterval = 5 * time.Second

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
	// Progress, when non-nil, receives upload progress lines and, with
	// Wait set, one line per observed import state. Intended for
	// os.Stderr.
	Progress io.Writer
	// Wait blocks until the import has produced a usable Image, rather
	// than returning as soon as the ImageImport is created. The wait ends
	// on the Image becoming Ready - a finished import is only a waypoint,
	// the deployable Image is what the operator is after.
	Wait bool
	// WaitTimeout bounds how long Wait polls before giving up. The zero
	// value means no timeout (wait until ctx is canceled).
	WaitTimeout time.Duration
	// WaitPollInterval overrides how often Wait re-checks. The zero value
	// uses imageImportWaitPollInterval; tests set a shorter interval so
	// they do not have to wait out the production default.
	WaitPollInterval time.Duration
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
// With opts.Wait it also blocks until the import produces a usable Image
// (see waitForImportedImage); without it, it returns the moment the
// ImageImport is created.
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

	result := ImageUploadResult{Upload: uploadResp, Import: imp}
	if !opts.Wait {
		return result, nil
	}
	// The result is returned alongside a wait failure on purpose: the
	// ImageImport was created either way, and the caller reports that
	// before it reports why the wait ended.
	return result, waitForImportedImage(ctx, k8sClient, opts, imageName)
}

// importWaitState is one observation of the pair --wait follows.
type importWaitState struct {
	// line is the one-line report of this observation.
	line string
	// ready is true once the Image is Ready and the wait is over.
	ready bool
	// failure is non-nil once the import, or the Image it created, has
	// failed terminally. The wait ends on it rather than running out the
	// clock.
	failure error
}

// waitForImportedImage polls until the Image the import creates is Ready,
// the import (or that Image) fails terminally, opts.WaitTimeout elapses
// (if set), or ctx is canceled. It follows Status's watch loop: one line
// per observed change, never a line per poll.
//
// The ImageImport is watched beside the Image because an import can end
// terminally without ever creating one - a name already taken on the Image
// or on a PartitionContent, a checksum mismatch, an ingest Job that fails.
// Watching only the Image would report every one of those as a timeout,
// which tells the operator nothing.
func waitForImportedImage(ctx context.Context, c client.Client, opts ImageUploadOptions, imageName string) error {
	waitCtx := ctx
	if opts.WaitTimeout > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, opts.WaitTimeout)
		defer cancel()
	}

	pollInterval := opts.WaitPollInterval
	if pollInterval <= 0 {
		pollInterval = imageImportWaitPollInterval
	}

	var lastPrinted string
	observe := func() (bool, error) {
		state, err := observeImportWait(waitCtx, c, opts, imageName)
		if err != nil {
			return false, err
		}
		if opts.Progress != nil && state.line != lastPrinted {
			_, _ = fmt.Fprintln(opts.Progress, state.line)
			lastPrinted = state.line
		}
		if state.failure != nil {
			return false, state.failure
		}
		return state.ready, nil
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		ready, err := observe()
		if err != nil {
			return err
		}
		if ready {
			return nil
		}

		select {
		case <-waitCtx.Done():
			if opts.WaitTimeout > 0 && errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("timed out after %s waiting for Image %s/%s to become Ready",
					opts.WaitTimeout, opts.Namespace, imageName)
			}
			return fmt.Errorf("canceled while waiting for Image %s/%s to become Ready: %w",
				opts.Namespace, imageName, waitCtx.Err())
		case <-ticker.C:
		}
	}
}

// observeImportWait reads the ImageImport and the Image once and renders
// what it found. A context error from either read is not reported as a
// read failure: the caller's select turns it into the timeout or
// cancellation message.
func observeImportWait(ctx context.Context, c client.Client, opts ImageUploadOptions, imageName string) (importWaitState, error) {
	imp := &keziov1alpha2.ImageImport{}
	importKey := client.ObjectKey{Namespace: opts.Namespace, Name: opts.Name}
	importState := "absent"
	switch err := c.Get(ctx, importKey, imp); {
	case err == nil:
		importState = orDash(imp.Status.State)
	case apierrors.IsNotFound(err):
		// The import this command just created is gone; nothing is left to
		// produce the Image, so waiting on is pointless.
		return importWaitState{
			line: fmt.Sprintf("imageimport %s/%s: state=absent", opts.Namespace, opts.Name),
			failure: fmt.Errorf("imageimport %s/%s was removed while waiting for Image %s/%s",
				opts.Namespace, opts.Name, opts.Namespace, imageName),
		}, nil
	case isContextError(err):
	default:
		return importWaitState{}, fmt.Errorf("get ImageImport %s/%s: %w", opts.Namespace, opts.Name, err)
	}

	image := &keziov1alpha2.Image{}
	imageKey := client.ObjectKey{Namespace: opts.Namespace, Name: imageName}
	imageState := "absent"
	imageFound := false
	switch err := c.Get(ctx, imageKey, image); {
	case err == nil:
		imageFound = true
		imageState = orDash(image.Status.State)
	case apierrors.IsNotFound(err), isContextError(err):
	default:
		return importWaitState{}, fmt.Errorf("get Image %s/%s: %w", opts.Namespace, imageName, err)
	}

	state := importWaitState{
		line: fmt.Sprintf("imageimport %s/%s: state=%s, image %s/%s: state=%s",
			opts.Namespace, opts.Name, importState, opts.Namespace, imageName, imageState),
	}
	switch {
	case imageFound && image.Status.State == keziov1alpha2.ImageStateReady:
		state.ready = true
	case imp.Status.State == keziov1alpha2.ImageImportStateFailed:
		state.failure = fmt.Errorf("imageimport %s/%s failed: %s",
			opts.Namespace, opts.Name,
			conditionDetail(imp.Status.Conditions, keziov1alpha2.ImageImportConditionReady))
	case imageFound && image.Status.State == keziov1alpha2.ImageStateFailed:
		state.failure = fmt.Errorf("image %s/%s failed: %s",
			opts.Namespace, imageName,
			conditionDetail(image.Status.Conditions, keziov1alpha2.ImageConditionReady))
	}
	return state, nil
}

// conditionDetail renders the named condition's reason and message, which
// is what an operator needs to act on a terminal failure.
func conditionDetail(conditions []metav1.Condition, conditionType string) string {
	cond := meta.FindStatusCondition(conditions, conditionType)
	if cond == nil {
		return "no " + conditionType + " condition recorded"
	}
	if cond.Message == "" {
		return cond.Reason
	}
	return cond.Reason + ": " + cond.Message
}

func isContextError(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}
