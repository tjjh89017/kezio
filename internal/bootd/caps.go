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

package bootd

import (
	"fmt"

	"github.com/go-logr/logr"
	"golang.org/x/sys/unix"
)

// sighup and sigterm are the signals the supervisor sends its dnsmasq
// child: SIGHUP re-reads the dhcp-hostsfile, SIGTERM shuts it down
// cleanly.
var (
	sighup  = unix.SIGHUP
	sigterm = unix.SIGTERM
)

// dnsmasqCaps are the capabilities dnsmasq requires to serve DHCP (it
// checks the latter two explicitly at startup and refuses to run
// without them - lab-verified against dnsmasq 2.91):
//
//   - CAP_NET_BIND_SERVICE: binding UDP ports 67 and 4011.
//   - CAP_NET_ADMIN: interface enumeration/config queries its DHCP
//     engine performs.
//   - CAP_NET_RAW: the raw socket it answers not-yet-addressed clients
//     through.
var dnsmasqCaps = []int{
	unix.CAP_NET_BIND_SERVICE,
	unix.CAP_NET_ADMIN,
	unix.CAP_NET_RAW,
}

// capKubeNames maps a unix.CAP_* value to the capability name
// Kubernetes' SecurityContext.Capabilities.Add expects - a fixed Linux
// ABI fact (the kernel's own capability numbering), not a bootd-specific
// requirement, so it carries no risk of drifting from what dnsmasq
// actually needs.
var capKubeNames = map[int]string{
	unix.CAP_NET_BIND_SERVICE: "NET_BIND_SERVICE",
	unix.CAP_NET_ADMIN:        "NET_ADMIN",
	unix.CAP_NET_RAW:          "NET_RAW",
}

// DnsmasqCapabilities is dnsmasqCaps expressed as the capability names
// Kubernetes' SecurityContext.Capabilities.Add expects, so callers
// outside this package (internal/controller's buildBootdDeployment) can
// build a pod's Capabilities.Add from the same list this package itself
// raises, instead of a hand-spelled copy that can silently drop an
// entry.
var DnsmasqCapabilities = dnsmasqCapabilityNames()

func dnsmasqCapabilityNames() []string {
	names := make([]string, len(dnsmasqCaps))
	for i, c := range dnsmasqCaps {
		name, ok := capKubeNames[c]
		if !ok {
			panic(fmt.Sprintf("bootd: no Kubernetes capability name registered for unix cap %d", c))
		}
		names[i] = name
	}
	return names
}

// raiseAmbientCaps copies each of dnsmasqCaps this process holds in its
// permitted set into its inheritable and ambient sets, so they survive
// execve into the dnsmasq child even when that child is non-root.
//
// In the pod this is belt-and-braces, not load-bearing: bootd runs as uid
// 0 (config/bootd's deployment), because Kubernetes grants a non-root
// container's added capabilities to its bounding set only - permitted
// comes out empty after execve with no_new_privs set, so a non-root bootd
// has nothing to raise (lab-verified: dnsmasq then dies with "missing
// required capability NET_ADMIN"). As root, execve re-grants the bounding
// set to the child on its own; the ambient raise matters for non-root
// runtimes that do grant permitted capabilities (the lab's setpriv
// harness), where only ambient carries them across execve.
//
// Best-effort: failures are logged and skipped, not returned - dnsmasq's
// own startup check is the definitive error surface, naming whatever
// capability is missing.
func raiseAmbientCaps(log logr.Logger) {
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	if err := unix.Capget(&hdr, &data[0]); err != nil {
		log.V(1).Info("reading process capabilities failed; leaving ambient set as is", "error", err.Error())
		return
	}
	changed := false
	for _, c := range dnsmasqCaps {
		// All of dnsmasqCaps are < 32, so only data[0] matters.
		bit := uint32(1) << uint(c)
		if data[0].Permitted&bit == 0 {
			log.V(1).Info("capability not in permitted set; cannot raise it for dnsmasq", "cap", c)
			continue
		}
		if data[0].Inheritable&bit == 0 {
			data[0].Inheritable |= bit
			changed = true
		}
	}
	if changed {
		if err := unix.Capset(&hdr, &data[0]); err != nil {
			log.V(1).Info("raising inheritable capabilities failed", "error", err.Error())
			return
		}
	}
	for _, c := range dnsmasqCaps {
		bit := uint32(1) << uint(c)
		if data[0].Permitted&bit == 0 {
			continue
		}
		if err := unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_RAISE, uintptr(c), 0, 0); err != nil {
			log.V(1).Info("raising ambient capability failed", "cap", c, "error", err.Error())
		}
	}
}
