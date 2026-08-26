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
	"sync"
	"sync/atomic"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	toolscache "k8s.io/client-go/tools/cache"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=subnets,verbs=get;list;watch
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=subnets/status,verbs=get;patch

// ReservationSink receives this bootd instance's own Subnet's lease-mode
// DHCP address reservation table every time it changes, keyed by
// normalized MAC, together with the Subnet status revision it was
// computed from. Dnsmasq implements it: SetReservations feeds
// RenderHostsfile alongside SetAllowedMACs, and once the rewrite+SIGHUP
// for that revision lands, SubnetDHCPCache.MarkApplied is called back
// with it (see Dnsmasq.OnApplied).
type ReservationSink interface {
	SetReservations(revision string, addresses map[string]string)
}

// SubnetDHCPCache watches this bootd instance's own Subnet object
// (SubnetName/SubnetNamespace) and pushes its status.dhcp.reservations
// (lease mode only; always empty in proxy mode, since proxyDHCP never
// assigns addresses) to a ReservationSink. Once that sink confirms a
// revision actually reached dnsmasq's hostsfile (MarkApplied), it writes
// status.dhcp.appliedRevision back onto the Subnet, so a Machine's
// deployer can wait for its reservation to actually be live before it
// powers the machine on for net boot.
//
// Fail-secure mirrors MACCache: nothing is pushed to the sink - and no
// reservation is ever treated as applied - until the informer's first
// cache sync completes.
type SubnetDHCPCache struct {
	SubnetName      string
	SubnetNamespace string
	// Client reads the Subnet and patches status.dhcp.appliedRevision.
	// Required.
	Client client.Client

	informers ctrlcache.Informers
	sink      ReservationSink

	synced atomic.Bool
	mu     sync.Mutex
	// revision is the last revision pushed to sink - MarkApplied only
	// acts when its argument still matches this, so a callback for a
	// since-superseded revision can never move appliedRevision back past
	// a newer one already reported.
	revision string
}

var _ manager.Runnable = (*SubnetDHCPCache)(nil)

// NewSubnetDHCPCache builds a SubnetDHCPCache pushing into sink and
// registers its event handler on informers' Subnet informer, filtered to
// exactly subnetNamespace/subnetName. It does not block on the initial
// sync - call mgr.Add on the returned cache (it implements
// manager.Runnable) to have that wait happen as part of the manager's
// own startup.
func NewSubnetDHCPCache(ctx context.Context, informers ctrlcache.Informers, c client.Client, subnetNamespace, subnetName string, sink ReservationSink) (*SubnetDHCPCache, error) {
	sc := &SubnetDHCPCache{
		SubnetName:      subnetName,
		SubnetNamespace: subnetNamespace,
		Client:          c,
		informers:       informers,
		sink:            sink,
	}

	informer, err := informers.GetInformer(ctx, &keziov1alpha3.Subnet{})
	if err != nil {
		return nil, fmt.Errorf("getting Subnet informer: %w", err)
	}
	if _, err := informer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc:    sc.onEvent,
		UpdateFunc: func(_, newObj any) { sc.onEvent(newObj) },
		DeleteFunc: sc.onEvent,
	}); err != nil {
		return nil, fmt.Errorf("registering Subnet event handler: %w", err)
	}
	return sc, nil
}

// Start implements manager.Runnable: waits for the Subnet informer's
// initial sync, pushes the first snapshot (a direct Get, since the
// informer's own add event for a single already-existing object may have
// already been delivered and dropped before synced flips true), then
// blocks until ctx is cancelled.
func (sc *SubnetDHCPCache) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("bootd-subnet-dhcp")
	if !sc.informers.WaitForCacheSync(ctx) {
		log.Error(fmt.Errorf("cache sync did not complete"), "Subnet DHCP cache never became ready; no reservation is ever applied until restart")
		<-ctx.Done()
		return nil
	}
	sc.synced.Store(true)

	var subnet keziov1alpha3.Subnet
	key := client.ObjectKey{Namespace: sc.SubnetNamespace, Name: sc.SubnetName}
	switch err := sc.Client.Get(ctx, key, &subnet); {
	case err == nil:
		sc.push(&subnet)
	case apierrors.IsNotFound(err):
		// Nothing to push yet; the informer's own add event will arrive
		// once the Subnet exists.
	default:
		log.Error(err, "getting Subnet for initial DHCP reservation snapshot")
	}

	log.Info("Subnet DHCP cache ready")
	<-ctx.Done()
	return nil
}

func (sc *SubnetDHCPCache) onEvent(obj any) {
	subnet, ok := obj.(*keziov1alpha3.Subnet)
	if !ok {
		tombstone, ok := obj.(toolscache.DeletedFinalStateUnknown)
		if !ok {
			return
		}
		subnet, ok = tombstone.Obj.(*keziov1alpha3.Subnet)
		if !ok {
			return
		}
	}
	if subnet.Namespace != sc.SubnetNamespace || subnet.Name != sc.SubnetName {
		return
	}
	if !sc.synced.Load() {
		return
	}
	sc.push(subnet)
}

// push computes the current reservation map from subnet's status and
// pushes it to sc.sink, recording the revision it pushed so MarkApplied
// can tell a stale callback apart from the current one.
func (sc *SubnetDHCPCache) push(subnet *keziov1alpha3.Subnet) {
	addresses := map[string]string{}
	var revision string
	if subnet.Spec.DHCP != nil && subnet.Spec.DHCP.Mode == keziov1alpha3.SubnetDHCPModeLease && subnet.Status.DHCP != nil {
		revision = subnet.Status.DHCP.Revision
		for _, r := range subnet.Status.DHCP.Reservations {
			mac, ok := NormalizeMAC(r.MAC)
			if !ok {
				continue
			}
			addresses[mac] = r.Address
		}
	}

	sc.mu.Lock()
	sc.revision = revision
	sc.mu.Unlock()

	if sc.sink != nil {
		sc.sink.SetReservations(revision, addresses)
	}
}

// MarkApplied patches status.dhcp.appliedRevision to revision on this
// cache's own Subnet - the callback Dnsmasq.OnApplied invokes once its
// hostsfile actually reflects revision. A stale revision (superseded by
// a newer push before this callback ran) is silently dropped rather than
// moving appliedRevision backward or sideways.
func (sc *SubnetDHCPCache) MarkApplied(ctx context.Context, revision string) {
	log := logf.FromContext(ctx).WithName("bootd-subnet-dhcp")

	sc.mu.Lock()
	current := sc.revision
	sc.mu.Unlock()
	if revision != current {
		return
	}

	var subnet keziov1alpha3.Subnet
	key := client.ObjectKey{Namespace: sc.SubnetNamespace, Name: sc.SubnetName}
	if err := sc.Client.Get(ctx, key, &subnet); err != nil {
		if !apierrors.IsNotFound(err) {
			log.Error(err, "getting Subnet to record applied DHCP revision")
		}
		return
	}
	if subnet.Status.DHCP == nil {
		subnet.Status.DHCP = &keziov1alpha3.SubnetDHCPStatus{}
	}
	if subnet.Status.DHCP.AppliedRevision == revision {
		return
	}
	patch := client.MergeFrom(subnet.DeepCopy())
	subnet.Status.DHCP.AppliedRevision = revision
	if err := sc.Client.Status().Patch(ctx, &subnet, patch); err != nil {
		log.Error(err, "recording applied DHCP revision on Subnet")
	}
}
