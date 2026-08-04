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
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
)

// imageDeletePollInterval is how often ImageDelete re-checks whether a
// deleted Image is actually gone while --wait is set.
const imageDeletePollInterval = 2 * time.Second

// ImageDeleteOptions configures ImageDelete.
type ImageDeleteOptions struct {
	Name      string
	Namespace string
	// Wait blocks until the Image is actually removed (its finalizer
	// drained by the controller), rather than returning as soon as the
	// delete call is accepted.
	Wait bool
	// WaitTimeout bounds how long Wait polls before giving up. The zero
	// value means no timeout (wait until ctx is canceled).
	WaitTimeout time.Duration
	// WaitPollInterval overrides how often Wait re-checks the Image.
	// The zero value uses imageDeletePollInterval; tests set a shorter
	// interval so they do not have to wait out the production default.
	WaitPollInterval time.Duration
	// Status, when non-nil, receives human-readable progress lines
	// (in particular, the referenced-by message while deletion is
	// blocked). Intended for os.Stderr.
	Status io.Writer
}

// ImageDelete implements `kezioctl image delete`: it deletes the named
// Image, then does a best-effort check for Machines that still reference
// it, so a caller sees why deletion is blocked instead of it appearing
// to hang. This check is inherently racy (a Machine could start or stop
// referencing the Image between the check and the controller's own
// authoritative one) — it exists purely to make a stuck deletion legible,
// never to gate or serialize the delete call itself, which the API
// server and the Image controller's finalizer already do correctly on
// their own.
func ImageDelete(ctx context.Context, c client.Client, opts ImageDeleteOptions) error {
	image := &keziov1alpha1.Image{}
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

	reportBlockingMachines(ctx, c, opts)

	if !opts.Wait {
		return nil
	}
	return waitForImageGone(ctx, c, opts)
}

// reportBlockingMachines does a best-effort lookup of Machines that
// reference the Image being deleted and writes a message to opts.Status
// naming them. Any error from the lookup itself is swallowed (this is
// purely informational; it must never fail the delete command over a
// problem listing Machines).
func reportBlockingMachines(ctx context.Context, c client.Client, opts ImageDeleteOptions) {
	if opts.Status == nil {
		return
	}
	refs, err := machinesReferencingImage(ctx, c, opts.Namespace, opts.Name)
	if err != nil || len(refs) == 0 {
		return
	}
	_, _ = fmt.Fprintf(opts.Status, "Image %s/%s is still referenced by %d Machine(s), deletion will complete once they no longer reference it:\n",
		opts.Namespace, opts.Name, len(refs))
	for _, ref := range refs {
		_, _ = fmt.Fprintf(opts.Status, "  - %s\n", ref)
	}
}

// machinesReferencingImage lists every Machine (cluster-wide, since a
// Machine in another namespace can reference this Image through a
// NameRef with an explicit Namespace) whose spec or recorded
// provisioning status names the given Image, and returns them as
// "namespace/name" strings for display.
func machinesReferencingImage(ctx context.Context, c client.Client, imageNamespace, imageName string) ([]string, error) {
	machines := &keziov1alpha1.MachineList{}
	if err := c.List(ctx, machines); err != nil {
		return nil, fmt.Errorf("list Machines: %w", err)
	}

	var refs []string
	for i := range machines.Items {
		m := &machines.Items[i]
		if machineReferencesImage(m, imageNamespace, imageName) {
			refs = append(refs, m.Namespace+"/"+m.Name)
		}
	}
	return refs, nil
}

// machineReferencesImage reports whether m names the given Image,
// through spec.imageRef, spec.dataImages, or a recorded
// status.provisioning entry. It mirrors the Image controller's own
// in-use check (internal/controller's collectImageRefs), kept as a
// separate, best-effort copy here: kezioctl is a client-only binary that
// must not depend on the controller package's reconciler machinery just
// to render a helpful message.
func machineReferencesImage(m *keziov1alpha1.Machine, imageNamespace, imageName string) bool {
	check := func(ref keziov1alpha1.NameRef) bool {
		return ref.Name == imageName && keziov1alpha1.ResolveNamespace(ref, m.Namespace) == imageNamespace
	}

	if m.Spec.ImageRef != nil && check(*m.Spec.ImageRef) {
		return true
	}
	for _, di := range m.Spec.DataImages {
		if check(di.ImageRef) {
			return true
		}
	}
	if m.Status.Provisioning != nil {
		if m.Status.Provisioning.Image != nil && check(m.Status.Provisioning.Image.ImageRef) {
			return true
		}
		for _, di := range m.Status.Provisioning.DataImages {
			if check(di.ImageRef) {
				return true
			}
		}
	}
	return false
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
	image := &keziov1alpha1.Image{}
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
			if opts.WaitTimeout > 0 {
				return fmt.Errorf("timed out after %s waiting for Image %s/%s to be removed", opts.WaitTimeout, opts.Namespace, opts.Name)
			}
			return waitCtx.Err()
		case <-ticker.C:
		}
	}
}
