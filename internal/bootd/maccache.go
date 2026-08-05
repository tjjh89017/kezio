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
// normalized boot MAC address, fed by a controller-runtime cache
// informer rather than a List/Get call per event - bootd runs
// per-site, outside the manager process, and its MAC allowlist must
// track Machine churn without putting per-boot load on the API server.
// Every change is pushed to the configured MACSink, which is how the
// dnsmasq MAC gate (the dhcp-hostsfile) stays current.
//
// Fail-secure by construction: nothing is ever pushed to the sink -
// which therefore keeps its initial, empty allowlist, booting nothing -
// until the informer's first cache sync completes, and permanently if
// that sync never completes (see Start). A machine that net-boots one
// cycle late because the cache was not ready yet is an acceptable
// cost; a machine or intruder net-booted because an unreachable API
// server was silently treated as "nothing enrolled, so allow" would
// not be.
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

// Start implements manager.Runnable: it waits for the Machine
// informer's initial sync, then marks the cache synced, pushes the
// first complete snapshot to the sink, and blocks until ctx is
// cancelled. If the sync never completes (API server unreachable at
// startup, context cancelled first), no snapshot is ever pushed - the
// sink keeps its empty allowlist and nothing boots - rather than
// guessing that an empty, never-synced cache means "nothing is
// enrolled yet, so an empty push is accurate anyway" (a cache that
// reports zero Machines because it failed to connect looks identical
// to one that reports zero Machines because there genuinely are none,
// and only the fail-secure default treats both cases the same, safe
// way).
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

// pushLocked pushes the current snapshot to the sink if the cache is
// synced; callers hold c.mu. Holding the lock across the sink call
// keeps pushes ordered - two racing informer events cannot deliver
// their snapshots to the sink in reversed order and leave the
// hostsfile stale. The sink's SetAllowedMACs is non-blocking (see
// Dnsmasq.SetAllowedMACs), so the lock is never held for long.
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

// add and remove maintain counts keyed by normalized MAC, rather than a
// plain set, because bootMACAddress uniqueness is only a convention
// (see internal/bootserver.Server.lookupMachine's doc comment) - two
// Machines can transiently or permanently share a MAC in a
// misconfigured cluster, and a plain set would let deleting one of them
// incorrectly revoke the gate for the other still-enrolled Machine.
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
