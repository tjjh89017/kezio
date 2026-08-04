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

// MACCache is the Kubernetes-backed MACGate: it keeps a local, live-
// updated set of every enrolled Machine's normalized boot MAC address,
// fed by a controller-runtime cache informer rather than a List/Get
// call per incoming packet - bootd runs per-site, outside the manager
// process, and a proxyDHCP responder that hit the API server once per
// DHCPDISCOVER broadcast would both be slow on the hot path and put
// unwanted load on the API server for traffic that, by definition,
// includes every foreign device on the segment, not just kezio's own.
//
// Fail-secure by construction: Allow reports false - not "answer
// unknown" - for every MAC until the informer's first cache sync
// completes, and permanently if that sync never completes (see Start).
// A machine that net-boots one cycle late because the cache was not
// ready yet is an acceptable cost; a machine or intruder net-booted
// because an unreachable API server was silently treated as "nothing
// enrolled, so allow" would not be.
type MACCache struct {
	mu     sync.RWMutex
	counts map[string]int // normalized MAC -> number of Machines currently claiming it

	synced    atomic.Bool
	informers ctrlcache.Informers
}

var _ MACGate = (*MACCache)(nil)
var _ manager.Runnable = (*MACCache)(nil)

// NewMACCache builds a MACCache and registers its event handler on
// informers' Machine informer. It does not block on the initial sync -
// call mgr.Add on the returned MACCache (it implements manager.Runnable)
// to have that wait happen as part of the manager's own startup, or
// call Start directly in a standalone binary.
func NewMACCache(ctx context.Context, informers ctrlcache.Informers) (*MACCache, error) {
	c := &MACCache{
		counts:    make(map[string]int),
		informers: informers,
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

// Start implements manager.Runnable: it waits for the Machine informer's
// initial sync, then marks the cache ready and blocks until ctx is
// cancelled. If the sync never completes (API server unreachable at
// startup, context cancelled first), the cache is left permanently
// unsynced - Allow keeps returning false - rather than guessing that an
// empty, never-synced cache means "nothing is enrolled yet, so deny is
// safe anyway" (a cache that reports zero Machines because it failed to
// connect looks identical to one that reports zero Machines because
// there genuinely are none, and only the fail-secure default treats
// both cases the same, safe way).
func (c *MACCache) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("bootd-maccache")
	if !c.informers.WaitForCacheSync(ctx) {
		log.Error(fmt.Errorf("cache sync did not complete"), "Machine MAC cache never became ready; denying all boot requests until restart")
		<-ctx.Done()
		return nil
	}
	c.synced.Store(true)
	log.Info("Machine MAC cache ready")
	<-ctx.Done()
	return nil
}

// Allow implements MACGate.
func (c *MACCache) Allow(mac net.HardwareAddr) bool {
	if !c.synced.Load() {
		return false
	}
	normalized, ok := bootserver.NormalizeMAC(mac.String())
	if !ok {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.counts[normalized] > 0
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
	c.remove(oldMachine.Spec.BootMACAddress)
	c.add(newMachine.Spec.BootMACAddress)
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
	mac, ok := bootserver.NormalizeMAC(rawMAC)
	if !ok {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts[mac]++
}

func (c *MACCache) remove(rawMAC string) {
	mac, ok := bootserver.NormalizeMAC(rawMAC)
	if !ok {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.counts[mac] <= 1 {
		delete(c.counts, mac)
		return
	}
	c.counts[mac]--
}
