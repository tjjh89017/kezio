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

package sitederive

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apimachineryruntime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

func newTestClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := apimachineryruntime.NewScheme()
	if err := keziov1alpha2.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func newMachine(namespace string, subnetRef keziov1alpha2.NameRef) *keziov1alpha2.Machine {
	return &keziov1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: namespace},
		Spec: keziov1alpha2.MachineSpec{
			BootMACAddress: "aa:bb:cc:dd:ee:01",
			SubnetRef:      subnetRef,
		},
	}
}

// newSubnet always names the Subnet "sub1"; namespace and seederNetworkRef
// are the fields callers vary.
func newSubnet(namespace string, seederNetworkRef *keziov1alpha2.NameRef) *keziov1alpha2.Subnet {
	return &keziov1alpha2.Subnet{
		ObjectMeta: metav1.ObjectMeta{Name: "sub1", Namespace: namespace},
		Spec: keziov1alpha2.SubnetSpec{
			SiteRef:          keziov1alpha2.NameRef{Name: "hq"},
			CIDR:             "192.0.2.0/24",
			BootdServerIP:    "192.0.2.2",
			BootdNetworkRef:  &keziov1alpha2.NameRef{Name: "bootd-nad"},
			SeederNetworkRef: seederNetworkRef,
			NodeSelector:     map[string]string{"kubernetes.io/hostname": "node-1"},
			DHCP:             &keziov1alpha2.SubnetDHCP{Mode: keziov1alpha2.SubnetDHCPModeProxy},
		},
	}
}

func TestResolveHappyPath(t *testing.T) {
	subnetRef := keziov1alpha2.NameRef{Name: "sub1"}
	seederRef := &keziov1alpha2.NameRef{Name: "seeder-nad"}
	machine := newMachine("ns1", subnetRef)
	subnet := newSubnet("ns1", seederRef)

	c := newTestClient(t, machine, subnet)

	got, err := Resolve(context.Background(), c, machine)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	if got.Identity != "ns1/sub1" {
		t.Errorf("Identity = %q, want %q", got.Identity, "ns1/sub1")
	}
	if got.Subnet == nil || got.Subnet.Name != "sub1" {
		t.Errorf("Subnet = %+v, want name %q", got.Subnet, "sub1")
	}
	if got.SeederNetworkRef == nil || got.SeederNetworkRef.Name != "seeder-nad" {
		t.Errorf("SeederNetworkRef = %+v, want name %q", got.SeederNetworkRef, "seeder-nad")
	}
	if got.NodeSelector["kubernetes.io/hostname"] != "node-1" {
		t.Errorf("NodeSelector = %+v, want kubernetes.io/hostname=node-1", got.NodeSelector)
	}
}

// Subnet names aren't cluster-unique; a bare Subnet.Name would collapse two
// unrelated segments' seeder demand into one Deployment.
func TestResolveNamespaceQualifiesIdentity(t *testing.T) {
	subnetRefA := keziov1alpha2.NameRef{Name: "sub1"}
	machineA := newMachine("region-a", subnetRefA)
	subnetA := newSubnet("region-a", nil)

	subnetRefB := keziov1alpha2.NameRef{Name: "sub1"}
	machineB := newMachine("region-b", subnetRefB)
	subnetB := newSubnet("region-b", nil)

	c := newTestClient(t, machineA, subnetA, machineB, subnetB)

	gotA, err := Resolve(context.Background(), c, machineA)
	if err != nil {
		t.Fatalf("Resolve(machineA) returned error: %v", err)
	}
	gotB, err := Resolve(context.Background(), c, machineB)
	if err != nil {
		t.Fatalf("Resolve(machineB) returned error: %v", err)
	}

	if gotA.Identity != "region-a/sub1" {
		t.Errorf("gotA.Identity = %q, want %q", gotA.Identity, "region-a/sub1")
	}
	if gotB.Identity != "region-b/sub1" {
		t.Errorf("gotB.Identity = %q, want %q", gotB.Identity, "region-b/sub1")
	}
	if gotA.Identity == gotB.Identity {
		t.Fatalf("same-named Subnets in different namespaces resolved to the same identity %q", gotA.Identity)
	}
}

func TestResolveMissingSubnet(t *testing.T) {
	machine := newMachine("ns1", keziov1alpha2.NameRef{Name: "ghost"})
	c := newTestClient(t, machine)

	got, err := Resolve(context.Background(), c, machine)
	if !errors.Is(err, ErrSubnetNotFound) {
		t.Fatalf("err = %v, want wrapping ErrSubnetNotFound", err)
	}
	if got.Identity != "" || got.Subnet != nil {
		t.Fatalf("Resolution = %+v, want zero value on error", got)
	}
}

// A bare subnetRef defaults against the Machine holding it.
func TestResolveCrossNamespace(t *testing.T) {
	subnetRef := keziov1alpha2.NameRef{Namespace: "y", Name: "sub1"}
	machine := newMachine("x", subnetRef)
	subnet := newSubnet("y", nil)

	c := newTestClient(t, machine, subnet)

	got, err := Resolve(context.Background(), c, machine)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	if got.Subnet.Namespace != "y" {
		t.Errorf("Subnet.Namespace = %q, want %q", got.Subnet.Namespace, "y")
	}
	if got.Identity != "y/sub1" {
		t.Errorf("Identity = %q, want %q", got.Identity, "y/sub1")
	}
}

// A Subnet with no SeederNetworkRef is a supported topology, not an error.
func TestResolveSubnetWithNoSeederNetworkRef(t *testing.T) {
	subnetRef := keziov1alpha2.NameRef{Name: "sub1"}
	machine := newMachine("ns1", subnetRef)
	subnet := newSubnet("ns1", nil)

	c := newTestClient(t, machine, subnet)

	got, err := Resolve(context.Background(), c, machine)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	if got.SeederNetworkRef != nil {
		t.Errorf("SeederNetworkRef = %+v, want nil", got.SeederNetworkRef)
	}
}

// A non-NotFound Get failure must propagate as-is, not get misclassified
// as ErrSubnetNotFound.
func TestResolveSubnetGetTransientErrorPropagates(t *testing.T) {
	subnetRef := keziov1alpha2.NameRef{Name: "sub1"}
	machine := newMachine("ns1", subnetRef)
	subnet := newSubnet("ns1", nil)

	wantErr := errors.New("injected transient failure")
	scheme := apimachineryruntime.NewScheme()
	if err := keziov1alpha2.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(machine, subnet).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cli client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*keziov1alpha2.Subnet); ok {
					return wantErr
				}
				return cli.Get(ctx, key, obj, opts...)
			},
		}).
		Build()

	got, err := Resolve(context.Background(), c, machine)
	if !errors.Is(err, wantErr) {
		t.Fatalf("Resolve() error = %v, want it to wrap %v", err, wantErr)
	}
	if errors.Is(err, ErrSubnetNotFound) {
		t.Fatalf("Resolve() error = %v, must not be misclassified as ErrSubnetNotFound", err)
	}
	if got.Identity != "" || got.Subnet != nil {
		t.Fatalf("Resolution = %+v, want zero value on error", got)
	}
}

// A namespace with no Subnet at all - the shape a Subnet-less environment
// (for example an image-only CI lane) always has - resolves to ok = false
// with a zero Resolution, not an error.
func TestResolveNamespaceSeederNoSubnets(t *testing.T) {
	c := newTestClient(t)

	got, ok, err := ResolveNamespaceSeeder(context.Background(), c, "ns1")
	if err != nil {
		t.Fatalf("ResolveNamespaceSeeder returned error: %v", err)
	}
	if ok {
		t.Fatalf("ok = true, want false with no Subnets present")
	}
	if got.Identity != "" || got.Subnet != nil {
		t.Fatalf("Resolution = %+v, want zero value", got)
	}
}

// A namespace whose only Subnets carry no SeederNetworkRef is a supported
// topology (none of them host seeders), not an error.
func TestResolveNamespaceSeederNoneHostSeeders(t *testing.T) {
	subnet := newSubnet("ns1", nil)
	c := newTestClient(t, subnet)

	_, ok, err := ResolveNamespaceSeeder(context.Background(), c, "ns1")
	if err != nil {
		t.Fatalf("ResolveNamespaceSeeder returned error: %v", err)
	}
	if ok {
		t.Fatalf("ok = true, want false when no Subnet in the namespace hosts seeders")
	}
}

// Exactly one seeder-hosting Subnet in the namespace resolves to it.
func TestResolveNamespaceSeederSingleMatch(t *testing.T) {
	seederRef := &keziov1alpha2.NameRef{Name: "seeder-nad"}
	subnet := newSubnet("ns1", seederRef)
	c := newTestClient(t, subnet)

	got, ok, err := ResolveNamespaceSeeder(context.Background(), c, "ns1")
	if err != nil {
		t.Fatalf("ResolveNamespaceSeeder returned error: %v", err)
	}
	if !ok {
		t.Fatalf("ok = false, want true")
	}
	if got.Identity != "ns1/sub1" {
		t.Errorf("Identity = %q, want %q", got.Identity, "ns1/sub1")
	}
	if got.SeederNetworkRef == nil || got.SeederNetworkRef.Name != "seeder-nad" {
		t.Errorf("SeederNetworkRef = %+v, want name %q", got.SeederNetworkRef, "seeder-nad")
	}
}

// Several Subnets in the namespace host seeders: the one with the
// lexicographically lowest Name wins, deterministically.
func TestResolveNamespaceSeederPicksLowestNameAmongSeederHostingSubnets(t *testing.T) {
	seederRef := &keziov1alpha2.NameRef{Name: "seeder-nad"}
	lowest := newSubnet("ns1", seederRef)
	lowest.Name = "aaa"
	highest := newSubnet("ns1", seederRef)
	highest.Name = "zzz"
	// A non-seeder-hosting Subnet with an even lower name must not win.
	noSeeder := newSubnet("ns1", nil)
	noSeeder.Name = "aaaa0"

	c := newTestClient(t, lowest, highest, noSeeder)

	got, ok, err := ResolveNamespaceSeeder(context.Background(), c, "ns1")
	if err != nil {
		t.Fatalf("ResolveNamespaceSeeder returned error: %v", err)
	}
	if !ok {
		t.Fatalf("ok = false, want true")
	}
	if got.Subnet.Name != "aaa" {
		t.Errorf("Subnet.Name = %q, want the lowest-named seeder-hosting Subnet %q", got.Subnet.Name, "aaa")
	}
}

// A seeder-hosting Subnet in a different namespace must never be picked.
func TestResolveNamespaceSeederIgnoresOtherNamespaces(t *testing.T) {
	seederRef := &keziov1alpha2.NameRef{Name: "seeder-nad"}
	other := newSubnet("ns2", seederRef)
	c := newTestClient(t, other)

	_, ok, err := ResolveNamespaceSeeder(context.Background(), c, "ns1")
	if err != nil {
		t.Fatalf("ResolveNamespaceSeeder returned error: %v", err)
	}
	if ok {
		t.Fatalf("ok = true, want false: the only seeder-hosting Subnet lives in a different namespace")
	}
}
