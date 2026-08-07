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
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// ErrEmptyToken reports that an authenticator was asked to accept the
// empty string as a bearer token. A deployment that leaves its token
// Secret empty must fail closed, not accept every request.
var ErrEmptyToken = errors.New("bearer token must not be empty")

// Authenticator checks the bearer token on incoming upload requests
// against one fixed token loaded at startup from a mounted Secret (see
// LoadToken). It never logs or echoes the token.
//
// This bearer check is the sole gate on writes; network exposure (Service
// type, Ingress, etc.) is the cluster operator's choice, not this
// package's concern. Deployments exposed beyond the cluster network
// should put TLS in front, since a bearer token over plain HTTP is only
// as safe as the network it crosses.
type Authenticator struct {
	// tokenHash is sha256(token), never the raw token, so a value copy or
	// crash dump doesn't hold the secret. Comparison hashes the presented
	// token the same way before a constant-time compare.
	tokenHash [sha256.Size]byte
}

// NewAuthenticator builds an Authenticator that accepts exactly token.
// token must be non-empty.
func NewAuthenticator(token string) (*Authenticator, error) {
	if token == "" {
		return nil, ErrEmptyToken
	}
	return &Authenticator{tokenHash: sha256.Sum256([]byte(token))}, nil
}

// LoadToken reads a bearer token from path (typically a Secret mounted as
// a file, for example /etc/kezio/token/token) and trims exactly one
// trailing newline, the shape `kubectl create secret generic --from-file`
// and most editors produce. It rejects a token that is empty after
// trimming, so a misconfigured (empty) Secret fails the service at
// startup instead of silently accepting every request.
func LoadToken(path string) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is an operator-supplied deployment flag, not user input
	if err != nil {
		return "", fmt.Errorf("read token file %s: %w", path, err)
	}
	token := strings.TrimSuffix(string(data), "\n")
	if token == "" {
		return "", fmt.Errorf("token file %s: %w", path, ErrEmptyToken)
	}
	return token, nil
}

// Check reports whether presented equals the configured token, in
// constant time with respect to both values.
func (a *Authenticator) Check(presented string) bool {
	presentedHash := sha256.Sum256([]byte(presented))
	return subtle.ConstantTimeCompare(presentedHash[:], a.tokenHash[:]) == 1
}

// bearerToken extracts the token from a standard "Authorization: Bearer
// <token>" header. It returns ok=false for any other scheme or a missing
// header, never a partial or best-effort token.
func bearerToken(r *http.Request) (token string, ok bool) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return "", false
	}
	return strings.TrimPrefix(h, prefix), true
}

// requireAuth wraps next so it only runs for requests bearing a valid
// bearer token; every other request gets 401 with no body beyond the
// standard status text, so a caller cannot distinguish "wrong token" from
// "no token" from any other rejection reason.
func requireAuth(auth *Authenticator, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok || !auth.Check(token) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="kezio-image-service"`)
			http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}
