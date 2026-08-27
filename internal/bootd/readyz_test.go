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
	"errors"
	"net"
	"testing"
)

func mustIPNet(t *testing.T, addr string, bits int) *net.IPNet {
	t.Helper()
	ip := net.ParseIP(addr)
	if ip == nil {
		t.Fatalf("invalid test IP %q", addr)
	}
	return &net.IPNet{IP: ip.Mask(net.CIDRMask(bits, 32)), Mask: net.CIDRMask(bits, 32)}
}

func addrsOf(cidrs ...string) []net.Addr {
	out := make([]net.Addr, 0, len(cidrs))
	for _, c := range cidrs {
		_, ipNet, err := net.ParseCIDR(c)
		if err != nil {
			panic(err)
		}
		out = append(out, ipNet)
	}
	return out
}

func TestProvisioningInterfaceReady_HasAddressInCIDR(t *testing.T) {
	provisioningNet := mustIPNet(t, "192.0.2.0", 24)
	lookup := func(ifaceName string) ([]net.Addr, error) {
		if ifaceName != "net1" {
			t.Fatalf("unexpected interface name %q", ifaceName)
		}
		return addrsOf("192.0.2.5/24"), nil
	}

	if err := ProvisioningInterfaceReady("net1", provisioningNet, lookup); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestProvisioningInterfaceReady_OnlyClusterAddress(t *testing.T) {
	provisioningNet := mustIPNet(t, "192.0.2.0", 24)
	lookup := func(string) ([]net.Addr, error) {
		// The lab outage this guards against: the pod carries only its
		// cluster (Calico) interface, no provisioning attachment at all.
		return addrsOf("10.244.0.7/16"), nil
	}

	if err := ProvisioningInterfaceReady("net1", provisioningNet, lookup); err == nil {
		t.Fatal("expected an error, got nil")
	}
}

func TestProvisioningInterfaceReady_NoAddressesAtAll(t *testing.T) {
	provisioningNet := mustIPNet(t, "192.0.2.0", 24)
	lookup := func(string) ([]net.Addr, error) { return nil, nil }

	if err := ProvisioningInterfaceReady("net1", provisioningNet, lookup); err == nil {
		t.Fatal("expected an error, got nil")
	}
}

func TestProvisioningInterfaceReady_LookupError(t *testing.T) {
	provisioningNet := mustIPNet(t, "192.0.2.0", 24)
	lookup := func(string) ([]net.Addr, error) { return nil, errors.New("no such interface") }

	if err := ProvisioningInterfaceReady("net1", provisioningNet, lookup); err == nil {
		t.Fatal("expected an error, got nil")
	}
}

func TestProvisioningInterfaceReady_EmptyInterfaceNameChecksEveryInterface(t *testing.T) {
	provisioningNet := mustIPNet(t, "192.0.2.0", 24)
	lookup := func(ifaceName string) ([]net.Addr, error) {
		if ifaceName != "" {
			t.Fatalf("expected empty interface name, got %q", ifaceName)
		}
		return addrsOf("10.244.0.7/16", "192.0.2.9/24"), nil
	}

	if err := ProvisioningInterfaceReady("", provisioningNet, lookup); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestProvisioningInterfaceReady_NoProvisioningCIDRConfigured(t *testing.T) {
	if err := ProvisioningInterfaceReady("net1", nil, func(string) ([]net.Addr, error) {
		t.Fatal("lookup should not be called with no provisioning CIDR")
		return nil, nil
	}); err == nil {
		t.Fatal("expected an error, got nil")
	}
}

func TestProvisioningInterfaceReady_NilLookupUsesProductionDefault(t *testing.T) {
	provisioningNet := mustIPNet(t, "127.0.0.0", 8)
	// The loopback interface always has 127.0.0.1/8 in a test sandbox, so
	// the production interfaceAddrs implementation (net.InterfaceAddrs)
	// should find it with no interface name filter.
	if err := ProvisioningInterfaceReady("", provisioningNet, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
