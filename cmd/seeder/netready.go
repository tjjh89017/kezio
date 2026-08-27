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

package main

import (
	"fmt"
	"net"
)

// defaultSeederInterface is the interface name Multus gives a seeder
// pod's attachment: seederPodAnnotations (internal/controller) sets
// v1.multus-cni.io/default-network, which REPLACES the pod's default
// network rather than adding to it, so the seeding Subnet's NAD always
// ends up as eth0 regardless of which NAD provided it.
const defaultSeederInterface = "eth0"

// interfaceAddrsFunc looks up the addresses currently assigned to
// ifaceName. A test seam for interfaceHasIPv4; interfaceAddrs
// (net.InterfaceByName().Addrs) is the production implementation.
type interfaceAddrsFunc func(ifaceName string) ([]net.Addr, error)

func interfaceAddrs(ifaceName string) ([]net.Addr, error) {
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return nil, err
	}
	return iface.Addrs()
}

// interfaceHasIPv4 reports an error unless ifaceName already holds an
// IPv4 address. lookup nil means interfaceAddrs; tests pass a stub
// instead of needing a real network namespace.
//
// This is the seeder's /healthz check: a seeder pod recreated with its
// Multus attachment silently failed to come up would otherwise report
// Ready off whatever the container runtime left it with (or nothing at
// all), and no leecher on the seeding Subnet could ever reach it - the
// same outage class a tracker pod fell through in the lab.
func interfaceHasIPv4(ifaceName string, lookup interfaceAddrsFunc) error {
	if lookup == nil {
		lookup = interfaceAddrs
	}
	addrs, err := lookup(ifaceName)
	if err != nil {
		return fmt.Errorf("looking up interface %q: %w", ifaceName, err)
	}
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if ipNet.IP.To4() != nil {
			return nil
		}
	}
	return fmt.Errorf("interface %q has no IPv4 address", ifaceName)
}
