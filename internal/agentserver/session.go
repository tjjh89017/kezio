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

package agentserver

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
	"github.com/tjjh89017/kezio/internal/bootserver"
)

// sessionTokenByteLength is the amount of randomness in a minted session
// token, before hex encoding - the same size internal/bootserver's boot
// token uses, for the same reasoning (comfortably beyond brute-force
// range for a bearer credential).
const sessionTokenByteLength = 32

// mintSessionToken generates a new agent-session token and returns both
// the token itself (returned exactly once, in RegisterResponse) and its
// SHA-256 hex digest (the only form ever persisted, in
// Machine.status.agentSession.tokenHash). It hashes with
// bootserver.HashToken - not a locally reimplemented equivalent - so
// this package's session tokens and internal/bootserver's boot tokens
// are indistinguishable in their hashing scheme, even though the two
// live in separate status fields and are never compared against each
// other.
func mintSessionToken() (token, hash string, err error) {
	buf := make([]byte, sessionTokenByteLength)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("generating agent session token: %w", err)
	}
	token = hex.EncodeToString(buf)
	return token, bootserver.HashToken(token), nil
}

// newAgentSessionStatus builds the status.agentSession value to persist
// for a freshly minted session: hash and expiry only, per
// MachineAgentSessionStatus's doc comment - the plaintext token is never
// stored.
func newAgentSessionStatus(hash string, now time.Time, ttl time.Duration) *keziov1alpha3.MachineAgentSessionStatus {
	return &keziov1alpha3.MachineAgentSessionStatus{
		TokenHash: hash,
		ExpiresAt: metav1.NewTime(now.Add(ttl)),
	}
}
