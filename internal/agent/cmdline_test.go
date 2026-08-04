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

package agent

import "testing"

func TestParseCmdline(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want Cmdline
	}{
		{
			name: "server and token present, surrounded by unrelated args",
			raw:  "BOOT_IMAGE=/vmlinuz boot=live fetch=http://x/fs.squashfs kezio.server=http://10.0.0.5:8090 kezio.token=abc123 console=ttyS0",
			want: Cmdline{Server: "http://10.0.0.5:8090", Token: "abc123"},
		},
		{
			name: "empty cmdline",
			raw:  "",
			want: Cmdline{},
		},
		{
			name: "only unrelated args",
			raw:  "boot=live quiet splash",
			want: Cmdline{},
		},
		{
			name: "flag with no value is ignored, not mistaken for a key",
			raw:  "quiet kezio.server=http://x kezio.token=tok",
			want: Cmdline{Server: "http://x", Token: "tok"},
		},
		{
			name: "duplicate key keeps the last occurrence",
			raw:  "kezio.token=first kezio.token=second",
			want: Cmdline{Token: "second"},
		},
		{
			name: "trailing newline and extra whitespace are tolerated",
			raw:  "  kezio.server=http://x   kezio.token=tok  \n",
			want: Cmdline{Server: "http://x", Token: "tok"},
		},
		{
			name: "a value containing '=' keeps everything after the first '='",
			raw:  "kezio.server=http://x/path?a=b",
			want: Cmdline{Server: "http://x/path?a=b"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseCmdline(tt.raw)
			if got != tt.want {
				t.Fatalf("ParseCmdline(%q) = %+v, want %+v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestReadCmdline(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		if _, err := ReadCmdline("testdata/does-not-exist"); err == nil {
			t.Fatalf("expected an error for a missing file")
		}
	})
}
