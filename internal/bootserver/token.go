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

package bootserver

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// tokenByteLength is the amount of randomness in a minted boot token,
// before hex encoding. 32 bytes (256 bits) is comfortably beyond brute
// force range for a value that is also short-lived and single-use.
const tokenByteLength = 32

// mintToken generates a new boot token and returns both the token itself
// (embedded in the kernel cmdline the agent reads) and the SHA-256 hex
// digest of it (the only form ever persisted, in
// Machine.status.netBoot.tokenHash). Never storing the token itself
// means a read of the Machine status - or of an etcd snapshot - cannot be
// used to register as the machine; only whoever received the live
// grub.cfg response can.
func mintToken() (token, hash string, err error) {
	buf := make([]byte, tokenByteLength)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("generating boot token: %w", err)
	}
	token = hex.EncodeToString(buf)
	return token, hashToken(token), nil
}

// hashToken returns the SHA-256 hex digest of token, in the same form
// mintToken returns and Machine.status.netBoot.tokenHash stores.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
