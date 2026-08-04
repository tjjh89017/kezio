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

import (
	"fmt"
	"os"
	"strings"
)

// ProcCmdlinePath is the default path Cmdline reads from; a real boot
// always has it. Tests override the path directly instead of going
// through this constant.
const ProcCmdlinePath = "/proc/cmdline"

// Cmdline holds the kezio-specific values internal/bootserver's
// renderNetBootConfig embeds in the kernel cmdline: the controller's
// base URL and the single-use registration token minted for this boot.
type Cmdline struct {
	// Server is the value of kezio.server=, the controller's externally
	// reachable base URL (bootserver.Config.ServerURL on the other end).
	Server string
	// Token is the value of kezio.token=, the single-use credential this
	// boot registers with.
	Token string
}

// ReadCmdline reads and parses the kernel cmdline at path.
func ReadCmdline(path string) (Cmdline, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Cmdline{}, fmt.Errorf("reading kernel cmdline: %w", err)
	}
	return ParseCmdline(string(data)), nil
}

// ParseCmdline extracts kezio.server= and kezio.token= from raw, a
// whitespace-separated kernel cmdline (the format /proc/cmdline and
// GRUB's "linux ..." directive both use). Unrecognized tokens, and
// tokens with no "=", are ignored: the cmdline carries plenty of values
// (boot=live, fetch=..., console=..., and whatever else the boot
// pipeline or an operator's Machine.spec.params add) that are none of
// this package's concern. A duplicate key keeps its last occurrence, the
// same behavior the kernel's own cmdline parser has.
func ParseCmdline(raw string) Cmdline {
	var c Cmdline
	for _, tok := range strings.Fields(raw) {
		key, value, ok := strings.Cut(tok, "=")
		if !ok {
			continue
		}
		switch key {
		case "kezio.server":
			c.Server = value
		case "kezio.token":
			c.Token = value
		}
	}
	return c
}
