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
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
	"github.com/tjjh89017/kezio/internal/bootserver"
	"github.com/tjjh89017/kezio/internal/subnetdhcp"
)

// dhcpAckRequeueInterval bounds how soon the controller retries after
// reserveAndAwaitDHCP finds a reservation was just written but bootd has
// not yet acknowledged it (status.dhcp.appliedRevision still behind
// status.dhcp.revision). Short and non-error (Busy honors
// Result.RequeueAfter without touching Machine error state or applying
// backoff), so the wait for bootd's SIGHUP-and-hostsfile-rewrite round
// trip does not stall behind Delayed's much longer fixed interval. A
// package variable, not a const, so a test can shorten it.
var dhcpAckRequeueInterval = 2 * time.Second

// reserveAndAwaitDHCP is armPXEAndPowerOn/armProvisionBootAndPowerOn's
// DHCP reservation step, called after issueBootToken and before arming
// PXE/power: when machine's Subnet is in lease mode, it allocates (or
// reuses) this Machine's fixed address, persists it to the Subnet's
// status.dhcp.reservations table, and waits for bootd to acknowledge the
// resulting revision before letting the caller proceed to arm PXE and
// power the machine on - so the machine never net-boots into a hostsfile
// bootd has not actually rewritten yet.
//
// proceed is false whenever the caller must return result immediately
// without arming anything: the Subnet is unresolvable yet (Delayed), the
// address pool is exhausted (Delayed, with the condition and Event this
// records), or bootd has not yet applied the revision this call just
// wrote (Busy, requeuing after dhcpAckRequeueInterval). proceed is true,
// with a zero Result, for a proxy-mode Subnet, a Machine with no
// resolvable boot MAC yet, or a reservation already acknowledged.
func (d *AgentDeployer) reserveAndAwaitDHCP(ctx context.Context, machine *keziov1alpha3.Machine) (result Result, proceed bool, err error) {
	subnetKey := client.ObjectKey{
		Namespace: subnetdhcp.ResolveNamespace(machine.Spec.SubnetRef, machine.Namespace),
		Name:      machine.Spec.SubnetRef.Name,
	}
	var subnet keziov1alpha3.Subnet
	if err := d.Client.Get(ctx, subnetKey, &subnet); err != nil {
		if apierrors.IsNotFound(err) {
			return Result{Outcome: Delayed}, false, nil
		}
		return Result{}, false, fmt.Errorf("agent deployer: getting Subnet %q for DHCP reservation: %w", subnetKey.Name, err)
	}

	if subnet.Spec.DHCP == nil || subnet.Spec.DHCP.Mode != keziov1alpha3.SubnetDHCPModeLease {
		return Result{}, true, nil
	}
	mac, ok := bootserver.NormalizeMAC(machine.Spec.BootMACAddress)
	if !ok {
		// No boot MAC known yet (typically pre-inspection): nothing to
		// key a reservation on. Proceed unreserved - an unenrolled MAC is
		// gated out by the MAC allowlist regardless (see
		// docs/crd-reference.md's bootMACAddress row).
		return Result{}, true, nil
	}

	subnetPtr, exhausted, err := d.reserveOnSubnet(ctx, subnetKey, machine.Name, mac)
	if err != nil {
		return Result{}, false, err
	}
	if exhausted {
		if err := d.recordDHCPPoolExhausted(ctx, subnetKey); err != nil {
			return Result{}, false, err
		}
		if d.Recorder != nil {
			d.Recorder.Eventf(machine, corev1.EventTypeWarning, "DHCPPoolExhausted",
				"Subnet %q's lease-mode DHCP address pool has no free address to reserve for this machine's boot", subnetKey.Name)
		}
		return Result{Outcome: Delayed}, false, nil
	}

	dhcp := subnetPtr.Status.DHCP
	if dhcp != nil && dhcp.Revision != "" && dhcp.AppliedRevision != dhcp.Revision {
		return Result{Outcome: Busy, RequeueAfter: dhcpAckRequeueInterval}, false, nil
	}
	return Result{}, true, nil
}

// reserveOnSubnet resolves machineName's DHCP reservation on the Subnet
// named by subnetKey, allocating a fresh one when the table carries none
// yet, retrying the whole selection against a freshly-Got Subnet on an
// apiserver conflict rather than ever rewriting a previously chosen
// address. exhausted is true when subnetdhcp.Reserve reports the address
// pool has no free entry left; subnet is then the freshest Get, for
// recordDHCPPoolExhausted to report against.
func (d *AgentDeployer) reserveOnSubnet(ctx context.Context, subnetKey client.ObjectKey, machineName, mac string) (subnet *keziov1alpha3.Subnet, exhausted bool, err error) {
	var current keziov1alpha3.Subnet
	if err := d.Client.Get(ctx, subnetKey, &current); err != nil {
		return nil, false, fmt.Errorf("agent deployer: getting Subnet %q for DHCP reservation: %w", subnetKey.Name, err)
	}

	for {
		_, changed, reserveErr := subnetdhcp.Reserve(&current, machineName, mac, metav1.Now())
		if reserveErr != nil {
			if errors.Is(reserveErr, subnetdhcp.ErrPoolExhausted) {
				return &current, true, nil
			}
			return nil, false, fmt.Errorf("agent deployer: reserving a DHCP address on Subnet %q: %w", subnetKey.Name, reserveErr)
		}
		if !changed {
			return &current, false, nil
		}

		if err := d.Client.Status().Update(ctx, &current); err != nil {
			if apierrors.IsConflict(err) {
				if err := d.Client.Get(ctx, subnetKey, &current); err != nil {
					return nil, false, fmt.Errorf("agent deployer: re-getting Subnet %q after a conflict: %w", subnetKey.Name, err)
				}
				continue
			}
			return nil, false, fmt.Errorf("agent deployer: persisting DHCP reservation on Subnet %q: %w", subnetKey.Name, err)
		}
		return &current, false, nil
	}
}

// recordDHCPPoolExhausted sets SubnetConditionDHCPPoolExhausted True on
// the Subnet named by subnetKey, retrying past an apiserver conflict by
// re-Getting and reapplying the same condition.
func (d *AgentDeployer) recordDHCPPoolExhausted(ctx context.Context, subnetKey client.ObjectKey) error {
	var subnet keziov1alpha3.Subnet
	for {
		if err := d.Client.Get(ctx, subnetKey, &subnet); err != nil {
			return fmt.Errorf("agent deployer: getting Subnet %q to record pool exhaustion: %w", subnetKey.Name, err)
		}
		apimeta.SetStatusCondition(&subnet.Status.Conditions, metav1.Condition{
			Type:               keziov1alpha3.SubnetConditionDHCPPoolExhausted,
			Status:             metav1.ConditionTrue,
			Reason:             "DHCPPoolExhausted",
			Message:            "the lease-mode DHCP address pool has no free address left for a new reservation",
			ObservedGeneration: subnet.Generation,
		})
		if err := d.Client.Status().Update(ctx, &subnet); err != nil {
			if apierrors.IsConflict(err) {
				continue
			}
			return fmt.Errorf("agent deployer: recording DHCP pool exhaustion on Subnet %q: %w", subnetKey.Name, err)
		}
		return nil
	}
}

// releaseDHCPReservation releases machine's DHCP reservation, if any, on
// its spec.subnetRef Subnet - called once a deploy step completes
// (Inspect reaching Complete, Provision's Succeeded phase) and as the
// first act of the delete walk's Deprovision step. A Subnet that no
// longer exists, or that never held a reservation for machine, is a
// silent no-op: releasing something already gone is exactly the goal.
func (d *AgentDeployer) releaseDHCPReservation(ctx context.Context, machine *keziov1alpha3.Machine) error {
	subnetKey := client.ObjectKey{
		Namespace: subnetdhcp.ResolveNamespace(machine.Spec.SubnetRef, machine.Namespace),
		Name:      machine.Spec.SubnetRef.Name,
	}
	var subnet keziov1alpha3.Subnet
	for {
		if err := d.Client.Get(ctx, subnetKey, &subnet); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("agent deployer: getting Subnet %q to release a DHCP reservation: %w", subnetKey.Name, err)
		}
		if !subnetdhcp.Release(&subnet, machine.Name) {
			return nil
		}
		if err := d.Client.Status().Update(ctx, &subnet); err != nil {
			if apierrors.IsConflict(err) {
				continue
			}
			return fmt.Errorf("agent deployer: releasing DHCP reservation on Subnet %q: %w", subnetKey.Name, err)
		}
		return nil
	}
}
