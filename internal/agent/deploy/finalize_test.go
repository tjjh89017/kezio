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

package deploy

import (
	"strings"
	"testing"
)

func TestEfiRemovableLoaderPathForArch_X86_64ReturnsBOOTX64(t *testing.T) {
	path, err := efiRemovableLoaderPathForArch("amd64")
	if err != nil {
		t.Fatalf("efiRemovableLoaderPathForArch(amd64): %v", err)
	}
	if path != `\EFI\BOOT\BOOTX64.EFI` {
		t.Fatalf("path = %q, want the x86_64 fallback loader path", path)
	}
}

// TestEfiRemovableLoaderPathForArch_Aarch64FailsExplicitly exercises the
// scope boundary: aarch64 is declared (kezio's two-architecture scope
// names it) but not implemented, so it must fail loudly rather than
// silently falling back to the x86_64 path.
func TestEfiRemovableLoaderPathForArch_Aarch64FailsExplicitly(t *testing.T) {
	path, err := efiRemovableLoaderPathForArch("arm64")
	if err == nil {
		t.Fatalf("efiRemovableLoaderPathForArch(arm64) = %q, nil; want an explicit not-implemented error", path)
	}
	if path != "" {
		t.Fatalf("path = %q, want empty on error", path)
	}
	if !strings.Contains(err.Error(), "aarch64") {
		t.Fatalf("error = %q, want it to name aarch64", err.Error())
	}
}

func TestEfiRemovableLoaderPathForArch_UnsupportedArchIsAnError(t *testing.T) {
	if _, err := efiRemovableLoaderPathForArch("riscv64"); err == nil {
		t.Fatal("efiRemovableLoaderPathForArch(riscv64): want an error - riscv is out of kezio's two-architecture scope")
	}
}
