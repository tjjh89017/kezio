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

// TestTFTPServer_LogsServedFile: a completed read must log at the default
// verbosity, so an operator can tell whether a client fetched its file.
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
		"//" + ShimFilename,
	} {
		if err := handler(name, &fakeReaderFrom{}); err == nil {
			t.Errorf("expected an error requesting %q (path traversal / non-basename)", name)
		}
	}
}

// TestTFTPServer_AcceptsLeadingSlash: GRUB and some firmware request
// paths with a leading "/", so exactly one leading slash is stripped
// before the allowlist match.
func TestTFTPServer_AcceptsLeadingSlash(t *testing.T) {
	dir := setupTFTPDir(t)
	srv := &TFTPServer{Dir: dir}
	handler := srv.readHandler(logf.Log.WithName("test"))

	rf := &fakeReaderFrom{}
	if err := handler("/"+ShimFilename, rf); err != nil {
		t.Fatalf("reading /%s: %v", ShimFilename, err)
	}
	if got := rf.buf.String(); got != "shim-bytes" {
		t.Errorf("/%s content = %q, want %q", ShimFilename, got, "shim-bytes")
	}
}

// TestTFTPServer_ServesGrubConfigFromMemory proves GrubConfigPath is
// answered from TFTPServer.GrubConfig (rendered in-memory content, see
// GrubConfigPath's doc comment) rather than any file under Dir, with
// and without GRUB's leading slash.
func TestTFTPServer_ServesGrubConfigFromMemory(t *testing.T) {
	dir := setupTFTPDir(t)
	const config = "configfile (http,192.0.2.1:8090)/boot/grub.cfg-${net_default_mac}\n"
	srv := &TFTPServer{Dir: dir, GrubConfig: config}
	handler := srv.readHandler(logf.Log.WithName("test"))

	for _, name := range []string{GrubConfigPath, "/" + GrubConfigPath} {
		rf := &fakeReaderFrom{}
		if err := handler(name, rf); err != nil {
			t.Fatalf("reading %q: %v", name, err)
		}
		if got := rf.buf.String(); got != config {
			t.Errorf("%q content = %q, want %q", name, got, config)
		}
	}
}

// TestTFTPServer_RejectsGrubConfigWhenUnset: an empty GrubConfig keeps
// the path fail-closed, rejected like any other unknown name.
func TestTFTPServer_RejectsGrubConfigWhenUnset(t *testing.T) {
	dir := setupTFTPDir(t)
	srv := &TFTPServer{Dir: dir}
	handler := srv.readHandler(logf.Log.WithName("test"))

	if err := handler(GrubConfigPath, &fakeReaderFrom{}); err == nil {
		t.Errorf("expected an error requesting %q with no GrubConfig configured", GrubConfigPath)
	}
}

// TestTFTPServer_GrubConfigDoesNotWidenAllowlist proves GrubConfig adds
// exactly one servable path: neighbors of grub/grub.cfg and
// traversal shaped like the grub directory stay rejected.
func TestTFTPServer_GrubConfigDoesNotWidenAllowlist(t *testing.T) {
	dir := setupTFTPDir(t)
	srv := &TFTPServer{Dir: dir, GrubConfig: "configfile x\n"}
	handler := srv.readHandler(logf.Log.WithName("test"))

	for _, name := range []string{
		"grub/x86_64-efi/grub.cfg",
		"grub/grub.cfg-amd64",
		"grub/../secret.txt",
		"grub/grub.cfg/../../secret.txt",
		"//" + GrubConfigPath,
	} {
		if err := handler(name, &fakeReaderFrom{}); err == nil {
			t.Errorf("expected an error requesting %q", name)
		}
	}
}
