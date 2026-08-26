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
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// DefaultDHCPReleasePath is where docker/bootd's image installs
// dnsmasq-utils's dhcp_release binary.
const DefaultDHCPReleasePath = "/usr/bin/dhcp_release"

// ReleaseFunc sends a DHCPRELEASE on behalf of a lease, so its address
// returns to the pool immediately instead of waiting out the lease time.
// A test seam: Dnsmasq.Release nil means execDHCPRelease.
type ReleaseFunc func(iface, ip, mac string) error

// execDHCPRelease runs dnsmasq-utils' dhcp_release binary at binaryPath,
// the only supported way to make dnsmasq forget a lease before it
// expires: dnsmasq itself has no signal or API for releasing one lease.
func execDHCPRelease(binaryPath string) ReleaseFunc {
	return func(iface, ip, mac string) error {
		out, err := exec.Command(binaryPath, iface, ip, mac).CombinedOutput()
		if err != nil {
			return fmt.Errorf("dhcp_release %s %s %s: %w: %s", iface, ip, mac, err, strings.TrimSpace(string(out)))
		}
		return nil
	}
}

// macDiff returns the entries of old absent from current - the MACs that
// stopped being enrolled since the previous push. Order does not matter;
// callers only use the result as a set.
func macDiff(old, current []string) []string {
	stillAllowed := make(map[string]bool, len(current))
	for _, mac := range current {
		stillAllowed[mac] = true
	}
	var removed []string
	for _, mac := range old {
		if !stillAllowed[mac] {
			removed = append(removed, mac)
		}
	}
	return removed
}

// parseLeaseLine parses one line of dnsmasq's lease file:
// "<expiry> <mac> <ip> <hostname> <client-id>". It returns ok=false for a
// line whose MAC does not parse, rather than erroring - a lease file is
// dnsmasq's own state, not input this package validates strictly.
func parseLeaseLine(line string) (mac, ip string, ok bool) {
	fields := strings.Fields(line)
	if len(fields) < 3 {
		return "", "", false
	}
	mac, ok = NormalizeMAC(fields[1])
	if !ok {
		return "", "", false
	}
	return mac, fields[2], true
}

// readLeaseFileByMAC reads path (dnsmasq's lease file) into a normalized
// MAC -> IP map. A missing file is not an error - releaseLeases has
// nothing to release from a lease file dnsmasq has not written yet.
func readLeaseFileByMAC(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	leases := make(map[string]string)
	for line := range strings.SplitSeq(string(data), "\n") {
		if line == "" {
			continue
		}
		mac, ip, ok := parseLeaseLine(line)
		if !ok {
			continue
		}
		leases[mac] = ip
	}
	return leases, nil
}

// filterLeaseFile rewrites path (dnsmasq's persisted lease file) keeping
// only lines whose MAC is in allowed, so a lease surviving a bootd
// restart on behalf of a since-deleted Machine is dropped before dnsmasq
// ever reads it back. Called once at startup, before dnsmasq launches
// (see Dnsmasq.Start). A missing file is not an error - there is nothing
// to filter yet.
func filterLeaseFile(path string, allowed []string) (dropped int, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	allowedSet := make(map[string]bool, len(allowed))
	for _, mac := range allowed {
		allowedSet[mac] = true
	}

	var kept []string
	for line := range strings.SplitSeq(strings.TrimRight(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		mac, _, ok := parseLeaseLine(line)
		if !ok || allowedSet[mac] {
			// A malformed line is left alone rather than silently dropped -
			// this rewrite's job is enforcing the MAC allowlist, not
			// repairing dnsmasq's own file format.
			kept = append(kept, line)
			continue
		}
		dropped++
	}

	content := ""
	if len(kept) > 0 {
		content = strings.Join(kept, "\n") + "\n"
	}
	if err := writeFileAtomic(path, content); err != nil {
		return dropped, err
	}
	return dropped, nil
}
