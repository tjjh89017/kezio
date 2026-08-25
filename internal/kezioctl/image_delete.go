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
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

// imageDeletePollInterval is how often ImageDelete re-checks whether a
// deleted Image is actually gone while --wait is set.
const imageDeletePollInterval = 2 * time.Second

// ImageDeleteOptions configures ImageDelete.
type ImageDeleteOptions struct {
	Name      string
	Namespace string
	// Wait blocks until the Image is actually removed, rather than
	// returning as soon as the delete call is accepted. An Image carries
	// no finalizer of its own; a delete this command issues is expected
	// to complete promptly. Any PartitionContent it referenced is a
	// separate object with its own finalizer (PartitionContentFinalizer)
	// that keeps shared content alive while another Image or an active
	// DeployRun still references it - that survival is server-side and
	// entirely transparent to this command.
	Wait bool
	// WaitTimeout bounds how long Wait polls before giving up. The zero
	// value means no timeout (wait until ctx is canceled).
	WaitTimeout time.Duration
	// WaitPollInterval overrides how often Wait re-checks the Image. The
	// zero value uses imageDeletePollInterval; tests set a shorter
	// interval so they do not have to wait out the production default.
	WaitPollInterval time.Duration
}

// ImageDelete implements `kezioctl image delete`: it deletes the named
// Image. Any PartitionContent it referenced is reference-counted and
// finalized server-side (see ImageDeleteOptions.Wait's doc comment), so
// this command does nothing beyond the delete call itself and, if asked,
// waiting for the Image object to actually disappear.
func ImageDelete(ctx context.Context, c client.Client, opts ImageDeleteOptions) error {
	image := &keziov1alpha3.Image{}
	key := client.ObjectKey{Namespace: opts.Namespace, Name: opts.Name}
	if err := c.Get(ctx, key, image); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("image %s/%s not found", opts.Namespace, opts.Name)
		}
		return fmt.Errorf("get Image %s/%s: %w", opts.Namespace, opts.Name, err)
	}

	if err := c.Delete(ctx, image); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete Image %s/%s: %w", opts.Namespace, opts.Name, err)
	}

	if !opts.Wait {
		return nil
	}
	return waitForImageGone(ctx, c, opts)
}

// waitForImageGone polls until the Image named in opts is actually
// removed, or opts.WaitTimeout elapses (if set), or ctx is canceled.
func waitForImageGone(ctx context.Context, c client.Client, opts ImageDeleteOptions) error {
	waitCtx := ctx
	if opts.WaitTimeout > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, opts.WaitTimeout)
		defer cancel()
	}

	pollInterval := opts.WaitPollInterval
	if pollInterval <= 0 {
		pollInterval = imageDeletePollInterval
	}

	key := client.ObjectKey{Namespace: opts.Namespace, Name: opts.Name}
	image := &keziov1alpha3.Image{}
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		err := c.Get(waitCtx, key, image)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("get Image %s/%s: %w", opts.Namespace, opts.Name, err)
		}

		select {
		case <-waitCtx.Done():
			if opts.WaitTimeout > 0 && errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("timed out after %s waiting for Image %s/%s to be removed", opts.WaitTimeout, opts.Namespace, opts.Name)
			}
			return fmt.Errorf("canceled while waiting for Image %s/%s to be removed: %w", opts.Namespace, opts.Name, waitCtx.Err())
		case <-ticker.C:
		}
	}
}
