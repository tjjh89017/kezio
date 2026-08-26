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
	"testing"
	"time"
)

func TestTokenStore_IssueThenLookupResolvesTheSameToken(t *testing.T) {
	s := NewTokenStore()

	token, status, err := s.Issue("aa:bb:cc:dd:ee:01", time.Now(), time.Hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if token == "" || status.TokenHash == "" {
		t.Fatalf("Issue returned an empty token or hash: token=%q status=%+v", token, status)
	}
	if status.TokenHash == token {
		t.Fatalf("Issue's status carries the plaintext token instead of its hash")
	}

	got, ok := s.Lookup("aa:bb:cc:dd:ee:01", status.TokenHash)
	if !ok {
		t.Fatalf("Lookup() ok = false, want true")
	}
	if got != token {
		t.Fatalf("Lookup() = %q, want %q", got, token)
	}
}

func TestTokenStore_LookupRejectsEmptyHash(t *testing.T) {
	s := NewTokenStore()
	if _, _, err := s.Issue("aa:bb:cc:dd:ee:01", time.Now(), time.Hour); err != nil {
		t.Fatalf("Issue: %v", err)
	}

	if _, ok := s.Lookup("aa:bb:cc:dd:ee:01", ""); ok {
		t.Fatalf("Lookup() with an empty hash ok = true, want false (consumed or never minted)")
	}
}

func TestTokenStore_LookupRejectsUnknownMAC(t *testing.T) {
	s := NewTokenStore()
	if _, ok := s.Lookup("aa:bb:cc:dd:ee:01", "deadbeef"); ok {
		t.Fatalf("Lookup() for a MAC nothing was ever Issue()d to ok = true, want false")
	}
}

func TestTokenStore_LookupRejectsSupersededHash(t *testing.T) {
	s := NewTokenStore()
	_, first, err := s.Issue("aa:bb:cc:dd:ee:01", time.Now(), time.Hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, _, err := s.Issue("aa:bb:cc:dd:ee:01", time.Now(), time.Hour); err != nil {
		t.Fatalf("Issue: %v", err)
	}

	if _, ok := s.Lookup("aa:bb:cc:dd:ee:01", first.TokenHash); ok {
		t.Fatalf("Lookup() for a hash superseded by a later Issue ok = true, want false")
	}
}

func TestTokenStore_IssueSetsExpiryFromNowPlusTTL(t *testing.T) {
	s := NewTokenStore()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	_, status, err := s.Issue("aa:bb:cc:dd:ee:01", now, 30*time.Minute)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	want := now.Add(30 * time.Minute)
	if !status.ExpiresAt.Time.Equal(want) {
		t.Fatalf("ExpiresAt = %v, want %v", status.ExpiresAt.Time, want)
	}
}
