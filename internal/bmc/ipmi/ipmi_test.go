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

package ipmi

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	ipmilib "github.com/bougou/go-ipmi"

	"github.com/tjjh89017/kezio/internal/bmc"
)

// fakeSession is a session that records every call it was asked to make and
// returns canned output/error, so a test can assert on the exact IPMI
// command a driver method issued without a real BMC or network.
type fakeSession struct {
	chassisControlCalls []ipmilib.ChassisControl
	chassisControlErr   error

	getChassisStatusOn  bool
	getChassisStatusErr error

	setBootDeviceCalls []setBootDeviceCall
	setBootDeviceErr   error

	closed bool
}

type setBootDeviceCall struct {
	selector ipmilib.BootDeviceSelector
	bootType ipmilib.BIOSBootType
	persist  bool
}

func (f *fakeSession) ChassisControl(_ context.Context, control ipmilib.ChassisControl) (*ipmilib.ChassisControlResponse, error) {
	f.chassisControlCalls = append(f.chassisControlCalls, control)
	if f.chassisControlErr != nil {
		return nil, f.chassisControlErr
	}
	return &ipmilib.ChassisControlResponse{}, nil
}

func (f *fakeSession) GetChassisStatus(_ context.Context) (*ipmilib.GetChassisStatusResponse, error) {
	if f.getChassisStatusErr != nil {
		return nil, f.getChassisStatusErr
	}
	return &ipmilib.GetChassisStatusResponse{PowerIsOn: f.getChassisStatusOn}, nil
}

func (f *fakeSession) SetBootDevice(_ context.Context, selector ipmilib.BootDeviceSelector, bootType ipmilib.BIOSBootType, persist bool) error {
	f.setBootDeviceCalls = append(f.setBootDeviceCalls, setBootDeviceCall{selector: selector, bootType: bootType, persist: persist})
	return f.setBootDeviceErr
}

func (f *fakeSession) Close(context.Context) error {
	f.closed = true
	return nil
}

// newTestDriver returns a driver whose dial always returns s, so a test can
// drive driver's bmc.BMC methods against a fakeSession without a real
// network dial.
func newTestDriver(s *fakeSession) *driver {
	return &driver{
		host:  "10.0.0.10",
		port:  defaultPort,
		creds: bmc.Credentials{Username: "admin", Password: "hunter2"},
		dial: func(context.Context, string, int, bmc.Credentials) (session, error) {
			return s, nil
		},
	}
}

func TestPowerOnIssuesChassisControlPowerUp(t *testing.T) {
	s := &fakeSession{}
	d := newTestDriver(s)

	if err := d.PowerOn(context.Background()); err != nil {
		t.Fatalf("PowerOn() error = %v", err)
	}
	if got := s.chassisControlCalls; len(got) != 1 || got[0] != ipmilib.ChassisControlPowerUp {
		t.Errorf("ChassisControl calls = %v, want exactly [PowerUp]", got)
	}
	if !s.closed {
		t.Error("PowerOn() did not close the session")
	}
}

func TestPowerOffIssuesChassisControlSoftShutdown(t *testing.T) {
	s := &fakeSession{}
	d := newTestDriver(s)

	if err := d.PowerOff(context.Background()); err != nil {
		t.Fatalf("PowerOff() error = %v", err)
	}
	if got := s.chassisControlCalls; len(got) != 1 || got[0] != ipmilib.ChassisControlSoftShutdown {
		t.Errorf("ChassisControl calls = %v, want exactly [SoftShutdown] (a graceful ACPI shutdown, not a hard power-down)", got)
	}
}

func TestPowerCycleIssuesChassisControlPowerCycle(t *testing.T) {
	s := &fakeSession{}
	d := newTestDriver(s)

	if err := d.PowerCycle(context.Background()); err != nil {
		t.Fatalf("PowerCycle() error = %v", err)
	}
	if got := s.chassisControlCalls; len(got) != 1 || got[0] != ipmilib.ChassisControlPowerCycle {
		t.Errorf("ChassisControl calls = %v, want exactly [PowerCycle]", got)
	}
}

func TestSetOneTimePXEBootEncodesEFIForceOnceOverride(t *testing.T) {
	s := &fakeSession{}
	d := newTestDriver(s)

	if err := d.SetOneTimePXEBoot(context.Background()); err != nil {
		t.Fatalf("SetOneTimePXEBoot() error = %v", err)
	}

	if len(s.setBootDeviceCalls) != 1 {
		t.Fatalf("SetBootDevice calls = %v, want exactly 1 call", s.setBootDeviceCalls)
	}
	call := s.setBootDeviceCalls[0]
	if call.selector != ipmilib.BootDeviceSelectorForcePXE {
		t.Errorf("SetBootDevice selector = %v, want BootDeviceSelectorForcePXE", call.selector)
	}
	if call.bootType != ipmilib.BIOSBootTypeEFI {
		t.Errorf("SetBootDevice bootType = %v, want BIOSBootTypeEFI (the pure-Go equivalent of ipmitool's options=efiboot)", call.bootType)
	}
	if call.persist {
		t.Error("SetBootDevice persist = true, want false: a one-time PXE boot must not survive across boots")
	}
}

func TestGetPowerStateMapsChassisStatus(t *testing.T) {
	tests := []struct {
		name string
		on   bool
		want bmc.PowerState
	}{
		{name: "power is on", on: true, want: bmc.PowerStateOn},
		{name: "power is off", on: false, want: bmc.PowerStateOff},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &fakeSession{getChassisStatusOn: tt.on}
			d := newTestDriver(s)

			got, err := d.GetPowerState(context.Background())
			if err != nil {
				t.Fatalf("GetPowerState() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("GetPowerState() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetPowerStateReturnsUnknownOnReadError(t *testing.T) {
	s := &fakeSession{getChassisStatusErr: errors.New("simulated BMC failure")}
	d := newTestDriver(s)

	got, err := d.GetPowerState(context.Background())
	if err == nil {
		t.Fatal("GetPowerState() succeeded, want an error")
	}
	if got != bmc.PowerStateUnknown {
		t.Errorf("GetPowerState() = %v, want PowerStateUnknown on a failed read", got)
	}
}

func TestPowerStateMapping(t *testing.T) {
	if got := powerState(true); got != bmc.PowerStateOn {
		t.Errorf("powerState(true) = %v, want PowerStateOn", got)
	}
	if got := powerState(false); got != bmc.PowerStateOff {
		t.Errorf("powerState(false) = %v, want PowerStateOff", got)
	}
}

func TestWithSessionClosesSessionEvenWhenFnFails(t *testing.T) {
	s := &fakeSession{}
	d := newTestDriver(s)

	err := d.withSession(context.Background(), func(session) error {
		return errors.New("simulated failure")
	})
	if err == nil {
		t.Fatal("withSession() succeeded, want the wrapped error")
	}
	if !s.closed {
		t.Error("withSession() did not close the session after fn returned an error")
	}
}

func TestWithSessionPropagatesDialError(t *testing.T) {
	d := &driver{
		host: "10.0.0.10", port: defaultPort,
		creds: bmc.Credentials{Username: "admin", Password: "hunter2"},
		dial: func(context.Context, string, int, bmc.Credentials) (session, error) {
			return nil, errors.New("simulated dial failure")
		},
	}

	if err := d.PowerOn(context.Background()); err == nil {
		t.Fatal("PowerOn() succeeded, want the dial error to propagate")
	}
}

func TestErrorsDoNotContainCredentials(t *testing.T) {
	const password = "correct-horse-battery-staple"
	s := &fakeSession{
		chassisControlErr:   errors.New("simulated failure"),
		getChassisStatusErr: errors.New("simulated failure"),
		setBootDeviceErr:    errors.New("simulated failure"),
	}
	d := &driver{
		host: "10.0.0.10", port: defaultPort,
		creds: bmc.Credentials{Username: "admin", Password: password},
		dial: func(context.Context, string, int, bmc.Credentials) (session, error) {
			return s, nil
		},
	}

	methods := map[string]func() error{
		"PowerOn":           func() error { return d.PowerOn(context.Background()) },
		"PowerOff":          func() error { return d.PowerOff(context.Background()) },
		"PowerCycle":        func() error { return d.PowerCycle(context.Background()) },
		"SetOneTimePXEBoot": func() error { return d.SetOneTimePXEBoot(context.Background()) },
		"GetPowerState": func() error {
			_, err := d.GetPowerState(context.Background())
			return err
		},
	}

	for name, call := range methods {
		t.Run(name, func(t *testing.T) {
			err := call()
			if err == nil {
				t.Fatal("call succeeded, want an error to check for leaked credentials")
			}
			if strings.Contains(err.Error(), password) {
				t.Errorf("%s error leaked the password: %q", name, err.Error())
			}
		})
	}
}

func TestHostPortParsesHostAndDefaultsPort(t *testing.T) {
	tests := []struct {
		name     string
		address  string
		wantHost string
		wantPort int
	}{
		{name: "host only defaults to 623", address: "ipmi://10.0.0.10", wantHost: "10.0.0.10", wantPort: defaultPort},
		{name: "host and port", address: "ipmi://10.0.0.10:6230", wantHost: "10.0.0.10", wantPort: 6230},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := url.Parse(tt.address)
			if err != nil {
				t.Fatalf("parsing test address: %v", err)
			}
			host, port, err := hostPort(u)
			if err != nil {
				t.Fatalf("hostPort() error = %v", err)
			}
			if host != tt.wantHost || port != tt.wantPort {
				t.Errorf("hostPort() = (%q, %d), want (%q, %d)", host, port, tt.wantHost, tt.wantPort)
			}
		})
	}
}

func TestHostPortRejectsAddressWithoutHost(t *testing.T) {
	u, err := url.Parse("ipmi://")
	if err != nil {
		t.Fatalf("parsing test address: %v", err)
	}
	if _, _, err := hostPort(u); err == nil {
		t.Fatal("hostPort() with no host succeeded, want an error")
	}
}

func TestHostPortRejectsAddressWithPath(t *testing.T) {
	u, err := url.Parse("ipmi://10.0.0.10/some/path")
	if err != nil {
		t.Fatalf("parsing test address: %v", err)
	}
	if _, _, err := hostPort(u); err == nil {
		t.Fatal("hostPort() with a path succeeded, want an error")
	}
}

// TestHostPortAcceptsPortAtUint16Range checks the port survives the full
// valid TCP/UDP range without truncation.
func TestHostPortAcceptsPortAtUint16Range(t *testing.T) {
	u, err := url.Parse("ipmi://10.0.0.10:65535")
	if err != nil {
		t.Fatalf("parsing test address: %v", err)
	}
	_, port, err := hostPort(u)
	if err != nil {
		t.Fatalf("hostPort() error = %v", err)
	}
	if port != 65535 {
		t.Errorf("hostPort() port = %d, want 65535", port)
	}
}

// TestConnectRegistersIPMIScheme confirms importing this package makes
// bmc.Connect resolve "ipmi://" to this driver.
func TestConnectRegistersIPMIScheme(t *testing.T) {
	b, err := bmc.Connect(context.Background(), "ipmi://10.0.0.10:6230", bmc.Credentials{Username: "admin", Password: "hunter2"}, bmc.Options{})
	if err != nil {
		t.Fatalf("bmc.Connect() error = %v", err)
	}
	if b == nil {
		t.Fatal("bmc.Connect() returned a nil BMC with no error")
	}
	if _, ok := b.(*driver); !ok {
		t.Errorf("bmc.Connect() returned a %T, want *ipmi.driver", b)
	}
}

func TestConnectRejectsAddressWithoutHost(t *testing.T) {
	_, err := bmc.Connect(context.Background(), "ipmi:///some/path", bmc.Credentials{Username: "admin", Password: "hunter2"}, bmc.Options{})
	if err == nil {
		t.Fatal("bmc.Connect() with no host succeeded, want an error")
	}
}
