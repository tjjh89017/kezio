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

import (
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

// TokenStore is the in-process hand-off between whoever decides a Machine
// should net boot right now (the controller/deployer, which mints a
// token exactly once per boot it arms - a BMC power-on or power-cycle
// intended to land the machine in the live environment) and this
// package's GET /boot/grub.cfg-<mac> handler, which only ever reads: see
// Issue and Lookup. A boot token's plaintext is never persisted on the
// Machine itself (only its hash, on Machine.status.netBoot - see
// mintToken's doc comment for why), so in-process memory shared by both
// sides is the primary place the plaintext lives between being minted and
// being handed to whichever grub.cfg fetch happens to land next. A
// manager restart losing an outstanding entry is no longer fatal to that
// boot, though: internal/deployer.AgentDeployer also writes the plaintext
// to a per-Machine Secret at the same moment it mints, and
// Server.lookupToken falls back to reading that Secret (then calls
// Restore to warm this store) on a Lookup miss - see BootTokenSecretName.
// A restart that also loses the request in flight still degrades the same
// way it always did: the next grub.cfg fetch renders with no
// kezio.token=, and the agent that boots into it idles instead of
// registering (see renderNetBootConfig).
//
// A single TokenStore is meant to be shared, one instance per manager
// process, between the component that mints (internal/deployer.
// AgentDeployer) and the component that reads (Server) - never one per
// side.
type TokenStore struct {
	mu      sync.Mutex
	entries map[string]tokenStoreEntry // key: normalized MAC
}

// tokenStoreEntry is one MAC's outstanding boot token: the plaintext
// alongside its hash, so Lookup can tell whether the entry still matches
// whatever hash the caller read off Machine.status.netBoot without
// re-hashing on every read.
type tokenStoreEntry struct {
	token string
	hash  string
}

// NewTokenStore returns an empty TokenStore ready to use.
func NewTokenStore() *TokenStore {
	return &TokenStore{entries: map[string]tokenStoreEntry{}}
}

// Issue mints a fresh boot token for mac, remembers its plaintext for a
// later Lookup, and returns the token together with the
// MachineNetBootStatus value the caller is responsible for persisting on
// the Machine's status.netBoot - the only form of this token meant to
// ever reach the API server. Replaces whatever entry mac already held:
// exactly one token is ever outstanding per MAC, which is what makes a
// Lookup against the hash actually persisted authoritative rather than
// merely "one of possibly several live tokens".
func (s *TokenStore) Issue(mac string, now time.Time, ttl time.Duration) (token string, status keziov1alpha3.MachineNetBootStatus, err error) {
	token, hash, err := mintToken()
	if err != nil {
		return "", keziov1alpha3.MachineNetBootStatus{}, err
	}

	s.mu.Lock()
	s.entries[mac] = tokenStoreEntry{token: token, hash: hash}
	s.mu.Unlock()

	return token, keziov1alpha3.MachineNetBootStatus{
		TokenHash: hash,
		ExpiresAt: metav1.NewTime(now.Add(ttl)),
	}, nil
}

// Restore installs an already-minted token into the store for mac,
// exactly as Issue leaves it, without generating a fresh one. It exists
// only for Server.lookupToken's Secret fallback: recovering a token from
// its per-Machine boot token Secret after a manager restart lost the
// in-memory entry Issue originally made, so the same request's Lookup
// (and every later one for this boot) hits the fast path. Overwrites any
// existing entry for mac, matching Issue's own "one live token per MAC"
// semantics.
func (s *TokenStore) Restore(mac, token, hash string) {
	s.mu.Lock()
	s.entries[mac] = tokenStoreEntry{token: token, hash: hash}
	s.mu.Unlock()
}

// Lookup returns the plaintext token outstanding for mac, if its
// remembered hash still equals wantHash - the Machine's current
// status.netBoot.tokenHash, as read by the caller in the same request
// this Lookup answers. ok is false, and the caller must render its
// config with no token at all, in every case that is not "a boot is
// currently armed and its token has not been consumed yet":
//   - wantHash is empty: no token was ever minted for this boot, or a
//     prior registration already consumed it (ingestRegistration clears
//     status.netBoot.tokenHash to "" on success, and "" can never equal a
//     real hash).
//   - the store has no entry for mac at all: a manager restart lost it,
//     or nothing has minted for this MAC yet in this process.
//   - the store's entry for mac carries a different hash: a later boot
//     was armed (and minted its own token) after wantHash was read by an
//     even later reconcile pass than the one this fetch's Machine read
//     reflects, or after the token status.netBoot.tokenHash names was
//     already consumed and superseded by a fresh arm.
func (s *TokenStore) Lookup(mac, wantHash string) (token string, ok bool) {
	if wantHash == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, found := s.entries[mac]
	if !found || entry.hash != wantHash {
		return "", false
	}
	return entry.token, true
}
