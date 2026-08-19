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

package bmc

import (
	"strings"
	"testing"
)

func TestCredentialsFromSecretData(t *testing.T) {
	got, err := CredentialsFromSecretData(map[string][]byte{
		"username": []byte("admin"),
		"password": []byte("hunter2"),
	})
	if err != nil {
		t.Fatalf("CredentialsFromSecretData() error = %v", err)
	}
	want := Credentials{Username: "admin", Password: "hunter2"}
	if got != want {
		t.Errorf("CredentialsFromSecretData() = %+v, want %+v", got, want)
	}
}

func TestCredentialsFromSecretDataTrimsTrailingWhitespace(t *testing.T) {
	got, err := CredentialsFromSecretData(map[string][]byte{
		"username": []byte("admin\n"),
		"password": []byte("hunter2 \t\n"),
	})
	if err != nil {
		t.Fatalf("CredentialsFromSecretData() error = %v", err)
	}
	want := Credentials{Username: "admin", Password: "hunter2"}
	if got != want {
		t.Errorf("CredentialsFromSecretData() = %+v, want %+v", got, want)
	}
}

func TestCredentialsFromSecretDataMissingKeys(t *testing.T) {
	tests := []struct {
		name          string
		data          map[string][]byte
		wantErrSubstr string
	}{
		{name: "missing both", data: map[string][]byte{}, wantErrSubstr: `missing "username" and "password" keys`},
		{name: "missing password", data: map[string][]byte{"username": []byte("admin")}, wantErrSubstr: `missing "password" key`},
		{name: "missing username", data: map[string][]byte{"password": []byte("hunter2")}, wantErrSubstr: `missing "username" key`},
		{name: "empty username", data: map[string][]byte{"username": []byte(""), "password": []byte("hunter2")}, wantErrSubstr: `missing "username" key`},
		{name: "empty password", data: map[string][]byte{"username": []byte("admin"), "password": []byte("")}, wantErrSubstr: `missing "password" key`},
		{name: "nil map", data: nil, wantErrSubstr: `missing "username" and "password" keys`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := CredentialsFromSecretData(tt.data)
			if err == nil {
				t.Fatalf("CredentialsFromSecretData(%v) succeeded, want an error", tt.data)
			}
			if !strings.Contains(err.Error(), tt.wantErrSubstr) {
				t.Errorf("CredentialsFromSecretData(%v) error = %q, want substring %q", tt.data, err.Error(), tt.wantErrSubstr)
			}
		})
	}
}

// TestCredentialsFromSecretDataErrorDoesNotLeakValues confirms the error
// path never echoes back whatever partial (or wrong-key) values it found -
// only the two well-known key names may appear.
func TestCredentialsFromSecretDataErrorDoesNotLeakValues(t *testing.T) {
	const secretPassword = "hunter2"
	_, err := CredentialsFromSecretData(map[string][]byte{
		"password": []byte(secretPassword),
		"user":     []byte("admin"), // wrong key name: "username" is required
	})
	if err == nil {
		t.Fatal("CredentialsFromSecretData() succeeded, want an error for the missing username key")
	}
	if strings.Contains(err.Error(), secretPassword) {
		t.Errorf("CredentialsFromSecretData() error leaked the password: %q", err.Error())
	}
}
