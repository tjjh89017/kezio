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
	c := &MACCache{counts: make(map[string]int), sink: sink}
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
	c := &MACCache{counts: make(map[string]int), sink: sink}

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

	c, err := NewMACCache(ctx, fakeInformers, sink)
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
