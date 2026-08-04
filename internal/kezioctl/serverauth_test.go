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
	"os"
	"path/filepath"
	"testing"
)

func TestResolveServerURL(t *testing.T) {
	t.Run("flag wins over env", func(t *testing.T) {
		t.Setenv(ServerEnvVar, "http://env")
		got, err := ResolveServerURL("http://flag")
		if err != nil || got != "http://flag" {
			t.Fatalf("got (%q, %v), want (http://flag, nil)", got, err)
		}
	})

	t.Run("falls back to env", func(t *testing.T) {
		t.Setenv(ServerEnvVar, "http://env")
		got, err := ResolveServerURL("")
		if err != nil || got != "http://env" {
			t.Fatalf("got (%q, %v), want (http://env, nil)", got, err)
		}
	})

	t.Run("errors when neither is set", func(t *testing.T) {
		t.Setenv(ServerEnvVar, "")
		if _, err := ResolveServerURL(""); err == nil {
			t.Fatal("expected an error")
		}
	})
}

func TestResolveToken_Precedence(t *testing.T) {
	dir := t.TempDir()
	tokenFilePath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFilePath, []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	envFilePath := filepath.Join(dir, "env-token")
	if err := os.WriteFile(envFilePath, []byte("env-file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("flag beats everything", func(t *testing.T) {
		t.Setenv(TokenEnvVar, "env-token")
		got, err := ResolveToken("flag-token", tokenFilePath)
		if err != nil || got != "flag-token" {
			t.Fatalf("got (%q, %v), want (flag-token, nil)", got, err)
		}
	})

	t.Run("env token beats token file flag", func(t *testing.T) {
		t.Setenv(TokenEnvVar, "env-token")
		got, err := ResolveToken("", tokenFilePath)
		if err != nil || got != "env-token" {
			t.Fatalf("got (%q, %v), want (env-token, nil)", got, err)
		}
	})

	t.Run("token file flag beats token file env", func(t *testing.T) {
		t.Setenv(TokenEnvVar, "")
		t.Setenv(TokenFileEnvVar, envFilePath)
		got, err := ResolveToken("", tokenFilePath)
		if err != nil || got != "file-token" {
			t.Fatalf("got (%q, %v), want (file-token, nil)", got, err)
		}
	})

	t.Run("falls back to token file env", func(t *testing.T) {
		t.Setenv(TokenEnvVar, "")
		t.Setenv(TokenFileEnvVar, envFilePath)
		got, err := ResolveToken("", "")
		if err != nil || got != "env-file-token" {
			t.Fatalf("got (%q, %v), want (env-file-token, nil)", got, err)
		}
	})

	t.Run("errors when nothing is set", func(t *testing.T) {
		t.Setenv(TokenEnvVar, "")
		t.Setenv(TokenFileEnvVar, "")
		if _, err := ResolveToken("", ""); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("errors on empty token file", func(t *testing.T) {
		emptyPath := filepath.Join(dir, "empty")
		if err := os.WriteFile(emptyPath, []byte("\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(TokenEnvVar, "")
		if _, err := ResolveToken("", emptyPath); err == nil {
			t.Fatal("expected an error for an empty token file")
		}
	})
}
