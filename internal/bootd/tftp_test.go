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

package bootd

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// fakeReaderFrom captures whatever readHandler reads from the file it
// opens, standing in for pin/tftp's io.ReaderFrom without needing a
// real UDP transfer.
type fakeReaderFrom struct {
	buf bytes.Buffer
}

func (f *fakeReaderFrom) ReadFrom(r io.Reader) (int64, error) {
	return f.buf.ReadFrom(r)
}

func setupTFTPDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ShimFilename), []byte("shim-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, GrubFilename), []byte("grub-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("do-not-serve"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "..", "outside"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "..", "outside", "escape.txt"), []byte("outside-file"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestTFTPServer_ServesAllowedFiles(t *testing.T) {
	dir := setupTFTPDir(t)
	srv := &TFTPServer{Dir: dir}
	log := logf.Log.WithName("test")
	handler := srv.readHandler(log)

	for name, want := range map[string]string{
		ShimFilename: "shim-bytes",
		GrubFilename: "grub-bytes",
	} {
		rf := &fakeReaderFrom{}
		if err := handler(name, rf); err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		if got := rf.buf.String(); got != want {
			t.Errorf("%s content = %q, want %q", name, got, want)
		}
	}
}

// TestTFTPServer_LogsServedFile pins the observability fix readHandler's
// success path relies on (see tftp.go): a completed read must log at the
// default verbosity, matching serveUDP's "answering PXE request" log for
// the same reason - without it, bootd's log cannot tell an operator
// whether a client that got a DHCP offer ever came back to fetch the file
// it named.
func TestTFTPServer_LogsServedFile(t *testing.T) {
	dir := setupTFTPDir(t)
	srv := &TFTPServer{Dir: dir}
	sink := &recordingSink{}
	handler := srv.readHandler(newRecordingLogger(sink))

	if err := handler(ShimFilename, &fakeReaderFrom{}); err != nil {
		t.Fatalf("reading %s: %v", ShimFilename, err)
	}

	if !containsSubstring(sink.messages(), "served TFTP file") {
		t.Errorf("log messages = %v, want one containing %q", sink.messages(), "served TFTP file")
	}
}

func TestTFTPServer_RejectsUnlistedFile(t *testing.T) {
	dir := setupTFTPDir(t)
	srv := &TFTPServer{Dir: dir}
	handler := srv.readHandler(logf.Log.WithName("test"))

	if err := handler("secret.txt", &fakeReaderFrom{}); err == nil {
		t.Error("expected an error requesting a file outside the allowlist")
	}
}

func TestTFTPServer_RejectsPathTraversal(t *testing.T) {
	dir := setupTFTPDir(t)
	srv := &TFTPServer{Dir: dir}
	handler := srv.readHandler(logf.Log.WithName("test"))

	for _, name := range []string{
		"../outside/escape.txt",
		"../../etc/passwd",
		"sub/" + ShimFilename,
		"/" + ShimFilename,
	} {
		if err := handler(name, &fakeReaderFrom{}); err == nil {
			t.Errorf("expected an error requesting %q (path traversal / non-basename)", name)
		}
	}
}
