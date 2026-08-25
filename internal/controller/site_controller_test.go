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
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

// newSiteTestReconciler returns a SiteReconciler with TrackerDeployment
// enabled against a fake image.
func newSiteTestReconciler() *SiteReconciler {
	return &SiteReconciler{
		Client:            k8sClient,
		Scheme:            k8sClient.Scheme(),
		TrackerDeployment: TrackerDeploymentConfig{Image: "tracker:test"},
	}
}

// testSite creates a bare Site named name in ns - the first step of the
// Site/Subnet creation order this package's webhook tests also follow:
// the Subnet's siteRef must resolve before the Subnet is admitted, and a
// Site's own seederSubnetRef needs a Subnet that already points back at
// it, so the Site is created first with no seederSubnetRef and updated
// afterward.
func testSite(ctx context.Context, ns, name string) *keziov1alpha2.Site {
	site := &keziov1alpha2.Site{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	ExpectWithOffset(1, k8sClient.Create(ctx, site)).To(Succeed())
	return site
}

// setSeederSubnetRef patches site.Spec.SeederSubnetRef and Tracker to the
// given values - the second step of the creation order testSite documents.
func setSeederSubnetRef(ctx context.Context, site *keziov1alpha2.Site, subnetName string, tracker keziov1alpha2.SiteTracker) {
	site.Spec.SeederSubnetRef = &keziov1alpha2.NameRef{Name: subnetName}
	site.Spec.Tracker = tracker
	ExpectWithOffset(1, k8sClient.Update(ctx, site)).To(Succeed())
}

// rack-2's identity and network values, shared by the subnetRefs test
// below to avoid repeating the same literals.
const (
	rack2SubnetName    = "rack-2"
	rack2CIDR          = "192.0.3.0/24"
	rack2BootdServerIP = "192.0.3.2"
)

// findTrackerDeployment lists the Deployments in ns carrying
// trackerDeploymentSiteLabel=siteName, failing the test unless there is
// exactly one.
func findTrackerDeployment(ctx context.Context, ns, siteName string) *appsv1.Deployment {
	var deployments appsv1.DeploymentList
	ExpectWithOffset(1, k8sClient.List(ctx, &deployments, client.InNamespace(ns), client.MatchingLabels{
		trackerDeploymentSiteLabel: siteName,
	})).To(Succeed())
	ExpectWithOffset(1, deployments.Items).To(HaveLen(1), "expected exactly one tracker Deployment for the Site")
	return &deployments.Items[0]
}

// markTrackerDeploymentAvailable writes the Available condition on the
// Site's tracker Deployment: no Deployment controller runs in envtest, so
// deploymentAvailable reads a missing condition as false unless a test
// writes it.
func markTrackerDeploymentAvailable(ctx context.Context, ns, siteName string) {
	dep := findTrackerDeployment(ctx, ns, siteName)
	dep.Status.Replicas = 1
	dep.Status.ReadyReplicas = 1
	dep.Status.AvailableReplicas = 1
	dep.Status.Conditions = []appsv1.DeploymentCondition{{
		Type:   appsv1.DeploymentAvailable,
		Status: corev1.ConditionTrue,
		Reason: "MinimumReplicasAvailable",
	}}
	ExpectWithOffset(1, k8sClient.Status().Update(ctx, dep)).To(Succeed())
}

var _ = Describe("Site Controller", func() {
	ctx := context.Background()

	It("creates a tracker Deployment single-homed on the seeding Subnet's NAD, bound to tracker.ip, with no Service", func() {
		ns := createSubnetTestNamespace(ctx)
		site := testSite(ctx, ns, "site-a")

		subnet := testSubnet(ns, func(s *keziov1alpha2.Subnet) {
			s.Spec.SiteRef = keziov1alpha2.NameRef{Name: "site-a"}
			s.Spec.SeederNetworkRef = &keziov1alpha2.NameRef{Name: "seeder-nad"}
		})
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())
		createTestNAD(ctx, ns, "seeder-nad", `{"ipam":{}}`)

		setSeederSubnetRef(ctx, site, subnet.Name, keziov1alpha2.SiteTracker{IP: "192.0.2.60"})

		r := newSiteTestReconciler()
		key := types.NamespacedName{Name: site.Name, Namespace: ns}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		dep := findTrackerDeployment(ctx, ns, "site-a")
		Expect(dep.Namespace).To(Equal(ns))

		raw, ok := dep.Spec.Template.Annotations[multusDefaultNetworkAnnotation]
		Expect(ok).To(BeTrue(), "tracker pod must carry the default-network-REPLACING annotation, not the additive one")
		var elements []trackerNetworkSelectionElement
		Expect(json.Unmarshal([]byte(raw), &elements)).To(Succeed())
		Expect(elements).To(HaveLen(1))
		Expect(elements[0].Name).To(Equal("seeder-nad"))
		Expect(elements[0].Namespace).To(Equal(ns))
		Expect(elements[0].IPs).To(Equal([]string{"192.0.2.60/24"}), "the ips entry must carry the seeding Subnet's own prefix length - the bridge CNI plugin rejects a bare address")

		Expect(dep.Spec.Template.Annotations).NotTo(HaveKey(multusNetworksAnnotation), "the tracker must not carry the additive annotation alongside the default-network one")

		var services corev1.ServiceList
		Expect(k8sClient.List(ctx, &services, client.InNamespace(ns))).To(Succeed())
		Expect(services.Items).To(BeEmpty(), "no Service must front the tracker - a ClusterIP would DNAT the address peers announce")

		var updated keziov1alpha2.Site
		Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())
		Expect(updated.Status.TrackerURL).To(Equal("http://192.0.2.60:6969/announce"))
	})

	It("derives the tracker's ips prefix length from the seeding Subnet's own cidr, not a hardcoded /24", func() {
		ns := createSubnetTestNamespace(ctx)
		site := testSite(ctx, ns, "site-a2")

		subnet := testSubnet(ns, func(s *keziov1alpha2.Subnet) {
			s.Spec.SiteRef = keziov1alpha2.NameRef{Name: "site-a2"}
			s.Spec.CIDR = "192.0.2.0/25"
			s.Spec.SeederNetworkRef = &keziov1alpha2.NameRef{Name: "seeder-nad"}
		})
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())
		createTestNAD(ctx, ns, "seeder-nad", `{"ipam":{}}`)

		setSeederSubnetRef(ctx, site, subnet.Name, keziov1alpha2.SiteTracker{IP: "192.0.2.4"})

		r := newSiteTestReconciler()
		key := types.NamespacedName{Name: site.Name, Namespace: ns}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		dep := findTrackerDeployment(ctx, ns, "site-a2")
		raw, ok := dep.Spec.Template.Annotations[multusDefaultNetworkAnnotation]
		Expect(ok).To(BeTrue())
		var elements []trackerNetworkSelectionElement
		Expect(json.Unmarshal([]byte(raw), &elements)).To(Succeed())
		Expect(elements).To(HaveLen(1))
		Expect(elements[0].IPs).To(Equal([]string{"192.0.2.4/25"}))
	})

	It("fails Valid/Ready with SeederSubnetCIDRUnparseable and creates no Deployment when the seeding Subnet's cidr cannot be parsed", func() {
		ns := createSubnetTestNamespace(ctx)
		site := testSite(ctx, ns, "site-a3")

		subnet := testSubnet(ns, func(s *keziov1alpha2.Subnet) {
			s.Spec.SiteRef = keziov1alpha2.NameRef{Name: "site-a3"}
			// Matches SubnetSpec.CIDR's admission-time regex (digits and
			// dots only) but is not a valid CIDR: the 300 octet is out of
			// range, so net.ParseCIDR rejects it at reconcile time instead.
			s.Spec.CIDR = "300.0.2.0/24"
			s.Spec.SeederNetworkRef = &keziov1alpha2.NameRef{Name: "seeder-nad"}
		})
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())
		createTestNAD(ctx, ns, "seeder-nad", `{"ipam":{}}`)

		setSeederSubnetRef(ctx, site, subnet.Name, keziov1alpha2.SiteTracker{IP: "192.0.2.4"})

		r := newSiteTestReconciler()
		key := types.NamespacedName{Name: site.Name, Namespace: ns}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var deployments appsv1.DeploymentList
		Expect(k8sClient.List(ctx, &deployments, client.InNamespace(ns), client.MatchingLabels{trackerAppComponentLabel: trackerComponentValue})).To(Succeed())
		Expect(deployments.Items).To(BeEmpty(), "an unparseable seeding Subnet cidr must never create a tracker Deployment pinned to a bare IP")

		var updated keziov1alpha2.Site
		Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())
		validCond := findCondition(updated.Status.Conditions, keziov1alpha2.SiteConditionValid)
		Expect(validCond).NotTo(BeNil())
		Expect(validCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(validCond.Reason).To(Equal("SeederSubnetCIDRUnparseable"))
		readyCond := findCondition(updated.Status.Conditions, keziov1alpha2.SiteConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(readyCond.Reason).To(Equal("SeederSubnetCIDRUnparseable"))
	})

	It("resolves a tracker URL and creates no Deployment for a Site using tracker.externalURL", func() {
		ns := createSubnetTestNamespace(ctx)
		site := testSite(ctx, ns, "site-b")

		subnet := testSubnet(ns, func(s *keziov1alpha2.Subnet) {
			s.Spec.SiteRef = keziov1alpha2.NameRef{Name: "site-b"}
			s.Spec.SeederNetworkRef = &keziov1alpha2.NameRef{Name: "seeder-nad"}
		})
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())

		setSeederSubnetRef(ctx, site, subnet.Name, keziov1alpha2.SiteTracker{ExternalURL: "http://tracker.example.com:6969/announce"})

		r := newSiteTestReconciler()
		key := types.NamespacedName{Name: site.Name, Namespace: ns}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var deployments appsv1.DeploymentList
		Expect(k8sClient.List(ctx, &deployments, client.InNamespace(ns), client.MatchingLabels{trackerAppComponentLabel: trackerComponentValue})).To(Succeed())
		Expect(deployments.Items).To(BeEmpty(), "a Site using tracker.externalURL must never get a tracker Deployment of its own")

		var updated keziov1alpha2.Site
		Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())
		Expect(updated.Status.TrackerURL).To(Equal("http://tracker.example.com:6969/announce"))

		readyCond := findCondition(updated.Status.Conditions, keziov1alpha2.SiteConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionTrue), "a Site with no tracker Deployment of its own is Ready once Valid")
	})

	It("creates no Deployment and reports no seeder for a Site with no seederSubnetRef", func() {
		ns := createSubnetTestNamespace(ctx)
		site := testSite(ctx, ns, "site-c")

		r := newSiteTestReconciler()
		key := types.NamespacedName{Name: site.Name, Namespace: ns}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var deployments appsv1.DeploymentList
		Expect(k8sClient.List(ctx, &deployments, client.InNamespace(ns), client.MatchingLabels{trackerAppComponentLabel: trackerComponentValue})).To(Succeed())
		Expect(deployments.Items).To(BeEmpty())

		var updated keziov1alpha2.Site
		Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())
		Expect(updated.Status.SeederReady).To(BeFalse())
		Expect(updated.Status.TrackerURL).To(BeEmpty())

		validCond := findCondition(updated.Status.Conditions, keziov1alpha2.SiteConditionValid)
		Expect(validCond).NotTo(BeNil())
		Expect(validCond.Status).To(Equal(metav1.ConditionTrue))
		readyCond := findCondition(updated.Status.Conditions, keziov1alpha2.SiteConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionTrue), "a Site with no seederSubnetRef has no tracker Deployment of its own, so Ready follows Valid")
	})

	It("reports the Subnets that select this Site in status.subnetRefs", func() {
		ns := createSubnetTestNamespace(ctx)
		site := testSite(ctx, ns, "site-d")

		first := testSubnet(ns, func(s *keziov1alpha2.Subnet) {
			s.Name = "rack-1"
			s.Spec.SiteRef = keziov1alpha2.NameRef{Name: "site-d"}
		})
		Expect(k8sClient.Create(ctx, first)).To(Succeed())
		second := testSubnet(ns, func(s *keziov1alpha2.Subnet) {
			s.Name = rack2SubnetName
			s.Spec.CIDR = rack2CIDR
			s.Spec.BootdServerIP = rack2BootdServerIP
			s.Spec.SiteRef = keziov1alpha2.NameRef{Name: "site-d"}
		})
		Expect(k8sClient.Create(ctx, second)).To(Succeed())
		other := testSubnet(ns, func(s *keziov1alpha2.Subnet) {
			s.Name = "rack-3"
			s.Spec.CIDR = "192.0.4.0/24"
			s.Spec.BootdServerIP = "192.0.4.2"
			s.Spec.SiteRef = keziov1alpha2.NameRef{Name: "site-other"}
		})
		Expect(k8sClient.Create(ctx, other)).To(Succeed())

		r := newSiteTestReconciler()
		key := types.NamespacedName{Name: site.Name, Namespace: ns}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var updated keziov1alpha2.Site
		Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())
		Expect(updated.Status.SubnetRefs).To(Equal([]string{"rack-1", "rack-2"}))
	})

	It("fails Valid/Ready with SeederSubnetNotFound when the referenced Subnet no longer exists", func() {
		ns := createSubnetTestNamespace(ctx)
		site := testSite(ctx, ns, "site-e")

		subnet := testSubnet(ns, func(s *keziov1alpha2.Subnet) {
			s.Spec.SiteRef = keziov1alpha2.NameRef{Name: "site-e"}
			s.Spec.SeederNetworkRef = &keziov1alpha2.NameRef{Name: "seeder-nad"}
		})
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())
		setSeederSubnetRef(ctx, site, subnet.Name, keziov1alpha2.SiteTracker{IP: "192.0.2.60"})
		Expect(k8sClient.Delete(ctx, subnet)).To(Succeed())

		r := newSiteTestReconciler()
		key := types.NamespacedName{Name: site.Name, Namespace: ns}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var updated keziov1alpha2.Site
		Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())
		validCond := findCondition(updated.Status.Conditions, keziov1alpha2.SiteConditionValid)
		Expect(validCond).NotTo(BeNil())
		Expect(validCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(validCond.Reason).To(Equal("SeederSubnetNotFound"))
		readyCond := findCondition(updated.Status.Conditions, keziov1alpha2.SiteConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(readyCond.Reason).To(Equal("SeederSubnetNotFound"))

		var deployments appsv1.DeploymentList
		Expect(k8sClient.List(ctx, &deployments, client.InNamespace(ns), client.MatchingLabels{trackerAppComponentLabel: trackerComponentValue})).To(Succeed())
		Expect(deployments.Items).To(BeEmpty())
	})

	It("fails Valid/Ready with SeederSubnetNotOwned when the referenced Subnet's siteRef points elsewhere", func() {
		ns := createSubnetTestNamespace(ctx)
		site := testSite(ctx, ns, "site-f")

		subnet := testSubnet(ns, func(s *keziov1alpha2.Subnet) {
			s.Spec.SiteRef = keziov1alpha2.NameRef{Name: "site-f"}
			s.Spec.SeederNetworkRef = &keziov1alpha2.NameRef{Name: "seeder-nad"}
		})
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())
		setSeederSubnetRef(ctx, site, subnet.Name, keziov1alpha2.SiteTracker{IP: "192.0.2.60"})

		// The Subnet's own siteRef drifts away after admission - the
		// webhook only checked this at Site-create time.
		subnet.Spec.SiteRef = keziov1alpha2.NameRef{Name: "some-other-site"}
		Expect(k8sClient.Update(ctx, subnet)).To(Succeed())

		r := newSiteTestReconciler()
		key := types.NamespacedName{Name: site.Name, Namespace: ns}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var updated keziov1alpha2.Site
		Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())
		validCond := findCondition(updated.Status.Conditions, keziov1alpha2.SiteConditionValid)
		Expect(validCond).NotTo(BeNil())
		Expect(validCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(validCond.Reason).To(Equal("SeederSubnetNotOwned"))
	})

	It("reports Ready=False naming the missing image, but Valid=True, when no tracker Deployment image is configured", func() {
		ns := createSubnetTestNamespace(ctx)
		site := testSite(ctx, ns, "site-g")

		subnet := testSubnet(ns, func(s *keziov1alpha2.Subnet) {
			s.Spec.SiteRef = keziov1alpha2.NameRef{Name: "site-g"}
			s.Spec.SeederNetworkRef = &keziov1alpha2.NameRef{Name: "seeder-nad"}
		})
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())
		createTestNAD(ctx, ns, "seeder-nad", `{"ipam":{}}`)
		setSeederSubnetRef(ctx, site, subnet.Name, keziov1alpha2.SiteTracker{IP: "192.0.2.60"})

		r := &SiteReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()} // zero-value TrackerDeployment: not enabled()
		key := types.NamespacedName{Name: site.Name, Namespace: ns}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var deployments appsv1.DeploymentList
		Expect(k8sClient.List(ctx, &deployments, client.InNamespace(ns), client.MatchingLabels{trackerAppComponentLabel: trackerComponentValue})).To(Succeed())
		Expect(deployments.Items).To(BeEmpty(), "no Deployment image configured must never create a half-configured Deployment")

		var updated keziov1alpha2.Site
		Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())
		validCond := findCondition(updated.Status.Conditions, keziov1alpha2.SiteConditionValid)
		Expect(validCond).NotTo(BeNil())
		Expect(validCond.Status).To(Equal(metav1.ConditionTrue), "an unconfigured manager is not a Site misconfiguration")

		readyCond := findCondition(updated.Status.Conditions, keziov1alpha2.SiteConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(readyCond.Reason).To(Equal("SiteTrackerDeploymentImageUnconfigured"))
	})

	It("reports Valid=Unknown but Ready=False when the seeding Subnet names a NAD that does not exist and the tracker Deployment cannot become available", func() {
		ns := createSubnetTestNamespace(ctx)
		site := testSite(ctx, ns, "site-j")

		subnet := testSubnet(ns, func(s *keziov1alpha2.Subnet) {
			s.Spec.SiteRef = keziov1alpha2.NameRef{Name: "site-j"}
			s.Spec.SeederNetworkRef = &keziov1alpha2.NameRef{Name: "seeder-nad-absent"}
		})
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())
		// No createTestNAD: the NAD the Subnet names never exists.
		setSeederSubnetRef(ctx, site, subnet.Name, keziov1alpha2.SiteTracker{IP: "192.0.2.60"})

		r := newSiteTestReconciler()
		key := types.NamespacedName{Name: site.Name, Namespace: ns}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var updated keziov1alpha2.Site
		Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())
		validCond := findCondition(updated.Status.Conditions, keziov1alpha2.SiteConditionValid)
		Expect(validCond).NotTo(BeNil())
		Expect(validCond.Status).To(Equal(metav1.ConditionUnknown), "kezio cannot validate the tracker address against a NAD it cannot read")
		Expect(validCond.Reason).To(Equal("SeederNADNotFound"))

		readyCond := findCondition(updated.Status.Conditions, keziov1alpha2.SiteConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionFalse), "an unavailable tracker Deployment is an observed fact, not an uncertainty")
		Expect(readyCond.Reason).To(Equal("TrackerDeploymentUnavailable"))
	})

	It("reports Ready=Unknown when the seeder NAD cannot be read but the tracker Deployment is available", func() {
		ns := createSubnetTestNamespace(ctx)
		site := testSite(ctx, ns, "site-k")

		subnet := testSubnet(ns, func(s *keziov1alpha2.Subnet) {
			s.Spec.SiteRef = keziov1alpha2.NameRef{Name: "site-k"}
			s.Spec.SeederNetworkRef = &keziov1alpha2.NameRef{Name: "seeder-nad-absent"}
		})
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())
		setSeederSubnetRef(ctx, site, subnet.Name, keziov1alpha2.SiteTracker{IP: "192.0.2.60"})

		r := newSiteTestReconciler()
		key := types.NamespacedName{Name: site.Name, Namespace: ns}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		markTrackerDeploymentAvailable(ctx, ns, "site-k")
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var updated keziov1alpha2.Site
		Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())
		readyCond := findCondition(updated.Status.Conditions, keziov1alpha2.SiteConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionUnknown), "a serving tracker plus an unreadable NAD leaves Ready undecided, not failed")
		Expect(readyCond.Reason).To(Equal("SeederNADNotFound"))
	})

	It("passes Valid when tracker.ip falls inside the seeding Subnet's cidr and outside the seeder NAD's pool", func() {
		ns := createSubnetTestNamespace(ctx)
		site := testSite(ctx, ns, "site-h")

		subnet := testSubnet(ns, func(s *keziov1alpha2.Subnet) {
			s.Spec.SiteRef = keziov1alpha2.NameRef{Name: "site-h"}
			s.Spec.SeederNetworkRef = &keziov1alpha2.NameRef{Name: "seeder-nad"}
		})
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())
		createTestNAD(ctx, ns, "seeder-nad", `{"ipam":{"type":"whereabouts","range":"192.0.2.128/25"}}`)
		setSeederSubnetRef(ctx, site, subnet.Name, keziov1alpha2.SiteTracker{IP: "192.0.2.60"})

		r := newSiteTestReconciler()
		key := types.NamespacedName{Name: site.Name, Namespace: ns}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var updated keziov1alpha2.Site
		Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())
		validCond := findCondition(updated.Status.Conditions, keziov1alpha2.SiteConditionValid)
		Expect(validCond).NotTo(BeNil())
		Expect(validCond.Status).To(Equal(metav1.ConditionTrue))
	})

	It("fails Valid when tracker.ip falls inside the seeder NAD's own pool", func() {
		ns := createSubnetTestNamespace(ctx)
		site := testSite(ctx, ns, "site-i")

		subnet := testSubnet(ns, func(s *keziov1alpha2.Subnet) {
			s.Spec.SiteRef = keziov1alpha2.NameRef{Name: "site-i"}
			s.Spec.SeederNetworkRef = &keziov1alpha2.NameRef{Name: "seeder-nad"}
		})
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())
		// tracker.ip falls inside this whereabouts range: the pool could
		// hand a seeder pod the tracker's own address.
		createTestNAD(ctx, ns, "seeder-nad", `{"ipam":{"type":"whereabouts","range":"192.0.2.0/24"}}`)
		setSeederSubnetRef(ctx, site, subnet.Name, keziov1alpha2.SiteTracker{IP: "192.0.2.60"})

		r := newSiteTestReconciler()
		key := types.NamespacedName{Name: site.Name, Namespace: ns}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var updated keziov1alpha2.Site
		Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())
		validCond := findCondition(updated.Status.Conditions, keziov1alpha2.SiteConditionValid)
		Expect(validCond).NotTo(BeNil())
		Expect(validCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(validCond.Reason).To(Equal("TrackerOverlapWhereabouts"))

		readyCond := findCondition(updated.Status.Conditions, keziov1alpha2.SiteConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(readyCond.Reason).To(Equal("TrackerOverlapWhereabouts"))
	})
})
