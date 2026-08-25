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

package controller

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/ingest"
	"github.com/tjjh89017/kezio/internal/planbuild"
	"github.com/tjjh89017/kezio/internal/seederdeploy"
	"github.com/tjjh89017/kezio/internal/store"
)

// imageSeederTestHash returns a distinct, valid-looking 40-character hex
// info hash for seq, keeping this file's sequence independent from the
// other test files' own.
func imageSeederTestHash(seq int) string {
	return fmt.Sprintf("%040x", seq+5000)
}

// mustCreateSeedingSite creates a Subnet declaring seederNetworkRef (the
// Site's data-plane segment) and a Site whose seederSubnetRef names it,
// both in "default". Returns the Site's identity string
// (sitederive.SiteIdentity's format).
func mustCreateSeedingSite(ctx context.Context, subnetName, siteName string) string {
	subnet := &keziov1alpha2.Subnet{
		ObjectMeta: metav1.ObjectMeta{Name: subnetName, Namespace: "default"},
		Spec: keziov1alpha2.SubnetSpec{
			SiteRef:          keziov1alpha2.NameRef{Name: siteName},
			CIDR:             "198.51.100.0/24",
			SeederNetworkRef: &keziov1alpha2.NameRef{Name: subnetName + "-nad"},
		},
	}
	Expect(k8sClient.Create(ctx, subnet)).To(Succeed())

	site := &keziov1alpha2.Site{
		ObjectMeta: metav1.ObjectMeta{Name: siteName, Namespace: "default"},
		Spec:       keziov1alpha2.SiteSpec{SeederSubnetRef: &keziov1alpha2.NameRef{Name: subnetName}},
	}
	Expect(k8sClient.Create(ctx, site)).To(Succeed())

	return "default/" + siteName
}

// mustSetSiteTrackerURL sets siteName's status.trackerURL directly - the
// Site reconciler is not running in these tests, so this stands in for
// it, the same way mustCreatePodForDeployment stands in for a scheduled
// pod.
func mustSetSiteTrackerURL(ctx context.Context, siteName, trackerURL string) {
	var site keziov1alpha2.Site
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: siteName, Namespace: "default"}, &site)).To(Succeed())
	site.Status.TrackerURL = trackerURL
	Expect(k8sClient.Status().Update(ctx, &site)).To(Succeed())
}

// mustCreateMachineSubnet creates a boot-plane Subnet a Machine can name
// in spec.subnetRef, belonging to siteName.
func mustCreateMachineSubnet(ctx context.Context, subnetName, siteName string) {
	subnet := &keziov1alpha2.Subnet{
		ObjectMeta: metav1.ObjectMeta{Name: subnetName, Namespace: "default"},
		Spec: keziov1alpha2.SubnetSpec{
			SiteRef:         keziov1alpha2.NameRef{Name: siteName},
			CIDR:            "192.0.2.0/24",
			BootdServerIP:   "192.0.2.2",
			BootdNetworkRef: &keziov1alpha2.NameRef{Name: subnetName + "-bootd-nad"},
			DHCP:            &keziov1alpha2.SubnetDHCP{Mode: keziov1alpha2.SubnetDHCPModeProxy},
		},
	}
	Expect(k8sClient.Create(ctx, subnet)).To(Succeed())
}

// newTestMachineOnSubnet builds an (uncreated) Machine naming imageName
// and subnetName - the minimal shape sitederive.Resolve needs, mirroring
// newTestMachine but with a caller-chosen subnetRef instead of a
// never-resolved placeholder.
func newTestMachineOnSubnet(name, imageName, subnetName string) *keziov1alpha2.Machine {
	return &keziov1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: keziov1alpha2.MachineSpec{
			BMC: keziov1alpha2.MachineBMC{
				Address:              "redfish://198.51.100.10/redfish/v1/Systems/1",
				CredentialsSecretRef: keziov1alpha2.SecretReference{Name: name + "-bmc-creds"},
			},
			ImageRef:  &keziov1alpha2.NameRef{Name: imageName},
			SubnetRef: keziov1alpha2.NameRef{Name: subnetName},
		},
	}
}

var _ = Describe("Image Controller seeder placement", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	newSeederImageReconciler := func(seeder ImageSeederConfig) *ImageReconciler {
		return &ImageReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Seeder: seeder}
	}

	It("produces exactly one seeder Deployment for two Machines on two different Subnets of one Site", func() {
		siteIdentity := mustCreateSeedingSite(ctx, "seed-subnet-701", "site-701")
		mustCreateMachineSubnet(ctx, "machine-subnet-701a", "site-701")
		mustCreateMachineSubnet(ctx, "machine-subnet-701b", "site-701")

		contentName := "pc-" + imageSeederTestHash(701)
		createReadyContent(ctx, contentName)

		img := newTestImageWithSlots("image-701", []keziov1alpha2.ImageSlot{
			{Number: 1, Role: keziov1alpha2.PartitionRoleData, ContentRef: &keziov1alpha2.NameRef{Name: contentName}},
		})
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		machineA := newTestMachineOnSubnet("machine-701a", img.Name, "machine-subnet-701a")
		Expect(k8sClient.Create(ctx, machineA)).To(Succeed())
		machineB := newTestMachineOnSubnet("machine-701b", img.Name, "machine-subnet-701b")
		Expect(k8sClient.Create(ctx, machineB)).To(Succeed())

		r := newSeederImageReconciler(ImageSeederConfig{Image: "example.test/kezio-seeder:test"})
		nn := types.NamespacedName{Name: img.Name, Namespace: "default"}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		depName := seederdeploy.Name(img.Name, siteIdentity)
		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: depName, Namespace: "default"}, &dep)).To(Succeed())
		Expect(dep.Spec.Replicas).NotTo(BeNil())
		Expect(*dep.Spec.Replicas).To(Equal(int32(1)))

		// The content is mounted by object name, not by info hash: the PVC
		// belongs to the PartitionContent, and the seeder derives the info
		// hash itself from the torrent.info it finds there.
		Expect(dep.Spec.Template.Spec.Volumes).To(HaveLen(1))
		Expect(dep.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName).To(Equal(store.PVCName(contentName)))
		Expect(dep.Spec.Template.Spec.Containers[0].VolumeMounts).To(ContainElement(corev1.VolumeMount{
			Name:      dep.Spec.Template.Spec.Volumes[0].Name,
			MountPath: ingest.ContentMountPath(contentName),
			ReadOnly:  true,
		}))

		// Exactly one Deployment for this Image: no second one under any
		// other name exists.
		var deployments appsv1.DeploymentList
		Expect(k8sClient.List(ctx, &deployments, client.InNamespace("default"), client.MatchingLabels{
			partitionContentAppComponentLabel: partitionContentSeederComponentValue,
		})).To(Succeed())
		count := 0
		for i := range deployments.Items {
			if metav1.IsControlledBy(&deployments.Items[i], img) {
				count++
			}
		}
		Expect(count).To(Equal(1))
	})

	It("produces two Deployments with distinct names for two Sites of the same Image", func() {
		siteIdentityA := mustCreateSeedingSite(ctx, "seed-subnet-702a", "site-702a")
		siteIdentityB := mustCreateSeedingSite(ctx, "seed-subnet-702b", "site-702b")
		mustCreateMachineSubnet(ctx, "machine-subnet-702a", "site-702a")
		mustCreateMachineSubnet(ctx, "machine-subnet-702b", "site-702b")

		contentName := "pc-" + imageSeederTestHash(702)
		createReadyContent(ctx, contentName)

		img := newTestImageWithSlots("image-702", []keziov1alpha2.ImageSlot{
			{Number: 1, Role: keziov1alpha2.PartitionRoleData, ContentRef: &keziov1alpha2.NameRef{Name: contentName}},
		})
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		machineA := newTestMachineOnSubnet("machine-702a", img.Name, "machine-subnet-702a")
		Expect(k8sClient.Create(ctx, machineA)).To(Succeed())
		machineB := newTestMachineOnSubnet("machine-702b", img.Name, "machine-subnet-702b")
		Expect(k8sClient.Create(ctx, machineB)).To(Succeed())

		r := newSeederImageReconciler(ImageSeederConfig{Image: "example.test/kezio-seeder:test"})
		nn := types.NamespacedName{Name: img.Name, Namespace: "default"}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		nameA := seederdeploy.Name(img.Name, siteIdentityA)
		nameB := seederdeploy.Name(img.Name, siteIdentityB)
		Expect(nameA).NotTo(Equal(nameB))

		var depA, depB appsv1.Deployment
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nameA, Namespace: "default"}, &depA)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nameB, Namespace: "default"}, &depB)).To(Succeed())
	})

	// The selector defect (fixed in image_seeder_placement.go by
	// imageSeederInstanceLabel): two seeder Deployments in one namespace
	// must never be able to select each other's pods, and
	// planbuild.Builder.resolveTorrentURL must never return a pod that does
	// not hold the content it was asked for. Confirmed to fail against the
	// pre-fix shape (a fixed two-label selector shared by every seeder
	// Deployment, matching the original buildSeederDeployment's package-level
	// `labels` literal) by temporarily reproducing that selector here and
	// observing both assertions below fail - see this task's report for
	// the exact steps.
	It("keeps two seeder Deployments' pod selectors from matching each other's pods", func() {
		siteIdentity := mustCreateSeedingSite(ctx, "seed-subnet-703", "site-703")
		mustCreateMachineSubnet(ctx, "machine-subnet-703", "site-703")

		contentNameA := "pc-" + imageSeederTestHash(703)
		contentNameB := "pc-" + imageSeederTestHash(704)
		createReadyContent(ctx, contentNameA)
		createReadyContent(ctx, contentNameB)

		imgA := newTestImageWithSlots("image-703a", []keziov1alpha2.ImageSlot{
			{Number: 1, Role: keziov1alpha2.PartitionRoleData, ContentRef: &keziov1alpha2.NameRef{Name: contentNameA}},
		})
		Expect(k8sClient.Create(ctx, imgA)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, imgA) })
		imgB := newTestImageWithSlots("image-703b", []keziov1alpha2.ImageSlot{
			{Number: 1, Role: keziov1alpha2.PartitionRoleData, ContentRef: &keziov1alpha2.NameRef{Name: contentNameB}},
		})
		Expect(k8sClient.Create(ctx, imgB)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, imgB) })

		machineA := newTestMachineOnSubnet("machine-703a", imgA.Name, "machine-subnet-703")
		Expect(k8sClient.Create(ctx, machineA)).To(Succeed())
		machineB := newTestMachineOnSubnet("machine-703b", imgB.Name, "machine-subnet-703")
		Expect(k8sClient.Create(ctx, machineB)).To(Succeed())

		r := newSeederImageReconciler(ImageSeederConfig{Image: "example.test/kezio-seeder:test"})
		nnA := types.NamespacedName{Name: imgA.Name, Namespace: "default"}
		nnB := types.NamespacedName{Name: imgB.Name, Namespace: "default"}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nnA})
		Expect(err).NotTo(HaveOccurred())
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nnB})
		Expect(err).NotTo(HaveOccurred())

		depNameA := seederdeploy.Name(imgA.Name, siteIdentity)
		depNameB := seederdeploy.Name(imgB.Name, siteIdentity)
		var depA, depB appsv1.Deployment
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: depNameA, Namespace: "default"}, &depA)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: depNameB, Namespace: "default"}, &depB)).To(Succeed())

		// envtest runs no kubelet: stand in for a scheduled pod under each
		// Deployment's own selector, each with a distinct PodIP.
		podA := mustCreatePodForDeployment(ctx, &depA, "10.10.0.1")
		podB := mustCreatePodForDeployment(ctx, &depB, "10.10.0.2")

		var listA corev1.PodList
		Expect(k8sClient.List(ctx, &listA, client.InNamespace("default"), client.MatchingLabels(depA.Spec.Selector.MatchLabels))).To(Succeed())
		Expect(listA.Items).To(HaveLen(1))
		Expect(listA.Items[0].Name).To(Equal(podA.Name), "dep A's selector must never match dep B's pod")

		var listB corev1.PodList
		Expect(k8sClient.List(ctx, &listB, client.InNamespace("default"), client.MatchingLabels(depB.Spec.Selector.MatchLabels))).To(Succeed())
		Expect(listB.Items).To(HaveLen(1))
		Expect(listB.Items[0].Name).To(Equal(podB.Name), "dep B's selector must never match dep A's pod")

		// The plan builder resolves the pod the same way (its own
		// resolveTorrentURL Gets the Deployment by name, then lists pods
		// by that Deployment's own selector) - see build_envtest_test.go's
		// TestBuilder_Build_Envtest for the coverage of that path end to
		// end. What this test adds is proving the two Deployments' own
		// selectors are themselves mutually exclusive at the source.
		Expect(depA.Spec.Selector.MatchLabels).NotTo(Equal(depB.Spec.Selector.MatchLabels))

		// Drive planbuild.Builder itself - the actual caller
		// resolveTorrentURL exists for - through both (Image, Site)
		// pairs with both Deployments and both pods present at once, and
		// pin that each resolves to its own pod's address, never the
		// other's.
		mhA := &keziov1alpha2.MachineHardware{
			ObjectMeta: metav1.ObjectMeta{Name: "seeder-lookup-703a", Namespace: "default"},
			Spec:       keziov1alpha2.MachineHardwareSpec{Disks: []keziov1alpha2.MachineHardwareDisk{{DeviceName: "/dev/vda", SizeBytes: 32 << 30}}},
		}
		Expect(k8sClient.Create(ctx, mhA)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, mhA) })
		mhB := &keziov1alpha2.MachineHardware{
			ObjectMeta: metav1.ObjectMeta{Name: "seeder-lookup-703b", Namespace: "default"},
			Spec:       keziov1alpha2.MachineHardwareSpec{Disks: []keziov1alpha2.MachineHardwareDisk{{DeviceName: "/dev/vda", SizeBytes: 32 << 30}}},
		}
		Expect(k8sClient.Create(ctx, mhB)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, mhB) })

		// DataImages, not ImageRef: neither image carries an ESP, and a
		// dataImages-only Machine attaches no default finalize hook (see
		// planbuild.Builder.resolveMachineHooks), so nothing here needs a
		// PostHook fixture.
		builder := &planbuild.Builder{Client: k8sClient}

		machineForA := &keziov1alpha2.Machine{
			ObjectMeta: metav1.ObjectMeta{Name: mhA.Name, Namespace: "default"},
			Spec: keziov1alpha2.MachineSpec{
				DataImages: []keziov1alpha2.MachineDataImage{{ImageRef: keziov1alpha2.NameRef{Name: imgA.Name}}},
				SubnetRef:  keziov1alpha2.NameRef{Name: "machine-subnet-703"},
			},
		}
		runA := &keziov1alpha2.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "run-703a", UID: types.UID("run-703a")}}
		planA, _, err := builder.Build(ctx, machineForA, runA)
		Expect(err).NotTo(HaveOccurred())
		Expect(planA.DataImages).To(HaveLen(1))
		Expect(planA.DataImages[0].Slots).To(HaveLen(1))
		torrentA := planA.DataImages[0].Slots[0].Torrent
		Expect(torrentA).NotTo(BeNil())
		Expect(torrentA.URL).To(ContainSubstring(podA.Status.PodIP), "must resolve to dep A's own pod")
		Expect(torrentA.URL).NotTo(ContainSubstring(podB.Status.PodIP), "must never resolve to dep B's pod")

		machineForB := &keziov1alpha2.Machine{
			ObjectMeta: metav1.ObjectMeta{Name: mhB.Name, Namespace: "default"},
			Spec: keziov1alpha2.MachineSpec{
				DataImages: []keziov1alpha2.MachineDataImage{{ImageRef: keziov1alpha2.NameRef{Name: imgB.Name}}},
				SubnetRef:  keziov1alpha2.NameRef{Name: "machine-subnet-703"},
			},
		}
		runB := &keziov1alpha2.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "run-703b", UID: types.UID("run-703b")}}
		planB, _, err := builder.Build(ctx, machineForB, runB)
		Expect(err).NotTo(HaveOccurred())
		Expect(planB.DataImages).To(HaveLen(1))
		Expect(planB.DataImages[0].Slots).To(HaveLen(1))
		torrentB := planB.DataImages[0].Slots[0].Torrent
		Expect(torrentB).NotTo(BeNil())
		Expect(torrentB.URL).To(ContainSubstring(podB.Status.PodIP), "must resolve to dep B's own pod")
		Expect(torrentB.URL).NotTo(ContainSubstring(podA.Status.PodIP), "must never resolve to dep A's pod")
	})

	It("gives each container the environment its contract requires, pins the BitTorrent port, and carries the Site's tracker URL", func() {
		siteIdentity := mustCreateSeedingSite(ctx, "seed-subnet-708", "site-708")
		mustSetSiteTrackerURL(ctx, "site-708", "http://198.51.100.9:6969/announce")
		mustCreateMachineSubnet(ctx, "machine-subnet-708", "site-708")

		contentName := "pc-" + imageSeederTestHash(708)
		createReadyContent(ctx, contentName)

		img := newTestImageWithSlots("image-708", []keziov1alpha2.ImageSlot{
			{Number: 1, Role: keziov1alpha2.PartitionRoleData, ContentRef: &keziov1alpha2.NameRef{Name: contentName}},
		})
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		machine := newTestMachineOnSubnet("machine-708", img.Name, "machine-subnet-708")
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())

		r := newSeederImageReconciler(ImageSeederConfig{Image: "example.test/kezio-seeder:test", MaxUploads: 11, MaxConnections: 33})
		nn := types.NamespacedName{Name: img.Name, Namespace: "default"}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		depName := seederdeploy.Name(img.Name, siteIdentity)
		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: depName, Namespace: "default"}, &dep)).To(Succeed())

		ezioContainer := mustFindContainer(&dep, "ezio")
		Expect(mustEnvValue(ezioContainer.Env, "EZIO_GRPC_LISTEN")).To(Equal("0.0.0.0:50051"))
		Expect(mustEnvValue(ezioContainer.Env, "EZIO_BT_PORT")).To(Equal("16881"))
		Expect(ezioContainer.Ports).To(ContainElement(corev1.ContainerPort{
			Name: "bt", ContainerPort: seederdeploy.EzioBTPort, Protocol: corev1.ProtocolTCP,
		}), "the declared container port must match the pinned EZIO_BT_PORT")

		registerContainer := mustFindContainer(&dep, "seeder-register")
		Expect(mustEnvValue(registerContainer.Env, "CONTENT_ROOT")).To(Equal(ingest.ContentMountRoot))
		Expect(mustEnvValue(registerContainer.Env, "EZIO_TARGET")).To(Equal("127.0.0.1:50051"))
		Expect(mustEnvValue(registerContainer.Env, "EZIO_MAX_UPLOADS")).To(Equal("11"))
		Expect(mustEnvValue(registerContainer.Env, "EZIO_MAX_CONNECTIONS")).To(Equal("33"))
		Expect(mustEnvValue(registerContainer.Env, "TRACKER_URL")).To(Equal("http://198.51.100.9:6969/announce"))
	})

	It("carries two different Sites' distinct tracker URLs on their own seeder Deployments", func() {
		siteIdentityA := mustCreateSeedingSite(ctx, "seed-subnet-709a", "site-709a")
		siteIdentityB := mustCreateSeedingSite(ctx, "seed-subnet-709b", "site-709b")
		mustSetSiteTrackerURL(ctx, "site-709a", "http://198.51.100.1:6969/announce")
		mustSetSiteTrackerURL(ctx, "site-709b", "http://198.51.100.2:6969/announce")
		mustCreateMachineSubnet(ctx, "machine-subnet-709a", "site-709a")
		mustCreateMachineSubnet(ctx, "machine-subnet-709b", "site-709b")

		contentName := "pc-" + imageSeederTestHash(709)
		createReadyContent(ctx, contentName)

		img := newTestImageWithSlots("image-709", []keziov1alpha2.ImageSlot{
			{Number: 1, Role: keziov1alpha2.PartitionRoleData, ContentRef: &keziov1alpha2.NameRef{Name: contentName}},
		})
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		machineA := newTestMachineOnSubnet("machine-709a", img.Name, "machine-subnet-709a")
		Expect(k8sClient.Create(ctx, machineA)).To(Succeed())
		machineB := newTestMachineOnSubnet("machine-709b", img.Name, "machine-subnet-709b")
		Expect(k8sClient.Create(ctx, machineB)).To(Succeed())

		r := newSeederImageReconciler(ImageSeederConfig{Image: "example.test/kezio-seeder:test"})
		nn := types.NamespacedName{Name: img.Name, Namespace: "default"}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		var depA, depB appsv1.Deployment
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: seederdeploy.Name(img.Name, siteIdentityA), Namespace: "default"}, &depA)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: seederdeploy.Name(img.Name, siteIdentityB), Namespace: "default"}, &depB)).To(Succeed())

		trackerURLA := mustEnvValue(mustFindContainer(&depA, "seeder-register").Env, "TRACKER_URL")
		trackerURLB := mustEnvValue(mustFindContainer(&depB, "seeder-register").Env, "TRACKER_URL")
		Expect(trackerURLA).To(Equal("http://198.51.100.1:6969/announce"))
		Expect(trackerURLB).To(Equal("http://198.51.100.2:6969/announce"))
		Expect(trackerURLA).NotTo(Equal(trackerURLB))
	})
})

// mustFindContainer returns the container named name from dep's pod
// template, failing the test immediately if it is not found.
func mustFindContainer(dep *appsv1.Deployment, name string) *corev1.Container {
	for i := range dep.Spec.Template.Spec.Containers {
		if dep.Spec.Template.Spec.Containers[i].Name == name {
			return &dep.Spec.Template.Spec.Containers[i]
		}
	}
	Fail(fmt.Sprintf("container %q not found in Deployment %s", name, dep.Name))
	return nil
}

// mustEnvValue returns env's value for name, failing the test via Gomega
// if it is absent - the Ginkgo-style counterpart to
// subnet_bootd_deployment_test.go's plain-testing.T envMust.
func mustEnvValue(env []corev1.EnvVar, name string) string {
	got, ok := envValue(env, name)
	Expect(ok).To(BeTrue(), fmt.Sprintf("env %s is not set", name))
	return got
}

// mustCreatePodForDeployment creates a Pod carrying dep's pod template
// labels plus podIP already set, standing in for a scheduled pod - the
// same technique build_envtest_test.go's fixtures use, since envtest runs
// no kubelet to ever assign one itself.
func mustCreatePodForDeployment(ctx context.Context, dep *appsv1.Deployment, podIP string) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dep.Name + "-pod",
			Namespace: dep.Namespace,
			Labels:    dep.Spec.Template.Labels,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "ezio", Image: "example/ezio:test"}},
		},
	}
	Expect(k8sClient.Create(ctx, pod)).To(Succeed())
	pod.Status.PodIP = podIP
	Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
	return pod
}

// testClock is a settable time source for driving ImageSeederConfig.Now
// in tests, so the grace-period countdown advances without sleeping real
// time.
type testClock struct{ t time.Time }

func (c *testClock) now() time.Time { return c.t }

var _ = Describe("PartitionContent status seeder reflection", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("carries a real per-Site status.seeders[] entry once a seeder Deployment is Available, never the retired placeholder site", func() {
		siteIdentity := mustCreateSeedingSite(ctx, "seed-subnet-707", "site-707")
		mustCreateMachineSubnet(ctx, "machine-subnet-707", "site-707")

		contentName := "pc-" + imageSeederTestHash(707)
		pc := createReadyContent(ctx, contentName)

		img := newTestImageWithSlots("image-707", []keziov1alpha2.ImageSlot{
			{Number: 1, Role: keziov1alpha2.PartitionRoleData, ContentRef: &keziov1alpha2.NameRef{Name: contentName}},
		})
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		machine := newTestMachineOnSubnet("machine-707", img.Name, "machine-subnet-707")
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())

		ir := &ImageReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Seeder: ImageSeederConfig{Image: "example.test/kezio-seeder:test"}}
		imgNN := types.NamespacedName{Name: img.Name, Namespace: "default"}
		_, err := ir.Reconcile(ctx, reconcile.Request{NamespacedName: imgNN})
		Expect(err).NotTo(HaveOccurred())

		depName := seederdeploy.Name(img.Name, siteIdentity)
		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: depName, Namespace: "default"}, &dep)).To(Succeed())
		dep.Status.Replicas = 1
		dep.Status.ReadyReplicas = 1
		dep.Status.AvailableReplicas = 1
		Expect(k8sClient.Status().Update(ctx, &dep)).To(Succeed())

		pr, cancel := newIndexedReconciler(ctx, PartitionContentPublishConfig{})
		DeferCleanup(cancel)
		pcNN := types.NamespacedName{Name: pc.Name, Namespace: "default"}
		reconcileAddsFinalizer(ctx, pr, pcNN)
		_, err = pr.Reconcile(ctx, reconcile.Request{NamespacedName: pcNN})
		Expect(err).NotTo(HaveOccurred())

		var got keziov1alpha2.PartitionContent
		Expect(k8sClient.Get(ctx, pcNN, &got)).To(Succeed())
		Expect(got.Status.Seeders).To(ConsistOf(keziov1alpha2.PartitionContentSeederSite{Site: siteIdentity, MachineCount: 1}))
		for _, s := range got.Status.Seeders {
			Expect(s.Site).NotTo(Equal("default"), "must never report the retired placeholder site string")
		}
	})
})

var _ = Describe("Image Controller seeder demand grouping", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("drives one Site's seeder Deployment through demand 1 -> 0 -> 1 inside the grace period", func() {
		siteIdentity := mustCreateSeedingSite(ctx, "seed-subnet-705", "site-705")
		mustCreateMachineSubnet(ctx, "machine-subnet-705", "site-705")

		contentName := "pc-" + imageSeederTestHash(705)
		createReadyContent(ctx, contentName)

		img := newTestImageWithSlots("image-705", []keziov1alpha2.ImageSlot{
			{Number: 1, Role: keziov1alpha2.PartitionRoleData, ContentRef: &keziov1alpha2.NameRef{Name: contentName}},
		})
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		machine := newTestMachineOnSubnet("machine-705", img.Name, "machine-subnet-705")
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())

		clock := &testClock{t: time.Now()}
		grace := 10 * time.Minute
		r := &ImageReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			Seeder: ImageSeederConfig{Image: "example.test/kezio-seeder:test", GracePeriod: grace, Now: clock.now},
		}
		nn := types.NamespacedName{Name: img.Name, Namespace: "default"}

		// Demand: 1.
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		depName := seederdeploy.Name(img.Name, siteIdentity)
		depKey := types.NamespacedName{Name: depName, Namespace: "default"}
		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, depKey, &dep)).To(Succeed())
		firstUID := dep.UID

		// Demand: 0. The Deployment must survive with a grace-period
		// countdown started, not be deleted immediately.
		Expect(k8sClient.Delete(ctx, machine)).To(Succeed())
		result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(grace))
		Expect(k8sClient.Get(ctx, depKey, &dep)).To(Succeed())
		Expect(dep.Annotations).To(HaveKey(imageSeederEmptySinceAnnotation))
		Expect(dep.UID).To(Equal(firstUID))

		// Demand: 1 again, mid-grace. The countdown must cancel and the
		// same Deployment must survive (never deleted and recreated).
		machine.ResourceVersion = ""
		machine.UID = ""
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, depKey, &dep)).To(Succeed())
		Expect(dep.Annotations).NotTo(HaveKey(imageSeederEmptySinceAnnotation))
		Expect(dep.UID).To(Equal(firstUID))
	})

	It("does not let a Machine with an unresolvable Site block another Site's seeder demand", func() {
		siteIdentity := mustCreateSeedingSite(ctx, "seed-subnet-706", "site-706")
		mustCreateMachineSubnet(ctx, "machine-subnet-706", "site-706")

		contentName := "pc-" + imageSeederTestHash(706)
		createReadyContent(ctx, contentName)

		img := newTestImageWithSlots("image-706", []keziov1alpha2.ImageSlot{
			{Number: 1, Role: keziov1alpha2.PartitionRoleData, ContentRef: &keziov1alpha2.NameRef{Name: contentName}},
		})
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		// A Machine naming a Subnet that does not exist: its Site can
		// never resolve.
		broken := newTestMachineOnSubnet("machine-706-broken", img.Name, "subnet-does-not-exist-706")
		Expect(k8sClient.Create(ctx, broken)).To(Succeed())
		// A Machine on a real, resolvable Site.
		good := newTestMachineOnSubnet("machine-706-good", img.Name, "machine-subnet-706")
		Expect(k8sClient.Create(ctx, good)).To(Succeed())

		r := &ImageReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Seeder: ImageSeederConfig{Image: "example.test/kezio-seeder:test"}}
		nn := types.NamespacedName{Name: img.Name, Namespace: "default"}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		depName := seederdeploy.Name(img.Name, siteIdentity)
		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: depName, Namespace: "default"}, &dep)).To(Succeed())
		Expect(dep.Spec.Replicas).NotTo(BeNil())
		Expect(*dep.Spec.Replicas).To(Equal(int32(1)))
	})
})

var _ = Describe("Image Controller seeder degradation reporting", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	newSeederImageReconciler := func(seeder ImageSeederConfig) *ImageReconciler {
		return &ImageReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Seeder: seeder}
	}

	mustGetImage := func(name string) *keziov1alpha2.Image {
		var img keziov1alpha2.Image
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, &img)).To(Succeed())
		return &img
	}

	It("surfaces a demanded seeder Deployment whose pod never becomes Ready, and stops surfacing once it does", func() {
		siteIdentity := mustCreateSeedingSite(ctx, "seed-subnet-710", "site-710")
		mustCreateMachineSubnet(ctx, "machine-subnet-710", "site-710")

		contentName := "pc-" + imageSeederTestHash(710)
		createReadyContent(ctx, contentName)

		img := newTestImageWithSlots("image-710", []keziov1alpha2.ImageSlot{
			{Number: 1, Role: keziov1alpha2.PartitionRoleData, ContentRef: &keziov1alpha2.NameRef{Name: contentName}},
		})
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		machine := newTestMachineOnSubnet("machine-710", img.Name, "machine-subnet-710")
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())

		r := newSeederImageReconciler(ImageSeederConfig{Image: "example.test/kezio-seeder:test"})
		nn := types.NamespacedName{Name: img.Name, Namespace: "default"}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		depName := seederdeploy.Name(img.Name, siteIdentity)
		depKey := types.NamespacedName{Name: depName, Namespace: "default"}
		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, depKey, &dep)).To(Succeed())

		// The pod never becomes Ready: AvailableReplicas stays at its
		// envtest default of 0. Reconciling again must record the
		// unready-since annotation and surface it on the Image.
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, depKey, &dep)).To(Succeed())
		Expect(dep.Annotations).To(HaveKey(imageSeederUnreadySinceAnnotation))

		img = mustGetImage(img.Name)
		cond := meta.FindStatusCondition(img.Status.Conditions, keziov1alpha2.ImageConditionSeederDegraded)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal("SeederPodUnready"))
		Expect(cond.Message).To(ContainSubstring(siteIdentity))

		// The pod becomes Ready: the annotation and the condition must
		// both clear.
		Expect(k8sClient.Get(ctx, depKey, &dep)).To(Succeed())
		dep.Status.Replicas = 1
		dep.Status.ReadyReplicas = 1
		dep.Status.AvailableReplicas = 1
		Expect(k8sClient.Status().Update(ctx, &dep)).To(Succeed())

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, depKey, &dep)).To(Succeed())
		Expect(dep.Annotations).NotTo(HaveKey(imageSeederUnreadySinceAnnotation))

		img = mustGetImage(img.Name)
		Expect(meta.FindStatusCondition(img.Status.Conditions, keziov1alpha2.ImageConditionSeederDegraded)).To(BeNil())
	})

	It("leaves a foreign Deployment under the expected name untouched and reports it, never adopting it", func() {
		siteIdentity := mustCreateSeedingSite(ctx, "seed-subnet-711", "site-711")
		mustCreateMachineSubnet(ctx, "machine-subnet-711", "site-711")

		contentName := "pc-" + imageSeederTestHash(711)
		createReadyContent(ctx, contentName)

		img := newTestImageWithSlots("image-711", []keziov1alpha2.ImageSlot{
			{Number: 1, Role: keziov1alpha2.PartitionRoleData, ContentRef: &keziov1alpha2.NameRef{Name: contentName}},
		})
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		depName := seederdeploy.Name(img.Name, siteIdentity)
		foreignLabels := map[string]string{"app": "not-kezio"}
		foreign := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: depName, Namespace: "default"},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: foreignLabels},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: foreignLabels},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "not-a-seeder", Image: "example.test/not-a-seeder:test"}},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, foreign)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, foreign) })
		foreignResourceVersionBefore := foreign.ResourceVersion

		machine := newTestMachineOnSubnet("machine-711", img.Name, "machine-subnet-711")
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())

		r := newSeederImageReconciler(ImageSeederConfig{Image: "example.test/kezio-seeder:test"})
		nn := types.NamespacedName{Name: img.Name, Namespace: "default"}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		// Never adopted, never mutated: same ResourceVersion, still not
		// controller-owned by img, and its foreign spec is untouched.
		var got appsv1.Deployment
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: depName, Namespace: "default"}, &got)).To(Succeed())
		Expect(got.ResourceVersion).To(Equal(foreignResourceVersionBefore), "the foreign Deployment must never be written to")
		Expect(metav1.IsControlledBy(&got, img)).To(BeFalse())
		Expect(got.Spec.Template.Spec.Containers).To(HaveLen(1))
		Expect(got.Spec.Template.Spec.Containers[0].Name).To(Equal("not-a-seeder"))

		gotImg := mustGetImage(img.Name)
		cond := meta.FindStatusCondition(gotImg.Status.Conditions, keziov1alpha2.ImageConditionSeederDegraded)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal("SeederDeploymentForeignOwner"))
		Expect(cond.Message).To(ContainSubstring(siteIdentity))
	})

	It("still creates a seeder Deployment, with no Multus annotation, for a Site whose seeding Subnet has no seederNetworkRef, and does not report it as degraded", func() {
		siteName := "site-712"
		subnetName := "seed-subnet-712"
		subnet := &keziov1alpha2.Subnet{
			ObjectMeta: metav1.ObjectMeta{Name: subnetName, Namespace: "default"},
			Spec: keziov1alpha2.SubnetSpec{
				SiteRef: keziov1alpha2.NameRef{Name: siteName},
				CIDR:    "198.51.100.0/24",
				// A Subnet must declare at least one plane; this one keeps
				// its boot half (shared with the machines it seeds) but
				// deliberately carries no SeederNetworkRef, so the seeder
				// still runs, just on the ordinary cluster network.
				BootdServerIP:   "198.51.100.2",
				BootdNetworkRef: &keziov1alpha2.NameRef{Name: subnetName + "-bootd-nad"},
				DHCP:            &keziov1alpha2.SubnetDHCP{Mode: keziov1alpha2.SubnetDHCPModeProxy},
			},
		}
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())
		site := &keziov1alpha2.Site{
			ObjectMeta: metav1.ObjectMeta{Name: siteName, Namespace: "default"},
			Spec:       keziov1alpha2.SiteSpec{SeederSubnetRef: &keziov1alpha2.NameRef{Name: subnetName}},
		}
		Expect(k8sClient.Create(ctx, site)).To(Succeed())
		siteIdentity := "default/" + siteName
		mustCreateMachineSubnet(ctx, "machine-subnet-712", siteName)

		contentName := "pc-" + imageSeederTestHash(712)
		createReadyContent(ctx, contentName)

		img := newTestImageWithSlots("image-712", []keziov1alpha2.ImageSlot{
			{Number: 1, Role: keziov1alpha2.PartitionRoleData, ContentRef: &keziov1alpha2.NameRef{Name: contentName}},
		})
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		machine := newTestMachineOnSubnet("machine-712", img.Name, "machine-subnet-712")
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())

		r := newSeederImageReconciler(ImageSeederConfig{Image: "example.test/kezio-seeder:test"})
		nn := types.NamespacedName{Name: img.Name, Namespace: "default"}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		depName := seederdeploy.Name(img.Name, siteIdentity)
		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: depName, Namespace: "default"}, &dep)).To(Succeed())
		Expect(dep.Spec.Template.Annotations).NotTo(HaveKey(multusDefaultNetworkAnnotation))

		gotImg := mustGetImage(img.Name)
		Expect(meta.FindStatusCondition(gotImg.Status.Conditions, keziov1alpha2.ImageConditionSeederDegraded)).To(BeNil(),
			"a seeding Subnet with no seederNetworkRef is a supported topology, not a degraded one")
	})

	It("creates no seeder Deployment for a Site with no seederSubnetRef at all, and names the unset reference on the Image", func() {
		siteName := "site-713"
		site := &keziov1alpha2.Site{
			ObjectMeta: metav1.ObjectMeta{Name: siteName, Namespace: "default"},
			// No SeederSubnetRef: this Site runs no seeder at all.
		}
		Expect(k8sClient.Create(ctx, site)).To(Succeed())
		siteIdentity := "default/" + siteName
		mustCreateMachineSubnet(ctx, "machine-subnet-713", siteName)

		contentName := "pc-" + imageSeederTestHash(713)
		createReadyContent(ctx, contentName)

		img := newTestImageWithSlots("image-713", []keziov1alpha2.ImageSlot{
			{Number: 1, Role: keziov1alpha2.PartitionRoleData, ContentRef: &keziov1alpha2.NameRef{Name: contentName}},
		})
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		machine := newTestMachineOnSubnet("machine-713", img.Name, "machine-subnet-713")
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())

		r := newSeederImageReconciler(ImageSeederConfig{Image: "example.test/kezio-seeder:test"})
		nn := types.NamespacedName{Name: img.Name, Namespace: "default"}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		var deployments appsv1.DeploymentList
		Expect(k8sClient.List(ctx, &deployments, client.InNamespace("default"), client.MatchingLabels{
			partitionContentAppComponentLabel: partitionContentSeederComponentValue,
		})).To(Succeed())
		for i := range deployments.Items {
			Expect(metav1.IsControlledBy(&deployments.Items[i], img)).To(BeFalse(), "no seeder Deployment must be created for a Site with no seederSubnetRef")
		}

		gotImg := mustGetImage(img.Name)
		cond := meta.FindStatusCondition(gotImg.Status.Conditions, keziov1alpha2.ImageConditionSeederDegraded)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal("SeederSubnetRefUnset"))
		Expect(cond.Message).To(ContainSubstring(siteIdentity))
	})

	It("qualifies a bare seederNetworkRef with the seeding Subnet's own namespace, not the Image's", func() {
		imageNamespace := "default"
		subnetNamespace := "seeder-ns-714"
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: subnetNamespace}})).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: subnetNamespace}})
		})

		siteName := "site-714"
		subnetName := "seed-subnet-714"
		subnet := &keziov1alpha2.Subnet{
			ObjectMeta: metav1.ObjectMeta{Name: subnetName, Namespace: subnetNamespace},
			Spec: keziov1alpha2.SubnetSpec{
				SiteRef:          keziov1alpha2.NameRef{Name: siteName, Namespace: imageNamespace},
				CIDR:             "198.51.100.0/24",
				SeederNetworkRef: &keziov1alpha2.NameRef{Name: subnetName + "-nad"},
			},
		}
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())
		site := &keziov1alpha2.Site{
			ObjectMeta: metav1.ObjectMeta{Name: siteName, Namespace: imageNamespace},
			Spec:       keziov1alpha2.SiteSpec{SeederSubnetRef: &keziov1alpha2.NameRef{Name: subnetName, Namespace: subnetNamespace}},
		}
		Expect(k8sClient.Create(ctx, site)).To(Succeed())
		siteIdentity := imageNamespace + "/" + siteName
		mustCreateMachineSubnet(ctx, "machine-subnet-714", siteName)

		contentName := "pc-" + imageSeederTestHash(714)
		createReadyContent(ctx, contentName)

		img := newTestImageWithSlots("image-714", []keziov1alpha2.ImageSlot{
			{Number: 1, Role: keziov1alpha2.PartitionRoleData, ContentRef: &keziov1alpha2.NameRef{Name: contentName}},
		})
		Expect(k8sClient.Create(ctx, img)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, img) })

		machine := newTestMachineOnSubnet("machine-714", img.Name, "machine-subnet-714")
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())

		r := newSeederImageReconciler(ImageSeederConfig{Image: "example.test/kezio-seeder:test"})
		nn := types.NamespacedName{Name: img.Name, Namespace: imageNamespace}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		depName := seederdeploy.Name(img.Name, siteIdentity)
		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: depName, Namespace: imageNamespace}, &dep)).To(Succeed())
		Expect(dep.Spec.Template.Annotations[multusDefaultNetworkAnnotation]).To(Equal(subnetNamespace+"/"+subnetName+"-nad"),
			"the bare NAD name must qualify against the seeding Subnet's own namespace, not the Image's")
	})
})
