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
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

const testUploadName = "ubuntu-2404-golden"

func TestImageUpload_ChecksumAndNamesPropagateIntoCR(t *testing.T) {
	srv := newImageServiceTestServer(t, "test-token")
	defer srv.Close()

	content := []byte("raw disk bytes for testing")
	path := writeTestImageFile(t, content)

	wantSum := sha256.Sum256(content)
	wantChecksum := "sha256:" + hex.EncodeToString(wantSum[:])

	c := fake.NewClientBuilder().WithScheme(Scheme).Build()

	res, err := ImageUpload(context.Background(), srv.Client(), c, ImageUploadOptions{
		File:      path,
		Name:      testUploadName,
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

	stored := &keziov1alpha2.ImageImport{}
	key := client.ObjectKey{Namespace: "kezio-system", Name: testUploadName}
	if err := c.Get(context.Background(), key, stored); err != nil {
		t.Fatalf("get created ImageImport: %v", err)
	}
	if stored.Spec.Source.Checksum != wantChecksum {
		t.Errorf("stored ImageImport checksum = %q, want %q", stored.Spec.Source.Checksum, wantChecksum)
	}
	if stored.Spec.Source.URL != "kezio-staged://ubuntu-2404-golden" {
		t.Errorf("stored ImageImport URL = %q, want kezio-staged://ubuntu-2404-golden", stored.Spec.Source.URL)
	}
	if stored.Spec.ImageName != testUploadName {
		t.Errorf("spec.imageName = %q, want it to default to --name", stored.Spec.ImageName)
	}
	if stored.Spec.ContentPrefix != testUploadName {
		t.Errorf("spec.contentPrefix = %q, want it to default to --name", stored.Spec.ContentPrefix)
	}
}

func TestImageUpload_ExplicitImageNameAndContentPrefixWin(t *testing.T) {
	srv := newImageServiceTestServer(t, "test-token")
	defer srv.Close()

	path := writeTestImageFile(t, []byte("data"))
	c := fake.NewClientBuilder().WithScheme(Scheme).Build()

	res, err := ImageUpload(context.Background(), srv.Client(), c, ImageUploadOptions{
		File:          path,
		Name:          "import-run-7",
		Namespace:     "default",
		ImageName:     "ubuntu-2404",
		ContentPrefix: "ubuntu-2404-golden",
		Server:        srv.URL,
		Token:         "test-token",
	})
	if err != nil {
		t.Fatalf("ImageUpload() error = %v", err)
	}
	if res.Import.Spec.ImageName != "ubuntu-2404" {
		t.Errorf("spec.imageName = %q, want ubuntu-2404", res.Import.Spec.ImageName)
	}
	if res.Import.Spec.ContentPrefix != "ubuntu-2404-golden" {
		t.Errorf("spec.contentPrefix = %q, want ubuntu-2404-golden", res.Import.Spec.ContentPrefix)
	}
}

func TestImageUpload_WrongTokenIsRejected(t *testing.T) {
	srv := newImageServiceTestServer(t, "correct-token")
	defer srv.Close()

	path := writeTestImageFile(t, []byte("data"))
	c := fake.NewClientBuilder().WithScheme(Scheme).Build()

	_, err := ImageUpload(context.Background(), srv.Client(), c, ImageUploadOptions{
		File:      path,
		Name:      "n",
		Namespace: "default",
		Server:    srv.URL,
		Token:     "wrong-token",
	})
	if err == nil {
		t.Fatal("expected an error for a wrong bearer token")
	}
	if !strings.Contains(err.Error(), "not authorized") {
		t.Errorf("error = %q, want it to mention authorization", err.Error())
	}
}

func TestImageUpload_UploadFailureDoesNotCreateImageImport(t *testing.T) {
	srv := newImageServiceTestServer(t, "test-token")
	defer srv.Close()

	path := writeTestImageFile(t, []byte("data"))
	c := fake.NewClientBuilder().WithScheme(Scheme).Build()

	_, err := ImageUpload(context.Background(), srv.Client(), c, ImageUploadOptions{
		File:      path,
		Name:      "should-not-exist",
		Namespace: "default",
		Server:    srv.URL,
		Token:     "wrong-token",
	})
	if err == nil {
		t.Fatal("expected an error when the upload fails")
	}

	list := &keziov1alpha2.ImageImportList{}
	if err := c.List(context.Background(), list); err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list.Items) != 0 {
		t.Fatalf("ImageImport CR created despite upload failure: %+v", list.Items)
	}
}

// waitLines returns only the lines --wait wrote, dropping the byte-count
// lines the upload itself writes to the same writer.
func waitLines(progress string) []string {
	var lines []string
	for _, line := range strings.Split(strings.TrimRight(progress, "\n"), "\n") {
		if strings.HasPrefix(line, "imageimport ") {
			lines = append(lines, line)
		}
	}
	return lines
}

// TestImageUpload_WaitReturnsOnceTheImageIsReady covers the journey the
// flag exists for: the import runs and the operator ends up holding an
// Image they can deploy.
func TestImageUpload_WaitReturnsOnceTheImageIsReady(t *testing.T) {
	srv := newImageServiceTestServer(t, "test-token")
	defer srv.Close()

	path := writeTestImageFile(t, []byte("raw disk bytes"))
	c := fake.NewClientBuilder().WithScheme(Scheme).Build()

	// The ingest Job is what creates the Image; here a goroutine stands in
	// for it, so the wait has to observe a state it did not start in.
	go func() {
		time.Sleep(5 * time.Millisecond)
		image := &keziov1alpha2.Image{
			ObjectMeta: metav1.ObjectMeta{Name: testUploadName, Namespace: "kezio-system"},
			Status:     keziov1alpha2.ImageStatus{State: keziov1alpha2.ImageStateReady},
		}
		_ = c.Create(context.Background(), image)
		_ = c.Update(context.Background(), image)
	}()

	var progress bytes.Buffer
	_, err := ImageUpload(context.Background(), srv.Client(), c, ImageUploadOptions{
		File:             path,
		Name:             testUploadName,
		Namespace:        "kezio-system",
		Server:           srv.URL,
		Token:            "test-token",
		Progress:         &progress,
		Wait:             true,
		WaitTimeout:      2 * time.Second,
		WaitPollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("ImageUpload() error = %v", err)
	}

	lines := waitLines(progress.String())
	if len(lines) == 0 {
		t.Fatalf("progress = %q, want at least one wait line", progress.String())
	}
	if !strings.Contains(lines[len(lines)-1], keziov1alpha2.ImageStateReady) {
		t.Errorf("last wait line = %q, want it to report the Image as Ready", lines[len(lines)-1])
	}
}

// TestImageUpload_WaitReportsTheImportFailureReason is the reason the wait
// watches the ImageImport at all: an import that fails terminally never
// creates the Image, so watching the Image alone would report nothing but
// a timeout and leave the operator without the cause.
func TestImageUpload_WaitReportsTheImportFailureReason(t *testing.T) {
	srv := newImageServiceTestServer(t, "test-token")
	defer srv.Close()

	path := writeTestImageFile(t, []byte("raw disk bytes"))
	c := fake.NewClientBuilder().WithScheme(Scheme).Build()

	const failureMessage = `partitioncontent "ubuntu-2404-golden-p1" already exists`
	go func() {
		key := client.ObjectKey{Namespace: "kezio-system", Name: testUploadName}
		imp := &keziov1alpha2.ImageImport{}
		for range 200 {
			if err := c.Get(context.Background(), key, imp); err == nil {
				break
			}
			time.Sleep(time.Millisecond)
		}
		imp.Status.State = keziov1alpha2.ImageImportStateFailed
		imp.Status.Conditions = []metav1.Condition{{
			Type:               keziov1alpha2.ImageImportConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             "ImportFailed",
			Message:            failureMessage,
			LastTransitionTime: metav1.Now(),
		}}
		_ = c.Update(context.Background(), imp)
	}()

	var progress bytes.Buffer
	_, err := ImageUpload(context.Background(), srv.Client(), c, ImageUploadOptions{
		File:             path,
		Name:             testUploadName,
		Namespace:        "kezio-system",
		Server:           srv.URL,
		Token:            "test-token",
		Progress:         &progress,
		Wait:             true,
		WaitTimeout:      2 * time.Second,
		WaitPollInterval: time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected an error when the ImageImport fails terminally")
	}
	if strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %q, want the import's own failure rather than a timeout", err.Error())
	}
	if !strings.Contains(err.Error(), "ImportFailed") || !strings.Contains(err.Error(), failureMessage) {
		t.Errorf("error = %q, want it to carry the import's reason and message", err.Error())
	}
}

func TestImageUpload_WaitTimesOutWhileTheImportIsStillRunning(t *testing.T) {
	srv := newImageServiceTestServer(t, "test-token")
	defer srv.Close()

	path := writeTestImageFile(t, []byte("raw disk bytes"))
	c := fake.NewClientBuilder().WithScheme(Scheme).Build()

	var progress bytes.Buffer
	_, err := ImageUpload(context.Background(), srv.Client(), c, ImageUploadOptions{
		File:             path,
		Name:             testUploadName,
		Namespace:        "kezio-system",
		Server:           srv.URL,
		Token:            "test-token",
		Progress:         &progress,
		Wait:             true,
		WaitTimeout:      50 * time.Millisecond,
		WaitPollInterval: time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected an error when the Image never becomes Ready")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %q, want it to report the timeout", err.Error())
	}
}

// TestImageUpload_WaitPrintsOnlyChangedStates guards the poll from being
// chatty: ingest takes minutes, and an unchanged state must not produce a
// line per poll.
func TestImageUpload_WaitPrintsOnlyChangedStates(t *testing.T) {
	srv := newImageServiceTestServer(t, "test-token")
	defer srv.Close()

	path := writeTestImageFile(t, []byte("raw disk bytes"))
	c := fake.NewClientBuilder().WithScheme(Scheme).Build()

	var progress bytes.Buffer
	_, err := ImageUpload(context.Background(), srv.Client(), c, ImageUploadOptions{
		File:             path,
		Name:             testUploadName,
		Namespace:        "kezio-system",
		Server:           srv.URL,
		Token:            "test-token",
		Progress:         &progress,
		Wait:             true,
		WaitTimeout:      50 * time.Millisecond,
		WaitPollInterval: time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected an error when the Image never becomes Ready")
	}

	lines := waitLines(progress.String())
	if len(lines) != 1 {
		t.Fatalf("wait lines = %q, want exactly one line while nothing changed", lines)
	}
}

func TestImageUpload_ImportAlreadyExists(t *testing.T) {
	srv := newImageServiceTestServer(t, "test-token")
	defer srv.Close()

	existing := &keziov1alpha2.ImageImport{}
	existing.Name = "dup"
	existing.Namespace = "default"
	c := fake.NewClientBuilder().WithScheme(Scheme).WithObjects(existing).Build()

	path := writeTestImageFile(t, []byte("x"))
	_, err := ImageUpload(context.Background(), srv.Client(), c, ImageUploadOptions{
		File:      path,
		Name:      "dup",
		Namespace: "default",
		Server:    srv.URL,
		Token:     "test-token",
	})
	if err == nil {
		t.Fatal("expected an error when the ImageImport already exists")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error = %q, want it to mention the ImageImport already exists", err.Error())
	}
}
