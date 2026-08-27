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
	"net"
)

// interfaceAddrsFunc looks up the addresses currently assigned to
// ifaceName, or - when ifaceName is empty - every address in the
// process's network namespace. A test seam for ProvisioningInterfaceReady;
// interfaceAddrs is the production implementation.
type interfaceAddrsFunc func(ifaceName string) ([]net.Addr, error)

// interfaceAddrs is interfaceAddrsFunc's production implementation:
// net.InterfaceAddrs for "every interface", net.InterfaceByName().Addrs
// for a named one.
func interfaceAddrs(ifaceName string) ([]net.Addr, error) {
	if ifaceName == "" {
		return net.InterfaceAddrs()
	}
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return nil, err
	}
	return iface.Addrs()
}

// ProvisioningInterfaceReady reports an error unless ifaceName (or, when
// empty, some interface in the process's network namespace - see
// Config.Interface's own doc comment for that default) already holds an
// IPv4 address inside provisioningNet. lookup nil means interfaceAddrs;
// tests pass a stub instead of needing a real network namespace.
//
// This is cmd/bootd's /readyz check: a bootd pod recreated without its
// Multus attachment (only the cluster/Calico interface present) opens
// its DHCP sockets and reports Kubernetes-Ready with no error, and no
// address anything on the provisioning segment can actually reach -
// the same silent gap a tracker pod fell through in the lab.
func ProvisioningInterfaceReady(ifaceName string, provisioningNet *net.IPNet, lookup interfaceAddrsFunc) error {
	if provisioningNet == nil {
		return fmt.Errorf("no provisioning CIDR configured")
	}
	if lookup == nil {
		lookup = interfaceAddrs
	}

	label := ifaceName
	if label == "" {
		label = "<any>"
	}

	addrs, err := lookup(ifaceName)
	if err != nil {
		return fmt.Errorf("looking up interface %q: %w", label, err)
	}
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip4 := ipNet.IP.To4()
		if ip4 == nil {
			continue
		}
		if provisioningNet.Contains(ip4) {
			return nil
		}
	}
	return fmt.Errorf("interface %q has no IPv4 address inside provisioning CIDR %s", label, provisioningNet)
}
