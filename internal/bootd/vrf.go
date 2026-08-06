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
	"fmt"
	"net"
	"os/exec"
	"strings"
	"syscall"

	"github.com/go-logr/logr"
	"golang.org/x/sys/unix"
)

// ProvisioningVRFName is the VRF device SetupProvisioningVRF creates
// when Config.ProvisioningGateway is set. Every provisioning-side
// socket shares it: dnsmasq's child process runs inside it via "ip vrf
// exec" (Dnsmasq.VRFName), and bootd's own listeners (ProxyServer,
// TFTPServer) bind to it directly via SO_BINDTODEVICE
// (bindToDeviceControl) - one routing domain, distinct from the pod's
// default table, for every socket that talks to the provisioning
// segment.
const ProvisioningVRFName = "kezio-prov"

// provisioningVRFTable is the routing table SetupProvisioningVRF
// installs the VRF's default route in. A VRF device owns a private
// table by construction - any number works - chosen once here so it
// never collides with the pod's main table (254) or a well-known
// table a CNI plugin might already use.
const provisioningVRFTable = 1101

// SetupProvisioningVRF creates the VRF device named ProvisioningVRFName,
// enslaves iface into it, and installs a default route via gateway in
// the VRF's own routing table. It is idempotent: safe to call again
// against state a previous bootd process in the same pod network
// namespace already set up (the VRF device, once created, outlives a
// container restart within the same pod), since every step but the
// device creation itself (ip link add) is naturally idempotent, and
// that one step is skipped when the device already exists.
//
// This requires the "ip" binary - present in bootd's own container
// image alongside dnsmasq for exactly this reason, see
// docker/bootd/Dockerfile - and CAP_NET_ADMIN, already granted for
// dnsmasq's own sake (see caps.go). Every step's failure is returned
// with the command's own output attached, so the two failure shapes
// lab-verified against this exact sequence are both surfaced loudly
// rather than swallowed: a kernel without VRF support (the vrf module,
// or CONFIG_NET_VRF, missing) fails the first "ip link add ... type
// vrf" with its own "RTNETLINK answers: Operation not supported", and
// a caller lacking CAP_NET_ADMIN fails the same command with
// "RTNETLINK answers: Operation not permitted". cmd/bootd treats
// either as a fatal startup error, exactly like every other required-
// config failure - a half-configured VRF (device created but nothing
// enslaved, or enslaved but with no route) is never left running.
func SetupProvisioningVRF(ctx context.Context, log logr.Logger, iface string, gateway net.IP) error {
	if iface == "" {
		return fmt.Errorf("a provisioning gateway is configured but no interface is set to enslave into the VRF")
	}
	gw := gateway.To4()
	if gw == nil {
		return fmt.Errorf("provisioning gateway %v is not a valid IPv4 address", gateway)
	}

	if _, err := runIP(ctx, "link", "show", ProvisioningVRFName); err != nil {
		if _, err := runIP(ctx, "link", "add", ProvisioningVRFName, "type", "vrf", "table", fmt.Sprint(provisioningVRFTable)); err != nil {
			return fmt.Errorf("creating VRF device %s: %w", ProvisioningVRFName, err)
		}
		log.Info("created provisioning VRF device", "vrf", ProvisioningVRFName, "table", provisioningVRFTable)
	}
	if _, err := runIP(ctx, "link", "set", ProvisioningVRFName, "up"); err != nil {
		return fmt.Errorf("bringing up VRF device %s: %w", ProvisioningVRFName, err)
	}
	if _, err := runIP(ctx, "link", "set", iface, "master", ProvisioningVRFName); err != nil {
		return fmt.Errorf("enslaving interface %s into VRF %s: %w", iface, ProvisioningVRFName, err)
	}
	if _, err := runIP(ctx, "route", "replace", "default", "via", gw.String(), "dev", iface, "table", fmt.Sprint(provisioningVRFTable)); err != nil {
		return fmt.Errorf("installing default route via %s in VRF %s's table: %w", gw, ProvisioningVRFName, err)
	}
	log.Info("provisioning VRF ready", "vrf", ProvisioningVRFName, "interface", iface, "gateway", gw.String())
	return nil
}

// runIP runs the "ip" binary with args and returns its combined
// output. A non-zero exit is wrapped together with that output, so
// every caller's error already carries the kernel's own explanation
// (RTNETLINK's message text, or the shell's "not found" if the binary
// itself is missing) instead of a bare exit code.
func runIP(ctx context.Context, args ...string) (string, error) {
	return runIPWithBinary(ctx, "ip", args...)
}

// runIPWithBinary is runIP with the binary itself overridable - a test
// seam, so tests can assert on the failure-wrapping behavior with a
// fake script instead of the real "ip" (which would need real
// CAP_NET_ADMIN and kernel VRF support to exercise meaningfully - see
// vrf_test.go and the containerized packet lab for that).
func runIPWithBinary(ctx context.Context, binary string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			return "", fmt.Errorf("running ip %s: %w", strings.Join(args, " "), err)
		}
		return "", fmt.Errorf("running ip %s: %s: %w", strings.Join(args, " "), msg, err)
	}
	return string(out), nil
}

// bindToDeviceControl returns a net.ListenConfig.Control function that
// binds the listening socket to device (SO_BINDTODEVICE) before it is
// bound to its address. This is the mechanism the kernel's own VRF
// documentation names for a process that wants to pick a VRF's routing
// table for a socket it creates itself, without running the whole
// process under "ip vrf exec": binding to a VRF device (or an
// interface already enslaved into one) makes that socket use the
// VRF's table for both send and receive. Lab-verified: a UDP socket
// bound this way to an enslaved provisioning interface's VRF
// successfully sent through the VRF's default route and the datagram
// arrived intact on the far side of that route.
func bindToDeviceControl(device string) func(network, address string, c syscall.RawConn) error {
	return func(_, _ string, c syscall.RawConn) error {
		var sockErr error
		if err := c.Control(func(fd uintptr) {
			sockErr = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, device)
		}); err != nil {
			return err
		}
		return sockErr
	}
}
