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

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
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
// It loads the local system itself instead of handing the decision back
// to the firmware. Returning to the firmware is not a boot: GRUB's "exit"
// leaves with EFI_SUCCESS, which the firmware records as this net boot
// option having booted successfully - it then stops walking BootOrder and
// starts its setup application, so the NVRAM entry the finalize hook
// wrote is never tried. "exit" therefore stays only as the last resort
// for a machine that carries no local system at all.
//
// The chainloaded file is the UEFI removable-media fallback path, which
// kezio's image contract requires every bootable Image to carry on its
// ESP (see internal/agent/deploy.efiRemovableLoaderPath) - the one path
// this fixed string can name without knowing the deployed system. Only
// the x86_64 name appears, matching bootd.ShimFilename/GrubFilename: the
// GRUB that reads this config is itself x86_64-only.
//
// "search" writes kezio_esp, not $root: a search that matches nothing
// leaves $root at the net boot device it already holds, so $root cannot
// report whether the ESP was found.
//
// Every outcome names itself on the console. A machine that fails here
// ends in the firmware's setup application, where the only other symptom
// is kezio waiting out its whole agent-connect timeout with nothing to
// say about what the firmware tried; GRUB's own diagnostic for a failed
// "boot" is the bare "error: unknown error." The echo lines carry no
// per-machine data, so this config stays the one fixed string every
// "not netbooting right now" answer returns.
const bootLocalConfig = `# kezio: this machine does not need the live boot environment right now.
set timeout=0
search --no-floppy --file --set=kezio_esp /EFI/BOOT/BOOTX64.EFI
if [ -n "${kezio_esp}" ]; then
  echo "kezio: starting the local system from (${kezio_esp})/EFI/BOOT/BOOTX64.EFI"
  set root=${kezio_esp}
  chainloader /EFI/BOOT/BOOTX64.EFI
  boot
  echo "kezio: that loader did not start the local system; its EFI/BOOT directory is incomplete"
else
  echo "kezio: no disk on this machine carries /EFI/BOOT/BOOTX64.EFI"
fi
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

// subnetBootBaseURL returns the base URL a machine on subnet's own
// segment already reaches: subnet's BootdServerIP on
// bootd.DefaultProxyPort, the exact value bootdEnv derives for
// BOOTD_BOOT_CONFIG_URL (internal/controller/subnet_bootd_config.go). A
// machine that fetched this grub.cfg through that address can reach the
// same address for its kernel/initrd/squashfs and agent registration,
// since bootd's reverse proxy already fronts both internal/bootserver and
// internal/agentserver there - this is what makes two Sites, whose
// Subnets cannot route to each other, each resolve to their own reachable
// address instead of one manager-wide default. ok is false when subnet is
// nil or declares no boot half (SubnetSpec.HasBootPlane), the caller's
// signal to keep the manager-wide Config.ServerURL/AgentServerURL.
func subnetBootBaseURL(subnet *keziov1alpha3.Subnet) (baseURL string, ok bool) {
	if subnet == nil || !subnet.Spec.HasBootPlane() {
		return "", false
	}
	return fmt.Sprintf("http://%s:%d", subnet.Spec.BootdServerIP, bootd.DefaultProxyPort), true
}

// resolveConsole picks the console= list a netbooting machine's cmdline
// carries: machineConsole (Machine.spec.console) if the machine set any,
// otherwise defaultConsole (Config.DefaultConsole). A Machine always wins
// over the operator-wide default when it names its own hardware console.
func resolveConsole(machineConsole, defaultConsole []string) []string {
	if len(machineConsole) > 0 {
		return machineConsole
	}
	return defaultConsole
}

// consoleCmdlineArgs renders console as a run of "console=<value>" kernel
// arguments, one per entry in order, each preceded by a space so it
// concatenates directly onto an existing cmdline. Empty for an empty
// console list - kezio adds nothing when no console is configured
// anywhere. The kernel treats a repeated console= as additive, with the
// last one becoming the primary console (what a bootloader's own output
// and /dev/console default to); the order callers pass in is preserved
// so that semantics stays under the caller's control.
func consoleCmdlineArgs(console []string) string {
	var b strings.Builder
	for _, c := range console {
		b.WriteString(" console=")
		b.WriteString(c)
	}
	return b.String()
}

// renderNetBootConfig builds the GRUB config for a machine that needs the
// live boot environment: GrubNetPath paths for kernel/initrd, plus a
// cmdline carrying boot=live, fetch=<squashfs URL> (real URL - live-boot's
// initrd is the consumer, not GRUB), kezio.server (Config.AgentServerURL,
// not ServerURL - see that field's doc comment for why conflating them
// misroutes every registration), kezio.token, and console= (see
// resolveConsole/consoleCmdlineArgs). token is the only value here that
// is not already resolved by the caller before this call; everything
// else comes from operator-controlled Config plus the caller-resolved
// console list. Errors on a malformed Config.ServerURL, surfaced
// per-request by the caller's fail-secure boot-local fallback.
func renderNetBootConfig(cfg Config, token string, console []string) (string, error) {
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
linux %s boot=live fetch=%s kezio.server=%s kezio.token=%s%s
initrd %s
boot
`, kernelPath, squashfsURL, agentServerURL, token, consoleCmdlineArgs(console), initrdPath), nil
}
