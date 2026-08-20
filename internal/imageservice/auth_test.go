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

package imageservice

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// An empty token Secret must fail closed, not build an Authenticator that
// accepts every request.
func TestNewAuthenticator_EmptyToken(t *testing.T) {
	auth, err := NewAuthenticator("")
	if auth != nil {
		t.Errorf("got non-nil Authenticator for empty token, want nil")
	}
	if !errors.Is(err, ErrEmptyToken) {
		t.Fatalf("err = %v, want ErrEmptyToken", err)
	}
}

// LoadToken trims exactly one trailing newline, the shape `kubectl create
// secret generic --from-file` and most editors produce.
func TestLoadToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("token\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	token, err := LoadToken(path)
	if err != nil {
		t.Fatalf("LoadToken: %v", err)
	}
	if token != "token" {
		t.Errorf("token = %q, want %q", token, "token")
	}
}

// LoadToken propagates a file-read error for a path that does not exist,
// rather than silently treating a missing Secret mount as an empty token.
func TestLoadToken_MissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist")

	if _, err := LoadToken(path); err == nil {
		t.Fatal("LoadToken on a missing path: got nil error, want non-nil")
	}
}

// A Secret file that is empty after trimming (here, only a newline) must be
// rejected the same way an empty token string is: fail closed at startup
// instead of silently accepting every request.
func TestLoadToken_EmptyAfterTrim(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := LoadToken(path)
	if !errors.Is(err, ErrEmptyToken) {
		t.Fatalf("err = %v, want ErrEmptyToken", err)
	}
}
