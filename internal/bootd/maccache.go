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
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"

	toolscache "k8s.io/client-go/tools/cache"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
	"github.com/tjjh89017/kezio/internal/subnetdhcp"
)

// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=machines,verbs=get;list;watch

// MACSink receives the current set of allowed boot MAC addresses every
// time it changes. Dnsmasq implements it by rewriting the
// dhcp-hostsfile and SIGHUPing dnsmasq (see Dnsmasq.SetAllowedMACs).
type MACSink interface {
	SetAllowedMACs(macs []string)
}

// MACCache keeps a local, live-updated set of every enrolled Machine's
// normalized boot MAC address, fed by a controller-runtime informer
// rather than a List/Get per event, so the MAC allowlist tracks Machine
// churn without per-boot API server load. Every change is pushed to the
// configured MACSink (the dnsmasq dhcp-hostsfile).
//
// Fail-secure by construction: nothing is pushed to the sink - which
// keeps its empty allowlist, booting nothing - until the informer's
// first cache sync completes, and permanently if it never completes (see
// Start). A one-cycle-late boot is an acceptable cost; a machine net-booted
// because an unreachable API server was treated as "nothing enrolled, so
// allow" is not.
type MACCache struct {
	mu     sync.Mutex
	counts map[string]int // normalized MAC -> number of Machines currently claiming it
	// localCounts tracks, of the Machines counted in counts, how many
	// currently resolve spec.subnetRef to localNamespace/localName - this
	// bootd instance's own Subnet. HasLocalMember reads it: the signal
	// Dnsmasq.SetReservations needs to tell a Machine's ordinary Complete
	// release (still enrolled, reservation gone because the deploy
	// finished on this same Subnet) apart from a spec.subnetRef change
	// (still enrolled, reservation gone because it now targets a
	// different Subnet).
	localCounts map[string]int
	// records remembers each currently-known Machine's own (mac, local)
	// pair, keyed by "namespace/name", so onUpdate/onDelete can undo
	// exactly what onAdd/onUpdate applied - including a subnetRef-only
	// change that leaves the MAC untouched, which counts alone cannot
	// detect.
	records map[string]machineRecord

	// localNamespace/localName name this bootd instance's own Subnet.
	// localName empty (the zero value) means HasLocalMember always
	// reports false - no Subnet identity to compare against, so a
	// reservation disappearing is never attributed to a subnetRef change.
	localNamespace, localName string

	synced    atomic.Bool
	informers ctrlcache.Informers
	sink      MACSink
}

// machineRecord is one Machine's last-applied contribution to counts and
// localCounts.
type machineRecord struct {
	mac   string
	local bool
}

var _ manager.Runnable = (*MACCache)(nil)

// NewMACCache builds a MACCache pushing into sink and registers its
// event handler on informers' Machine informer. localNamespace/localName
// name this bootd instance's own Subnet (see HasLocalMember); pass empty
// strings when no Subnet is configured (BOOTD_SUBNET_NAME unset).
// It does not block on the initial sync - call mgr.Add on the returned
// MACCache (it implements manager.Runnable) to have that wait happen as
// part of the manager's own startup, or call Start directly in a
// standalone binary.
func NewMACCache(ctx context.Context, informers ctrlcache.Informers, sink MACSink, localNamespace, localName string) (*MACCache, error) {
	c := &MACCache{
		counts:         make(map[string]int),
		localCounts:    make(map[string]int),
		records:        make(map[string]machineRecord),
		localNamespace: localNamespace,
		localName:      localName,
		informers:      informers,
		sink:           sink,
	}

	informer, err := informers.GetInformer(ctx, &keziov1alpha3.Machine{})
	if err != nil {
		return nil, fmt.Errorf("getting Machine informer: %w", err)
	}
	if _, err := informer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc:    c.onAdd,
		UpdateFunc: c.onUpdate,
		DeleteFunc: c.onDelete,
	}); err != nil {
		return nil, fmt.Errorf("registering Machine event handler: %w", err)
	}

	return c, nil
}

// Start implements manager.Runnable: waits for the Machine informer's
// initial sync, then pushes the first snapshot to the sink and blocks
// until ctx is cancelled. If sync never completes, no snapshot is ever
// pushed and nothing boots - a cache reporting zero Machines because it
// failed to connect is indistinguishable from one reporting zero because
// there are none, so only the fail-secure default is safe for both.
func (c *MACCache) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("bootd-maccache")
	if !c.informers.WaitForCacheSync(ctx) {
		log.Error(fmt.Errorf("cache sync did not complete"), "Machine MAC cache never became ready; denying all boot requests until restart")
		<-ctx.Done()
		return nil
	}
	c.mu.Lock()
	c.synced.Store(true)
	c.pushLocked()
	c.mu.Unlock()
	log.Info("Machine MAC cache ready")
	<-ctx.Done()
	return nil
}

// Snapshot returns the sorted set of currently allowed MACs, or nil
// while the cache has not synced yet (see Start's fail-secure
// contract).
func (c *MACCache) Snapshot() []string {
	if !c.synced.Load() {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snapshotLocked()
}

// snapshotLocked builds the sorted MAC list; callers hold c.mu.
func (c *MACCache) snapshotLocked() []string {
	macs := make([]string, 0, len(c.counts))
	for mac := range c.counts {
		macs = append(macs, mac)
	}
	slices.Sort(macs)
	return macs
}

// pushLocked pushes the current snapshot if synced; callers hold c.mu.
// Holding the lock across the sink call keeps pushes ordered - two
// racing informer events can't deliver snapshots to the sink reversed
// and leave the hostsfile stale.
func (c *MACCache) pushLocked() {
	if !c.synced.Load() || c.sink == nil {
		return
	}
	c.sink.SetAllowedMACs(c.snapshotLocked())
}

func (c *MACCache) onAdd(obj any) {
	machine, ok := obj.(*keziov1alpha3.Machine)
	if !ok {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.applyLocked(machine, machine)
	c.pushLocked()
}

func (c *MACCache) onUpdate(oldObj, newObj any) {
	oldMachine, ok := oldObj.(*keziov1alpha3.Machine)
	if !ok {
		return
	}
	newMachine, ok := newObj.(*keziov1alpha3.Machine)
	if !ok {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.applyLocked(oldMachine, newMachine)
	c.pushLocked()
}

func (c *MACCache) onDelete(obj any) {
	machine, ok := obj.(*keziov1alpha3.Machine)
	if !ok {
		// A watch that misses a delete event can hand back a
		// DeletedFinalStateUnknown wrapping the last known object
		// instead of the object itself; unwrap it rather than
		// silently leaking that Machine's MAC in the count forever.
		tombstone, ok := obj.(toolscache.DeletedFinalStateUnknown)
		if !ok {
			return
		}
		machine, ok = tombstone.Obj.(*keziov1alpha3.Machine)
		if !ok {
			return
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.applyLocked(machine, nil)
	c.pushLocked()
}

// applyLocked replaces identity's record with the one derived from
// newMachine (nil for a delete), undoing the old contribution and adding
// the new one. identity and newMachine name the same Machine (namespace
// and name never change), except on delete where newMachine is nil.
// Called for every Add/Update/Delete event, including a subnetRef-only
// change that leaves the MAC untouched - counts alone would miss that,
// but localCounts must not.
func (c *MACCache) applyLocked(identity, newMachine *keziov1alpha3.Machine) {
	key := identity.Namespace + "/" + identity.Name
	if prev, ok := c.records[key]; ok {
		c.removeRecordLocked(prev)
		delete(c.records, key)
	}
	if newMachine == nil {
		return
	}
	mac, ok := NormalizeMAC(newMachine.Spec.BootMACAddress)
	if !ok {
		return
	}
	rec := machineRecord{mac: mac, local: c.isLocalLocked(newMachine)}
	c.records[key] = rec
	c.addRecordLocked(rec)
}

// isLocalLocked reports whether machine's spec.subnetRef resolves to
// this cache's own configured Subnet. Always false when localName is
// empty (no Subnet configured).
func (c *MACCache) isLocalLocked(machine *keziov1alpha3.Machine) bool {
	if c.localName == "" {
		return false
	}
	ns := subnetdhcp.ResolveNamespace(machine.Spec.SubnetRef, machine.Namespace)
	return ns == c.localNamespace && machine.Spec.SubnetRef.Name == c.localName
}

// addRecordLocked and removeRecordLocked maintain counts/localCounts
// keyed by normalized MAC, not a plain set, because bootMACAddress
// uniqueness is only a convention (see
// internal/bootserver.Server.lookupMachine) - a plain set would let
// deleting one of two Machines sharing a MAC revoke the gate for the
// other still-enrolled one.
func (c *MACCache) addRecordLocked(rec machineRecord) {
	c.counts[rec.mac]++
	if rec.local {
		c.localCounts[rec.mac]++
	}
}

func (c *MACCache) removeRecordLocked(rec machineRecord) {
	if c.counts[rec.mac] <= 1 {
		delete(c.counts, rec.mac)
	} else {
		c.counts[rec.mac]--
	}
	if rec.local {
		if c.localCounts[rec.mac] <= 1 {
			delete(c.localCounts, rec.mac)
		} else {
			c.localCounts[rec.mac]--
		}
	}
}

// HasLocalMember reports whether any Machine currently claiming mac (as
// its normalized boot MAC address) resolves spec.subnetRef to this
// bootd instance's own Subnet. Dnsmasq.SetReservations uses it to tell a
// Machine's ordinary Complete release (still enrolled, still on this
// Subnet - its reservation disappearing means the deploy finished, and
// the OS keeps its lease) apart from a spec.subnetRef change (no longer
// claimed locally - the address must be actively released instead of
// waiting out the lease). Always false before the cache has synced, or
// when it was built with no local Subnet identity (see NewMACCache).
func (c *MACCache) HasLocalMember(mac string) bool {
	if !c.synced.Load() {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.localCounts[mac] > 0
}
