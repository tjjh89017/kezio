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
	"fmt"
	"strings"

	"github.com/tjjh89017/kezio/internal/bootd"
)

// grubContentType is the Content-Type every grub.cfg response is served
// with. GRUB itself does not consult it, but a correct, non-sniffable
// content type is cheap defense against a browser or proxy in the path
// reinterpreting the body as something else.
const grubContentType = "text/plain; charset=utf-8"

// bootLocalConfig is the GRUB config handed back for every machine that
// does not currently need the live boot environment: unknown MAC, lookup
// failure, no net boot needed, or a failed token mint. It is a fixed
// string with no per-request data interpolated, so every "not netbooting
// right now" case is byte-identical - a device probing MACs cannot
// distinguish "not yours" from "not right now" from "never heard of it".
//
// "exit" returns control to the firmware's own boot order (ordinarily the
// local disk) without GRUB attempting to load anything further.
const bootLocalConfig = `# kezio: this machine does not need the live boot environment right now.
set timeout=0
exit
`

// grubSearchRedirectConfig is the answer to the identity-free "grub.cfg"
// name in the config search. Debian's netboot GRUB, loaded over UEFI HTTP
// Boot, does not perform the per-machine search at all - no UUID name, no
// MAC name; it asks for plain grub.cfg once and drops to its prompt on a
// 404. This stub sends it back through the MAC-keyed name using its own
// variables, so the per-machine decision still happens where it always
// does; the stub itself is one fixed string for every requester, the
// same no-information property bootLocalConfig has. GRUB expands
// net_default_mac colon-separated, which the MAC-keyed handler accepts
// alongside the dash form.
const grubSearchRedirectConfig = `# kezio: re-entering the config search with this machine's MAC.
set timeout=0
configfile ${prefix}/grub.cfg-01-${net_default_mac}
`

// GrubNetPath converts an HTTP base URL plus an absolute path into GRUB's
// network file syntax: "http://192.0.2.1:8090" + "/boot/x" becomes
// "(http,192.0.2.1:8090)/boot/x". GRUB does not understand bare URLs -
// grub_file_open treats only a leading "(" as naming a device, otherwise
// resolving the path relative to $root (the TFTP server on a net boot)
// and failing; "(<protocol>,<server[:port]>)/<path>" is the form GRUB's
// network stack actually resolves. Only http is accepted: GRUB's netboot
// images carry an http module but no TLS stack.
//
// Forwards to internal/bootd.GrubNetPath, the single source of truth -
// this package already imports internal/bootd for its shared
// ShimFilename/GrubFilename constants (see efi.go), and bootd cannot
// import back without a cycle, so the definition lives there.
func GrubNetPath(serverURL, filePath string) (string, error) {
	return bootd.GrubNetPath(serverURL, filePath)
}

// renderNetBootConfig builds the GRUB config for a machine that needs the
// live boot environment: GrubNetPath paths for kernel/initrd, plus a
// cmdline carrying boot=live, fetch=<squashfs URL> (real URL - live-boot's
// initrd is the consumer, not GRUB), kezio.server (Config.AgentServerURL,
// not ServerURL - see that field's doc comment for why conflating them
// misroutes every registration), and kezio.token. token is the only
// per-request value in the output; everything else comes from
// operator-controlled Config. Errors on a malformed Config.ServerURL,
// surfaced per-request by the caller's fail-secure boot-local fallback.
func renderNetBootConfig(cfg Config, token string) (string, error) {
	base := strings.TrimRight(cfg.ServerURL, "/")
	kernelPath, err := GrubNetPath(base, "/boot/artifacts/"+cfg.KernelPath)
	if err != nil {
		return "", err
	}
	initrdPath, err := GrubNetPath(base, "/boot/artifacts/"+cfg.InitrdPath)
	if err != nil {
		return "", err
	}
	squashfsURL := fmt.Sprintf("%s/boot/artifacts/%s", base, cfg.SquashfsPath)
	agentServerURL := strings.TrimRight(cfg.AgentServerURL, "/")

	return fmt.Sprintf(`set timeout=5
linux %s boot=live fetch=%s kezio.server=%s kezio.token=%s
initrd %s
boot
`, kernelPath, squashfsURL, agentServerURL, token, initrdPath), nil
}
