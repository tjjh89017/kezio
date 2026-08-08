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
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
)

func TestImageDelete_NotFound(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(Scheme).Build()

	err := ImageDelete(context.Background(), c, ImageDeleteOptions{Name: "missing", Namespace: "default"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("ImageDelete() error = %v, want a not-found error", err)
	}
}

func TestImageDelete_RemovesImageWithoutFinalizer(t *testing.T) {
	image := &keziov1alpha1.Image{ObjectMeta: metav1.ObjectMeta{Name: "golden", Namespace: "default"}}
	c := fake.NewClientBuilder().WithScheme(Scheme).WithObjects(image).Build()

	if err := ImageDelete(context.Background(), c, ImageDeleteOptions{Name: "golden", Namespace: "default"}); err != nil {
		t.Fatalf("ImageDelete() error = %v", err)
	}

	err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "golden"}, &keziov1alpha1.Image{})
	if err == nil {
		t.Fatal("expected the Image to be gone")
	}
}

func TestImageDelete_ReportsBlockingMachines(t *testing.T) {
	image := &keziov1alpha1.Image{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "golden",
			Namespace:  "default",
			Finalizers: []string{keziov1alpha1.FinalizerName},
		},
	}
	machine := &keziov1alpha1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-01", Namespace: "default"},
		Spec: keziov1alpha1.MachineSpec{
			BMC:            keziov1alpha1.MachineBMC{Address: "redfish://example"},
			BootMACAddress: "aa:bb:cc:dd:ee:01",
			ImageRef:       &keziov1alpha1.NameRef{Name: "golden"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(Scheme).WithObjects(image, machine).Build()

	var status bytes.Buffer
	// A finalizer set means the fake client's Delete only stamps
	// DeletionTimestamp; it does not remove the object without something
	// draining the finalizer (matching real apiserver behavior, and the
	// exact situation ImageDelete's referenced-by message is for).
	err := ImageDelete(context.Background(), c, ImageDeleteOptions{
		Name:      "golden",
		Namespace: "default",
		Status:    &status,
	})
	if err != nil {
		t.Fatalf("ImageDelete() error = %v", err)
	}

	if !strings.Contains(status.String(), "default/node-01") {
		t.Errorf("status output = %q, want it to mention default/node-01", status.String())
	}

	stored := &keziov1alpha1.Image{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "golden"}, stored); err != nil {
		t.Fatalf("expected the Image to still exist (finalizer blocks it): %v", err)
	}
	if stored.DeletionTimestamp.IsZero() {
		t.Error("expected DeletionTimestamp to be set")
	}
}

func TestImageDelete_DataImagesAndProvisioningStatusAlsoCount(t *testing.T) {
	image := &keziov1alpha1.Image{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "data-disk",
			Namespace:  "default",
			Finalizers: []string{keziov1alpha1.FinalizerName},
		},
	}
	viaDataImage := &keziov1alpha1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "via-dataimage", Namespace: "default"},
		Spec: keziov1alpha1.MachineSpec{
			BMC:            keziov1alpha1.MachineBMC{Address: "redfish://example"},
			BootMACAddress: "aa:bb:cc:dd:ee:02",
			DataImages: []keziov1alpha1.MachineDataImage{
				{ImageRef: keziov1alpha1.NameRef{Name: "data-disk"}},
			},
		},
	}
	viaStatus := &keziov1alpha1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "via-status", Namespace: "default"},
		Spec: keziov1alpha1.MachineSpec{
			BMC:            keziov1alpha1.MachineBMC{Address: "redfish://example"},
			BootMACAddress: "aa:bb:cc:dd:ee:03",
		},
		Status: keziov1alpha1.MachineStatus{
			Provisioning: &keziov1alpha1.MachineProvisioningStatus{
				Image: &keziov1alpha1.MachineProvisionedImage{ImageRef: keziov1alpha1.NameRef{Name: "data-disk"}},
			},
		},
	}
	unrelated := &keziov1alpha1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: "default"},
		Spec: keziov1alpha1.MachineSpec{
			BMC:            keziov1alpha1.MachineBMC{Address: "redfish://example"},
			BootMACAddress: "aa:bb:cc:dd:ee:04",
		},
	}
	c := fake.NewClientBuilder().WithScheme(Scheme).WithObjects(image, viaDataImage, viaStatus, unrelated).
		WithStatusSubresource(&keziov1alpha1.Machine{}).Build()
	// WithStatusSubresource above means Status must be set via the status
	// writer normally; the fake client still honors an initial object's
	// Status as given via WithObjects, so this only affects later
	// updates, none of which this test performs.

	refs, err := machinesReferencingImage(context.Background(), c, "default", "data-disk")
	if err != nil {
		t.Fatalf("machinesReferencingImage() error = %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("refs = %v, want exactly 2 (via-dataimage, via-status)", refs)
	}
}

func TestImageDelete_Wait_BlocksUntilFinalizerDrained(t *testing.T) {
	image := &keziov1alpha1.Image{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "golden",
			Namespace:  "default",
			Finalizers: []string{keziov1alpha1.FinalizerName},
		},
	}
	c := fake.NewClientBuilder().WithScheme(Scheme).WithObjects(image).Build()

	// Simulate the controller draining the finalizer shortly after
	// deletion starts, from a separate goroutine, the way the real Image
	// controller would once nothing references it.
	go func() {
		for {
			time.Sleep(10 * time.Millisecond)
			got := &keziov1alpha1.Image{}
			if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "golden"}, got); err != nil {
				return // already gone
			}
			if got.DeletionTimestamp.IsZero() {
				continue
			}
			controllerutil.RemoveFinalizer(got, keziov1alpha1.FinalizerName)
			_ = c.Update(context.Background(), got)
			return
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := ImageDelete(ctx, c, ImageDeleteOptions{
		Name:             "golden",
		Namespace:        "default",
		Wait:             true,
		WaitPollInterval: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("ImageDelete() error = %v", err)
	}
}

func TestImageDelete_Wait_TimesOut(t *testing.T) {
	image := &keziov1alpha1.Image{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "stuck",
			Namespace:  "default",
			Finalizers: []string{keziov1alpha1.FinalizerName},
		},
	}
	c := fake.NewClientBuilder().WithScheme(Scheme).WithObjects(image).Build()

	err := ImageDelete(context.Background(), c, ImageDeleteOptions{
		Name:        "stuck",
		Namespace:   "default",
		Wait:        true,
		WaitTimeout: 50 * time.Millisecond,
	})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("ImageDelete() error = %v, want a timeout error", err)
	}
}
