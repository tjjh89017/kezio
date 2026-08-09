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

package sitederive

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
)

func newTestClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme (client-go): %v", err)
	}
	if err := keziov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme (kezio): %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func newMachine(namespace string, subnetRef keziov1alpha1.NameRef) *keziov1alpha1.Machine {
	return &keziov1alpha1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: namespace},
		Spec: keziov1alpha1.MachineSpec{
			BootMACAddress: "aa:bb:cc:dd:ee:01",
			SubnetRef:      subnetRef,
		},
	}
}

func newSubnet(namespace string, siteRef keziov1alpha1.NameRef) *keziov1alpha1.Subnet {
	return &keziov1alpha1.Subnet{
		ObjectMeta: metav1.ObjectMeta{Name: "sub1", Namespace: namespace},
		Spec: keziov1alpha1.SubnetSpec{
			SiteRef:         siteRef,
			CIDR:            "192.0.2.0/24",
			BootdServerIP:   "192.0.2.2",
			BootdNetworkRef: keziov1alpha1.NameRef{Name: "bootd-nad"},
			DHCP:            keziov1alpha1.SubnetDHCP{Mode: keziov1alpha1.SubnetDHCPModeProxy},
		},
	}
}

// newSite always names the Site "hq"; namespace is the only field callers vary.
func newSite(namespace string) *keziov1alpha1.Site {
	return &keziov1alpha1.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "hq", Namespace: namespace},
	}
}

func TestResolveHappyPath(t *testing.T) {
	subnetRef := keziov1alpha1.NameRef{Name: "sub1"}
	siteRef := keziov1alpha1.NameRef{Name: "hq"}
	machine := newMachine("ns1", subnetRef)
	subnet := newSubnet("ns1", siteRef)
	site := newSite("ns1")

	c := newTestClient(t, machine, subnet, site)

	got, err := Resolve(context.Background(), c, machine)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	if got.SiteName != "ns1/hq" {
		t.Errorf("SiteName = %q, want %q", got.SiteName, "ns1/hq")
	}
	if got.Site == nil || got.Site.Name != "hq" {
		t.Errorf("Site = %+v, want name %q", got.Site, "hq")
	}
	if got.Subnet == nil || got.Subnet.Name != "sub1" {
		t.Errorf("Subnet = %+v, want name %q", got.Subnet, "sub1")
	}
}

// Site names aren't cluster-unique; a bare Site.Name would collapse two
// unrelated segments' seeder demand into one Deployment.
func TestResolveNamespaceQualifiesIdentity(t *testing.T) {
	subnetRefA := keziov1alpha1.NameRef{Name: "sub1"}
	siteRefA := keziov1alpha1.NameRef{Name: "hq"}
	machineA := newMachine("region-a", subnetRefA)
	subnetA := newSubnet("region-a", siteRefA)
	siteA := newSite("region-a")

	subnetRefB := keziov1alpha1.NameRef{Name: "sub1"}
	siteRefB := keziov1alpha1.NameRef{Name: "hq"}
	machineB := newMachine("region-b", subnetRefB)
	subnetB := newSubnet("region-b", siteRefB)
	siteB := newSite("region-b")

	c := newTestClient(t, machineA, subnetA, siteA, machineB, subnetB, siteB)

	gotA, err := Resolve(context.Background(), c, machineA)
	if err != nil {
		t.Fatalf("Resolve(machineA) returned error: %v", err)
	}
	gotB, err := Resolve(context.Background(), c, machineB)
	if err != nil {
		t.Fatalf("Resolve(machineB) returned error: %v", err)
	}

	if gotA.SiteName != "region-a/hq" {
		t.Errorf("gotA.SiteName = %q, want %q", gotA.SiteName, "region-a/hq")
	}
	if gotB.SiteName != "region-b/hq" {
		t.Errorf("gotB.SiteName = %q, want %q", gotB.SiteName, "region-b/hq")
	}
	if gotA.SiteName == gotB.SiteName {
		t.Fatalf("same-named Sites in different namespaces resolved to the same identity %q", gotA.SiteName)
	}
}

func TestResolveMissingSubnet(t *testing.T) {
	machine := newMachine("ns1", keziov1alpha1.NameRef{Name: "ghost"})
	c := newTestClient(t, machine)

	got, err := Resolve(context.Background(), c, machine)
	if !errors.Is(err, ErrSubnetNotFound) {
		t.Fatalf("err = %v, want wrapping ErrSubnetNotFound", err)
	}
	if errors.Is(err, ErrSiteNotFound) {
		t.Fatalf("err = %v, must not also classify as ErrSiteNotFound", err)
	}
	if got != (Resolution{}) {
		t.Fatalf("Resolution = %+v, want zero value on error", got)
	}
}

func TestResolveMissingSite(t *testing.T) {
	subnetRef := keziov1alpha1.NameRef{Name: "sub1"}
	machine := newMachine("ns1", subnetRef)
	subnet := newSubnet("ns1", keziov1alpha1.NameRef{Name: "ghost-site"})
	c := newTestClient(t, machine, subnet)

	got, err := Resolve(context.Background(), c, machine)
	if !errors.Is(err, ErrSiteNotFound) {
		t.Fatalf("err = %v, want wrapping ErrSiteNotFound", err)
	}
	if errors.Is(err, ErrSubnetNotFound) {
		t.Fatalf("err = %v, must not also classify as ErrSubnetNotFound", err)
	}
	if got != (Resolution{}) {
		t.Fatalf("Resolution = %+v, want zero value on error", got)
	}
}

// A bare siteRef defaults against the Subnet holding it, not the Machine.
// The Site lives only in "y", so a wrong default (against "x") can't find it.
func TestResolveCrossNamespace(t *testing.T) {
	subnetRef := keziov1alpha1.NameRef{Namespace: "y", Name: "sub1"}
	siteRef := keziov1alpha1.NameRef{Name: "hq"}
	machine := newMachine("x", subnetRef)
	subnet := newSubnet("y", siteRef)
	site := newSite("y")

	c := newTestClient(t, machine, subnet, site)

	got, err := Resolve(context.Background(), c, machine)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	if got.Subnet.Namespace != "y" {
		t.Errorf("Subnet.Namespace = %q, want %q", got.Subnet.Namespace, "y")
	}
	if got.Site.Namespace != "y" {
		t.Errorf("Site.Namespace = %q, want %q", got.Site.Namespace, "y")
	}
	if got.SiteName != "y/hq" {
		t.Errorf("SiteName = %q, want %q", got.SiteName, "y/hq")
	}
}

// A Site with no SeederSubnetRef is a supported topology, not an error.
func TestResolveSiteWithNoSeederSubnet(t *testing.T) {
	subnetRef := keziov1alpha1.NameRef{Name: "sub1"}
	siteRef := keziov1alpha1.NameRef{Name: "hq"}
	machine := newMachine("ns1", subnetRef)
	subnet := newSubnet("ns1", siteRef)
	site := newSite("ns1")

	c := newTestClient(t, machine, subnet, site)

	got, err := Resolve(context.Background(), c, machine)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	if got.Site.Spec.SeederSubnetRef != nil {
		t.Errorf("Site.Spec.SeederSubnetRef = %+v, want nil", got.Site.Spec.SeederSubnetRef)
	}
}

// A non-NotFound Get failure must propagate as-is, not get misclassified
// as ErrSiteNotFound.
func TestResolveSiteGetTransientErrorPropagates(t *testing.T) {
	subnetRef := keziov1alpha1.NameRef{Name: "sub1"}
	siteRef := keziov1alpha1.NameRef{Name: "hq"}
	machine := newMachine("ns1", subnetRef)
	subnet := newSubnet("ns1", siteRef)
	site := newSite("ns1")

	wantErr := errors.New("injected transient failure")
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme (client-go): %v", err)
	}
	if err := keziov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme (kezio): %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(machine, subnet, site).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cli client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*keziov1alpha1.Site); ok {
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
	if errors.Is(err, ErrSiteNotFound) {
		t.Fatalf("Resolve() error = %v, must not be misclassified as ErrSiteNotFound", err)
	}
	if got != (Resolution{}) {
		t.Fatalf("Resolution = %+v, want zero value on error", got)
	}
}
