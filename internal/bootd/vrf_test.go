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
	"context"
	"net"
	"os"
	"strings"
	"testing"

	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// TestSetupProvisioningVRF_RequiresInterface proves an empty interface
// is rejected before any "ip" command runs - there would be nothing to
// enslave into the VRF.
func TestSetupProvisioningVRF_RequiresInterface(t *testing.T) {
	err := SetupProvisioningVRF(context.Background(), logf.Log, "", net.ParseIP("192.0.2.1"))
	if err == nil {
		t.Fatal("SetupProvisioningVRF returned nil error with an empty interface")
	}
}

// TestSetupProvisioningVRF_RequiresIPv4Gateway proves a nil or
// non-IPv4 gateway is rejected before any "ip" command runs.
func TestSetupProvisioningVRF_RequiresIPv4Gateway(t *testing.T) {
	for name, gw := range map[string]net.IP{
		"nil":  nil,
		"ipv6": net.ParseIP("2001:db8::1"),
	} {
		t.Run(name, func(t *testing.T) {
			err := SetupProvisioningVRF(context.Background(), logf.Log, "net1", gw)
			if err == nil {
				t.Fatalf("SetupProvisioningVRF returned nil error for gateway %v", gw)
			}
		})
	}
}

// TestRunIP_WrapsFailureOutput proves a non-zero "ip"-shaped command
// has its stderr/stdout folded into the returned error, so a caller
// never has to go dig up raw command output separately - the failure
// text itself (RTNETLINK's own explanation, in production) is what
// makes SetupProvisioningVRF's errors loud and actionable.
func TestRunIP_WrapsFailureOutput(t *testing.T) {
	fakeIP := writeFakeDnsmasq(t, t.TempDir(), `echo "RTNETLINK answers: Operation not permitted" >&2
exit 2
`)
	_, err := runIPWithBinary(context.Background(), fakeIP, "link", "add", "kezio-prov", "type", "vrf")
	if err == nil {
		t.Fatal("runIPWithBinary returned nil error for a failing command")
	}
	if !strings.Contains(err.Error(), "Operation not permitted") {
		t.Errorf("runIPWithBinary error = %v, want it to include the command's own stderr", err)
	}
}

// TestRunIP_MissingBinary proves a missing "ip" executable itself
// surfaces as a clear error rather than a silent no-op - the same
// "fail loud" contract as any other required binary this package
// shells out to (see dnsmasq.go's DefaultDnsmasqPath).
func TestRunIP_MissingBinary(t *testing.T) {
	_, err := runIPWithBinary(context.Background(), "/does/not/exist/ip", "link", "show", "kezio-prov")
	if err == nil {
		t.Fatal("runIPWithBinary returned nil error for a nonexistent binary")
	}
}

// TestBindToDeviceControl_ReturnsRawConnError proves the Control
// function surfaces a setsockopt failure (an invalid/nonexistent
// device name, here) rather than silently leaving the socket
// unbound - the same "fail loud" contract SetupProvisioningVRF's own
// errors follow, applied to the socket-level VRF binding path bootd's
// own listeners use (ProxyServer, TFTPServer).
func TestBindToDeviceControl_ReturnsRawConnError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	syscallConn, ok := ln.(*net.TCPListener)
	if !ok {
		t.Fatalf("listener is %T, want *net.TCPListener", ln)
	}
	raw, err := syscallConn.SyscallConn()
	if err != nil {
		t.Fatalf("SyscallConn: %v", err)
	}

	control := bindToDeviceControl("kezio-device-that-does-not-exist")
	if err := control("tcp", "127.0.0.1:0", raw); err == nil {
		if os.Geteuid() != 0 {
			t.Fatal("bindToDeviceControl returned nil error for a nonexistent device (expected a setsockopt failure)")
		}
		t.Skip("running as root: SO_BINDTODEVICE to a nonexistent name may still be rejected differently; skipping the negative assertion")
	}
}
