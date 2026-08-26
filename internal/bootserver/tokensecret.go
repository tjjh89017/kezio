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

package bootserver

// bootTokenSecretNameSuffix is appended to a Machine's own name to name
// its boot token Secret (see BootTokenSecretName).
const bootTokenSecretNameSuffix = "-boot-token"

// BootTokenSecretName returns the deterministic name of machineName's
// boot token Secret: internal/deployer.AgentDeployer writes it, in the
// Machine's own namespace, at the same moment it mints the token through
// TokenStore.Issue, so a manager restart that loses TokenStore's
// in-memory entries can still recover the plaintext (see
// Server.lookupToken's fallback). Exported so internal/deployer names the
// same Secret this package reads back.
func BootTokenSecretName(machineName string) string {
	return machineName + bootTokenSecretNameSuffix
}

// Boot token Secret data keys. The Secret is Opaque and carries exactly
// these three keys: the plaintext token, the MAC it was minted for (a
// defensive cross-check against BootTokenSecretName's implicit binding to
// the Machine, not the primary lookup key), and the RFC3339 expiry
// TokenStore.Issue computed alongside it.
const (
	BootTokenSecretKeyToken     = "token"
	BootTokenSecretKeyMAC       = "mac"
	BootTokenSecretKeyExpiresAt = "expiresAt"
)
