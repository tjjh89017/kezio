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

package agentserver

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
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

// validSession reports whether token is the live, unexpired session
// credential machine's status currently holds. Comparison hashes token
// first (Machine.status.agentSession.tokenHash is never the plaintext)
// and then compares in constant time with respect to both hashes,
// mirroring internal/imageservice.Authenticator.Check's discipline for
// the same reason: this is a bearer-credential check, and equality
// checking should not run in time proportional to how much of the
// correct hash a guess happens to match.
func validSession(machine *keziov1alpha1.Machine, token string, now time.Time) bool {
	sess := machine.Status.AgentSession
	if sess == nil || sess.TokenHash == "" {
		return false
	}
	if !sess.ExpiresAt.After(now) {
		return false
	}
	presented := bootserver.HashToken(token)
	return subtle.ConstantTimeCompare([]byte(presented), []byte(sess.TokenHash)) == 1
}

// newAgentSessionStatus builds the status.agentSession value to persist
// for a freshly minted session: hash and expiry only, per
// MachineAgentSessionStatus's doc comment.
func newAgentSessionStatus(hash string, now time.Time, ttl time.Duration) *keziov1alpha1.MachineAgentSessionStatus {
	return &keziov1alpha1.MachineAgentSessionStatus{
		TokenHash: hash,
		ExpiresAt: metav1.NewTime(now.Add(ttl)),
	}
}
