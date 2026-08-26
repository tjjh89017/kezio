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
	"slices"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache/informertest"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

func machine(name, mac string) *keziov1alpha3.Machine {
	return &keziov1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       keziov1alpha3.MachineSpec{BootMACAddress: mac},
	}
}

// machineOnSubnet is machine, plus a spec.subnetRef naming subnetName -
// for the HasLocalMember tests, which need a Machine's Subnet identity,
// not just its MAC.
func machineOnSubnet(name, mac, subnetName string) *keziov1alpha3.Machine {
	m := machine(name, mac)
	m.Spec.SubnetRef = keziov1alpha3.NameRef{Name: subnetName}
	return m
}

func newTestMACCacheWithLocalSubnet(localName string) (*MACCache, *macSinkRecorder) {
	sink := &macSinkRecorder{}
	c := &MACCache{
		counts:      make(map[string]int),
		localCounts: make(map[string]int),
		records:     make(map[string]machineRecord),
		localName:   localName,
		sink:        sink,
	}
	c.synced.Store(true)
	return c, sink
}

// macSinkRecorder records every push it receives, so tests can assert
// both the final allowlist and that a push happened at all.
type macSinkRecorder struct {
	mu     sync.Mutex
	pushes [][]string
}

func (s *macSinkRecorder) SetAllowedMACs(macs []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pushes = append(s.pushes, slices.Clone(macs))
}

func (s *macSinkRecorder) last() ([]string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pushes) == 0 {
		return nil, false
	}
	return s.pushes[len(s.pushes)-1], true
}

func newTestMACCache() (*MACCache, *macSinkRecorder) {
	sink := &macSinkRecorder{}
	c := &MACCache{
		counts:      make(map[string]int),
		localCounts: make(map[string]int),
		records:     make(map[string]machineRecord),
		sink:        sink,
	}
	c.synced.Store(true)
	return c, sink
}

func wantLast(t *testing.T, sink *macSinkRecorder, want []string) {
	t.Helper()
	got, ok := sink.last()
	if !ok {
		t.Fatalf("no push received, want %v", want)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("last push = %v, want %v", got, want)
	}
}

func TestMACCache_NoPushUntilSynced(t *testing.T) {
	sink := &macSinkRecorder{}
	c := &MACCache{
		counts:      make(map[string]int),
		localCounts: make(map[string]int),
		records:     make(map[string]machineRecord),
		sink:        sink,
	}

	c.onAdd(machine("m1", "aa:bb:cc:dd:ee:01"))
	if _, ok := sink.last(); ok {
		t.Error("sink received a push before the cache reported synced")
	}
	if got := c.Snapshot(); got != nil {
		t.Errorf("Snapshot() = %v before sync, want nil", got)
	}
}

func TestMACCache_AddAndDelete(t *testing.T) {
	c, sink := newTestMACCache()
	m := machine("m1", "AA:BB:CC:DD:EE:01")

	c.onAdd(m)
	wantLast(t, sink, []string{"aa:bb:cc:dd:ee:01"})

	c.onDelete(m)
	wantLast(t, sink, []string{})
}

func TestMACCache_SharedMACSurvivesOneDelete(t *testing.T) {
	// Two Machines sharing a MAC is a misconfiguration bootserver's own
	// lookupMachine also refuses to arbitrate between - but the cache
	// must not let deleting one revoke access for the other still-live
	// Machine (see add/remove's doc comment on refcounting).
	c, sink := newTestMACCache()
	m1 := machine("m1", "aa:bb:cc:dd:ee:01")
	m2 := machine("m2", "aa:bb:cc:dd:ee:01")

	c.onAdd(m1)
	c.onAdd(m2)
	c.onDelete(m1)
	wantLast(t, sink, []string{"aa:bb:cc:dd:ee:01"})
	c.onDelete(m2)
	wantLast(t, sink, []string{})
}

func TestMACCache_UpdateMovesMAC(t *testing.T) {
	c, sink := newTestMACCache()
	old := machine("m1", "aa:bb:cc:dd:ee:01")
	updated := machine("m1", "aa:bb:cc:dd:ee:02")

	c.onAdd(old)
	c.onUpdate(old, updated)
	wantLast(t, sink, []string{"aa:bb:cc:dd:ee:02"})
}

// TestMACCache_CaseInsensitiveMAC: bootMACAddress is validated
// case-insensitively, but dnsmasq's hostsfile wants one canonical
// spelling, so every case variant must land in the pushed allowlist as
// the same lower-case MAC (via NormalizeMAC).
func TestMACCache_CaseInsensitiveMAC(t *testing.T) {
	tests := []struct {
		name    string
		specMAC string
	}{
		{"lowercase spec", "aa:bb:cc:dd:ee:01"},
		{"uppercase spec", "AA:BB:CC:DD:EE:01"},
		{"mixed-case spec", "Aa:bB:Cc:dD:eE:01"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, sink := newTestMACCache()
			c.onAdd(machine("m1", tc.specMAC))
			wantLast(t, sink, []string{"aa:bb:cc:dd:ee:01"})
		})
	}
}

func TestMACCache_DeleteTombstoneUnwrapped(t *testing.T) {
	c, sink := newTestMACCache()
	m := machine("m1", "aa:bb:cc:dd:ee:01")
	c.onAdd(m)

	c.onDelete(toolscache.DeletedFinalStateUnknown{Key: "default/m1", Obj: m})
	wantLast(t, sink, []string{})
}

func TestMACCache_MalformedMACIgnored(t *testing.T) {
	c, _ := newTestMACCache()
	c.onAdd(machine("m1", "not-a-mac"))
	if len(c.counts) != 0 {
		t.Errorf("counts = %v, want empty (malformed MAC must not be indexed)", c.counts)
	}
}

// TestMACCache_StartGatesOnSync: nothing reaches the sink until
// WaitForCacheSync succeeds, and the first post-sync push carries a
// Machine added through the informer before Start observed sync.
func TestMACCache_StartGatesOnSync(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := keziov1alpha3.AddToScheme(scheme); err != nil {
		t.Fatalf("building scheme: %v", err)
	}
	fakeInformers := &informertest.FakeInformers{Scheme: scheme}
	sink := &macSinkRecorder{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c, err := NewMACCache(ctx, fakeInformers, sink, "", "")
	if err != nil {
		t.Fatalf("NewMACCache: %v", err)
	}

	fakeInformer, err := fakeInformers.FakeInformerFor(ctx, &keziov1alpha3.Machine{})
	if err != nil {
		t.Fatalf("FakeInformerFor: %v", err)
	}
	fakeInformer.Add(machine("m1", "aa:bb:cc:dd:ee:01"))

	if _, ok := sink.last(); ok {
		t.Fatal("sink received a push before Start/WaitForCacheSync ran")
	}

	fakeInformer.Synced = true
	startErrCh := make(chan error, 1)
	go func() { startErrCh <- c.Start(ctx) }()

	deadline := time.After(2 * time.Second)
	for !c.synced.Load() {
		select {
		case <-deadline:
			t.Fatal("MACCache never became synced")
		case <-time.After(time.Millisecond):
		}
	}

	wantLast(t, sink, []string{"aa:bb:cc:dd:ee:01"})

	cancel()
	if err := <-startErrCh; err != nil {
		t.Errorf("Start returned an error after ctx cancellation: %v", err)
	}
}

// TestMACCache_HasLocalMemberTracksSubnetRef proves HasLocalMember
// follows a Machine's spec.subnetRef, including a subnetRef-only change
// that leaves the MAC itself untouched - the signal Dnsmasq needs to
// tell a spec.subnetRef change apart from an ordinary Complete release.
func TestMACCache_HasLocalMemberTracksSubnetRef(t *testing.T) {
	c, _ := newTestMACCacheWithLocalSubnet("subnet-a")
	mac := "aa:bb:cc:dd:ee:01"

	onA := machineOnSubnet("m1", mac, "subnet-a")
	c.onAdd(onA)
	if !c.HasLocalMember(mac) {
		t.Fatal("HasLocalMember false for a Machine on the local Subnet")
	}

	onB := machineOnSubnet("m1", mac, "subnet-b")
	c.onUpdate(onA, onB)
	if c.HasLocalMember(mac) {
		t.Error("HasLocalMember true after the Machine's subnetRef moved away from the local Subnet")
	}
	// The MAC is still enrolled (moved, not deleted): SetAllowedMACs'
	// own allowlist must still carry it.
	if !slices.Contains(c.Snapshot(), mac) {
		t.Error("Snapshot dropped the MAC after a subnetRef-only change")
	}

	c.onUpdate(onB, onA)
	if !c.HasLocalMember(mac) {
		t.Error("HasLocalMember false after the Machine's subnetRef moved back to the local Subnet")
	}
}

// TestMACCache_HasLocalMemberFalseWithNoLocalSubnet proves HasLocalMember
// always reports false when the cache was built with no local Subnet
// identity (BOOTD_SUBNET_NAME unset) - there is nothing to compare a
// subnetRef against.
func TestMACCache_HasLocalMemberFalseWithNoLocalSubnet(t *testing.T) {
	c, _ := newTestMACCache()
	mac := "aa:bb:cc:dd:ee:01"
	c.onAdd(machineOnSubnet("m1", mac, "subnet-a"))
	if c.HasLocalMember(mac) {
		t.Error("HasLocalMember true with no local Subnet identity configured")
	}
}

// TestMACCache_HasLocalMemberFalseBeforeSync mirrors Snapshot's own
// fail-secure gate: no answer is trustworthy before the informer's
// initial sync completes.
func TestMACCache_HasLocalMemberFalseBeforeSync(t *testing.T) {
	c := &MACCache{
		counts:      make(map[string]int),
		localCounts: make(map[string]int),
		records:     make(map[string]machineRecord),
		localName:   "subnet-a",
	}
	if c.HasLocalMember("aa:bb:cc:dd:ee:01") {
		t.Error("HasLocalMember true before the cache reported synced")
	}
}
