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
	"net"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	toolscache "k8s.io/client-go/tools/cache"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/cache/informertest"
)

func machine(name, mac string) *keziov1alpha1.Machine {
	return &keziov1alpha1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       keziov1alpha1.MachineSpec{BootMACAddress: mac},
	}
}

func newTestMACCache() *MACCache {
	c := &MACCache{counts: make(map[string]int)}
	c.synced.Store(true)
	return c
}

func TestMACCache_DeniesUntilSynced(t *testing.T) {
	c := &MACCache{counts: make(map[string]int)}
	c.add("aa:bb:cc:dd:ee:01")
	if c.Allow(knownMAC) {
		t.Error("Allow returned true before the cache reported synced")
	}
	c.synced.Store(true)
	if !c.Allow(knownMAC) {
		t.Error("Allow returned false for an enrolled MAC once synced")
	}
}

func TestMACCache_AddAndDelete(t *testing.T) {
	c := newTestMACCache()
	m := machine("m1", "AA:BB:CC:DD:EE:01")

	c.onAdd(m)
	if !c.Allow(knownMAC) {
		t.Fatal("Allow false after onAdd for an enrolled MAC")
	}
	if c.Allow(unknownMAC) {
		t.Fatal("Allow true for a MAC that was never added")
	}

	c.onDelete(m)
	if c.Allow(knownMAC) {
		t.Fatal("Allow true after onDelete for the only Machine claiming this MAC")
	}
}

func TestMACCache_SharedMACSurvivesOneDelete(t *testing.T) {
	// Two Machines sharing a MAC is a misconfiguration bootserver's own
	// lookupMachine also refuses to arbitrate between - but the cache
	// must not let deleting one revoke access for the other still-live
	// Machine (see add/remove's doc comment on refcounting).
	c := newTestMACCache()
	m1 := machine("m1", "aa:bb:cc:dd:ee:01")
	m2 := machine("m2", "aa:bb:cc:dd:ee:01")

	c.onAdd(m1)
	c.onAdd(m2)
	c.onDelete(m1)
	if !c.Allow(knownMAC) {
		t.Fatal("Allow false after deleting only one of two Machines sharing a MAC")
	}
	c.onDelete(m2)
	if c.Allow(knownMAC) {
		t.Fatal("Allow true after deleting every Machine claiming this MAC")
	}
}

func TestMACCache_UpdateMovesMAC(t *testing.T) {
	c := newTestMACCache()
	old := machine("m1", "aa:bb:cc:dd:ee:01")
	updated := machine("m1", "aa:bb:cc:dd:ee:02")

	c.onAdd(old)
	c.onUpdate(old, updated)

	if c.Allow(knownMAC) {
		t.Error("Allow true for the old MAC after the Machine's bootMACAddress changed")
	}
	if !c.Allow(net.HardwareAddr{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0x02}) {
		t.Error("Allow false for the new MAC after the Machine's bootMACAddress changed")
	}
}

// TestMACCache_CaseInsensitiveMAC is a table-driven check of the case
// variant this cache must not be fooled by: Machine.spec.bootMACAddress
// is validated case-insensitively (see keziov1alpha1.MACAddressPattern)
// so an operator or seeder can write it in either case, while the
// value looked up on the wire always comes from net.HardwareAddr.String
// (see Allow), which is always lower-case. add/remove and Allow all
// normalize through bootserver.NormalizeMAC, so every case combination
// below must resolve to the same enrolled MAC.
func TestMACCache_CaseInsensitiveMAC(t *testing.T) {
	lookup := net.HardwareAddr{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0x01}

	tests := []struct {
		name          string
		specMAC       string
		wantAllowedOn net.HardwareAddr
	}{
		{"lowercase spec, lowercase lookup", "aa:bb:cc:dd:ee:01", lookup},
		{"uppercase spec, lowercase lookup", "AA:BB:CC:DD:EE:01", lookup},
		{"mixed-case spec, lowercase lookup", "Aa:bB:Cc:dD:eE:01", lookup},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestMACCache()
			c.onAdd(machine("m1", tc.specMAC))
			if !c.Allow(tc.wantAllowedOn) {
				t.Errorf("Allow(%v) = false after enrolling bootMACAddress %q, want true", tc.wantAllowedOn, tc.specMAC)
			}
		})
	}
}

func TestMACCache_DeleteTombstoneUnwrapped(t *testing.T) {
	c := newTestMACCache()
	m := machine("m1", "aa:bb:cc:dd:ee:01")
	c.onAdd(m)

	c.onDelete(toolscache.DeletedFinalStateUnknown{Key: "default/m1", Obj: m})
	if c.Allow(knownMAC) {
		t.Error("Allow true after a DeletedFinalStateUnknown delete event")
	}
}

func TestMACCache_MalformedMACIgnored(t *testing.T) {
	c := newTestMACCache()
	c.onAdd(machine("m1", "not-a-mac"))
	// Nothing to assert beyond "did not panic and added nothing"; a
	// non-matching MAC lookup below confirms the map stayed empty.
	if len(c.counts) != 0 {
		t.Errorf("counts = %v, want empty (malformed MAC must not be indexed)", c.counts)
	}
}

// TestMACCache_StartGatesOnSync exercises NewMACCache/Start end to end
// against a fake Informers implementation: Allow must stay false until
// WaitForCacheSync succeeds, and an Add event delivered through the
// informer (not through the unexported onAdd helper directly) must
// still reach Allow.
func TestMACCache_StartGatesOnSync(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := keziov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("building scheme: %v", err)
	}
	fakeInformers := &informertest.FakeInformers{Scheme: scheme}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c, err := NewMACCache(ctx, fakeInformers)
	if err != nil {
		t.Fatalf("NewMACCache: %v", err)
	}

	fakeInformer, err := fakeInformers.FakeInformerFor(ctx, &keziov1alpha1.Machine{})
	if err != nil {
		t.Fatalf("FakeInformerFor: %v", err)
	}
	fakeInformer.Add(machine("m1", "aa:bb:cc:dd:ee:01"))

	if c.Allow(knownMAC) {
		t.Fatal("Allow true before Start/WaitForCacheSync ran")
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

	if !c.Allow(knownMAC) {
		t.Error("Allow false for a MAC added through the informer before Start observed sync")
	}

	cancel()
	if err := <-startErrCh; err != nil {
		t.Errorf("Start returned an error after ctx cancellation: %v", err)
	}
}
