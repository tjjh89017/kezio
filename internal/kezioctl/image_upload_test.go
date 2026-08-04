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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
)

// newFakeUploadServer builds an httptest server that behaves like
// internal/imageservice for the one endpoint ImageUpload calls: it
// computes the real sha256 of the received bytes (so tests can assert
// the checksum genuinely propagates end-to-end from the server's
// response into the created Image, not from a value the test just made
// up) and requires the bearer token "test-token".
func newFakeUploadServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/uploads/")
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upload body: %v", err)
		}
		sum := sha256.Sum256(data)
		checksum := "sha256:" + hex.EncodeToString(sum[:])

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(UploadResponse{
			Name:      name,
			URL:       "kezio-staged://" + name,
			SizeBytes: int64(len(data)),
			Checksum:  checksum,
		})
	}))
}

func TestImageUpload_FromFile_ChecksumPropagatesIntoCR(t *testing.T) {
	srv := newFakeUploadServer(t)
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "golden.qcow2")
	content := []byte("qcow2 disk bytes for testing")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	wantSum := sha256.Sum256(content)
	wantChecksum := "sha256:" + hex.EncodeToString(wantSum[:])

	scheme := Scheme
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	res, err := ImageUpload(context.Background(), srv.Client(), c, ImageUploadOptions{
		File:      path,
		Name:      "ubuntu-2404-golden",
		Namespace: "kezio-system",
		Server:    srv.URL,
		Token:     "test-token",
	})
	if err != nil {
		t.Fatalf("ImageUpload() error = %v", err)
	}

	if res.Upload.Checksum != wantChecksum {
		t.Fatalf("Upload.Checksum = %q, want %q", res.Upload.Checksum, wantChecksum)
	}
	if res.Image.Spec.Source.Checksum != wantChecksum {
		t.Errorf("Image.Spec.Source.Checksum = %q, want %q", res.Image.Spec.Source.Checksum, wantChecksum)
	}
	if res.Image.Spec.Source.URL != "kezio-staged://ubuntu-2404-golden" {
		t.Errorf("Image.Spec.Source.URL = %q, want kezio-staged://ubuntu-2404-golden", res.Image.Spec.Source.URL)
	}
	if res.Image.Spec.Source.Format != keziov1alpha1.ImageFormatQCOW2 {
		t.Errorf("Image.Spec.Source.Format = %q, want %q", res.Image.Spec.Source.Format, keziov1alpha1.ImageFormatQCOW2)
	}

	stored := &keziov1alpha1.Image{}
	key := client.ObjectKey{Namespace: "kezio-system", Name: "ubuntu-2404-golden"}
	if err := c.Get(context.Background(), key, stored); err != nil {
		t.Fatalf("get created Image: %v", err)
	}
	if stored.Spec.Source.Checksum != wantChecksum {
		t.Errorf("stored Image checksum = %q, want %q", stored.Spec.Source.Checksum, wantChecksum)
	}
}

func TestImageUpload_FromStdin_RequiresExplicitFormat(t *testing.T) {
	srv := newFakeUploadServer(t)
	defer srv.Close()

	c := fake.NewClientBuilder().WithScheme(Scheme).Build()

	_, err := ImageUpload(context.Background(), srv.Client(), c, ImageUploadOptions{
		File:      StdinArg,
		Name:      "n",
		Namespace: "default",
		Server:    srv.URL,
		Token:     "test-token",
		Stdin:     strings.NewReader("data"),
	})
	if err == nil {
		t.Fatal("expected an error requiring --format for stdin")
	}
}

func TestImageUpload_FromStdin_ChecksumPropagatesIntoCR(t *testing.T) {
	srv := newFakeUploadServer(t)
	defer srv.Close()

	content := "streamed from stdin, size unknown upfront"
	wantSum := sha256.Sum256([]byte(content))
	wantChecksum := "sha256:" + hex.EncodeToString(wantSum[:])

	c := fake.NewClientBuilder().WithScheme(Scheme).Build()

	res, err := ImageUpload(context.Background(), srv.Client(), c, ImageUploadOptions{
		File:      StdinArg,
		Name:      "from-stdin",
		Namespace: "default",
		Format:    keziov1alpha1.ImageFormatRaw,
		Server:    srv.URL,
		Token:     "test-token",
		Stdin:     strings.NewReader(content),
	})
	if err != nil {
		t.Fatalf("ImageUpload() error = %v", err)
	}
	if res.Upload.Checksum != wantChecksum {
		t.Fatalf("Upload.Checksum = %q, want %q", res.Upload.Checksum, wantChecksum)
	}
	if res.Image.Spec.Source.Checksum != wantChecksum {
		t.Errorf("Image.Spec.Source.Checksum = %q, want %q", res.Image.Spec.Source.Checksum, wantChecksum)
	}
}

func TestImageUpload_ImageAlreadyExists(t *testing.T) {
	srv := newFakeUploadServer(t)
	defer srv.Close()

	existing := &keziov1alpha1.Image{}
	existing.Name = "dup"
	existing.Namespace = "default"
	existing.Spec.Source.Format = keziov1alpha1.ImageFormatRaw
	c := fake.NewClientBuilder().WithScheme(Scheme).WithObjects(existing).Build()

	_, err := ImageUpload(context.Background(), srv.Client(), c, ImageUploadOptions{
		File:      StdinArg,
		Name:      "dup",
		Namespace: "default",
		Format:    keziov1alpha1.ImageFormatRaw,
		Server:    srv.URL,
		Token:     "test-token",
		Stdin:     strings.NewReader("x"),
	})
	if err == nil {
		t.Fatal("expected an error when the Image already exists")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error = %q, want it to mention the Image already exists", err.Error())
	}
}
