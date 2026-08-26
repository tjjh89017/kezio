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
	"maps"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache/informertest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

// reservationSinkRecorder records every SetReservations push, so tests
// can assert both the final map/revision and that a push happened at
// all.
type reservationSinkRecorder struct {
	mu        sync.Mutex
	revisions []string
	addresses []map[string]string
}

func (s *reservationSinkRecorder) SetReservations(revision string, addresses map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revisions = append(s.revisions, revision)
	s.addresses = append(s.addresses, maps.Clone(addresses))
}

func (s *reservationSinkRecorder) last() (revision string, addresses map[string]string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.revisions) == 0 {
		return "", nil, false
	}
	n := len(s.revisions) - 1
	return s.revisions[n], s.addresses[n], true
}

// testDHCPSubnetNamespace is the namespace every testDHCPSubnet fixture
// uses; every test in this file only ever needs one namespace, so it is
// not a parameter.
const testDHCPSubnetNamespace = "site-hq"

func testDHCPSubnet(name string, mutate func(*keziov1alpha3.Subnet)) *keziov1alpha3.Subnet {
	s := &keziov1alpha3.Subnet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testDHCPSubnetNamespace},
		Spec: keziov1alpha3.SubnetSpec{
			CIDR: "192.0.2.0/24",
			DHCP: &keziov1alpha3.SubnetDHCP{Mode: keziov1alpha3.SubnetDHCPModeLease},
		},
	}
	if mutate != nil {
		mutate(s)
	}
	return s
}

func newSubnetDHCPTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := keziov1alpha3.AddToScheme(scheme); err != nil {
		t.Fatalf("building scheme: %v", err)
	}
	return scheme
}

func startSubnetDHCPCache(t *testing.T, subnet *keziov1alpha3.Subnet) (*SubnetDHCPCache, *reservationSinkRecorder, client.Client) {
	t.Helper()
	scheme := newSubnetDHCPTestScheme(t)
	fakeInformers := &informertest.FakeInformers{Scheme: scheme}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&keziov1alpha3.Subnet{}).
		WithObjects(subnet).
		Build()
	sink := &reservationSinkRecorder{}

	ctx, cancel := context.WithCancel(context.Background())
	sc, err := NewSubnetDHCPCache(ctx, fakeInformers, c, subnet.Namespace, subnet.Name, sink)
	if err != nil {
		cancel()
		t.Fatalf("NewSubnetDHCPCache: %v", err)
	}

	fakeInformer, err := fakeInformers.FakeInformerFor(ctx, &keziov1alpha3.Subnet{})
	if err != nil {
		cancel()
		t.Fatalf("FakeInformerFor: %v", err)
	}
	fakeInformer.Synced = true

	startErrCh := make(chan error, 1)
	go func() { startErrCh <- sc.Start(ctx) }()

	deadline := time.After(2 * time.Second)
	for !sc.synced.Load() {
		select {
		case <-deadline:
			t.Fatal("SubnetDHCPCache never became synced")
		case <-time.After(time.Millisecond):
		}
	}

	t.Cleanup(func() {
		cancel()
		if err := <-startErrCh; err != nil {
			t.Errorf("Start returned an error after ctx cancellation: %v", err)
		}
	})
	return sc, sink, c
}

func TestSubnetDHCPCache_PushesInitialReservationsAfterSync(t *testing.T) {
	subnet := testDHCPSubnet("rack-1", func(s *keziov1alpha3.Subnet) {
		s.Status.DHCP = &keziov1alpha3.SubnetDHCPStatus{
			Revision: "rev-1",
			Reservations: []keziov1alpha3.DHCPReservation{
				{Machine: "m1", MAC: "AA:BB:CC:DD:EE:01", Address: "192.0.2.10"},
			},
		}
	})
	_, sink, _ := startSubnetDHCPCache(t, subnet)

	revision, addresses, ok := sink.last()
	if !ok {
		t.Fatal("no push received after sync")
	}
	if revision != "rev-1" {
		t.Errorf("revision = %q, want %q", revision, "rev-1")
	}
	want := map[string]string{"aa:bb:cc:dd:ee:01": "192.0.2.10"}
	if !maps.Equal(addresses, want) {
		t.Errorf("addresses = %v, want %v (MAC normalized to lower case)", addresses, want)
	}
}

func TestSubnetDHCPCache_ProxyModeNeverEmitsAddresses(t *testing.T) {
	subnet := testDHCPSubnet("rack-1", func(s *keziov1alpha3.Subnet) {
		s.Spec.DHCP.Mode = keziov1alpha3.SubnetDHCPModeProxy
		// A reservation present despite proxy mode should never happen in
		// practice (the deployer only ever writes one in lease mode), but
		// the cache must not emit it even if it somehow is there.
		s.Status.DHCP = &keziov1alpha3.SubnetDHCPStatus{
			Revision:     "rev-1",
			Reservations: []keziov1alpha3.DHCPReservation{{Machine: "m1", MAC: "aa:bb:cc:dd:ee:01", Address: "192.0.2.10"}},
		}
	})
	_, sink, _ := startSubnetDHCPCache(t, subnet)

	revision, addresses, ok := sink.last()
	if !ok {
		t.Fatal("no push received after sync")
	}
	if revision != "" {
		t.Errorf("revision = %q, want empty in proxy mode", revision)
	}
	if len(addresses) != 0 {
		t.Errorf("addresses = %v, want empty in proxy mode", addresses)
	}
}

func TestSubnetDHCPCache_IgnoresOtherSubnets(t *testing.T) {
	subnet := testDHCPSubnet("rack-1", nil)
	sc, sink, _ := startSubnetDHCPCache(t, subnet)

	other := testDHCPSubnet("rack-2", func(s *keziov1alpha3.Subnet) {
		s.Status.DHCP = &keziov1alpha3.SubnetDHCPStatus{Revision: "should-be-ignored"}
	})
	before, _, _ := sink.last()
	sc.onEvent(other)
	after, _, _ := sink.last()
	if after != before {
		t.Errorf("a push happened for an unrelated Subnet: before=%q after=%q", before, after)
	}
}

func TestSubnetDHCPCache_MarkAppliedPatchesStatus(t *testing.T) {
	subnet := testDHCPSubnet("rack-1", func(s *keziov1alpha3.Subnet) {
		s.Status.DHCP = &keziov1alpha3.SubnetDHCPStatus{
			Revision:     "rev-1",
			Reservations: []keziov1alpha3.DHCPReservation{{Machine: "m1", MAC: "aa:bb:cc:dd:ee:01", Address: "192.0.2.10"}},
		}
	})
	sc, _, c := startSubnetDHCPCache(t, subnet)

	sc.MarkApplied(context.Background(), "rev-1")

	var got keziov1alpha3.Subnet
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(subnet), &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.DHCP.AppliedRevision != "rev-1" {
		t.Errorf("AppliedRevision = %q, want %q", got.Status.DHCP.AppliedRevision, "rev-1")
	}
}

func TestSubnetDHCPCache_MarkAppliedDropsStaleRevision(t *testing.T) {
	subnet := testDHCPSubnet("rack-1", func(s *keziov1alpha3.Subnet) {
		s.Status.DHCP = &keziov1alpha3.SubnetDHCPStatus{
			Revision:     "rev-2",
			Reservations: []keziov1alpha3.DHCPReservation{{Machine: "m1", MAC: "aa:bb:cc:dd:ee:01", Address: "192.0.2.10"}},
		}
	})
	sc, _, c := startSubnetDHCPCache(t, subnet)

	// sc.revision is rev-2 (the initial push); a callback naming the
	// stale rev-1 must not touch appliedRevision at all.
	sc.MarkApplied(context.Background(), "rev-1")

	var got keziov1alpha3.Subnet
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(subnet), &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.DHCP.AppliedRevision != "" {
		t.Errorf("AppliedRevision = %q, want unset (stale revision must be dropped)", got.Status.DHCP.AppliedRevision)
	}
}
