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

package ingest

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func writeTempFile(t *testing.T, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data.bin")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestVerifyChecksum_Empty(t *testing.T) {
	path := writeTempFile(t, []byte("anything"))
	if err := VerifyChecksum(path, ""); err != nil {
		t.Errorf("empty checksum should be a no-op, got %v", err)
	}
}

func TestVerifyChecksum_Match(t *testing.T) {
	content := []byte("hello world")
	sum := sha256.Sum256(content)
	path := writeTempFile(t, content)

	if err := VerifyChecksum(path, "sha256:"+hex.EncodeToString(sum[:])); err != nil {
		t.Errorf("expected checksum to match, got %v", err)
	}
	// Algorithm name is case-insensitive.
	if err := VerifyChecksum(path, "SHA256:"+hex.EncodeToString(sum[:])); err != nil {
		t.Errorf("expected case-insensitive algorithm match, got %v", err)
	}
}

func TestVerifyChecksum_Mismatch(t *testing.T) {
	path := writeTempFile(t, []byte("hello world"))
	if err := VerifyChecksum(path, "sha256:0000000000000000000000000000000000000000000000000000000000000000"); err == nil {
		t.Fatal("expected a mismatch error")
	}
}

func TestVerifyChecksum_UnsupportedAlgorithm(t *testing.T) {
	path := writeTempFile(t, []byte("hello world"))
	if err := VerifyChecksum(path, "md5:abcdef"); err == nil {
		t.Fatal("expected an unsupported-algorithm error")
	}
}

func TestVerifyChecksum_MalformedForm(t *testing.T) {
	path := writeTempFile(t, []byte("hello world"))
	if err := VerifyChecksum(path, "not-a-checksum"); err == nil {
		t.Fatal("expected a malformed-form error")
	}
}
