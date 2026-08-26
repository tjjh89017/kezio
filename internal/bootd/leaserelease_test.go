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

package bootd

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestMacDiff(t *testing.T) {
	tests := []struct {
		name    string
		old     []string
		current []string
		want    []string
	}{
		{"nothing removed", []string{"aa:bb:cc:dd:ee:01"}, []string{"aa:bb:cc:dd:ee:01"}, nil},
		{"one removed", []string{"aa:bb:cc:dd:ee:01", "aa:bb:cc:dd:ee:02"}, []string{"aa:bb:cc:dd:ee:01"}, []string{"aa:bb:cc:dd:ee:02"}},
		{"everything removed", []string{"aa:bb:cc:dd:ee:01"}, nil, []string{"aa:bb:cc:dd:ee:01"}},
		{"an addition alone removes nothing", nil, []string{"aa:bb:cc:dd:ee:01"}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := macDiff(tc.old, tc.current)
			sort.Strings(got)
			sort.Strings(tc.want)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("macDiff(%v, %v) = %v, want %v", tc.old, tc.current, got, tc.want)
			}
		})
	}
}

func TestParseLeaseLine(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		mac, ip string
		ok      bool
	}{
		{"normal line", "1893456000 aa:bb:cc:dd:ee:01 192.0.2.50 host-1 *", "aa:bb:cc:dd:ee:01", "192.0.2.50", true},
		{"uppercase MAC normalizes", "1893456000 AA:BB:CC:DD:EE:01 192.0.2.50 host-1 *", "aa:bb:cc:dd:ee:01", "192.0.2.50", true},
		{"too few fields", "1893456000 aa:bb:cc:dd:ee:01", "", "", false},
		{"malformed MAC", "1893456000 not-a-mac 192.0.2.50 host-1 *", "", "", false},
		{"empty line", "", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mac, ip, ok := parseLeaseLine(tc.line)
			if ok != tc.ok || mac != tc.mac || ip != tc.ip {
				t.Errorf("parseLeaseLine(%q) = (%q, %q, %v), want (%q, %q, %v)", tc.line, mac, ip, ok, tc.mac, tc.ip, tc.ok)
			}
		})
	}
}

func TestReadLeaseFileByMAC(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dnsmasq.leases")

	t.Run("missing file is not an error", func(t *testing.T) {
		got, err := readLeaseFileByMAC(path)
		if err != nil {
			t.Fatalf("readLeaseFileByMAC: %v", err)
		}
		if got != nil {
			t.Errorf("readLeaseFileByMAC(missing) = %v, want nil", got)
		}
	})

	content := "1893456000 aa:bb:cc:dd:ee:01 192.0.2.50 host-1 *\n" +
		"1893456000 aa:bb:cc:dd:ee:02 192.0.2.51 host-2 *\n" +
		"garbage line\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readLeaseFileByMAC(path)
	if err != nil {
		t.Fatalf("readLeaseFileByMAC: %v", err)
	}
	want := map[string]string{"aa:bb:cc:dd:ee:01": "192.0.2.50", "aa:bb:cc:dd:ee:02": "192.0.2.51"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("readLeaseFileByMAC = %v, want %v", got, want)
	}
}

func TestFilterLeaseFile(t *testing.T) {
	t.Run("missing file is not an error", func(t *testing.T) {
		dir := t.TempDir()
		dropped, err := filterLeaseFile(filepath.Join(dir, "dnsmasq.leases"), nil)
		if err != nil {
			t.Fatalf("filterLeaseFile: %v", err)
		}
		if dropped != 0 {
			t.Errorf("dropped = %d, want 0", dropped)
		}
	})

	t.Run("drops leases for MACs outside the allowlist", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "dnsmasq.leases")
		content := "1893456000 aa:bb:cc:dd:ee:01 192.0.2.50 host-1 *\n" +
			"1893456000 aa:bb:cc:dd:ee:02 192.0.2.51 host-2 *\n"
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}

		dropped, err := filterLeaseFile(path, []string{"aa:bb:cc:dd:ee:01"})
		if err != nil {
			t.Fatalf("filterLeaseFile: %v", err)
		}
		if dropped != 1 {
			t.Errorf("dropped = %d, want 1", dropped)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		want := "1893456000 aa:bb:cc:dd:ee:01 192.0.2.50 host-1 *\n"
		if string(got) != want {
			t.Errorf("filtered lease file = %q, want %q", got, want)
		}
	})

	t.Run("empty allowlist drops everything, leaving an empty file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "dnsmasq.leases")
		if err := os.WriteFile(path, []byte("1893456000 aa:bb:cc:dd:ee:01 192.0.2.50 host-1 *\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		dropped, err := filterLeaseFile(path, nil)
		if err != nil {
			t.Fatalf("filterLeaseFile: %v", err)
		}
		if dropped != 1 {
			t.Errorf("dropped = %d, want 1", dropped)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("filtered lease file = %q, want empty", got)
		}
	})

	t.Run("a malformed line is preserved untouched", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "dnsmasq.leases")
		if err := os.WriteFile(path, []byte("garbage line\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		dropped, err := filterLeaseFile(path, nil)
		if err != nil {
			t.Fatalf("filterLeaseFile: %v", err)
		}
		if dropped != 0 {
			t.Errorf("dropped = %d, want 0 (malformed lines are left alone, not dropped)", dropped)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "garbage line\n" {
			t.Errorf("filtered lease file = %q, want the malformed line preserved", got)
		}
	})
}

// TestExecDHCPRelease_ReportsFailure proves a failing dhcp_release
// invocation surfaces both the exit error and the process's own output,
// rather than a bare "exit status 1" that names no cause.
func TestExecDHCPRelease_ReportsFailure(t *testing.T) {
	script := "#!/bin/sh\necho 'no such lease' >&2\nexit 1\n"
	path := filepath.Join(t.TempDir(), "fake-dhcp_release")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	err := execDHCPRelease(path)("net1", "192.0.2.50", "aa:bb:cc:dd:ee:01")
	if err == nil {
		t.Fatal("execDHCPRelease returned nil for a failing dhcp_release, want an error")
	}
	if !strings.Contains(err.Error(), "no such lease") {
		t.Errorf("execDHCPRelease error = %v, want it to carry the process's own output", err)
	}
}
