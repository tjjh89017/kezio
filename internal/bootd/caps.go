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

// raiseAmbientCaps copies each of dnsmasqCaps that this process holds
// in its permitted set into its inheritable and ambient sets, so they
// survive execve into the dnsmasq child even when that child is
// non-root.
//
// In the pod this is belt-and-braces, not the load-bearing mechanism:
// bootd runs as uid 0 there (config/bootd's deployment), because
// Kubernetes puts a non-root container's added capabilities in the
// bounding set only - permitted comes out empty after execve of a
// binary with no file capabilities, and allowPrivilegeEscalation=false
// (no_new_privs) forbids regaining them via file capabilities, so a
// non-root bootd has nothing to raise (lab-verified: dnsmasq then dies
// with "missing required capability NET_ADMIN"). As root, execve
// re-grants the bounding set's capabilities to the child by itself.
// The ambient raise still matters for any non-root environment whose
// runtime does grant permitted capabilities (the containerized lab's
// setpriv harness): only the ambient set carries them across execve
// into a capability-less, non-root binary.
//
// Best-effort: each failure is logged and skipped rather than
// returned, because in environments that already put the capability
// in the ambient set there is nothing to do, and the definitive error
// surface is dnsmasq's own startup check, which names any capability
// it is missing.
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
