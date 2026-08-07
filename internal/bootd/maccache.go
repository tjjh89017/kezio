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
	"slices"
	"sync"
	"sync/atomic"

	toolscache "k8s.io/client-go/tools/cache"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/bootserver"
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

	synced    atomic.Bool
	informers ctrlcache.Informers
	sink      MACSink
}

var _ manager.Runnable = (*MACCache)(nil)

// NewMACCache builds a MACCache pushing into sink and registers its
// event handler on informers' Machine informer. It does not block on
// the initial sync - call mgr.Add on the returned MACCache (it
// implements manager.Runnable) to have that wait happen as part of the
// manager's own startup, or call Start directly in a standalone
// binary.
func NewMACCache(ctx context.Context, informers ctrlcache.Informers, sink MACSink) (*MACCache, error) {
	c := &MACCache{
		counts:    make(map[string]int),
		informers: informers,
		sink:      sink,
	}

	informer, err := informers.GetInformer(ctx, &keziov1alpha1.Machine{})
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
	machine, ok := obj.(*keziov1alpha1.Machine)
	if !ok {
		return
	}
	c.add(machine.Spec.BootMACAddress)
}

func (c *MACCache) onUpdate(oldObj, newObj any) {
	oldMachine, ok := oldObj.(*keziov1alpha1.Machine)
	if !ok {
		return
	}
	newMachine, ok := newObj.(*keziov1alpha1.Machine)
	if !ok {
		return
	}
	if oldMachine.Spec.BootMACAddress == newMachine.Spec.BootMACAddress {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.removeLocked(oldMachine.Spec.BootMACAddress)
	c.addLocked(newMachine.Spec.BootMACAddress)
	c.pushLocked()
}

func (c *MACCache) onDelete(obj any) {
	machine, ok := obj.(*keziov1alpha1.Machine)
	if !ok {
		// A watch that misses a delete event can hand back a
		// DeletedFinalStateUnknown wrapping the last known object
		// instead of the object itself; unwrap it rather than
		// silently leaking that Machine's MAC in the count forever.
		tombstone, ok := obj.(toolscache.DeletedFinalStateUnknown)
		if !ok {
			return
		}
		machine, ok = tombstone.Obj.(*keziov1alpha1.Machine)
		if !ok {
			return
		}
	}
	c.remove(machine.Spec.BootMACAddress)
}

// add and remove maintain counts keyed by normalized MAC, not a plain
// set, because bootMACAddress uniqueness is only a convention (see
// internal/bootserver.Server.lookupMachine) - a plain set would let
// deleting one of two Machines sharing a MAC revoke the gate for the
// other still-enrolled one.
func (c *MACCache) add(rawMAC string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.addLocked(rawMAC)
	c.pushLocked()
}

func (c *MACCache) addLocked(rawMAC string) {
	mac, ok := bootserver.NormalizeMAC(rawMAC)
	if !ok {
		return
	}
	c.counts[mac]++
}

func (c *MACCache) remove(rawMAC string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.removeLocked(rawMAC)
	c.pushLocked()
}

func (c *MACCache) removeLocked(rawMAC string) {
	mac, ok := bootserver.NormalizeMAC(rawMAC)
	if !ok {
		return
	}
	if c.counts[mac] <= 1 {
		delete(c.counts, mac)
		return
	}
	c.counts[mac]--
}
