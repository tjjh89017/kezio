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
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/imageservice"
)

// newImageServiceTestServer builds an httptest.Server wrapping the real,
// in-tree internal/imageservice.Server (not a hand-rolled stand-in), so
// ImageUpload's tests exercise the actual staging/auth/checksum logic the
// production image service runs, without needing a cluster.
func newImageServiceTestServer(t *testing.T, token string) *httptest.Server {
	t.Helper()
	staging, err := imageservice.NewStaging(t.TempDir())
	if err != nil {
		t.Fatalf("NewStaging() error = %v", err)
	}
	auth, err := imageservice.NewAuthenticator(token)
	if err != nil {
		t.Fatalf("NewAuthenticator() error = %v", err)
	}
	srv := imageservice.NewServer(staging, auth, 0, nil, nil)
	return httptest.NewServer(srv.Handler())
}

func writeTestImageFile(t *testing.T, content []byte) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "golden.raw")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeTestLayoutFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "layout.json")
	if err := os.WriteFile(path, []byte(testLayoutJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestImageUpload_ChecksumAndLayoutPropagateIntoCR(t *testing.T) {
	srv := newImageServiceTestServer(t, "test-token")
	defer srv.Close()

	content := []byte("raw disk bytes for testing")
	path := writeTestImageFile(t, content)
	layoutPath := writeTestLayoutFile(t)

	wantSum := sha256.Sum256(content)
	wantChecksum := "sha256:" + hex.EncodeToString(wantSum[:])

	c := fake.NewClientBuilder().WithScheme(Scheme).Build()

	res, err := ImageUpload(context.Background(), srv.Client(), c, ImageUploadOptions{
		File:       path,
		Name:       "ubuntu-2404-golden",
		Namespace:  "kezio-system",
		LayoutFile: layoutPath,
		Server:     srv.URL,
		Token:      "test-token",
	})
	if err != nil {
		t.Fatalf("ImageUpload() error = %v", err)
	}

	if res.Upload.Checksum != wantChecksum {
		t.Fatalf("Upload.Checksum = %q, want %q", res.Upload.Checksum, wantChecksum)
	}
	if res.Image.Spec.Source == nil || res.Image.Spec.Source.Checksum != wantChecksum {
		t.Errorf("Image.Spec.Source.Checksum = %+v, want %q", res.Image.Spec.Source, wantChecksum)
	}
	if res.Image.Spec.Source.URL != "kezio-staged://ubuntu-2404-golden" {
		t.Errorf("Image.Spec.Source.URL = %q, want kezio-staged://ubuntu-2404-golden", res.Image.Spec.Source.URL)
	}
	if res.Image.Spec.Layout.PartitionTable != keziov1alpha2.PartitionTableGPT {
		t.Errorf("Image.Spec.Layout.PartitionTable = %q, want gpt", res.Image.Spec.Layout.PartitionTable)
	}
	if len(res.Image.Spec.Layout.Slots) != 1 {
		t.Fatalf("Image.Spec.Layout.Slots = %+v, want one slot", res.Image.Spec.Layout.Slots)
	}

	stored := &keziov1alpha2.Image{}
	key := client.ObjectKey{Namespace: "kezio-system", Name: "ubuntu-2404-golden"}
	if err := c.Get(context.Background(), key, stored); err != nil {
		t.Fatalf("get created Image: %v", err)
	}
	if stored.Spec.Source.Checksum != wantChecksum {
		t.Errorf("stored Image checksum = %q, want %q", stored.Spec.Source.Checksum, wantChecksum)
	}
}

func TestImageUpload_WrongTokenIsRejected(t *testing.T) {
	srv := newImageServiceTestServer(t, "correct-token")
	defer srv.Close()

	path := writeTestImageFile(t, []byte("data"))
	layoutPath := writeTestLayoutFile(t)
	c := fake.NewClientBuilder().WithScheme(Scheme).Build()

	_, err := ImageUpload(context.Background(), srv.Client(), c, ImageUploadOptions{
		File:       path,
		Name:       "n",
		Namespace:  "default",
		LayoutFile: layoutPath,
		Server:     srv.URL,
		Token:      "wrong-token",
	})
	if err == nil {
		t.Fatal("expected an error for a wrong bearer token")
	}
	if !strings.Contains(err.Error(), "not authorized") {
		t.Errorf("error = %q, want it to mention authorization", err.Error())
	}
}

func TestImageUpload_UploadFailureDoesNotCreateImage(t *testing.T) {
	srv := newImageServiceTestServer(t, "test-token")
	defer srv.Close()

	path := writeTestImageFile(t, []byte("data"))
	layoutPath := writeTestLayoutFile(t)
	c := fake.NewClientBuilder().WithScheme(Scheme).Build()

	_, err := ImageUpload(context.Background(), srv.Client(), c, ImageUploadOptions{
		File:       path,
		Name:       "should-not-exist",
		Namespace:  "default",
		LayoutFile: layoutPath,
		Server:     srv.URL,
		Token:      "wrong-token",
	})
	if err == nil {
		t.Fatal("expected an error when the upload fails")
	}

	list := &keziov1alpha2.ImageList{}
	if err := c.List(context.Background(), list); err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list.Items) != 0 {
		t.Fatalf("Image CR created despite upload failure: %+v", list.Items)
	}
}

func TestImageUpload_ImageAlreadyExists(t *testing.T) {
	srv := newImageServiceTestServer(t, "test-token")
	defer srv.Close()

	layoutPath := writeTestLayoutFile(t)
	existing := &keziov1alpha2.Image{}
	existing.Name = "dup"
	existing.Namespace = "default"
	c := fake.NewClientBuilder().WithScheme(Scheme).WithObjects(existing).Build()

	path := writeTestImageFile(t, []byte("x"))
	_, err := ImageUpload(context.Background(), srv.Client(), c, ImageUploadOptions{
		File:       path,
		Name:       "dup",
		Namespace:  "default",
		LayoutFile: layoutPath,
		Server:     srv.URL,
		Token:      "test-token",
	})
	if err == nil {
		t.Fatal("expected an error when the Image already exists")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error = %q, want it to mention the Image already exists", err.Error())
	}
}

func TestImageUpload_MissingLayoutFile(t *testing.T) {
	srv := newImageServiceTestServer(t, "test-token")
	defer srv.Close()

	path := writeTestImageFile(t, []byte("data"))
	c := fake.NewClientBuilder().WithScheme(Scheme).Build()

	_, err := ImageUpload(context.Background(), srv.Client(), c, ImageUploadOptions{
		File:       path,
		Name:       "n",
		Namespace:  "default",
		LayoutFile: "/nonexistent/layout.json",
		Server:     srv.URL,
		Token:      "test-token",
	})
	if err == nil {
		t.Fatal("expected an error when the layout file is missing")
	}
}
