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

// dnsmasqCaps are the capabilities dnsmasq requires to serve DHCP as a
// non-root user (it checks the latter two explicitly at startup and
// refuses to run without them - lab-verified against dnsmasq 2.91):
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

// raiseAmbientCaps copies each of dnsmasqCaps that this process holds
// in its permitted set into its inheritable and ambient sets, so they
// survive execve into the dnsmasq child. The container runtime grants
// the pod's requested capabilities to bootd's own (non-root) process,
// but only ambient capabilities carry across execve for a non-root
// child with no file capabilities - and file capabilities on the
// dnsmasq binary would be discarded anyway under the pod's
// allowPrivilegeEscalation=false (no_new_privs), which ambient
// capabilities are exempt from by design.
//
// Best-effort: each failure is logged and skipped rather than
// returned, because in environments that already put the capability
// in the ambient set (or run everything as root, like the lab
// container) there is nothing to do, and the definitive error surface
// is dnsmasq's own startup check, which names any capability it is
// missing.
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
