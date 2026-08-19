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

package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

func newMachineStatusWriteScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := keziov1alpha2.AddToScheme(s); err != nil {
		t.Fatalf("adding kezio scheme: %v", err)
	}
	return s
}

// TestApplyMachineStatusDropsCallbacksOnWriteFailure is the unit-level seam
// test for the events/metrics-behind-post-save-callbacks design: a failing
// status write must drop every queued callback along with the error, not
// just skip the ones a caller expects.
func TestApplyMachineStatusDropsCallbacksOnWriteFailure(t *testing.T) {
	s := newMachineStatusWriteScheme(t)
	machine := &keziov1alpha2.Machine{ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: "default"}}
	base := fake.NewClientBuilder().WithScheme(s).WithObjects(machine).Build()
	writeErr := errors.New("boom")
	c := interceptor.NewClient(base, interceptor.Funcs{
		SubResourcePatch: func(context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
			return writeErr
		},
	})

	r := &MachineReconciler{Client: c, Scheme: s}
	var ran bool
	err := r.applyMachineStatus(context.Background(), machine, func() { ran = true })
	if !errors.Is(err, writeErr) {
		t.Fatalf("applyMachineStatus() error = %v, want it to wrap %v", err, writeErr)
	}
	if ran {
		t.Fatal("callback ran despite the status write failing")
	}
}

// TestApplyMachineStatusRunsCallbacksInOrderOnlyOnSuccess is the companion
// success-path case: once the write succeeds, every callback runs exactly
// once, in the order given.
func TestApplyMachineStatusRunsCallbacksInOrderOnlyOnSuccess(t *testing.T) {
	s := newMachineStatusWriteScheme(t)
	machine := &keziov1alpha2.Machine{ObjectMeta: metav1.ObjectMeta{Name: "m2", Namespace: "default"}}
	base := fake.NewClientBuilder().WithScheme(s).WithObjects(machine).Build()
	c := interceptor.NewClient(base, interceptor.Funcs{
		SubResourcePatch: func(context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
			return nil
		},
	})

	r := &MachineReconciler{Client: c, Scheme: s}
	var order []int
	err := r.applyMachineStatus(context.Background(), machine,
		func() { order = append(order, 1) },
		func() { order = append(order, 2) },
	)
	if err != nil {
		t.Fatalf("applyMachineStatus() error = %v, want nil", err)
	}
	if want := []int{1, 2}; len(order) != len(want) || order[0] != want[0] || order[1] != want[1] {
		t.Fatalf("callback order = %v, want %v", order, want)
	}
}

// TestJitterDefaultBounds checks the default jitter implementation stays
// within the documented 0-25% band.
func TestJitterDefaultBounds(t *testing.T) {
	d := 100 * time.Millisecond
	upperBound := d + d/4
	for i := 0; i < 200; i++ {
		got := jitter(d)
		if got < d || got >= upperBound {
			t.Fatalf("jitter(%v) = %v, want within [%v, %v)", d, got, d, upperBound)
		}
	}
}

// TestJitterIdentityOverride confirms the package variable can be replaced
// wholesale, the seam a test relies on to keep interval assertions exact.
func TestJitterIdentityOverride(t *testing.T) {
	orig := jitter
	defer func() { jitter = orig }()
	jitter = func(d time.Duration) time.Duration { return d }

	d := 7 * time.Second
	if got := jitter(d); got != d {
		t.Fatalf("jitter() under identity override = %v, want %v", got, d)
	}
}
