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

package deployer

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/tjjh89017/kezio/internal/bmc"
)

// fakeBMCScheme is registered with internal/bmc's registry (see init), so
// a test Machine pointing spec.bmc.address at "kezio-testbmc://<key>" gets
// a fake in-memory BMC back from AgentDeployer.connectBMC instead of real
// hardware.
const fakeBMCScheme = "kezio-testbmc"

func init() {
	bmc.Register(fakeBMCScheme, fakeBMCConnect)
}

// fakeBMC is an in-memory bmc.BMC that records every call and lets a test
// script a failure or a specific power-state read-back from any method.
type fakeBMC struct {
	mu sync.Mutex

	state bmc.PowerState

	setOneTimePXEBootCalls int
	powerOnCalls           int
	powerOffCalls          int
	forcePowerOffCalls     int
	powerCycleCalls        int
	getPowerStateCalls     int

	setOneTimePXEBootErr error
	powerOnErr           error
	powerOffErr          error
	forcePowerOffErr     error
	powerCycleErr        error
	getPowerStateErr     error

	// ignorePowerOff simulates a machine that acknowledges the graceful
	// PowerOff request but never actually acts on it - PowerOff() then
	// still succeeds without error, but state is left unchanged.
	ignorePowerOff bool

	gotCreds bmc.Credentials
}

func (f *fakeBMC) PowerOn(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.powerOnCalls++
	if f.powerOnErr != nil {
		return f.powerOnErr
	}
	f.state = bmc.PowerStateOn
	return nil
}

func (f *fakeBMC) PowerOff(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.powerOffCalls++
	if f.powerOffErr != nil {
		return f.powerOffErr
	}
	if !f.ignorePowerOff {
		f.state = bmc.PowerStateOff
	}
	return nil
}

func (f *fakeBMC) ForcePowerOff(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forcePowerOffCalls++
	if f.forcePowerOffErr != nil {
		return f.forcePowerOffErr
	}
	f.state = bmc.PowerStateOff
	return nil
}

func (f *fakeBMC) PowerCycle(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.powerCycleCalls++
	if f.powerCycleErr != nil {
		return f.powerCycleErr
	}
	f.state = bmc.PowerStateOn
	return nil
}

func (f *fakeBMC) GetPowerState(context.Context) (bmc.PowerState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getPowerStateCalls++
	if f.getPowerStateErr != nil {
		return "", f.getPowerStateErr
	}
	return f.state, nil
}

func (f *fakeBMC) SetOneTimePXEBoot(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setOneTimePXEBootCalls++
	return f.setOneTimePXEBootErr
}

func (f *fakeBMC) calls() (setPXE, powerOn, powerOff, forcePowerOff, powerCycle, getState int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.setOneTimePXEBootCalls, f.powerOnCalls, f.powerOffCalls, f.forcePowerOffCalls, f.powerCycleCalls, f.getPowerStateCalls
}

var (
	fakeBMCsMu sync.Mutex
	fakeBMCs   = map[string]*fakeBMC{}
)

// fakeBMCFor returns (creating an initially-off fake on first use) the
// fake BMC for one "kezio-testbmc://<key>" address, keyed separately from
// bmc.Register's process-global registry so distinct tests using distinct
// keys never observe each other's calls.
func fakeBMCFor(key string) *fakeBMC {
	fakeBMCsMu.Lock()
	defer fakeBMCsMu.Unlock()
	f, ok := fakeBMCs[key]
	if !ok {
		f = &fakeBMC{state: bmc.PowerStateOff}
		fakeBMCs[key] = f
	}
	return f
}

func fakeBMCConnect(_ context.Context, address *url.URL, creds bmc.Credentials, _ bmc.Options) (bmc.BMC, error) {
	f := fakeBMCFor(address.Host + address.Path)
	f.mu.Lock()
	f.gotCreds = creds
	f.mu.Unlock()
	return f, nil
}

// fakeBMCAddress builds a unique "kezio-testbmc://" address for t, so
// tests never collide on the same *fakeBMC.
func fakeBMCAddress(t *testing.T) string {
	t.Helper()
	return fakeBMCScheme + "://" + strings.ReplaceAll(t.Name(), "/", "-")
}

// fakeBMCForAddress resolves the *fakeBMC an address built by
// fakeBMCAddress dials through.
func fakeBMCForAddress(address string) *fakeBMC {
	return fakeBMCFor(strings.TrimPrefix(address, fakeBMCScheme+"://"))
}

// fakeBMCDialErrScheme is a second registered scheme whose driver always
// fails with a net.Error, the shape isNetworkUnreachable classifies as
// deployer.Delayed rather than deployer.Failed.
const fakeBMCDialErrScheme = "kezio-testbmc-dialerr"

func init() {
	bmc.Register(fakeBMCDialErrScheme, fakeBMCDialErrConnect)
}

func fakeBMCDialErrConnect(context.Context, *url.URL, bmc.Credentials, bmc.Options) (bmc.BMC, error) {
	return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
}
