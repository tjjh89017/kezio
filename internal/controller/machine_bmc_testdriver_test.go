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

package controller

import (
	"context"
	"net/url"
	"sync"

	"github.com/tjjh89017/kezio/internal/bmc"
)

// controllerTestBMCScheme is the URL scheme controllerTestBMCConnect
// registers with internal/bmc's registry (see this file's init), used by
// the AgentFactory envtest walk (machine_agent_deployer_envtest_test.go)
// so its Machine's configured BMC resolves to a fast, in-memory fake
// instead of a driver dialing real hardware. This package's own separate
// test binary needs its own registration - internal/deployer's identical
// seam (internal/deployer/agent_bmc_test.go) lives in a different Go test
// binary and is not visible here.
const controllerTestBMCScheme = "kezio-testbmc"

func init() {
	bmc.Register(controllerTestBMCScheme, controllerTestBMCConnect)
}

// controllerTestBMC is a minimal fake bmc.BMC: the envtest walk only
// needs Register's happy path (SetOneTimePXEBoot, GetPowerState, PowerOn)
// to succeed so the Machine advances to Inspecting the same way it did
// before BMC wiring landed - it does not exercise BMC failure handling,
// which internal/deployer/agent_bmc_test.go already covers at the unit
// level.
type controllerTestBMC struct {
	mu    sync.Mutex
	state bmc.PowerState
}

func (f *controllerTestBMC) PowerOn(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = bmc.PowerStateOn
	return nil
}

func (f *controllerTestBMC) PowerOff(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = bmc.PowerStateOff
	return nil
}

func (f *controllerTestBMC) ForcePowerOff(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = bmc.PowerStateOff
	return nil
}

func (f *controllerTestBMC) PowerCycle(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = bmc.PowerStateOn
	return nil
}

func (f *controllerTestBMC) GetPowerState(context.Context) (bmc.PowerState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, nil
}

func (f *controllerTestBMC) SetOneTimePXEBoot(context.Context) error {
	return nil
}

func controllerTestBMCConnect(context.Context, *url.URL, bmc.Credentials, bmc.Options) (bmc.BMC, error) {
	return &controllerTestBMC{state: bmc.PowerStateOff}, nil
}
