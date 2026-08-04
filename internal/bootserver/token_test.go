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

import "testing"

func TestMintTokenProducesDistinctTokensAndMatchingHash(t *testing.T) {
	tokenA, hashA, err := mintToken()
	if err != nil {
		t.Fatalf("mintToken: %v", err)
	}
	tokenB, hashB, err := mintToken()
	if err != nil {
		t.Fatalf("mintToken: %v", err)
	}

	if tokenA == "" || len(tokenA) != tokenByteLength*2 {
		t.Fatalf("token has unexpected length: %q", tokenA)
	}
	if tokenA == tokenB {
		t.Fatalf("two mints produced the same token: %q", tokenA)
	}
	if hashA == hashB {
		t.Fatalf("two distinct tokens hashed to the same value: %q", hashA)
	}
	if hashA != hashToken(tokenA) {
		t.Fatalf("hashToken(tokenA) = %q, want the hash mintToken returned (%q)", hashToken(tokenA), hashA)
	}
	// The hash must never just be the token itself under a different
	// name - that would defeat the entire point of only ever storing
	// the hash.
	if hashA == tokenA {
		t.Fatalf("hash equals the token itself")
	}
}
