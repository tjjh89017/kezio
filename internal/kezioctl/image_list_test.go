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
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

func TestWriteImageList_SingleNamespace(t *testing.T) {
	notBootable := false
	images := []keziov1alpha3.Image{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "golden",
				CreationTimestamp: metav1.NewTime(time.Now().Add(-2 * time.Hour)),
			},
			Spec: keziov1alpha3.ImageSpec{
				OSFamily: keziov1alpha3.OSFamilyLinux,
			},
			Status: keziov1alpha3.ImageStatus{
				State: keziov1alpha3.ImageStateReady,
				Conditions: []metav1.Condition{
					{Type: keziov1alpha3.ImageConditionReady, Status: metav1.ConditionTrue, Reason: "Ready", Message: "ready"},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "data-disk",
				CreationTimestamp: metav1.NewTime(time.Now().Add(-30 * time.Second)),
			},
			Spec: keziov1alpha3.ImageSpec{
				Bootable: &notBootable,
				OSFamily: keziov1alpha3.OSFamilyOther,
			},
			// Status.State left empty: still Pending, no Ready condition.
		},
	}

	var buf bytes.Buffer
	if err := WriteImageList(&buf, images, false); err != nil {
		t.Fatalf("WriteImageList() error = %v", err)
	}
	out := buf.String()

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3 (header + 2 rows):\n%s", len(lines), out)
	}
	if !strings.HasPrefix(lines[0], "NAME") || strings.Contains(lines[0], "NAMESPACE") {
		t.Errorf("header = %q, want NAME-first without NAMESPACE column", lines[0])
	}
	if !strings.Contains(lines[0], "READY") {
		t.Errorf("header = %q, want a READY column", lines[0])
	}
	if !strings.Contains(lines[1], "golden") || !strings.Contains(lines[1], "Ready") || !strings.Contains(lines[1], "true") {
		t.Errorf("row 1 = %q, want it to mention golden/Ready/true", lines[1])
	}
	if !strings.Contains(lines[2], "data-disk") || !strings.Contains(lines[2], "Pending") || !strings.Contains(lines[2], "false") {
		t.Errorf("row 2 = %q, want it to mention data-disk/Pending/false", lines[2])
	}
}

func TestWriteImageList_AllNamespacesAddsNamespaceColumn(t *testing.T) {
	images := []keziov1alpha3.Image{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "golden",
				Namespace:         "team-a",
				CreationTimestamp: metav1.NewTime(time.Now()),
			},
		},
	}

	var buf bytes.Buffer
	if err := WriteImageList(&buf, images, true); err != nil {
		t.Fatalf("WriteImageList() error = %v", err)
	}
	out := buf.String()

	if !strings.HasPrefix(out, "NAMESPACE") {
		t.Errorf("header should start with NAMESPACE, got %q", out)
	}
	if !strings.Contains(out, "team-a") {
		t.Errorf("expected namespace team-a in output, got %q", out)
	}
}

func TestImageList_RespectsNamespaceScoping(t *testing.T) {
	imgA := &keziov1alpha3.Image{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns-a"}}
	imgB := &keziov1alpha3.Image{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "ns-b"}}
	c := fake.NewClientBuilder().WithScheme(Scheme).WithObjects(imgA, imgB).Build()

	got, err := ImageList(context.Background(), c, ImageListOptions{Namespace: "ns-a"})
	if err != nil {
		t.Fatalf("ImageList() error = %v", err)
	}
	if len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("ImageList(ns-a) = %v, want just image a", got)
	}

	all, err := ImageList(context.Background(), c, ImageListOptions{AllNamespaces: true})
	if err != nil {
		t.Fatalf("ImageList(all) error = %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("ImageList(all) = %v, want 2 images", all)
	}
}
