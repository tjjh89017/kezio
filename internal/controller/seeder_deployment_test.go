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

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/seederdeploy"
)

// newSeederUnitTestScheme adds appsv1 on top of newPartitionStorageTestScheme,
// needed by tests that drive reconcileSeederDeployments against a fake client.
func newSeederUnitTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := newPartitionStorageTestScheme(t)
	if err := appsv1.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme (apps/v1): %v", err)
	}
	return s
}

// seederUnitTestFixture builds an Image, a Site whose SeederSubnetRef
// resolves to subnet, and one Machine on subnet actively provisioning
// that image - the minimum graph giving non-empty demand for one site.
func seederUnitTestFixture() (image *keziov1alpha1.Image, site *keziov1alpha1.Site, subnet *keziov1alpha1.Subnet, machine *keziov1alpha1.Machine) {
	image = &keziov1alpha1.Image{ObjectMeta: metav1.ObjectMeta{Name: "os-image", Namespace: "default"}}
	subnet = &keziov1alpha1.Subnet{
		ObjectMeta: metav1.ObjectMeta{Name: "hq-rack-1", Namespace: "default"},
		Spec: keziov1alpha1.SubnetSpec{
			SiteRef:         keziov1alpha1.NameRef{Name: "hq"},
			CIDR:            "192.0.2.0/24",
			BootdServerIP:   "192.0.2.2",
			BootdNetworkRef: keziov1alpha1.NameRef{Name: "bootd-nad"},
			DHCP:            keziov1alpha1.SubnetDHCP{Mode: keziov1alpha1.SubnetDHCPModeProxy},
		},
	}
	site = &keziov1alpha1.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "hq", Namespace: "default"},
		Spec:       keziov1alpha1.SiteSpec{SeederSubnetRef: &keziov1alpha1.NameRef{Name: "hq-rack-1"}},
	}
	machine = &keziov1alpha1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: "default"},
		Spec: keziov1alpha1.MachineSpec{
			ImageRef:  &keziov1alpha1.NameRef{Name: "os-image"},
			SubnetRef: keziov1alpha1.NameRef{Name: "hq-rack-1"},
		},
		Status: keziov1alpha1.MachineStatus{State: keziov1alpha1.MachineStateProvisioning},
	}
	return image, site, subnet, machine
}

func newSeederUnitTestReconciler(c client.Client) *ImageReconciler {
	return &ImageReconciler{
		Client: c,
		Scheme: c.Scheme(),
		SeederDeployment: SeederDeploymentConfig{
			Image: "ezio-seeder:test",
		},
	}
}

func TestReconcileSeederDeployments_ListMachinesErrorPropagates(t *testing.T) {
	image, _, _, _ := seederUnitTestFixture()
	wantErr := errors.New("injected transient failure")
	c := fake.NewClientBuilder().
		WithScheme(newSeederUnitTestScheme(t)).
		WithObjects(image).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cli client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*keziov1alpha1.MachineList); ok {
					return wantErr
				}
				return cli.List(ctx, list, opts...)
			},
		}).
		Build()

	r := newSeederUnitTestReconciler(c)
	_, err := r.reconcileSeederDeployments(context.Background(), image)
	if !errors.Is(err, wantErr) {
		t.Fatalf("reconcileSeederDeployments() error = %v, want it to wrap %v", err, wantErr)
	}
}

// seederUnitTestSiteName is the namespace-qualified site key
// seederUnitTestFixture's "hq" Site derives to.
const seederUnitTestSiteName = "default/hq"

// The List call is intercepted to return no Deployments, simulating an
// object created after this reconcile's own List already ran.
func TestReconcileSeederDeployments_CreateAlreadyExistsIsTolerated(t *testing.T) {
	image, site, subnet, machine := seederUnitTestFixture()
	racedDep := seederUnitTestOwnedDeployment(t, image)
	c := fake.NewClientBuilder().
		WithScheme(newSeederUnitTestScheme(t)).
		WithObjects(image, site, subnet, machine, racedDep).
		WithStatusSubresource(&keziov1alpha1.Image{}).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cli client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*appsv1.DeploymentList); ok {
					return nil
				}
				return cli.List(ctx, list, opts...)
			},
		}).
		Build()

	r := newSeederUnitTestReconciler(c)
	if _, err := r.reconcileSeederDeployments(context.Background(), image); err != nil {
		t.Fatalf("reconcileSeederDeployments() error = %v, want AlreadyExists-on-own-object tolerated", err)
	}

	updated := &keziov1alpha1.Image{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(image), updated); err != nil {
		t.Fatalf("get image: %v", err)
	}
	if cond := apimeta.FindStatusCondition(updated.Status.Conditions, keziov1alpha1.ImageConditionSeederDegraded); cond != nil {
		t.Errorf("ImageConditionSeederDegraded = %+v, want no condition for a genuinely-owned raced object", cond)
	}
	found := false
	for _, s := range updated.Status.Seeders {
		if s.Site == seederUnitTestSiteName {
			found = true
		}
	}
	if !found {
		t.Errorf("Status.Seeders = %+v, want site %q counted as served", updated.Status.Seeders, seederUnitTestSiteName)
	}
}

func TestReconcileSeederDeployments_CreateForeignObjectIsNotAdopted(t *testing.T) {
	image, site, subnet, machine := seederUnitTestFixture()
	foreignDep := seederUnitTestForeignDeployment()
	c := fake.NewClientBuilder().
		WithScheme(newSeederUnitTestScheme(t)).
		WithObjects(image, site, subnet, machine, foreignDep).
		WithStatusSubresource(&keziov1alpha1.Image{}).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cli client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*appsv1.DeploymentList); ok {
					return nil
				}
				return cli.List(ctx, list, opts...)
			},
		}).
		Build()

	r := newSeederUnitTestReconciler(c)
	if _, err := r.reconcileSeederDeployments(context.Background(), image); err != nil {
		t.Fatalf("reconcileSeederDeployments() error = %v, want no error, only non-adoption", err)
	}

	updated := &keziov1alpha1.Image{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(image), updated); err != nil {
		t.Fatalf("get image: %v", err)
	}
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, keziov1alpha1.ImageConditionSeederDegraded)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("ImageConditionSeederDegraded = %+v, want True with the foreign site surfaced", cond)
	}
	if cond.Reason != "SeederDeploymentForeignOwner" {
		t.Errorf("ImageConditionSeederDegraded.Reason = %q, want %q", cond.Reason, "SeederDeploymentForeignOwner")
	}
	for _, s := range updated.Status.Seeders {
		if s.Site == seederUnitTestSiteName {
			t.Errorf("Status.Seeders = %+v, want site %q NOT counted as served by a foreign object", updated.Status.Seeders, seederUnitTestSiteName)
		}
	}

	stillForeign := &appsv1.Deployment{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(foreignDep), stillForeign); err != nil {
		t.Fatalf("get foreign deployment: %v", err)
	}
	if metav1.IsControlledBy(stillForeign, image) {
		t.Errorf("foreign deployment got adopted by image, want it left exactly as found")
	}

	// Once the name-squatting object is removed, the condition must clear.
	if err := c.Delete(context.Background(), foreignDep); err != nil {
		t.Fatalf("delete foreign deployment: %v", err)
	}
	if _, err := r.reconcileSeederDeployments(context.Background(), image); err != nil {
		t.Fatalf("reconcileSeederDeployments() second call error = %v", err)
	}
	cleared := &keziov1alpha1.Image{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(image), cleared); err != nil {
		t.Fatalf("get image after clearing: %v", err)
	}
	if cond := apimeta.FindStatusCondition(cleared.Status.Conditions, keziov1alpha1.ImageConditionSeederDegraded); cond != nil {
		t.Errorf("ImageConditionSeederDegraded = %+v, want cleared once the foreign object is removed and a real Deployment created", cond)
	}
	found := false
	for _, s := range cleared.Status.Seeders {
		if s.Site == seederUnitTestSiteName {
			found = true
		}
	}
	if !found {
		t.Errorf("Status.Seeders = %+v, want site %q counted as served once the foreign object is gone", cleared.Status.Seeders, seederUnitTestSiteName)
	}
}

func TestReconcileSeederDeployments_TwoCausesShareDegradedReason(t *testing.T) {
	image, site, subnet, machine := seederUnitTestFixture()
	foreignDep := seederUnitTestForeignDeployment()

	branchSite := &keziov1alpha1.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "branch", Namespace: "default"},
		// No SeederSubnetRef triggers the SeederSubnetRefUnset cause.
	}
	branchSubnet := &keziov1alpha1.Subnet{
		ObjectMeta: metav1.ObjectMeta{Name: "branch-sub", Namespace: "default"},
		Spec: keziov1alpha1.SubnetSpec{
			SiteRef:         keziov1alpha1.NameRef{Name: "branch"},
			CIDR:            "192.0.2.0/24",
			BootdServerIP:   "192.0.2.2",
			BootdNetworkRef: keziov1alpha1.NameRef{Name: "bootd-nad"},
			DHCP:            keziov1alpha1.SubnetDHCP{Mode: keziov1alpha1.SubnetDHCPModeProxy},
		},
	}
	branchMachine := &keziov1alpha1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m-branch", Namespace: "default"},
		Spec: keziov1alpha1.MachineSpec{
			ImageRef:  &keziov1alpha1.NameRef{Name: "os-image"},
			SubnetRef: keziov1alpha1.NameRef{Name: "branch-sub"},
		},
		Status: keziov1alpha1.MachineStatus{State: keziov1alpha1.MachineStateProvisioning},
	}

	c := fake.NewClientBuilder().
		WithScheme(newSeederUnitTestScheme(t)).
		WithObjects(image, site, subnet, machine, foreignDep, branchSite, branchSubnet, branchMachine).
		WithStatusSubresource(&keziov1alpha1.Image{}).
		Build()

	r := newSeederUnitTestReconciler(c)
	if _, err := r.reconcileSeederDeployments(context.Background(), image); err != nil {
		t.Fatalf("reconcileSeederDeployments() error = %v, want no error, only both causes surfaced", err)
	}

	updated := &keziov1alpha1.Image{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(image), updated); err != nil {
		t.Fatalf("get image: %v", err)
	}
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, keziov1alpha1.ImageConditionSeederDegraded)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("ImageConditionSeederDegraded = %+v, want True with both causes surfaced", cond)
	}
	if cond.Reason != "SeederDegraded" {
		t.Errorf("ImageConditionSeederDegraded.Reason = %q, want %q when two causes are active at once", cond.Reason, "SeederDegraded")
	}
	// Compares the exact message, not a substring: the foreign-owner
	// cause's own message already contains "; ", so a substring check for
	// the join separator would pass even with only one cause emitted.
	// Causes are appended in the fixed order unsetSeederSubnet,
	// missingSeederSubnet, foreignOwner, unready.
	wantMessage := "Site.spec.seederSubnetRef is unset for site(s) default/branch; no seeder Deployment is created there and any existing one is left untouched rather than pushed onto a network-less spec, so Machines there cannot build a deploy plan for this Image until it is set" +
		"; seeder deployment name is already controlled by a different Image for site(s) " + seederUnitTestSiteName +
		"; not adopted or mutated, and the site is not counted as served"
	if cond.Message != wantMessage {
		t.Errorf("ImageConditionSeederDegraded.Message =\n%q\nwant\n%q", cond.Message, wantMessage)
	}
}

func TestReconcileSeederDeployments_NoSeederSubnetIsSurfaced(t *testing.T) {
	image, site, subnet, machine := seederUnitTestFixture()
	site.Spec.SeederSubnetRef = nil
	c := fake.NewClientBuilder().
		WithScheme(newSeederUnitTestScheme(t)).
		WithObjects(image, site, subnet, machine).
		WithStatusSubresource(&keziov1alpha1.Image{}).
		Build()

	r := newSeederUnitTestReconciler(c)
	if _, err := r.reconcileSeederDeployments(context.Background(), image); err != nil {
		t.Fatalf("reconcileSeederDeployments() error = %v, want no error, only a missing Deployment", err)
	}

	deps := &appsv1.DeploymentList{}
	if err := c.List(context.Background(), deps, client.InNamespace("default")); err != nil {
		t.Fatalf("list deployments: %v", err)
	}
	if len(deps.Items) != 0 {
		t.Errorf("Deployments = %+v, want none created for a site with no seeder Subnet", deps.Items)
	}

	updated := &keziov1alpha1.Image{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(image), updated); err != nil {
		t.Fatalf("get image: %v", err)
	}
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, keziov1alpha1.ImageConditionSeederDegraded)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("ImageConditionSeederDegraded = %+v, want True with the seeder-less site surfaced", cond)
	}
	if cond.Reason != "SeederSubnetRefUnset" {
		t.Errorf("ImageConditionSeederDegraded.Reason = %q, want %q", cond.Reason, "SeederSubnetRefUnset")
	}
}

// A same-named Image was deleted and recreated before owner-ref GC of its
// old seeder Deployment caught up. metav1.IsControlledBy compares owner
// UID, not name, so the stale Deployment (owned by the old, different-UID
// Image) is still recognized as foreign.
func TestReconcileSeederDeployments_GCRaceStaleObjectIsNotAdoptedOrUpdated(t *testing.T) {
	image, site, subnet, machine := seederUnitTestFixture()
	staleDep := seederUnitTestStaleOwnerDeployment(t, image)
	originalSpec := *staleDep.Spec.DeepCopy()
	c := fake.NewClientBuilder().
		WithScheme(newSeederUnitTestScheme(t)).
		WithObjects(image, site, subnet, machine, staleDep).
		WithStatusSubresource(&keziov1alpha1.Image{}).
		Build()

	r := newSeederUnitTestReconciler(c)
	if _, err := r.reconcileSeederDeployments(context.Background(), image); err != nil {
		t.Fatalf("reconcileSeederDeployments() error = %v, want no error, only non-adoption", err)
	}

	stillStale := &appsv1.Deployment{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(staleDep), stillStale); err != nil {
		t.Fatalf("get stale deployment: %v", err)
	}
	if !equalityDeepEqualDeploymentSpec(originalSpec, stillStale.Spec) {
		t.Errorf("stale deployment Spec got updated onto current demand, want it left exactly as found")
	}
	if metav1.IsControlledBy(stillStale, image) {
		t.Errorf("stale deployment got adopted by the recreated image, want it left exactly as found")
	}

	updated := &keziov1alpha1.Image{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(image), updated); err != nil {
		t.Fatalf("get image: %v", err)
	}
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, keziov1alpha1.ImageConditionSeederDegraded)
	if cond == nil || cond.Reason != "SeederDeploymentForeignOwner" {
		t.Errorf("ImageConditionSeederDegraded = %+v, want Reason %q for the stale same-name object", cond, "SeederDeploymentForeignOwner")
	}
}

func TestReconcileSeederDeployments_ForeignObjectNeverDeleted(t *testing.T) {
	image := &keziov1alpha1.Image{ObjectMeta: metav1.ObjectMeta{Name: "os-image", Namespace: "default"}}
	dep := seederUnitTestForeignDeployment()
	dep.Annotations[seederDeploymentEmptySinceAnnotation] = time.Now().Add(-2 * defaultSeederGracePeriod).Format(time.RFC3339)
	deleteCalled := false
	c := fake.NewClientBuilder().
		WithScheme(newSeederUnitTestScheme(t)).
		WithObjects(image, dep).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, cli client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if d, ok := obj.(*appsv1.Deployment); ok && d.Name == dep.Name {
					deleteCalled = true
				}
				return cli.Delete(ctx, obj, opts...)
			},
		}).
		Build()

	r := newSeederUnitTestReconciler(c)
	if _, err := r.reconcileSeederDeployments(context.Background(), image); err != nil {
		t.Fatalf("reconcileSeederDeployments() error = %v", err)
	}
	if deleteCalled {
		t.Errorf("reconcileSeederDeployments() deleted a foreign deployment, want it left untouched")
	}
	stillThere := &appsv1.Deployment{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(dep), stillThere); err != nil {
		t.Errorf("foreign deployment was removed, want it left in place: %v", err)
	}
}

// seederUnitTestOwnedDeployment builds the Deployment
// reconcileSeederDeployments would itself create for
// seederUnitTestFixture's (image, "hq") pair, owned by image.
func seederUnitTestOwnedDeployment(t *testing.T, image *keziov1alpha1.Image) *appsv1.Deployment {
	t.Helper()
	r := &ImageReconciler{Scheme: newSeederUnitTestScheme(t)}
	subnet := &keziov1alpha1.Subnet{
		ObjectMeta: metav1.ObjectMeta{Name: "hq-rack-1", Namespace: "default"},
		Spec:       keziov1alpha1.SubnetSpec{NodeSelector: nil},
	}
	dep, err := r.buildSeederDeployment(image, seederUnitTestSiteName, subnet)
	if err != nil {
		t.Fatalf("buildSeederDeployment: %v", err)
	}
	return dep
}

// seederUnitTestForeignDeployment returns a Deployment at the name and
// site annotation reconcileSeederDeployments expects for
// seederUnitTestFixture's (image, "hq") pair, with no controller owner
// reference (a hand-applied object squatting the name).
func seederUnitTestForeignDeployment() *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        seederdeploy.Name("os-image", seederUnitTestSiteName),
			Namespace:   "default",
			Labels:      map[string]string{SeederDeploymentImageLabel: "os-image"},
			Annotations: map[string]string{SeederDeploymentSiteAnnotation: seederUnitTestSiteName},
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "unrelated"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "unrelated"}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "placeholder", Image: "unrelated:latest"}},
				},
			},
		},
	}
}

// seederUnitTestStaleOwnerDeployment returns a Deployment at the same
// name/site as seederUnitTestForeignDeployment, owned by a same-Name,
// different-UID Image (owner-ref GC of the deleted original hasn't run
// yet). metav1.IsControlledBy compares UID, so this is still foreign.
func seederUnitTestStaleOwnerDeployment(t *testing.T, image *keziov1alpha1.Image) *appsv1.Deployment {
	t.Helper()
	deletedImage := image.DeepCopy()
	deletedImage.UID = types.UID("stale-deleted-image-uid")
	dep := seederUnitTestForeignDeployment()
	if err := ctrl.SetControllerReference(deletedImage, dep, newSeederUnitTestScheme(t)); err != nil {
		t.Fatalf("SetControllerReference: %v", err)
	}
	return dep
}

// equalityDeepEqualDeploymentSpec uses the same comparison
// reconcileSeederDeployments' own drift check uses.
func equalityDeepEqualDeploymentSpec(a, b appsv1.DeploymentSpec) bool {
	return equality.Semantic.DeepEqual(a, b)
}

func TestReconcileSeederDeployments_CreateErrorPropagates(t *testing.T) {
	image, site, subnet, machine := seederUnitTestFixture()
	wantErr := errors.New("injected transient failure")
	c := fake.NewClientBuilder().
		WithScheme(newSeederUnitTestScheme(t)).
		WithObjects(image, site, subnet, machine).
		WithStatusSubresource(&keziov1alpha1.Image{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cli client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*appsv1.Deployment); ok {
					return wantErr
				}
				return cli.Create(ctx, obj, opts...)
			},
		}).
		Build()

	r := newSeederUnitTestReconciler(c)
	_, err := r.reconcileSeederDeployments(context.Background(), image)
	if !errors.Is(err, wantErr) {
		t.Fatalf("reconcileSeederDeployments() error = %v, want it to wrap %v", err, wantErr)
	}
}

// seederUnitTestDrainedDeployment returns a Deployment already past its
// grace period for site "default/hq" (image "os-image"), owned by image;
// ownership must hold or reconcileSeederDeployments treats it as foreign
// instead of draining it.
func seederUnitTestDrainedDeployment(t *testing.T, image *keziov1alpha1.Image) *appsv1.Deployment {
	t.Helper()
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "seeder-drained",
			Namespace: "default",
			Labels:    map[string]string{SeederDeploymentImageLabel: "os-image"},
			Annotations: map[string]string{
				SeederDeploymentSiteAnnotation:       "default/hq",
				seederDeploymentEmptySinceAnnotation: time.Now().Add(-2 * defaultSeederGracePeriod).Format(time.RFC3339),
			},
		},
	}
	if err := ctrl.SetControllerReference(image, dep, newSeederUnitTestScheme(t)); err != nil {
		t.Fatalf("SetControllerReference: %v", err)
	}
	return dep
}

func TestReconcileSeederDeployments_DeleteNotFoundIsTolerated(t *testing.T) {
	image := &keziov1alpha1.Image{ObjectMeta: metav1.ObjectMeta{Name: "os-image", Namespace: "default"}}
	dep := seederUnitTestDrainedDeployment(t, image)
	c := fake.NewClientBuilder().
		WithScheme(newSeederUnitTestScheme(t)).
		WithObjects(image, dep).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, cli client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, ok := obj.(*appsv1.Deployment); ok {
					return apierrors.NewNotFound(appsv1.Resource("deployments"), obj.GetName())
				}
				return cli.Delete(ctx, obj, opts...)
			},
		}).
		Build()

	r := newSeederUnitTestReconciler(c)
	if _, err := r.reconcileSeederDeployments(context.Background(), image); err != nil {
		t.Fatalf("reconcileSeederDeployments() error = %v, want NotFound on Delete tolerated", err)
	}
}

func TestReconcileSeederDeployments_DeleteErrorPropagates(t *testing.T) {
	image := &keziov1alpha1.Image{ObjectMeta: metav1.ObjectMeta{Name: "os-image", Namespace: "default"}}
	dep := seederUnitTestDrainedDeployment(t, image)
	wantErr := errors.New("injected transient failure")
	c := fake.NewClientBuilder().
		WithScheme(newSeederUnitTestScheme(t)).
		WithObjects(image, dep).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, cli client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, ok := obj.(*appsv1.Deployment); ok {
					return wantErr
				}
				return cli.Delete(ctx, obj, opts...)
			},
		}).
		Build()

	r := newSeederUnitTestReconciler(c)
	_, err := r.reconcileSeederDeployments(context.Background(), image)
	if !errors.Is(err, wantErr) {
		t.Fatalf("reconcileSeederDeployments() error = %v, want it to wrap %v", err, wantErr)
	}
}

func TestUpdateSeederStatus_StatusUpdateErrorPropagates(t *testing.T) {
	image := &keziov1alpha1.Image{ObjectMeta: metav1.ObjectMeta{Name: "os-image", Namespace: "default"}}
	wantErr := errors.New("injected transient failure")
	c := fake.NewClientBuilder().
		WithScheme(newPartitionStorageTestScheme(t)).
		WithObjects(image).
		WithStatusSubresource(&keziov1alpha1.Image{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, cli client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				return wantErr
			},
		}).
		Build()

	r := &ImageReconciler{Client: c, Scheme: c.Scheme()}
	sites := map[string]int32{"default/hq": 2}
	err := r.updateSeederStatus(context.Background(), image, sites, seederDegradedSites{})
	if !errors.Is(err, wantErr) {
		t.Fatalf("updateSeederStatus() error = %v, want it to wrap %v", err, wantErr)
	}
}

func TestMachineHoldsSeederReference(t *testing.T) {
	withReason := func(reason string) []metav1.Condition {
		return []metav1.Condition{{Type: keziov1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: reason}}
	}

	cases := []struct {
		name  string
		state string
		conds []metav1.Condition
		want  bool
	}{
		{"Provisioning holds", keziov1alpha1.MachineStateProvisioning, nil, true},
		{"Error retrying a provisioning failure holds", keziov1alpha1.MachineStateError, withReason(reasonProvisionFailed), true},
		{"Error retrying a register failure does not hold", keziov1alpha1.MachineStateError, withReason(reasonRegisterFailed), false},
		{"Error retrying an inspect failure does not hold", keziov1alpha1.MachineStateError, withReason(reasonInspectFailed), false},
		{"Error with no recorded reason does not hold", keziov1alpha1.MachineStateError, nil, false},
		{"Enrolling does not hold", keziov1alpha1.MachineStateEnrolling, nil, false},
		{"Inspecting does not hold", keziov1alpha1.MachineStateInspecting, nil, false},
		{"Available does not hold", keziov1alpha1.MachineStateAvailable, nil, false},
		{"Provisioned does not hold", keziov1alpha1.MachineStateProvisioned, nil, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			machine := &keziov1alpha1.Machine{
				Status: keziov1alpha1.MachineStatus{
					State:      tc.state,
					Conditions: tc.conds,
				},
			}
			if got := machineHoldsSeederReference(machine); got != tc.want {
				t.Errorf("machineHoldsSeederReference(state=%s) = %v, want %v", tc.state, got, tc.want)
			}
		})
	}
}

func TestSeederDemandBySite(t *testing.T) {
	image := &keziov1alpha1.Image{ObjectMeta: metav1.ObjectMeta{Name: "os-image", Namespace: "default"}}

	siteHQ := &keziov1alpha1.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "hq", Namespace: "default"},
		Spec:       keziov1alpha1.SiteSpec{SeederSubnetRef: &keziov1alpha1.NameRef{Name: "hq-rack-1"}},
	}
	siteBranch := &keziov1alpha1.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "branch", Namespace: "default"},
		// No SeederSubnetRef: a supported "no local seeder" topology.
	}

	newSubnet := func(name, site string) *keziov1alpha1.Subnet {
		return &keziov1alpha1.Subnet{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: keziov1alpha1.SubnetSpec{
				SiteRef:         keziov1alpha1.NameRef{Name: site},
				CIDR:            "192.0.2.0/24",
				BootdServerIP:   "192.0.2.2",
				BootdNetworkRef: keziov1alpha1.NameRef{Name: "bootd-nad"},
				DHCP:            keziov1alpha1.SubnetDHCP{Mode: keziov1alpha1.SubnetDHCPModeProxy},
			},
		}
	}
	// Two distinct Subnets, both members of Site "hq".
	subRack1 := newSubnet("hq-rack-1", "hq")
	subRack2 := newSubnet("hq-rack-2", "hq")
	subBranch := newSubnet("branch-sub", "branch")

	provisioning := func(name, subnet string, ref keziov1alpha1.NameRef, dataRefs ...keziov1alpha1.NameRef) keziov1alpha1.Machine {
		dataImages := make([]keziov1alpha1.MachineDataImage, 0, len(dataRefs))
		for _, d := range dataRefs {
			dataImages = append(dataImages, keziov1alpha1.MachineDataImage{ImageRef: d})
		}
		return keziov1alpha1.Machine{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: keziov1alpha1.MachineSpec{
				ImageRef:   &ref,
				DataImages: dataImages,
				SubnetRef:  keziov1alpha1.NameRef{Name: subnet},
			},
			Status: keziov1alpha1.MachineStatus{State: keziov1alpha1.MachineStateProvisioning},
		}
	}

	machines := &keziov1alpha1.MachineList{
		Items: []keziov1alpha1.Machine{
			// Two Subnets, both Site "hq": must collapse to one demand entry.
			provisioning("m1", "hq-rack-1", keziov1alpha1.NameRef{Name: "os-image"}),
			provisioning("m2", "hq-rack-2", keziov1alpha1.NameRef{Name: "os-image"}),
			provisioning("m3", "branch-sub", keziov1alpha1.NameRef{Name: "os-image"}),
			// References the target Image twice (imageRef and dataImages):
			// must still count once.
			provisioning("m4", "branch-sub", keziov1alpha1.NameRef{Name: "os-image"}, keziov1alpha1.NameRef{Name: "os-image"}),
			// Different Image: must not count.
			provisioning("m5", "hq-rack-1", keziov1alpha1.NameRef{Name: "other-image"}),
			// Available: does not hold a seeder reference, must not count.
			{
				ObjectMeta: metav1.ObjectMeta{Name: "m6", Namespace: "default"},
				Spec:       keziov1alpha1.MachineSpec{ImageRef: &keziov1alpha1.NameRef{Name: "os-image"}, SubnetRef: keziov1alpha1.NameRef{Name: "hq-rack-1"}},
				Status:     keziov1alpha1.MachineStatus{State: keziov1alpha1.MachineStateAvailable},
			},
			// Dangling subnetRef: must be skipped, not fail the whole call.
			{
				ObjectMeta: metav1.ObjectMeta{Name: "m7", Namespace: "default"},
				Spec: keziov1alpha1.MachineSpec{
					ImageRef:  &keziov1alpha1.NameRef{Name: "os-image"},
					SubnetRef: keziov1alpha1.NameRef{Name: "ghost-subnet"},
				},
				Status: keziov1alpha1.MachineStatus{
					State: keziov1alpha1.MachineStateError,
					Conditions: []metav1.Condition{
						{Type: keziov1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: reasonProvisionFailed},
					},
				},
			},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(newPartitionStorageTestScheme(t)).
		WithObjects(siteHQ, siteBranch, subRack1, subRack2, subBranch).
		Build()

	got, err := seederDemandBySite(context.Background(), c, machines, image)
	if err != nil {
		t.Fatalf("seederDemandBySite returned error: %v", err)
	}

	// Demand is keyed by namespace-qualified site identity, not bare name.
	want := map[string]struct {
		count    int32
		siteName string
	}{
		"default/hq":     {2, "hq"},
		"default/branch": {2, "branch"},
	}
	if len(got) != len(want) {
		t.Fatalf("seederDemandBySite() = %+v, want counts %v", got, want)
	}
	for key, w := range want {
		sd, ok := got[key]
		if !ok {
			t.Errorf("missing demand entry for site %q", key)
			continue
		}
		if sd.Count != w.count {
			t.Errorf("seederDemandBySite()[%q].Count = %d, want %d", key, sd.Count, w.count)
		}
		if sd.Site == nil || sd.Site.Name != w.siteName {
			t.Errorf("seederDemandBySite()[%q].Site = %+v, want Site named %q", key, sd.Site, w.siteName)
		}
	}
	if got["default/branch"].Site.Spec.SeederSubnetRef != nil {
		t.Errorf("branch Site.Spec.SeederSubnetRef = %+v, want nil (no local seeder)", got["default/branch"].Site.Spec.SeederSubnetRef)
	}
}

// A dangling SeederSubnetRef must come back as errSeederSubnetNotFound,
// not the (nil, nil) an unset ref returns - otherwise the two are
// indistinguishable to the caller.
func TestResolveSeederSubnet_DanglingRefIsDistinguishableFromUnset(t *testing.T) {
	site := &keziov1alpha1.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "hq", Namespace: "default"},
		Spec:       keziov1alpha1.SiteSpec{SeederSubnetRef: &keziov1alpha1.NameRef{Name: "ghost-seeder-subnet"}},
	}
	c := fake.NewClientBuilder().
		WithScheme(newPartitionStorageTestScheme(t)).
		WithObjects(site).
		Build()

	r := &ImageReconciler{Client: c, Scheme: c.Scheme()}
	subnet, err := r.resolveSeederSubnet(context.Background(), site)
	if !errors.Is(err, errSeederSubnetNotFound) {
		t.Fatalf("resolveSeederSubnet() error = %v, want it to wrap %v", err, errSeederSubnetNotFound)
	}
	if subnet != nil {
		t.Errorf("resolveSeederSubnet() subnet = %+v, want nil for a dangling ref", subnet)
	}
	if !strings.Contains(err.Error(), "default/ghost-seeder-subnet") {
		t.Errorf("resolveSeederSubnet() error = %q, want it to name the missing Subnet %q", err, "default/ghost-seeder-subnet")
	}

	site.Spec.SeederSubnetRef = nil
	subnet, err = r.resolveSeederSubnet(context.Background(), site)
	if subnet != nil || err != nil {
		t.Errorf("resolveSeederSubnet() = (%+v, %v) for an unset ref, want (nil, nil) - the supported no-local-seeder topology", subnet, err)
	}
}

func TestResolveSeederSubnet_NonNotFoundErrorPropagates(t *testing.T) {
	site := &keziov1alpha1.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "hq", Namespace: "default"},
		Spec:       keziov1alpha1.SiteSpec{SeederSubnetRef: &keziov1alpha1.NameRef{Name: "hq-rack-1"}},
	}

	wantErr := errors.New("injected transient failure")
	c := fake.NewClientBuilder().
		WithScheme(newPartitionStorageTestScheme(t)).
		WithObjects(site).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cli client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*keziov1alpha1.Subnet); ok {
					return wantErr
				}
				return cli.Get(ctx, key, obj, opts...)
			},
		}).
		Build()

	r := &ImageReconciler{Client: c, Scheme: c.Scheme()}
	subnet, err := r.resolveSeederSubnet(context.Background(), site)
	if !errors.Is(err, wantErr) {
		t.Fatalf("resolveSeederSubnet() error = %v, want it to wrap %v", err, wantErr)
	}
	if subnet != nil {
		t.Errorf("resolveSeederSubnet() subnet = %+v, want nil on error", subnet)
	}
}

// Unlike a dangling subnetRef/siteRef, which is skipped, any other error
// sitederive.Resolve returns must propagate rather than being treated as
// a per-Machine misconfiguration.
func TestSeederDemandBySite_NonNotFoundErrorPropagates(t *testing.T) {
	image := &keziov1alpha1.Image{ObjectMeta: metav1.ObjectMeta{Name: "os-image", Namespace: "default"}}

	machines := &keziov1alpha1.MachineList{
		Items: []keziov1alpha1.Machine{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: "default"},
				Spec: keziov1alpha1.MachineSpec{
					ImageRef:  &keziov1alpha1.NameRef{Name: "os-image"},
					SubnetRef: keziov1alpha1.NameRef{Name: "hq-rack-1"},
				},
				Status: keziov1alpha1.MachineStatus{State: keziov1alpha1.MachineStateProvisioning},
			},
		},
	}

	wantErr := errors.New("injected transient failure")
	c := fake.NewClientBuilder().
		WithScheme(newPartitionStorageTestScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cli client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*keziov1alpha1.Subnet); ok {
					return wantErr
				}
				return cli.Get(ctx, key, obj, opts...)
			},
		}).
		Build()

	got, err := seederDemandBySite(context.Background(), c, machines, image)
	if !errors.Is(err, wantErr) {
		t.Fatalf("seederDemandBySite() error = %v, want it to wrap %v", err, wantErr)
	}
	if got != nil {
		t.Errorf("seederDemandBySite() demand = %+v, want nil on propagated error, not a Machine silently skipped", got)
	}
}

// The current < candidate case (both positive) isn't exercised by any
// envtest scenario, since those never have two sites draining at once.
func TestSoonestRequeue(t *testing.T) {
	cases := []struct {
		name               string
		current, candidate time.Duration
		want               time.Duration
	}{
		{"zero current (no candidate yet) takes any candidate", 0, 3 * time.Minute, 3 * time.Minute},
		{"non-positive candidate is ignored", 5 * time.Minute, 0, 5 * time.Minute},
		{"smaller positive candidate replaces current", 5 * time.Minute, 2 * time.Minute, 2 * time.Minute},
		{"larger positive candidate is kept out", 2 * time.Minute, 5 * time.Minute, 2 * time.Minute},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := soonestRequeue(tc.current, tc.candidate); got != tc.want {
				t.Errorf("soonestRequeue(%v, %v) = %v, want %v", tc.current, tc.candidate, got, tc.want)
			}
		})
	}
}

// Without this early return, every reconcile would rewrite status and
// re-trigger a reconcile.
func TestUpdateSeederStatus_NoOpWhenUnchanged(t *testing.T) {
	image := &keziov1alpha1.Image{
		ObjectMeta: metav1.ObjectMeta{Name: "os-image", Namespace: "default"},
		Status: keziov1alpha1.ImageStatus{
			Seeders: []keziov1alpha1.ImageSeederSiteStatus{
				{Site: "default/hq", MachineCount: 2},
			},
		},
	}

	updateCalled := false
	c := fake.NewClientBuilder().
		WithScheme(newPartitionStorageTestScheme(t)).
		WithObjects(image).
		WithStatusSubresource(&keziov1alpha1.Image{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, cli client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				updateCalled = true
				return cli.SubResource(subResourceName).Update(ctx, obj, opts...)
			},
		}).
		Build()

	r := &ImageReconciler{Client: c, Scheme: c.Scheme()}
	sites := map[string]int32{"default/hq": 2}
	if err := r.updateSeederStatus(context.Background(), image, sites, seederDegradedSites{}); err != nil {
		t.Fatalf("updateSeederStatus() error = %v", err)
	}
	if updateCalled {
		t.Errorf("updateSeederStatus() called Status().Update when the computed status already matched what was stored")
	}
}

// Covers the AvailableReplicas > 0 branch, which no envtest scenario can
// reach (envtest runs no controller-manager, so a fake Deployment's
// status never self-populates). The annotation is re-fetched from the
// client, since clearSeederTimeAnnotation clears it via Patch rather than
// mutating the caller's struct in place.
func TestCheckSeederDeploymentReady_AvailableClearsUnreadyAnnotation(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "seeder-recovering",
			Namespace: "default",
			Annotations: map[string]string{
				seederDeploymentUnreadySinceAnnotation: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
			},
		},
		Status: appsv1.DeploymentStatus{AvailableReplicas: 1},
	}
	c := fake.NewClientBuilder().
		WithScheme(newSeederUnitTestScheme(t)).
		WithObjects(dep).
		Build()
	r := newSeederUnitTestReconciler(c)

	unready, requeueAfter, err := r.checkSeederDeploymentReady(context.Background(), dep, time.Now())
	if err != nil {
		t.Fatalf("checkSeederDeploymentReady() error = %v", err)
	}
	if unready {
		t.Errorf("checkSeederDeploymentReady() unready = true, want false for AvailableReplicas > 0")
	}
	if requeueAfter != 0 {
		t.Errorf("checkSeederDeploymentReady() requeueAfter = %v, want 0", requeueAfter)
	}

	stored := &appsv1.Deployment{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(dep), stored); err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if _, marked := stored.Annotations[seederDeploymentUnreadySinceAnnotation]; marked {
		t.Errorf("stored Deployment still carries %s, want it cleared once the pod recovered", seederDeploymentUnreadySinceAnnotation)
	}
}

func TestCheckSeederDeploymentReady_AvailableNoAnnotationSkipsWrite(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "seeder-healthy", Namespace: "default"},
		Status:     appsv1.DeploymentStatus{AvailableReplicas: 1},
	}
	patchCalled := false
	c := fake.NewClientBuilder().
		WithScheme(newSeederUnitTestScheme(t)).
		WithObjects(dep).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cli client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				patchCalled = true
				return cli.Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	r := newSeederUnitTestReconciler(c)

	unready, requeueAfter, err := r.checkSeederDeploymentReady(context.Background(), dep, time.Now())
	if err != nil {
		t.Fatalf("checkSeederDeploymentReady() error = %v", err)
	}
	if unready {
		t.Errorf("checkSeederDeploymentReady() unready = true, want false for AvailableReplicas > 0")
	}
	if requeueAfter != 0 {
		t.Errorf("checkSeederDeploymentReady() requeueAfter = %v, want 0", requeueAfter)
	}
	if patchCalled {
		t.Errorf("checkSeederDeploymentReady() called Patch, want no write when there was no annotation to clear")
	}
}

// seederDeploymentName's determinism and length bound tests moved to
// internal/seederdeploy (Name, TestName) when it was exported.
