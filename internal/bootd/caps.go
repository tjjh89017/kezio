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

// dnsmasqCaps are the raw capabilities raiseAmbientCaps raises, in the
// same order as the exported DnsmasqCapabilities (identity.go) - keep
// the two lists in step, since they name the same three capabilities
// dnsmasq checks explicitly at startup and refuses to run without
// (lab-verified against dnsmasq 2.91).
var dnsmasqCaps = []int{
	unix.CAP_NET_BIND_SERVICE,
	unix.CAP_NET_ADMIN,
	unix.CAP_NET_RAW,
}

// raiseAmbientCaps copies each of dnsmasqCaps this process holds in its
// permitted set into its inheritable and ambient sets, so they survive
// execve into the dnsmasq child even when that child is non-root.
//
// In the pod this is belt-and-braces, not load-bearing: bootd runs as uid
// 0 (buildBootdDeployment's SecurityContext), because Kubernetes grants a
// non-root container's added capabilities to its bounding set only -
// permitted comes out empty after execve with no_new_privs set, so a
// non-root bootd has nothing to raise (lab-verified: dnsmasq then dies
// with "missing required capability NET_ADMIN"). As root, execve
// re-grants the bounding set to the child on its own; the ambient raise
// matters only for a non-root runtime that does grant permitted
// capabilities.
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
