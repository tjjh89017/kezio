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
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

func TestImageDelete_NotFound(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(Scheme).Build()

	err := ImageDelete(context.Background(), c, ImageDeleteOptions{Name: "missing", Namespace: "default"})
	if err == nil {
		t.Fatal("expected an error for a missing Image")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want it to mention not found", err.Error())
	}
}

func TestImageDelete_DeletesTheImage(t *testing.T) {
	img := &keziov1alpha2.Image{ObjectMeta: metav1.ObjectMeta{Name: "gone-soon", Namespace: "default"}}
	c := fake.NewClientBuilder().WithScheme(Scheme).WithObjects(img).Build()

	if err := ImageDelete(context.Background(), c, ImageDeleteOptions{Name: "gone-soon", Namespace: "default"}); err != nil {
		t.Fatalf("ImageDelete() error = %v", err)
	}

	err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "gone-soon"}, &keziov1alpha2.Image{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("Get() after delete = %v, want NotFound", err)
	}
}

func TestImageDelete_WaitReturnsOnceGone(t *testing.T) {
	img := &keziov1alpha2.Image{ObjectMeta: metav1.ObjectMeta{Name: "waited-for", Namespace: "default"}}
	c := fake.NewClientBuilder().WithScheme(Scheme).WithObjects(img).Build()

	err := ImageDelete(context.Background(), c, ImageDeleteOptions{
		Name:             "waited-for",
		Namespace:        "default",
		Wait:             true,
		WaitPollInterval: time.Millisecond,
		WaitTimeout:      time.Second,
	})
	if err != nil {
		t.Fatalf("ImageDelete() error = %v", err)
	}
}

func TestImageDelete_WaitCanceledByOuterContext(t *testing.T) {
	// A finalizer keeps the Image present past the delete call. The outer
	// context is canceled well before WaitTimeout elapses, so the error
	// must report cancellation, not a bogus timeout.
	img := &keziov1alpha2.Image{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "outer-canceled",
			Namespace:  "default",
			Finalizers: []string{"kezio.kojuro.date/test-hold"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(Scheme).WithObjects(img).Build()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()

	err := ImageDelete(ctx, c, ImageDeleteOptions{
		Name:             "outer-canceled",
		Namespace:        "default",
		Wait:             true,
		WaitPollInterval: time.Millisecond,
		WaitTimeout:      time.Minute,
	})
	if err == nil {
		t.Fatal("expected a cancellation error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %q, want it to wrap context.Canceled", err.Error())
	}
	if strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %q, must not claim a timeout when the outer context was canceled", err.Error())
	}
}

func TestImageDelete_WaitTimesOut(t *testing.T) {
	// A finalizer keeps the Image present past the delete call, so Wait
	// must give up once WaitTimeout elapses rather than block forever.
	img := &keziov1alpha2.Image{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "stuck",
			Namespace:  "default",
			Finalizers: []string{"kezio.kojuro.date/test-hold"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(Scheme).WithObjects(img).Build()

	err := ImageDelete(context.Background(), c, ImageDeleteOptions{
		Name:             "stuck",
		Namespace:        "default",
		Wait:             true,
		WaitPollInterval: time.Millisecond,
		WaitTimeout:      20 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %q, want it to mention timing out", err.Error())
	}
}
