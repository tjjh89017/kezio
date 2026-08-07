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

package bmc

import "fmt"

// Keys match the builtin "kubernetes.io/basic-auth" Secret type, so an
// operator can reuse that type verbatim for a Machine's BMC credentials
// instead of inventing a kezio-specific shape.
const (
	SecretKeyUsername = "username"
	SecretKeyPassword = "password"
)

// CredentialsFromSecretData takes a Secret's Data map directly
// (map[string][]byte) so callers don't need to import a Kubernetes API
// type just to call this. Both keys must be present and non-empty, or it
// returns an error naming the missing key(s) but never the Secret's
// contents - failing loudly here rather than connecting to the BMC with an
// empty username or password.
func CredentialsFromSecretData(data map[string][]byte) (Credentials, error) {
	username, password := data[SecretKeyUsername], data[SecretKeyPassword]
	switch {
	case len(username) == 0 && len(password) == 0:
		return Credentials{}, fmt.Errorf("bmc: secret data missing %q and %q keys", SecretKeyUsername, SecretKeyPassword)
	case len(username) == 0:
		return Credentials{}, fmt.Errorf("bmc: secret data missing %q key", SecretKeyUsername)
	case len(password) == 0:
		return Credentials{}, fmt.Errorf("bmc: secret data missing %q key", SecretKeyPassword)
	}

	return Credentials{Username: string(username), Password: string(password)}, nil
}
