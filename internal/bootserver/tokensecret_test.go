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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

// newSecretFallbackTestServer builds a Server exactly like
// newTestServerUnarmed (an empty Tokens - no Issue call for any machine),
// with corev1 also registered so tests can seed the per-Machine boot
// token Secret this package's fallback lookup reads.
func newSecretFallbackTestServer(t *testing.T, machine *keziov1alpha3.Machine, secret *corev1.Secret) *Server {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := keziov1alpha3.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme(corev1): %v", err)
	}

	builder := fake.NewClientBuilder().
		WithScheme(scheme).
		WithIndex(&keziov1alpha3.Machine{}, MachineBootMACIndexField, IndexMachineBootMAC).
		WithStatusSubresource(&keziov1alpha3.Machine{}).
		WithObjects(machine)
	if secret != nil {
		builder = builder.WithObjects(secret)
	}
	c := builder.Build()

	s := New(c, Config{ArtifactsDir: "", ServerURL: "http://boot.example.test:8090"})
	s.Tokens = NewTokenStore()
	return s
}

// bootTokenSecretFixture builds the Secret writeBootTokenSecret would
// have written for machine's boot MAC: token/mac/expiresAt keyed exactly
// as BootTokenSecretName/BootTokenSecretKey* name them.
func bootTokenSecretFixture(machine *keziov1alpha3.Machine, token string, expiresAt time.Time) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      BootTokenSecretName(machine.Name),
			Namespace: machine.Namespace,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			BootTokenSecretKeyToken:     []byte(token),
			BootTokenSecretKeyMAC:       []byte(testMAC),
			BootTokenSecretKeyExpiresAt: []byte(expiresAt.UTC().Format(time.RFC3339)),
		},
	}
}

// TestHandleGrubConfig_SecretFallbackServesTokenOnStoreMiss pins the
// manager-restart recovery path: TokenStore has no entry at all (as after
// a restart), but the Machine's boot token Secret still carries the
// plaintext that hashes to status.netBoot.tokenHash - the fetch must
// still embed kezio.token=, and the store must come out warmed for the
// next fetch.
func TestHandleGrubConfig_SecretFallbackServesTokenOnStoreMiss(t *testing.T) {
	machine := newTestMachine(keziov1alpha3.MachineStateInspecting)
	const plaintext = "deadbeefcafefeed"
	expiresAt := time.Now().Add(time.Hour)
	machine.Status.NetBoot = &keziov1alpha3.MachineNetBootStatus{
		TokenHash: hashToken(plaintext),
		ExpiresAt: metav1.NewTime(expiresAt),
	}
	secret := bootTokenSecretFixture(machine, plaintext, expiresAt)
	s := newSecretFallbackTestServer(t, machine, secret)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boot/grub.cfg-"+testMAC, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if extractToken(t, body) != plaintext {
		t.Fatalf("net-boot config did not embed the Secret's plaintext token: %q", body)
	}

	mac, ok := NormalizeMAC(testMAC)
	if !ok {
		t.Fatalf("test MAC does not normalize: %q", testMAC)
	}
	if warmed, ok := s.Tokens.Lookup(mac, machine.Status.NetBoot.TokenHash); !ok || warmed != plaintext {
		t.Fatalf("TokenStore was not warmed from the Secret fallback: token=%q ok=%v", warmed, ok)
	}
}

// TestHandleGrubConfig_SecretFallbackRejectsExpiredSecret covers a Secret
// whose own expiresAt has already passed: even though its token still
// hashes to status.netBoot.tokenHash, it must not be honoured.
func TestHandleGrubConfig_SecretFallbackRejectsExpiredSecret(t *testing.T) {
	machine := newTestMachine(keziov1alpha3.MachineStateInspecting)
	const plaintext = "deadbeefcafefeed"
	machine.Status.NetBoot = &keziov1alpha3.MachineNetBootStatus{
		TokenHash: hashToken(plaintext),
		ExpiresAt: metav1.NewTime(time.Now().Add(time.Hour)),
	}
	secret := bootTokenSecretFixture(machine, plaintext, time.Now().Add(-time.Minute))
	s := newSecretFallbackTestServer(t, machine, secret)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boot/grub.cfg-"+testMAC, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); containsAll(body, "kezio.token=") {
		t.Fatalf("net-boot config embedded a token from an expired Secret: %q", body)
	}
}

// TestHandleGrubConfig_SecretFallbackRejectsHashMismatch covers a Secret
// whose token no longer hashes to the Machine's current
// status.netBoot.tokenHash - a stale Secret this package must not treat
// as authoritative over the Machine's own recorded hash.
func TestHandleGrubConfig_SecretFallbackRejectsHashMismatch(t *testing.T) {
	machine := newTestMachine(keziov1alpha3.MachineStateInspecting)
	machine.Status.NetBoot = &keziov1alpha3.MachineNetBootStatus{
		TokenHash: hashToken("current-token"),
		ExpiresAt: metav1.NewTime(time.Now().Add(time.Hour)),
	}
	secret := bootTokenSecretFixture(machine, "stale-token", time.Now().Add(time.Hour))
	s := newSecretFallbackTestServer(t, machine, secret)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boot/grub.cfg-"+testMAC, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); containsAll(body, "kezio.token=") {
		t.Fatalf("net-boot config embedded a token from a hash-mismatched Secret: %q", body)
	}
}

// TestHandleGrubConfig_SecretFallbackAbsentSecretOmitsToken covers "no
// Secret at all" (never written, or a namespace/name that does not
// match) alongside the pre-existing "no TokenStore entry" case: both must
// fail quiet to no token, never an error response.
func TestHandleGrubConfig_SecretFallbackAbsentSecretOmitsToken(t *testing.T) {
	machine := newTestMachine(keziov1alpha3.MachineStateInspecting)
	machine.Status.NetBoot = &keziov1alpha3.MachineNetBootStatus{
		TokenHash: hashToken("some-token"),
		ExpiresAt: metav1.NewTime(time.Now().Add(time.Hour)),
	}
	s := newSecretFallbackTestServer(t, machine, nil)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boot/grub.cfg-"+testMAC, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); containsAll(body, "kezio.token=") {
		t.Fatalf("net-boot config embedded a token with no Secret and no TokenStore entry: %q", body)
	}
}
