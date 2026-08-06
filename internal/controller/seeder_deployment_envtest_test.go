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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/store"
)

// seederDeploymentTestGracePeriod is a fixed, arbitrary grace period the
// tests below advance a fake clock past or stop short of; its value only
// needs to be distinct from zero and from the elapsed offsets the tests
// use.
const seederDeploymentTestGracePeriod = 5 * time.Minute

// newSeederDeploymentReconciler returns an ImageReconciler with
// SeederDeploymentConfig enabled and a fake clock the test controls via
// the returned *time.Time, so grace-period countdowns are exercised
// without sleeping.
func newSeederDeploymentReconciler() (*ImageReconciler, *time.Time) {
	now := time.Now()
	clock := &now
	r := &ImageReconciler{
		Client: k8sClient,
		Scheme: k8sClient.Scheme(),
		SeederDeployment: SeederDeploymentConfig{
			Image:       "ezio-seeder:test",
			StoreVolume: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			GracePeriod: seederDeploymentTestGracePeriod,
			Now:         func() time.Time { return *clock },
		},
	}
	return r, clock
}

// createSeederTestMachine creates a Machine in state referencing image
// at site, bypassing MachineReconciler (this suite only needs the
// Machine's spec/status shape the Image reconciler reads, not a real
// deploy walk). Each condition is stamped with a LastTransitionTime (the
// API server requires one) if the caller did not already set one.
func createSeederTestMachine(ctx context.Context, namespace, image, site, state string, conds ...metav1.Condition) *keziov1alpha1.Machine {
	machine := &keziov1alpha1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("m-%s", rand.String(5)),
			Namespace: namespace,
		},
		Spec: keziov1alpha1.MachineSpec{
			BMC: &keziov1alpha1.MachineBMC{
				Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
				CredentialsSecretRef: keziov1alpha1.SecretReference{Name: "seeder-test-bmc"},
			},
			BootMACAddress: fmt.Sprintf("aa:bb:cc:dd:ee:%02x", rand.Intn(250)+1),
			ImageRef:       &keziov1alpha1.NameRef{Name: image},
			NetworkSite:    site,
		},
	}
	ExpectWithOffset(1, k8sClient.Create(ctx, machine)).To(Succeed())

	for i := range conds {
		if conds[i].LastTransitionTime.IsZero() {
			conds[i].LastTransitionTime = metav1.Now()
		}
	}
	machine.Status.State = state
	machine.Status.Conditions = conds
	ExpectWithOffset(1, k8sClient.Status().Update(ctx, machine)).To(Succeed())
	return machine
}

// reconcileImage calls Reconcile enough times to actually run onChange:
// the first call on a freshly created Image only attaches the finalizer
// and returns early (see Reconcile's doc comment), so a single call
// would never reach reconcileSeederDeployments.
func reconcileImage(ctx context.Context, r *ImageReconciler, key types.NamespacedName) (reconcile.Result, error) {
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		return reconcile.Result{}, err
	}
	return r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
}

// seederDeploymentsForImage lists every Deployment owned by image (via
// seederDeploymentImageLabel), keyed by the site annotation
// reconcileSeederDeployments stamps.
func seederDeploymentsForImage(ctx context.Context, image *keziov1alpha1.Image) map[string]appsv1.Deployment {
	var deployments appsv1.DeploymentList
	ExpectWithOffset(1, k8sClient.List(ctx, &deployments,
		client.InNamespace(image.Namespace),
		client.MatchingLabels{seederDeploymentImageLabel: image.Name},
	)).To(Succeed())

	bySite := make(map[string]appsv1.Deployment, len(deployments.Items))
	for _, dep := range deployments.Items {
		bySite[dep.Annotations[seederDeploymentSiteAnnotation]] = dep
	}
	return bySite
}

// cleanupSeederTest deletes every given Machine, deletes any seeder
// Deployment left over for image (envtest runs no garbage collector, so
// an owner-reference alone never actually reaps one here), and then
// drains image's finalizer (deleteImageAndFinalize). reconcileImage runs
// the real ImageReconciler (unlike seeder_controller_test.go's
// createReadyImage callers, which never touch the reconciler and so
// never attach a finalizer), so leaving an Image behind here would leak
// a Ready Image - with the fixture's fixed content hash - into every
// other envtest spec in this shared suite that lists Ready Images
// (notably SeederReconciler's readyContentHashes).
func cleanupSeederTest(ctx context.Context, r *ImageReconciler, image *keziov1alpha1.Image, machines ...*keziov1alpha1.Machine) {
	for _, m := range machines {
		ExpectWithOffset(1, client.IgnoreNotFound(k8sClient.Delete(ctx, m))).To(Succeed())
	}
	for _, dep := range seederDeploymentsForImage(ctx, image) {
		ExpectWithOffset(1, client.IgnoreNotFound(k8sClient.Delete(ctx, &dep))).To(Succeed())
	}
	deleteImageAndFinalize(ctx, r, types.NamespacedName{Name: image.Name, Namespace: image.Namespace}, image)
}

var _ = Describe("Seeder Deployment lifecycle", func() {
	ctx := context.Background()

	It("does nothing when SeederDeployment is not configured", func() {
		image := createReadyImage(ctx, mustFixtureHash())
		r := &ImageReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		DeferCleanup(func() { cleanupSeederTest(ctx, r, image) })

		result, err := reconcileImage(ctx, r, types.NamespacedName{Name: image.Name, Namespace: image.Namespace})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeZero())

		bySite := seederDeploymentsForImage(ctx, image)
		Expect(bySite).To(BeEmpty())
	})

	It("creates a Deployment, owned by the Image, when a Machine enters Provisioning", func() {
		image := createReadyImage(ctx, mustFixtureHash())
		r, _ := newSeederDeploymentReconciler()
		key := types.NamespacedName{Name: image.Name, Namespace: image.Namespace}

		machine := createSeederTestMachine(ctx, image.Namespace, image.Name, "site-a", keziov1alpha1.MachineStateProvisioning)
		DeferCleanup(func() { cleanupSeederTest(ctx, r, image, machine) })

		_, err := reconcileImage(ctx, r, key)
		Expect(err).NotTo(HaveOccurred())

		bySite := seederDeploymentsForImage(ctx, image)
		Expect(bySite).To(HaveKey("site-a"))
		dep := bySite["site-a"]
		Expect(dep.OwnerReferences).To(HaveLen(1))
		Expect(dep.OwnerReferences[0].Name).To(Equal(image.Name))
		Expect(dep.OwnerReferences[0].Kind).To(Equal("Image"))

		updated := &keziov1alpha1.Image{}
		Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
		Expect(updated.Status.Seeders).To(ConsistOf(
			keziov1alpha1.ImageSeederSiteStatus{Site: "site-a", MachineCount: 1},
		))
	})

	It("counts every Provisioning Machine referencing the Image at one site", func() {
		image := createReadyImage(ctx, mustFixtureHash())
		r, _ := newSeederDeploymentReconciler()
		key := types.NamespacedName{Name: image.Name, Namespace: image.Namespace}

		m1 := createSeederTestMachine(ctx, image.Namespace, image.Name, "site-a", keziov1alpha1.MachineStateProvisioning)
		m2 := createSeederTestMachine(ctx, image.Namespace, image.Name, "site-a", keziov1alpha1.MachineStateProvisioning)
		DeferCleanup(func() { cleanupSeederTest(ctx, r, image, m1, m2) })

		_, err := reconcileImage(ctx, r, key)
		Expect(err).NotTo(HaveOccurred())

		bySite := seederDeploymentsForImage(ctx, image)
		Expect(bySite).To(HaveLen(1))

		updated := &keziov1alpha1.Image{}
		Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
		Expect(updated.Status.Seeders).To(ConsistOf(
			keziov1alpha1.ImageSeederSiteStatus{Site: "site-a", MachineCount: 2},
		))
	})

	It("creates one Deployment per site for the same Image", func() {
		image := createReadyImage(ctx, mustFixtureHash())
		r, _ := newSeederDeploymentReconciler()
		key := types.NamespacedName{Name: image.Name, Namespace: image.Namespace}

		mA := createSeederTestMachine(ctx, image.Namespace, image.Name, "site-a", keziov1alpha1.MachineStateProvisioning)
		mB := createSeederTestMachine(ctx, image.Namespace, image.Name, "site-b", keziov1alpha1.MachineStateProvisioning)
		DeferCleanup(func() { cleanupSeederTest(ctx, r, image, mA, mB) })

		_, err := reconcileImage(ctx, r, key)
		Expect(err).NotTo(HaveOccurred())

		bySite := seederDeploymentsForImage(ctx, image)
		Expect(bySite).To(HaveLen(2))
		Expect(bySite).To(HaveKey("site-a"))
		Expect(bySite).To(HaveKey("site-b"))
		Expect(bySite["site-a"].Name).NotTo(Equal(bySite["site-b"].Name))

		updated := &keziov1alpha1.Image{}
		Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
		Expect(updated.Status.Seeders).To(ConsistOf(
			keziov1alpha1.ImageSeederSiteStatus{Site: "site-a", MachineCount: 1},
			keziov1alpha1.ImageSeederSiteStatus{Site: "site-b", MachineCount: 1},
		))
	})

	It("keeps a Machine in error backoff retrying a provisioning failure holding its reference", func() {
		image := createReadyImage(ctx, mustFixtureHash())
		r, _ := newSeederDeploymentReconciler()
		key := types.NamespacedName{Name: image.Name, Namespace: image.Namespace}

		machine := createSeederTestMachine(ctx, image.Namespace, image.Name, "site-a", keziov1alpha1.MachineStateError,
			metav1.Condition{Type: keziov1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: reasonProvisionFailed})
		DeferCleanup(func() { cleanupSeederTest(ctx, r, image, machine) })

		_, err := reconcileImage(ctx, r, key)
		Expect(err).NotTo(HaveOccurred())

		bySite := seederDeploymentsForImage(ctx, image)
		Expect(bySite).To(HaveKey("site-a"))
	})

	It("does not create a Deployment for a Machine in error backoff retrying a register failure", func() {
		image := createReadyImage(ctx, mustFixtureHash())
		r, _ := newSeederDeploymentReconciler()
		key := types.NamespacedName{Name: image.Name, Namespace: image.Namespace}

		machine := createSeederTestMachine(ctx, image.Namespace, image.Name, "site-a", keziov1alpha1.MachineStateError,
			metav1.Condition{Type: keziov1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: reasonRegisterFailed})
		DeferCleanup(func() { cleanupSeederTest(ctx, r, image, machine) })

		_, err := reconcileImage(ctx, r, key)
		Expect(err).NotTo(HaveOccurred())

		Expect(seederDeploymentsForImage(ctx, image)).To(BeEmpty())
	})

	It("keeps the Deployment through the grace period after demand drops, then deletes it once the grace period elapses", func() {
		image := createReadyImage(ctx, mustFixtureHash())
		r, clock := newSeederDeploymentReconciler()
		key := types.NamespacedName{Name: image.Name, Namespace: image.Namespace}

		machine := createSeederTestMachine(ctx, image.Namespace, image.Name, "site-a", keziov1alpha1.MachineStateProvisioning)
		DeferCleanup(func() { cleanupSeederTest(ctx, r, image, machine) })

		_, err := reconcileImage(ctx, r, key)
		Expect(err).NotTo(HaveOccurred())
		Expect(seederDeploymentsForImage(ctx, image)).To(HaveKey("site-a"))

		By("the Machine finishing (Provisioned) - demand drops to zero")
		machine.Status.State = keziov1alpha1.MachineStateProvisioned
		Expect(k8sClient.Status().Update(ctx, machine)).To(Succeed())

		result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))

		By("the Deployment surviving immediately after demand drops (grace period, not immediate teardown)")
		bySite := seederDeploymentsForImage(ctx, image)
		Expect(bySite).To(HaveKey("site-a"))

		updated := &keziov1alpha1.Image{}
		Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
		Expect(updated.Status.Seeders).To(ConsistOf(
			keziov1alpha1.ImageSeederSiteStatus{Site: "site-a", MachineCount: 0},
		))

		By("reconciling again before the grace period elapses - still present")
		*clock = clock.Add(seederDeploymentTestGracePeriod / 2)
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(seederDeploymentsForImage(ctx, image)).To(HaveKey("site-a"))

		By("reconciling once the grace period has elapsed - the Deployment is deleted")
		*clock = clock.Add(seederDeploymentTestGracePeriod)
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(seederDeploymentsForImage(ctx, image)).NotTo(HaveKey("site-a"))

		Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
		Expect(updated.Status.Seeders).To(BeEmpty())
	})

	It("cancels the grace-period countdown when demand returns before it elapses", func() {
		image := createReadyImage(ctx, mustFixtureHash())
		r, clock := newSeederDeploymentReconciler()
		key := types.NamespacedName{Name: image.Name, Namespace: image.Namespace}

		machine := createSeederTestMachine(ctx, image.Namespace, image.Name, "site-a", keziov1alpha1.MachineStateProvisioning)
		var second *keziov1alpha1.Machine
		DeferCleanup(func() {
			machines := []*keziov1alpha1.Machine{machine}
			if second != nil {
				machines = append(machines, second)
			}
			cleanupSeederTest(ctx, r, image, machines...)
		})

		_, err := reconcileImage(ctx, r, key)
		Expect(err).NotTo(HaveOccurred())
		firstDeployment := seederDeploymentsForImage(ctx, image)["site-a"]

		By("the Machine finishing - demand drops to zero and the grace countdown starts")
		machine.Status.State = keziov1alpha1.MachineStateProvisioned
		Expect(k8sClient.Status().Update(ctx, machine)).To(Succeed())
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		By("a second Machine starting a deploy against the same Image and site before the grace period elapses")
		*clock = clock.Add(seederDeploymentTestGracePeriod / 2)
		second = createSeederTestMachine(ctx, image.Namespace, image.Name, "site-a", keziov1alpha1.MachineStateProvisioning)
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		By("advancing past the original grace deadline - the Deployment survives because the countdown was cancelled")
		*clock = clock.Add(seederDeploymentTestGracePeriod)
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		bySite := seederDeploymentsForImage(ctx, image)
		Expect(bySite).To(HaveKey("site-a"))
		Expect(bySite["site-a"].Name).To(Equal(firstDeployment.Name), "the same Deployment should have been reused, not recreated")

		updated := &keziov1alpha1.Image{}
		Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
		Expect(updated.Status.Seeders).To(ConsistOf(
			keziov1alpha1.ImageSeederSiteStatus{Site: "site-a", MachineCount: 1},
		))
	})

	It("is garbage collected when the Image itself is deleted", func() {
		image := createReadyImage(ctx, mustFixtureHash())
		r, _ := newSeederDeploymentReconciler()
		key := types.NamespacedName{Name: image.Name, Namespace: image.Namespace}

		machine := createSeederTestMachine(ctx, image.Namespace, image.Name, "site-a", keziov1alpha1.MachineStateProvisioning)
		DeferCleanup(func() { cleanupSeederTest(ctx, r, image, machine) })

		_, err := reconcileImage(ctx, r, key)
		Expect(err).NotTo(HaveOccurred())
		dep := seederDeploymentsForImage(ctx, image)["site-a"]
		Expect(dep.OwnerReferences).To(HaveLen(1))
		Expect(dep.OwnerReferences[0].Controller).NotTo(BeNil())
		Expect(*dep.OwnerReferences[0].Controller).To(BeTrue())
	})
})

// mustFixtureHash returns a fresh, valid partition content hash for
// createReadyImage; this suite only needs *an* Image with a Ready state
// and a non-empty status.partitions to reach - the seeder Deployment
// lifecycle logic never inspects the content itself.
func mustFixtureHash() store.InfoHash {
	_, h := writeFixtureContent()
	return h
}

// createSeederDeploymentPod creates a ReplicaSet owned by dep and a Pod
// owned by that ReplicaSet, with status.podIP already set to podIP -
// standing in for what a real Deployment controller and kubelet would
// otherwise produce (envtest runs neither), so podsForDeployment's
// ownership-chain lookup has something to find.
func createSeederDeploymentPod(ctx context.Context, dep *appsv1.Deployment, podIP string) {
	trueVal := true
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s", dep.Name, rand.String(5)),
			Namespace: dep.Namespace,
			Labels:    dep.Spec.Selector.MatchLabels,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       dep.Name,
				UID:        dep.UID,
				Controller: &trueVal,
			}},
		},
		Spec: appsv1.ReplicaSetSpec{
			Selector: dep.Spec.Selector,
			Template: dep.Spec.Template,
		},
	}
	ExpectWithOffset(1, k8sClient.Create(ctx, rs)).To(Succeed())

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s", rs.Name, rand.String(5)),
			Namespace: dep.Namespace,
			Labels:    dep.Spec.Selector.MatchLabels,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "ReplicaSet",
				Name:       rs.Name,
				UID:        rs.UID,
				Controller: &trueVal,
			}},
		},
		Spec: dep.Spec.Template.Spec,
	}
	ExpectWithOffset(1, k8sClient.Create(ctx, pod)).To(Succeed())
	pod.Status.PodIP = podIP
	ExpectWithOffset(1, k8sClient.Status().Update(ctx, pod)).To(Succeed())
}

var _ = Describe("Seeder Deployment content", func() {
	ctx := context.Background()

	// seederDeploymentContentTarget is the dial target
	// createSeederDeploymentPod's fixed pod IP resolves to, on the fixed
	// gRPC port every per-Image seeder container listens on.
	const seederDeploymentContentPodIP = "10.9.9.9"

	It("adds every Ready-Image content partition to the pod behind a newly created per-Image Deployment", func() {
		fixtureRoot, hash := writeFixtureContent()
		image := createReadyImage(ctx, hash)
		registry := newFakeSeederRegistry()

		r := &ImageReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			SeederDeployment: SeederDeploymentConfig{
				Image:       "ezio-seeder:test",
				StoreVolume: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
				TrackerURL:  "http://tracker.kezio-system.svc:6969/announce",
				StoreRoot:   fixtureRoot,
				Dial:        registry.dial,
			},
		}
		key := types.NamespacedName{Name: image.Name, Namespace: image.Namespace}

		machine := createSeederTestMachine(ctx, image.Namespace, image.Name, "site-a", keziov1alpha1.MachineStateProvisioning)
		DeferCleanup(func() { cleanupSeederTest(ctx, r, image, machine) })

		_, err := reconcileImage(ctx, r, key)
		Expect(err).NotTo(HaveOccurred())
		dep := seederDeploymentsForImage(ctx, image)["site-a"]

		createSeederDeploymentPod(ctx, &dep, seederDeploymentContentPodIP)

		// The pod did not exist during the reconcile above (its
		// Deployment had just been created), so a second pass is what
		// actually finds it and syncs content - the same "requeue and
		// pick it up next time" shape the rest of this reconciler uses.
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		target := fmt.Sprintf("%s:%d", seederDeploymentContentPodIP, seederDeploymentGRPCPort)
		daemon := registry.daemons[target]
		Expect(daemon).NotTo(BeNil(), "expected a dial to the per-Image seeder pod's own address")
		Expect(daemon.torrents).To(HaveKey(hash.String()))
	})

	It("does not dial any pod when SeederDeployment carries no tracker/store configuration", func() {
		image := createReadyImage(ctx, mustFixtureHash())
		r, _ := newSeederDeploymentReconciler()
		dialed := false
		r.SeederDeployment.Dial = func(string) (SeederEZIOClient, error) {
			dialed = true
			return nil, fmt.Errorf("dial should not have been called")
		}
		key := types.NamespacedName{Name: image.Name, Namespace: image.Namespace}

		machine := createSeederTestMachine(ctx, image.Namespace, image.Name, "site-a", keziov1alpha1.MachineStateProvisioning)
		DeferCleanup(func() { cleanupSeederTest(ctx, r, image, machine) })

		_, err := reconcileImage(ctx, r, key)
		Expect(err).NotTo(HaveOccurred())
		dep := seederDeploymentsForImage(ctx, image)["site-a"]
		createSeederDeploymentPod(ctx, &dep, seederDeploymentContentPodIP)

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(dialed).To(BeFalse(), "contentEnabled() is false (no TrackerURL/StoreRoot) - nothing should have dialed the pod")
	})
})
