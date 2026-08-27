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
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tjjh89017/kezio/internal/seederdeploy"
)

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

func TestInterfaceHasIPv4_Present(t *testing.T) {
	lookup := func(ifaceName string) ([]net.Addr, error) {
		if ifaceName != "eth0" {
			t.Fatalf("unexpected interface name %q", ifaceName)
		}
		return addrsOf("192.0.2.5/24"), nil
	}
	if err := interfaceHasIPv4("eth0", lookup); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestInterfaceHasIPv4_NoAddresses(t *testing.T) {
	lookup := func(string) ([]net.Addr, error) { return nil, nil }
	if err := interfaceHasIPv4("eth0", lookup); err == nil {
		t.Fatal("expected an error, got nil")
	}
}

func TestInterfaceHasIPv4_OnlyIPv6(t *testing.T) {
	lookup := func(string) ([]net.Addr, error) {
		_, ipNet, _ := net.ParseCIDR("fe80::1/64")
		return []net.Addr{ipNet}, nil
	}
	if err := interfaceHasIPv4("eth0", lookup); err == nil {
		t.Fatal("expected an error, got nil")
	}
}

func TestInterfaceHasIPv4_LookupError(t *testing.T) {
	lookup := func(string) ([]net.Addr, error) { return nil, errors.New("no such interface") }
	if err := interfaceHasIPv4("eth0", lookup); err == nil {
		t.Fatal("expected an error, got nil")
	}
}

func TestInterfaceHasIPv4_NilLookupUsesProductionDefault(t *testing.T) {
	// The loopback interface always has 127.0.0.1/8 in a test sandbox.
	if err := interfaceHasIPv4("lo", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestTorrentMux_HealthzFailsWithoutInterfaceAddress(t *testing.T) {
	idx := newTorrentIndex()
	// "kezio-test-no-such-interface" never exists, so interfaceHasIPv4's
	// production lookup always errors - exercising the wiring from the
	// handler through to net.InterfaceByName without a stub.
	mux := torrentMux(idx, "kezio-test-no-such-interface")

	req := httptest.NewRequest(http.MethodGet, seederdeploy.TorrentHealthzPath, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected %d, got %d", http.StatusServiceUnavailable, rec.Code)
	}
}

func TestTorrentMux_HealthzOKWithInterfaceAddress(t *testing.T) {
	idx := newTorrentIndex()
	mux := torrentMux(idx, "lo")

	req := httptest.NewRequest(http.MethodGet, seederdeploy.TorrentHealthzPath, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected %d, got %d", http.StatusOK, rec.Code)
	}
}
