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
	"net/url"
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

// GrubNetPath converts an HTTP base URL plus an absolute path into
// GRUB's network file syntax: "http://192.0.2.1:8090" + "/boot/x"
// becomes "(http,192.0.2.1:8090)/boot/x". GRUB does not understand
// bare URLs at all - grub_file_open treats only a leading "(" as
// naming a device, so a path like "http://host/x" is opened as a
// relative path on the current $root device (the TFTP server, on a
// net boot) and fails; the "(<protocol>,<server[:port]>)/<path>"
// device syntax is the one form GRUB's network stack
// (grub_net_open_real, which parses the optional ":port" itself)
// actually resolves. Only the http scheme is accepted: GRUB's netboot
// images carry an http module but no TLS stack, so an https URL here
// could never be fetched by the firmware-side consumer this syntax
// exists for.
func GrubNetPath(serverURL, filePath string) (string, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return "", fmt.Errorf("parsing server URL %q: %w", serverURL, err)
	}
	if u.Scheme != "http" {
		return "", fmt.Errorf("server URL %q: GRUB's network stack supports only http, not %q", serverURL, u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("server URL %q carries no host", serverURL)
	}
	if !strings.HasPrefix(filePath, "/") {
		filePath = "/" + filePath
	}
	return fmt.Sprintf("(%s,%s)%s%s", u.Scheme, u.Host, strings.TrimRight(u.Path, "/"), filePath), nil
}

// renderNetBootConfig builds the GRUB config for a machine that needs to
// load the live boot environment: GRUB network paths (see GrubNetPath -
// GRUB cannot open bare URLs) for the kernel and initrd artifacts, and
// a cmdline carrying boot=live plus fetch=<squashfs URL> (the
// parameters live-boot's initrd reads to fetch the root file
// system over HTTP instead of a local disk - real URLs, since the
// initrd's downloader is the consumer there, not GRUB), kezio.server
// (so the agent knows where to register - internal/agentserver's own
// address, Config.AgentServerURL, not this boot config server's own
// ServerURL: see Config.AgentServerURL's doc comment for why conflating
// the two sends every registration to a server that never mounts
// internal/agentserver's routes), and kezio.token (the freshly minted,
// single-use credential it registers with). token is the only
// per-request value that ever appears in this output; everything else
// comes from Config, which the operator controls, not the requesting
// firmware. The error case is a malformed Config.ServerURL, an operator
// misconfiguration surfaced per-request by the caller's fail-secure
// boot-local fallback (see handleGrubConfig).
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
