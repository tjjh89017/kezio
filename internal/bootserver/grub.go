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

import (
	"fmt"
	"strings"
)

// grubContentType is the Content-Type every grub.cfg response is served
// with. GRUB itself does not consult it, but a correct, non-sniffable
// content type is cheap defense against a browser or proxy in the path
// reinterpreting the body as something else.
const grubContentType = "text/plain; charset=utf-8"

// bootLocalConfig is the GRUB config handed back for every machine that
// does not currently need to load the live boot environment: an unknown
// MAC (a foreign machine on the same L2 segment, or a lookup failure -
// see Server.handleGrubConfig's fail-secure fallback), a known machine
// whose state does not call for a net boot, and a machine whose token
// mint failed. It is a fixed, static string: no per-machine or
// per-request data is ever interpolated into it, which is also what
// keeps every "not netbooting right now" case byte-identical - a device
// probing MACs on the segment cannot distinguish "not yours" from
// "not right now" from "never heard of it" by response shape.
//
// "exit" returns control to the firmware's own boot order without GRUB
// attempting to load anything further, so the firmware proceeds to its
// next configured boot entry - ordinarily the local disk.
const bootLocalConfig = `# kezio: this machine does not need the live boot environment right now.
set timeout=0
exit
`

// renderNetBootConfig builds the GRUB config for a machine that needs to
// load the live boot environment: HTTP URLs for the kernel and initrd
// artifacts, and a cmdline carrying boot=live plus fetch=<squashfs URL>
// (the parameters live-boot's initrd reads to fetch the root file
// system over HTTP instead of a local disk), kezio.server (so the agent
// knows where to register), and kezio.token (the freshly minted,
// single-use credential it registers with). token is the only
// per-request value that ever appears in this output; everything else
// comes from Config, which the operator controls, not the requesting
// firmware.
func renderNetBootConfig(cfg Config, token string) string {
	base := strings.TrimRight(cfg.ServerURL, "/")
	kernelURL := fmt.Sprintf("%s/boot/artifacts/%s", base, cfg.KernelPath)
	initrdURL := fmt.Sprintf("%s/boot/artifacts/%s", base, cfg.InitrdPath)
	squashfsURL := fmt.Sprintf("%s/boot/artifacts/%s", base, cfg.SquashfsPath)

	return fmt.Sprintf(`set timeout=5
linux %s boot=live fetch=%s kezio.server=%s kezio.token=%s
initrd %s
boot
`, kernelURL, squashfsURL, cfg.ServerURL, token, initrdURL)
}
