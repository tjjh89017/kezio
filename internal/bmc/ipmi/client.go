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
	"fmt"

	ipmilib "github.com/bougou/go-ipmi"

	"github.com/tjjh89017/kezio/internal/bmc"
)

// session is the subset of *ipmilib.Client this driver calls, seamed out
// so driver's methods are unit-testable against a fake session with no
// real BMC or network. Signatures mirror ipmilib.Client's exactly, so
// *ipmilib.Client satisfies this with no adapter.
type session interface {
	// ChassisControl: IPMI section 28.3 - power up/down/cycle/soft-shutdown.
	ChassisControl(ctx context.Context, control ipmilib.ChassisControl) (*ipmilib.ChassisControlResponse, error)
	// GetChassisStatus: IPMI section 28.2; PowerIsOn feeds GetPowerState.
	GetChassisStatus(ctx context.Context) (*ipmilib.GetChassisStatusResponse, error)
	// SetBootDevice: Set System Boot Options (IPMI section 28.12) - see
	// the package doc for the values SetOneTimePXEBoot passes.
	SetBootDevice(ctx context.Context, bootDeviceSelector ipmilib.BootDeviceSelector, bootType ipmilib.BIOSBootType, persist bool) error
	// Close tears down the session; see withSession for why its error is
	// not propagated to a caller.
	Close(ctx context.Context) error
}

// dialFunc opens one IPMI 2.0/RMCP+ session against host:port. driver
// holds one as a field (rather than calling dial directly) so tests can
// substitute a fake that never touches the network.
type dialFunc func(ctx context.Context, host string, port int, creds bmc.Credentials) (session, error)

// _ asserts *ipmilib.Client satisfies session at compile time, so a
// breaking signature change in bougou/go-ipmi fails the build here rather
// than at runtime.
var _ session = (*ipmilib.Client)(nil)

// dial is the dialFunc connect wires up for production use: a real IPMI
// 2.0/RMCP+ (lanplus) session via bougou/go-ipmi at ADMINISTRATOR
// privilege (see the package doc for why).
func dial(ctx context.Context, host string, port int, creds bmc.Credentials) (session, error) {
	client, err := ipmilib.NewClient(host, port, creds.Username, creds.Password)
	if err != nil {
		return nil, fmt.Errorf("ipmi: creating client: %w", err)
	}
	client.WithMaxPrivilegeLevel(ipmilib.PrivilegeLevelAdministrator)

	if err := client.Connect(ctx); err != nil {
		return nil, fmt.Errorf("ipmi: connecting: %w", err)
	}
	return client, nil
}
