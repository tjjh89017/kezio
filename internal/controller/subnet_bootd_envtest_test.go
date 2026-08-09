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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
)

// createTestNAD creates a NetworkAttachmentDefinition, as unstructured
// (the shape nadvalidate.ConfigFromUnstructured reads), named name in
// namespace, with spec.config set to configJSON.
func createTestNAD(ctx context.Context, namespace, name, configJSON string) {
	nad := &unstructured.Unstructured{}
	nad.SetGroupVersionKind(networkAttachmentDefinitionGVK)
	nad.SetNamespace(namespace)
	nad.SetName(name)
	Expect(unstructured.SetNestedField(nad.Object, configJSON, "spec", "config")).To(Succeed())
	ExpectWithOffset(1, k8sClient.Create(ctx, nad)).To(Succeed())
}

// bootdStaticNADConfig is a minimal static-ipam NAD config assigning
// serverIP, the same plugin-chain shape
// config/bootd/networkattachmentdefinition.example.yaml uses.
func bootdStaticNADConfig(serverIP string) string {
	return fmt.Sprintf(`{"cniVersion":"0.3.1","name":"test-boot-network","plugins":[{"type":"macvlan","master":"eth1","mode":"bridge","ipam":{"type":"static","addresses":[{"address":"%s/24"}]}}]}`, serverIP)
}

// newSubnetTestReconciler returns a SubnetReconciler with
// BootdDeployment enabled against fake images.
func newSubnetTestReconciler() *SubnetReconciler {
	return &SubnetReconciler{
		Client: k8sClient,
		Scheme: k8sClient.Scheme(),
		BootdDeployment: BootdDeploymentConfig{
			Image:              "bootd:test",
			BootArtifactsImage: "boot-artifacts:test",
		},
	}
}

// createSubnetTestNamespace creates a fresh namespace for one spec,
// with the PSA label, the kezio-bootd ServiceAccount, and Site "hq"
// already present, so specs exercise the healthy case unless they
// deliberately withhold one.
func createSubnetTestNamespace(ctx context.Context) string {
	name := fmt.Sprintf("subnet-test-%s", rand.String(5))
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   name,
		Labels: map[string]string{"pod-security.kubernetes.io/enforce": "privileged"},
	}}
	ExpectWithOffset(1, k8sClient.Create(ctx, ns)).To(Succeed())
	DeferCleanup(func() {
		ExpectWithOffset(1, k8sClient.Delete(ctx, ns)).To(Succeed())
	})
	createBootdServiceAccount(ctx, name)
	createTestSite(ctx, name)
	return name
}

// createBootdServiceAccount creates the ServiceAccount buildBootdDeployment
// stamps as serviceAccountName, in namespace ns.
func createBootdServiceAccount(ctx context.Context, ns string) {
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: bootdDefaultServiceAccountName, Namespace: ns}}
	ExpectWithOffset(1, k8sClient.Create(ctx, sa)).To(Succeed())
}

// createTestSite creates the Site named "hq", testSubnet's default
// siteRef, in namespace ns.
func createTestSite(ctx context.Context, ns string) {
	site := &keziov1alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "hq", Namespace: ns}}
	ExpectWithOffset(1, k8sClient.Create(ctx, site)).To(Succeed())
}

// createBareNamespace creates a fresh namespace with none of the bootd
// prerequisites createSubnetTestNamespace normally supplies, for tests
// that add back exactly the prerequisite(s) they mean to keep.
func createBareNamespace(ctx context.Context) string {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("subnet-test-%s", rand.String(5))}}
	ExpectWithOffset(1, k8sClient.Create(ctx, ns)).To(Succeed())
	DeferCleanup(func() {
		ExpectWithOffset(1, k8sClient.Delete(ctx, ns)).To(Succeed())
	})
	return ns.Name
}

func findCondition(conds []metav1.Condition, condType string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == condType {
			return &conds[i]
		}
	}
	return nil
}

var _ = Describe("Subnet bootd Deployment reconciliation", func() {
	ctx := context.Background()

	It("rejects a Subnet with an empty bootdServerIP at admission, so it can never be left to default or IPAM allocation", func() {
		ns := createSubnetTestNamespace(ctx)
		subnet := testSubnet(ns, func(s *keziov1alpha1.Subnet) { s.Spec.BootdServerIP = "" })
		err := k8sClient.Create(ctx, subnet)
		Expect(err).To(HaveOccurred())
		Expect(kerrors.IsInvalid(err)).To(BeTrue(), "expected schema validation to reject the empty bootdServerIP, got: %v", err)
	})

	It("creates exactly one bootd Deployment, in the Subnet's own namespace, with the NAD annotation and required env", func() {
		ns := createSubnetTestNamespace(ctx)
		createTestNAD(ctx, ns, "boot-nad", bootdStaticNADConfig("192.0.2.2"))

		subnet := testSubnet(ns)
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())

		r := newSubnetTestReconciler()
		key := types.NamespacedName{Name: subnet.Name, Namespace: ns}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var deployments appsv1.DeploymentList
		Expect(k8sClient.List(ctx, &deployments, client.InNamespace(ns))).To(Succeed())
		Expect(deployments.Items).To(HaveLen(1), "expected exactly one bootd Deployment for the Subnet")

		dep := deployments.Items[0]
		Expect(dep.Namespace).To(Equal(ns))
		Expect(dep.Spec.Template.Annotations[multusNetworksAnnotation]).To(Equal(ns + "/boot-nad"))

		var bootd *corev1.Container
		for i := range dep.Spec.Template.Spec.Containers {
			if dep.Spec.Template.Spec.Containers[i].Name == "bootd" {
				bootd = &dep.Spec.Template.Spec.Containers[i]
			}
		}
		Expect(bootd).NotTo(BeNil())
		Expect(bootd.Env).To(ContainElement(corev1.EnvVar{Name: "BOOTD_SERVER_IP", Value: "192.0.2.2"}))
		Expect(bootd.Env).To(ContainElement(corev1.EnvVar{Name: "BOOTD_PROVISIONING_CIDR", Value: "192.0.2.0/24"}))

		By("re-reconciling does not create a second Deployment")
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.List(ctx, &deployments, client.InNamespace(ns))).To(Succeed())
		Expect(deployments.Items).To(HaveLen(1))
	})

	It("issues no write on a quiescent reconcile - resourceVersion is unchanged by a second reconcile with nothing changed", func() {
		// resourceVersion is checked rather than DeepEqual, since a
		// field the apiserver defaults on write (e.g.
		// terminationMessagePath) would make DeepEqual see drift forever.
		ns := createSubnetTestNamespace(ctx)
		createTestNAD(ctx, ns, "boot-nad", bootdStaticNADConfig("192.0.2.2"))

		subnet := testSubnet(ns)
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())

		r := newSubnetTestReconciler()
		key := types.NamespacedName{Name: subnet.Name, Namespace: ns}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var before appsv1.Deployment
		depKey := types.NamespacedName{Namespace: ns, Name: bootdDeploymentName(subnet.Name)}
		Expect(k8sClient.Get(ctx, depKey, &before)).To(Succeed())

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var after appsv1.Deployment
		Expect(k8sClient.Get(ctx, depKey, &after)).To(Succeed())
		Expect(after.ResourceVersion).To(Equal(before.ResourceVersion), "a quiescent reconcile must issue no Update - resourceVersion must not move")
		Expect(after.Generation).To(Equal(before.Generation))
	})

	It("updates the existing Deployment in place when the Subnet's spec changes", func() {
		ns := createSubnetTestNamespace(ctx)
		createTestNAD(ctx, ns, "boot-nad", bootdStaticNADConfig("192.0.2.2"))

		subnet := testSubnet(ns)
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())

		r := newSubnetTestReconciler()
		key := types.NamespacedName{Name: subnet.Name, Namespace: ns}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var before appsv1.Deployment
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "kezio-bootd-rack-1"}, &before)).To(Succeed())

		By("changing the Subnet's CIDR")
		Expect(k8sClient.Get(ctx, key, subnet)).To(Succeed())
		subnet.Spec.CIDR = "192.0.3.0/24"
		// bootdServerIP must move too, or CheckBootdServerIPInCIDR
		// withholds the Deployment update entirely.
		subnet.Spec.BootdServerIP = "192.0.3.2"
		Expect(k8sClient.Update(ctx, subnet)).To(Succeed())

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var deployments appsv1.DeploymentList
		Expect(k8sClient.List(ctx, &deployments, client.InNamespace(ns))).To(Succeed())
		Expect(deployments.Items).To(HaveLen(1), "the Subnet change must update the existing Deployment, not create a duplicate")

		var after appsv1.Deployment
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "kezio-bootd-rack-1"}, &after)).To(Succeed())
		Expect(after.UID).To(Equal(before.UID), "same Deployment object, updated in place")

		var bootd *corev1.Container
		for i := range after.Spec.Template.Spec.Containers {
			if after.Spec.Template.Spec.Containers[i].Name == "bootd" {
				bootd = &after.Spec.Template.Spec.Containers[i]
			}
		}
		Expect(bootd.Env).To(ContainElement(corev1.EnvVar{Name: "BOOTD_PROVISIONING_CIDR", Value: "192.0.3.0/24"}))
	})

	It("updates the existing Deployment's NodeSelector in place when the Subnet's nodeSelector changes", func() {
		ns := createSubnetTestNamespace(ctx)
		createTestNAD(ctx, ns, "boot-nad", bootdStaticNADConfig("192.0.2.2"))

		subnet := testSubnet(ns)
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())

		r := newSubnetTestReconciler()
		key := types.NamespacedName{Name: subnet.Name, Namespace: ns}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var before appsv1.Deployment
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "kezio-bootd-rack-1"}, &before)).To(Succeed())
		Expect(before.Spec.Template.Spec.NodeSelector).To(BeNil(), "no nodeSelector set yet - unconstrained")

		By("setting the Subnet's nodeSelector")
		Expect(k8sClient.Get(ctx, key, subnet)).To(Succeed())
		subnet.Spec.NodeSelector = map[string]string{"segment": "rack-1-vlan"}
		Expect(k8sClient.Update(ctx, subnet)).To(Succeed())

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var deployments appsv1.DeploymentList
		Expect(k8sClient.List(ctx, &deployments, client.InNamespace(ns))).To(Succeed())
		Expect(deployments.Items).To(HaveLen(1), "the Subnet change must update the existing Deployment, not create a duplicate")

		var after appsv1.Deployment
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "kezio-bootd-rack-1"}, &after)).To(Succeed())
		Expect(after.UID).To(Equal(before.UID), "same Deployment object, updated in place")
		Expect(after.Spec.Template.Spec.NodeSelector).To(Equal(map[string]string{"segment": "rack-1-vlan"}))
	})

	It("still creates the Deployment when bootdServerIP disagrees with the NAD's static address, but fails Ready", func() {
		ns := createSubnetTestNamespace(ctx)
		// NAD address (192.0.2.9) deliberately differs from
		// bootdServerIP (192.0.2.2) below - the mismatch
		// CheckBootdAddress exists to catch.
		createTestNAD(ctx, ns, "boot-nad", bootdStaticNADConfig("192.0.2.9"))

		subnet := testSubnet(ns)
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())

		r := newSubnetTestReconciler()
		key := types.NamespacedName{Name: subnet.Name, Namespace: ns}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var deployments appsv1.DeploymentList
		Expect(k8sClient.List(ctx, &deployments, client.InNamespace(ns))).To(Succeed())
		Expect(deployments.Items).To(HaveLen(1), "a CheckBootdAddress Violation must not withhold the Deployment")

		var updated keziov1alpha1.Subnet
		Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())

		bootdCond := findCondition(updated.Status.Conditions, ConditionBootdAddressValid)
		Expect(bootdCond).NotTo(BeNil())
		Expect(bootdCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(bootdCond.Reason).To(Equal("BootdAddressMismatch"))

		readyCond := findCondition(updated.Status.Conditions, keziov1alpha1.ConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(readyCond.Reason).To(Equal("BootdAddressMismatch"))
	})

	It("reports Ready=Unknown, not True or a Violation, when the bootd NAD's config is unparseable and the Deployment is already Available", func() {
		ns := createSubnetTestNamespace(ctx)
		createTestNAD(ctx, ns, "boot-nad", "not valid json")

		subnet := testSubnet(ns)
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())

		r := newSubnetTestReconciler()
		key := types.NamespacedName{Name: subnet.Name, Namespace: ns}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var afterFirst keziov1alpha1.Subnet
		Expect(k8sClient.Get(ctx, key, &afterFirst)).To(Succeed())
		bootdCond := findCondition(afterFirst.Status.Conditions, ConditionBootdAddressValid)
		Expect(bootdCond).NotTo(BeNil())
		Expect(bootdCond.Status).To(Equal(metav1.ConditionUnknown))

		By("stamping the Deployment Available, as the real Deployment controller would, so unavailability cannot also explain a non-True Ready")
		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "kezio-bootd-rack-1"}, &dep)).To(Succeed())
		dep.Status.Conditions = []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue}}
		Expect(k8sClient.Status().Update(ctx, &dep)).To(Succeed())

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var updated keziov1alpha1.Subnet
		Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())
		readyCond := findCondition(updated.Status.Conditions, keziov1alpha1.ConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionUnknown), "an unparseable NAD is a real blind spot, not proof of misconfiguration - Ready must be Unknown, not True or False")
	})

	It("writes no seeder conditions when SeederNetworkRef is nil, and Ready reflects Deployment availability once validation passes", func() {
		ns := createSubnetTestNamespace(ctx)
		createTestNAD(ctx, ns, "boot-nad", bootdStaticNADConfig("192.0.2.2"))

		subnet := testSubnet(ns)
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())

		r := newSubnetTestReconciler()
		key := types.NamespacedName{Name: subnet.Name, Namespace: ns}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var updated keziov1alpha1.Subnet
		Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())
		Expect(findCondition(updated.Status.Conditions, ConditionSeederOverlapValid)).To(BeNil())
		Expect(findCondition(updated.Status.Conditions, ConditionSeederStaticMultiImage)).To(BeNil())

		By("Ready stays False - the Deployment envtest just created never becomes Available (no real Deployment controller runs here)")
		readyCond := findCondition(updated.Status.Conditions, keziov1alpha1.ConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(readyCond.Reason).To(Equal("BootdDeploymentUnavailable"))

		By("stamping the Deployment Available, as the real Deployment controller would")
		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "kezio-bootd-rack-1"}, &dep)).To(Succeed())
		dep.Status.Conditions = []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue}}
		Expect(k8sClient.Status().Update(ctx, &dep)).To(Succeed())

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())
		readyCond = findCondition(updated.Status.Conditions, keziov1alpha1.ConditionReady)
		Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))
		Expect(readyCond.Reason).To(Equal("BootdReady"))
	})

	It("flags seeder static ipam as Advisory once more than one Image is deploying concurrently at the Subnet's Site", func() {
		ns := createSubnetTestNamespace(ctx)
		createTestNAD(ctx, ns, "boot-nad", bootdStaticNADConfig("192.0.2.2"))
		createTestNAD(ctx, ns, "seeder-nad", bootdStaticNADConfig("192.0.2.50"))

		subnet := testSubnet(ns, func(s *keziov1alpha1.Subnet) {
			s.Spec.SiteRef = keziov1alpha1.NameRef{Name: "hq"}
			s.Spec.SeederNetworkRef = &keziov1alpha1.NameRef{Name: "seeder-nad"}
		})
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())

		// Two seeder Deployments for two Images, annotated with the
		// namespace-qualified Site identity concurrentImages expects.
		for _, image := range []string{"image-a", "image-b"} {
			dep := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("kezio-seeder-%s-%s", image, rand.String(5)),
					Namespace: ns,
					Labels: map[string]string{
						AppComponentLabel:          SeederComponentValue,
						SeederDeploymentImageLabel: image,
					},
					Annotations: map[string]string{SeederDeploymentSiteAnnotation: ns + "/hq"},
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": image}},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": image}},
						Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "busybox"}}},
					},
				},
			}
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())
		}

		r := newSubnetTestReconciler()
		key := types.NamespacedName{Name: subnet.Name, Namespace: ns}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var updated keziov1alpha1.Subnet
		Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())
		multiImageCond := findCondition(updated.Status.Conditions, ConditionSeederStaticMultiImage)
		Expect(multiImageCond).NotTo(BeNil())
		Expect(multiImageCond.Status).To(Equal(metav1.ConditionTrue), "Advisory maps to True with its own reason, not a Violation")
		Expect(multiImageCond.Reason).To(Equal("SeederStaticIPAMMultiImage"))

		overlapCond := findCondition(updated.Status.Conditions, ConditionSeederOverlapValid)
		Expect(overlapCond).NotTo(BeNil())
		Expect(overlapCond.Status).To(Equal(metav1.ConditionTrue))
	})

	It("prunes both seeder conditions entirely, not merely staling them, once SeederNetworkRef is cleared", func() {
		ns := createSubnetTestNamespace(ctx)
		createTestNAD(ctx, ns, "boot-nad", bootdStaticNADConfig("192.0.2.2"))
		createTestNAD(ctx, ns, "seeder-nad", bootdStaticNADConfig("192.0.2.50"))

		subnet := testSubnet(ns, func(s *keziov1alpha1.Subnet) {
			s.Spec.SeederNetworkRef = &keziov1alpha1.NameRef{Name: "seeder-nad"}
		})
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())

		r := newSubnetTestReconciler()
		key := types.NamespacedName{Name: subnet.Name, Namespace: ns}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var updated keziov1alpha1.Subnet
		Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())
		Expect(findCondition(updated.Status.Conditions, ConditionSeederOverlapValid)).NotTo(BeNil(), "SeederNetworkRef set must write the seeder conditions before this test can prove they get pruned")
		Expect(findCondition(updated.Status.Conditions, ConditionSeederStaticMultiImage)).NotTo(BeNil())

		By("clearing SeederNetworkRef - the seeder conditions no longer apply")
		Expect(k8sClient.Get(ctx, key, subnet)).To(Succeed())
		subnet.Spec.SeederNetworkRef = nil
		Expect(k8sClient.Update(ctx, subnet)).To(Succeed())

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())

		Expect(findCondition(updated.Status.Conditions, ConditionSeederOverlapValid)).To(BeNil(), "the obsolete condition must be removed, not left frozen at its last verdict")
		Expect(findCondition(updated.Status.Conditions, ConditionSeederStaticMultiImage)).To(BeNil())
	})

	It("refuses the bootd Deployment and fails Ready when bootdServerIP is outside the Subnet's cidr, and recovers once corrected", func() {
		ns := createSubnetTestNamespace(ctx)
		// NAD address matches bootdServerIP exactly, so only
		// CheckBootdServerIPInCIDR (not ConditionBootdAddressValid) can
		// catch this out-of-cidr case.
		createTestNAD(ctx, ns, "boot-nad", bootdStaticNADConfig("203.0.113.5"))

		subnet := testSubnet(ns, func(s *keziov1alpha1.Subnet) {
			s.Spec.BootdServerIP = "203.0.113.5"
		})
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())

		r := newSubnetTestReconciler()
		key := types.NamespacedName{Name: subnet.Name, Namespace: ns}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var deployments appsv1.DeploymentList
		Expect(k8sClient.List(ctx, &deployments, client.InNamespace(ns))).To(Succeed())
		Expect(deployments.Items).To(BeEmpty(), "an out-of-cidr bootdServerIP must withhold the bootd Deployment - PXE clients on cidr would be handed a next-server off their own segment")

		var updated keziov1alpha1.Subnet
		Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())

		cidrCond := findCondition(updated.Status.Conditions, ConditionBootdServerIPInCIDR)
		Expect(cidrCond).NotTo(BeNil())
		Expect(cidrCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cidrCond.Reason).To(Equal("BootdServerIPOutsideCIDR"))

		By("ConditionBootdAddressValid alone still reports OK - it has no cidr to compare against")
		addrCond := findCondition(updated.Status.Conditions, ConditionBootdAddressValid)
		Expect(addrCond).NotTo(BeNil())
		Expect(addrCond.Status).To(Equal(metav1.ConditionTrue))

		readyCond := findCondition(updated.Status.Conditions, keziov1alpha1.ConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(readyCond.Reason).To(Equal("BootdServerIPOutsideCIDR"))

		By("correcting bootdServerIP to fall inside cidr - the Deployment must now be created and the condition clear")
		Expect(k8sClient.Get(ctx, key, subnet)).To(Succeed())
		subnet.Spec.BootdServerIP = "192.0.2.2"
		Expect(k8sClient.Update(ctx, subnet)).To(Succeed())

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.List(ctx, &deployments, client.InNamespace(ns))).To(Succeed())
		Expect(deployments.Items).To(HaveLen(1), "the Deployment must be created once bootdServerIP is corrected")

		Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())
		cidrCond = findCondition(updated.Status.Conditions, ConditionBootdServerIPInCIDR)
		Expect(cidrCond).NotTo(BeNil())
		Expect(cidrCond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cidrCond.Reason).To(Equal("BootdServerIPInCIDR"))
	})

	It("refuses the bootd Deployment and fails Ready when the lease range is reversed", func() {
		ns := createSubnetTestNamespace(ctx)
		createTestNAD(ctx, ns, "boot-nad", bootdStaticNADConfig("192.0.2.2"))

		subnet := testSubnet(ns, func(s *keziov1alpha1.Subnet) {
			s.Spec.DHCP = keziov1alpha1.SubnetDHCP{
				Mode:            keziov1alpha1.SubnetDHCPModeLease,
				LeaseRangeStart: "192.0.2.250",
				LeaseRangeEnd:   "192.0.2.10",
			}
		})
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())

		r := newSubnetTestReconciler()
		key := types.NamespacedName{Name: subnet.Name, Namespace: ns}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var deployments appsv1.DeploymentList
		Expect(k8sClient.List(ctx, &deployments, client.InNamespace(ns))).To(Succeed())
		Expect(deployments.Items).To(BeEmpty(), "a reversed lease range must withhold the bootd Deployment, dnsmasq cannot serve it")

		var updated keziov1alpha1.Subnet
		Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())

		leaseCond := findCondition(updated.Status.Conditions, ConditionDHCPLeaseRangeValid)
		Expect(leaseCond).NotTo(BeNil())
		Expect(leaseCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(leaseCond.Reason).To(Equal("DHCPLeaseRangeReversed"))

		readyCond := findCondition(updated.Status.Conditions, keziov1alpha1.ConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(readyCond.Reason).To(Equal("DHCPLeaseRangeReversed"))
	})

	It("refuses the bootd Deployment and fails Ready when the lease range falls outside the Subnet's cidr", func() {
		ns := createSubnetTestNamespace(ctx)
		createTestNAD(ctx, ns, "boot-nad", bootdStaticNADConfig("192.0.2.2"))

		subnet := testSubnet(ns, func(s *keziov1alpha1.Subnet) {
			s.Spec.DHCP = keziov1alpha1.SubnetDHCP{
				Mode:            keziov1alpha1.SubnetDHCPModeLease,
				LeaseRangeStart: "10.0.0.5",
				LeaseRangeEnd:   "10.0.0.50",
			}
		})
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())

		r := newSubnetTestReconciler()
		key := types.NamespacedName{Name: subnet.Name, Namespace: ns}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var deployments appsv1.DeploymentList
		Expect(k8sClient.List(ctx, &deployments, client.InNamespace(ns))).To(Succeed())
		Expect(deployments.Items).To(BeEmpty(), "an out-of-cidr lease range must withhold the bootd Deployment")

		var updated keziov1alpha1.Subnet
		Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())

		leaseCond := findCondition(updated.Status.Conditions, ConditionDHCPLeaseRangeValid)
		Expect(leaseCond).NotTo(BeNil())
		Expect(leaseCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(leaseCond.Reason).To(Equal("DHCPLeaseRangeOutsideCIDR"))

		readyCond := findCondition(updated.Status.Conditions, keziov1alpha1.ConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(readyCond.Reason).To(Equal("DHCPLeaseRangeOutsideCIDR"))
	})

	It("sets a controller owner reference from the bootd Deployment to its Subnet, and drops the Subnet on delete with no finalizer to block it", func() {
		ns := createSubnetTestNamespace(ctx)
		createTestNAD(ctx, ns, "boot-nad", bootdStaticNADConfig("192.0.2.2"))

		subnet := testSubnet(ns)
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())

		r := newSubnetTestReconciler()
		key := types.NamespacedName{Name: subnet.Name, Namespace: ns}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var dep appsv1.Deployment
		depKey := types.NamespacedName{Namespace: ns, Name: bootdDeploymentName(subnet.Name)}
		Expect(k8sClient.Get(ctx, depKey, &dep)).To(Succeed())

		By("the owner reference is what garbage collection needs to reap the Deployment - envtest runs no garbage collector, so this is asserted directly rather than by observing a cascade delete")
		Expect(dep.OwnerReferences).To(HaveLen(1))
		owner := dep.OwnerReferences[0]
		Expect(owner.Kind).To(Equal("Subnet"))
		Expect(owner.Name).To(Equal(subnet.Name))
		Expect(owner.UID).To(Equal(subnet.UID))
		Expect(owner.Controller).NotTo(BeNil())
		Expect(*owner.Controller).To(BeTrue())

		By("deleting the Subnet succeeds immediately - no finalizer waits on anything")
		Expect(k8sClient.Delete(ctx, subnet)).To(Succeed())
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var deleted keziov1alpha1.Subnet
		getErr := k8sClient.Get(ctx, key, &deleted)
		Expect(kerrors.IsNotFound(getErr)).To(BeTrue(), "no finalizer means the API server removes the Subnet on delete without waiting on this controller")
	})

	It("reports a collision and withholds the Deployment when two Subnets target the same bootdNetworkRef", func() {
		ns := createSubnetTestNamespace(ctx)
		createTestNAD(ctx, ns, "boot-nad", bootdStaticNADConfig("192.0.2.2"))

		first := testSubnet(ns)
		Expect(k8sClient.Create(ctx, first)).To(Succeed())

		second := testSubnet(ns, func(s *keziov1alpha1.Subnet) {
			s.Name = "rack-2"
			s.Spec.BootdServerIP = "192.0.2.3"
		})
		Expect(k8sClient.Create(ctx, second)).To(Succeed())

		r := newSubnetTestReconciler()
		firstKey := types.NamespacedName{Name: first.Name, Namespace: ns}
		secondKey := types.NamespacedName{Name: second.Name, Namespace: ns}

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: firstKey})
		Expect(err).NotTo(HaveOccurred())
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: secondKey})
		Expect(err).NotTo(HaveOccurred())

		var updatedFirst, updatedSecond keziov1alpha1.Subnet
		Expect(k8sClient.Get(ctx, firstKey, &updatedFirst)).To(Succeed())
		Expect(k8sClient.Get(ctx, secondKey, &updatedSecond)).To(Succeed())

		By("each Subnet names the other party by name")
		firstCollision := findCondition(updatedFirst.Status.Conditions, ConditionBootdNetworkCollision)
		Expect(firstCollision).NotTo(BeNil())
		Expect(firstCollision.Status).To(Equal(metav1.ConditionFalse))
		Expect(firstCollision.Message).To(ContainSubstring(second.Name))

		secondCollision := findCondition(updatedSecond.Status.Conditions, ConditionBootdNetworkCollision)
		Expect(secondCollision).NotTo(BeNil())
		Expect(secondCollision.Status).To(Equal(metav1.ConditionFalse))
		Expect(secondCollision.Message).To(ContainSubstring(first.Name))

		By("Ready is False for both, with the collision reason - not the generic BootdDeploymentUnavailable message")
		firstReady := findCondition(updatedFirst.Status.Conditions, keziov1alpha1.ConditionReady)
		Expect(firstReady.Status).To(Equal(metav1.ConditionFalse))
		Expect(firstReady.Reason).To(Equal("BootdNetworkCollision"))

		By("second also has a BootdServerIP mismatch, but the collision - the check that actually withholds its Deployment - takes precedence over the non-blocking address mismatch")
		secondReady := findCondition(updatedSecond.Status.Conditions, keziov1alpha1.ConditionReady)
		Expect(secondReady.Status).To(Equal(metav1.ConditionFalse))
		Expect(secondReady.Reason).To(Equal("BootdNetworkCollision"))
		Expect(secondReady.Message).To(ContainSubstring(first.Name))

		By("neither Subnet gets a bootd Deployment created while the collision persists")
		var deployments appsv1.DeploymentList
		Expect(k8sClient.List(ctx, &deployments, client.InNamespace(ns))).To(Succeed())
		Expect(deployments.Items).To(BeEmpty(), "reconciling a colliding Subnet must not create its bootd Deployment")
	})

	It("recovers a withheld Deployment once the colliding BootdNetworkRef is repointed at a non-colliding NAD", func() {
		ns := createSubnetTestNamespace(ctx)
		createTestNAD(ctx, ns, "boot-nad", bootdStaticNADConfig("192.0.2.2"))
		createTestNAD(ctx, ns, "boot-nad-2", bootdStaticNADConfig("192.0.3.2"))

		first := testSubnet(ns)
		Expect(k8sClient.Create(ctx, first)).To(Succeed())

		second := testSubnet(ns, func(s *keziov1alpha1.Subnet) {
			s.Name = "rack-2"
			s.Spec.BootdServerIP = "192.0.2.3"
		})
		Expect(k8sClient.Create(ctx, second)).To(Succeed())

		r := newSubnetTestReconciler()
		firstKey := types.NamespacedName{Name: first.Name, Namespace: ns}
		secondKey := types.NamespacedName{Name: second.Name, Namespace: ns}

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: firstKey})
		Expect(err).NotTo(HaveOccurred())
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: secondKey})
		Expect(err).NotTo(HaveOccurred())

		var deployments appsv1.DeploymentList
		Expect(k8sClient.List(ctx, &deployments, client.InNamespace(ns))).To(Succeed())
		Expect(deployments.Items).To(BeEmpty(), "the collision must withhold both Deployments before this test can prove recovery")

		By("repointing second's BootdNetworkRef at a non-colliding NAD, on its own distinct segment")
		Expect(k8sClient.Get(ctx, secondKey, second)).To(Succeed())
		second.Spec.CIDR = "192.0.3.0/24"
		second.Spec.BootdServerIP = "192.0.3.2"
		second.Spec.BootdNetworkRef = keziov1alpha1.NameRef{Name: "boot-nad-2"}
		Expect(k8sClient.Update(ctx, second)).To(Succeed())

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: firstKey})
		Expect(err).NotTo(HaveOccurred())
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: secondKey})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.List(ctx, &deployments, client.InNamespace(ns))).To(Succeed())
		Expect(deployments.Items).To(HaveLen(2), "both Subnets must get their bootd Deployment once the collision is resolved")

		var updatedFirst, updatedSecond keziov1alpha1.Subnet
		Expect(k8sClient.Get(ctx, firstKey, &updatedFirst)).To(Succeed())
		Expect(k8sClient.Get(ctx, secondKey, &updatedSecond)).To(Succeed())

		Expect(findCondition(updatedFirst.Status.Conditions, ConditionBootdNetworkCollision)).To(BeNil(), "the pruned collision condition must not be left frozen at its last verdict")
		Expect(findCondition(updatedSecond.Status.Conditions, ConditionBootdNetworkCollision)).To(BeNil())
	})

	It("does not flag a collision for two Subnets in one Site on distinct bootdNetworkRefs", func() {
		ns := createSubnetTestNamespace(ctx)
		createTestNAD(ctx, ns, "boot-nad", bootdStaticNADConfig("192.0.2.2"))
		createTestNAD(ctx, ns, "boot-nad-2", bootdStaticNADConfig("192.0.3.2"))

		first := testSubnet(ns)
		Expect(k8sClient.Create(ctx, first)).To(Succeed())

		second := testSubnet(ns, func(s *keziov1alpha1.Subnet) {
			s.Name = "rack-2"
			s.Spec.CIDR = "192.0.3.0/24"
			s.Spec.BootdServerIP = "192.0.3.2"
			s.Spec.BootdNetworkRef = keziov1alpha1.NameRef{Name: "boot-nad-2"}
		})
		Expect(k8sClient.Create(ctx, second)).To(Succeed())

		r := newSubnetTestReconciler()
		firstKey := types.NamespacedName{Name: first.Name, Namespace: ns}
		secondKey := types.NamespacedName{Name: second.Name, Namespace: ns}

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: firstKey})
		Expect(err).NotTo(HaveOccurred())
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: secondKey})
		Expect(err).NotTo(HaveOccurred())

		var updatedFirst, updatedSecond keziov1alpha1.Subnet
		Expect(k8sClient.Get(ctx, firstKey, &updatedFirst)).To(Succeed())
		Expect(k8sClient.Get(ctx, secondKey, &updatedSecond)).To(Succeed())

		Expect(findCondition(updatedFirst.Status.Conditions, ConditionBootdNetworkCollision)).To(BeNil(), "distinct bootdNetworkRefs in one Site are a legitimate multi-Subnet topology, not a collision")
		Expect(findCondition(updatedSecond.Status.Conditions, ConditionBootdNetworkCollision)).To(BeNil())

		var deployments appsv1.DeploymentList
		Expect(k8sClient.List(ctx, &deployments, client.InNamespace(ns))).To(Succeed())
		Expect(deployments.Items).To(HaveLen(2), "each non-colliding Subnet still gets its own bootd Deployment")
	})

	It("does not flag a collision for two Subnets in different namespaces naming the same bootdNetworkRef, since a NAD is itself namespaced", func() {
		nsA := createSubnetTestNamespace(ctx)
		nsB := createSubnetTestNamespace(ctx)
		createTestNAD(ctx, nsA, "boot-nad", bootdStaticNADConfig("192.0.2.2"))
		createTestNAD(ctx, nsB, "boot-nad", bootdStaticNADConfig("192.0.2.2"))

		subnetA := testSubnet(nsA)
		Expect(k8sClient.Create(ctx, subnetA)).To(Succeed())
		subnetB := testSubnet(nsB)
		Expect(k8sClient.Create(ctx, subnetB)).To(Succeed())

		r := newSubnetTestReconciler()
		keyA := types.NamespacedName{Name: subnetA.Name, Namespace: nsA}
		keyB := types.NamespacedName{Name: subnetB.Name, Namespace: nsB}

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: keyA})
		Expect(err).NotTo(HaveOccurred())
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: keyB})
		Expect(err).NotTo(HaveOccurred())

		var updatedA, updatedB keziov1alpha1.Subnet
		Expect(k8sClient.Get(ctx, keyA, &updatedA)).To(Succeed())
		Expect(k8sClient.Get(ctx, keyB, &updatedB)).To(Succeed())

		Expect(findCondition(updatedA.Status.Conditions, ConditionBootdNetworkCollision)).To(BeNil(), "same NAD name in different namespaces names two distinct NAD objects, not one shared segment")
		Expect(findCondition(updatedB.Status.Conditions, ConditionBootdNetworkCollision)).To(BeNil())
	})

	It("reports a collision for two Subnets in different namespaces when one names the other's NAD via an explicit BootdNetworkRef.Namespace", func() {
		nsA := createSubnetTestNamespace(ctx)
		nsB := createSubnetTestNamespace(ctx)
		createTestNAD(ctx, nsB, "boot-nad", bootdStaticNADConfig("192.0.2.2"))

		subnetA := testSubnet(nsA, func(s *keziov1alpha1.Subnet) {
			s.Spec.BootdNetworkRef = keziov1alpha1.NameRef{Namespace: nsB, Name: "boot-nad"}
		})
		Expect(k8sClient.Create(ctx, subnetA)).To(Succeed())

		subnetB := testSubnet(nsB)
		Expect(k8sClient.Create(ctx, subnetB)).To(Succeed())

		r := newSubnetTestReconciler()
		keyA := types.NamespacedName{Name: subnetA.Name, Namespace: nsA}
		keyB := types.NamespacedName{Name: subnetB.Name, Namespace: nsB}

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: keyA})
		Expect(err).NotTo(HaveOccurred())
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: keyB})
		Expect(err).NotTo(HaveOccurred())

		var updatedA, updatedB keziov1alpha1.Subnet
		Expect(k8sClient.Get(ctx, keyA, &updatedA)).To(Succeed())
		Expect(k8sClient.Get(ctx, keyB, &updatedB)).To(Succeed())

		By("both resolve to nsB/boot-nad, so this is a genuine collision even though the two Subnets live in different namespaces")
		collisionA := findCondition(updatedA.Status.Conditions, ConditionBootdNetworkCollision)
		Expect(collisionA).NotTo(BeNil())
		Expect(collisionA.Status).To(Equal(metav1.ConditionFalse))
		Expect(collisionA.Message).To(ContainSubstring(nsB))
		Expect(collisionA.Message).To(ContainSubstring(subnetB.Name))
		Expect(collisionA.Message).To(ContainSubstring(nsB+"/boot-nad"), "the shared NAD is named by its resolved namespace, not the bare ref name")

		collisionB := findCondition(updatedB.Status.Conditions, ConditionBootdNetworkCollision)
		Expect(collisionB).NotTo(BeNil())
		Expect(collisionB.Status).To(Equal(metav1.ConditionFalse))
		Expect(collisionB.Message).To(ContainSubstring(nsA))
		Expect(collisionB.Message).To(ContainSubstring(subnetA.Name))
		Expect(collisionB.Message).To(ContainSubstring(nsB+"/boot-nad"), "subnetB's own BootdNetworkRef is implicit, but it must still resolve to nsB")

		By("neither Subnet gets a bootd Deployment created while the collision persists")
		var deploymentsA, deploymentsB appsv1.DeploymentList
		Expect(k8sClient.List(ctx, &deploymentsA, client.InNamespace(nsA))).To(Succeed())
		Expect(deploymentsA.Items).To(BeEmpty())
		Expect(k8sClient.List(ctx, &deploymentsB, client.InNamespace(nsB))).To(Succeed())
		Expect(deploymentsB.Items).To(BeEmpty())
	})

	It("reports a collision and refuses to adopt or overwrite a pre-existing Deployment this controller does not own", func() {
		ns := createSubnetTestNamespace(ctx)
		createTestNAD(ctx, ns, "boot-nad", bootdStaticNADConfig("192.0.2.2"))

		subnet := testSubnet(ns)
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())

		By("hand-applying a Deployment at the exact name the reconciler would use, with no owner reference")
		handApplied := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      bootdDeploymentName(subnet.Name),
				Namespace: ns,
			},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "hand-applied"}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "hand-applied"}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "busybox"}}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, handApplied)).To(Succeed())
		originalGeneration := handApplied.Generation

		r := newSubnetTestReconciler()
		key := types.NamespacedName{Name: subnet.Name, Namespace: ns}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		By("the hand-applied Deployment is untouched: no owner reference stamped, no spec overwritten")
		var afterReconcile appsv1.Deployment
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: handApplied.Name}, &afterReconcile)).To(Succeed())
		Expect(afterReconcile.OwnerReferences).To(BeEmpty())
		Expect(afterReconcile.Generation).To(Equal(originalGeneration))
		Expect(afterReconcile.Spec.Template.Spec.Containers[0].Image).To(Equal("busybox"))

		var updated keziov1alpha1.Subnet
		Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())
		ownershipCond := findCondition(updated.Status.Conditions, ConditionBootdDeploymentOwnership)
		Expect(ownershipCond).NotTo(BeNil())
		Expect(ownershipCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(ownershipCond.Message).To(ContainSubstring(handApplied.Name))

		readyCond := findCondition(updated.Status.Conditions, keziov1alpha1.ConditionReady)
		Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(readyCond.Reason).To(Equal("BootdDeploymentOwnership"))
	})

	It("bounds RequeueAfter while a foreign Deployment occupies the name, and creates its own once the foreign one is gone", func() {
		ns := createSubnetTestNamespace(ctx)
		createTestNAD(ctx, ns, "boot-nad", bootdStaticNADConfig("192.0.2.2"))

		subnet := testSubnet(ns)
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())

		By("hand-applying a Deployment at the exact name the reconciler would use, with no owner reference")
		handApplied := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      bootdDeploymentName(subnet.Name),
				Namespace: ns,
			},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "hand-applied"}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "hand-applied"}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "busybox"}}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, handApplied)).To(Succeed())

		r := newSubnetTestReconciler()
		key := types.NamespacedName{Name: subnet.Name, Namespace: ns}
		result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		By("Owns' owner-reference watch cannot see this foreign Deployment's later deletion, so onChange must not fall back to the manager's default resync")
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		Expect(result.RequeueAfter).To(BeNumerically("<=", bootdDeploymentOwnershipRequeueAfter))

		var updated keziov1alpha1.Subnet
		Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())
		ownershipCond := findCondition(updated.Status.Conditions, ConditionBootdDeploymentOwnership)
		Expect(ownershipCond).NotTo(BeNil())
		Expect(ownershipCond.Status).To(Equal(metav1.ConditionFalse))

		By("deleting the foreign Deployment and reconciling again - the Subnet's own Deployment must now be created")
		Expect(k8sClient.Delete(ctx, handApplied)).To(Succeed())

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: bootdDeploymentName(subnet.Name)}, &dep)).To(Succeed())
		Expect(dep.OwnerReferences).To(HaveLen(1), "this Deployment is now the reconciler's own, not the hand-applied one")
		Expect(dep.OwnerReferences[0].UID).To(Equal(subnet.UID))

		Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())
		Expect(findCondition(updated.Status.Conditions, ConditionBootdDeploymentOwnership)).To(BeNil(), "the ownership violation must clear once the foreign object is gone")
	})

	It("reports BootdNamespacePSALabelMissing, and only that, when the namespace lacks the PSA label but the ServiceAccount exists", func() {
		ns := createBareNamespace(ctx)
		createBootdServiceAccount(ctx, ns)
		createTestNAD(ctx, ns, "boot-nad", bootdStaticNADConfig("192.0.2.2"))
		createTestSite(ctx, ns)

		subnet := testSubnet(ns)
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())

		r := newSubnetTestReconciler()
		key := types.NamespacedName{Name: subnet.Name, Namespace: ns}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var updated keziov1alpha1.Subnet
		Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())

		labelCond := findCondition(updated.Status.Conditions, ConditionBootdNamespacePSALabel)
		Expect(labelCond).NotTo(BeNil())
		Expect(labelCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(labelCond.Reason).To(Equal("BootdNamespacePSALabelMissing"))
		Expect(labelCond.Message).To(ContainSubstring("pod-security.kubernetes.io/enforce=privileged"))

		By("the ServiceAccount check reports present, not the same cause")
		saCond := findCondition(updated.Status.Conditions, ConditionBootdServiceAccount)
		Expect(saCond).NotTo(BeNil())
		Expect(saCond.Status).To(Equal(metav1.ConditionTrue))
		Expect(saCond.Reason).To(Equal("BootdServiceAccountPresent"))

		readyCond := findCondition(updated.Status.Conditions, keziov1alpha1.ConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(readyCond.Reason).To(Equal("BootdNamespacePSALabelMissing"), "Ready must name the missing label, not the generic BootdDeploymentUnavailable placeholder")

		By("neither prerequisite is in violationBlocks, so the Deployment must still be created")
		var deployments appsv1.DeploymentList
		Expect(k8sClient.List(ctx, &deployments, client.InNamespace(ns))).To(Succeed())
		Expect(deployments.Items).To(HaveLen(1), "a missing PSA label must not withhold the bootd Deployment - it is reported, not blocking")
	})

	It("reports BootdServiceAccountMissing, and only that, when the kezio-bootd ServiceAccount is absent but the PSA label is present", func() {
		ns := createBareNamespace(ctx)
		var nsObj corev1.Namespace
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ns}, &nsObj)).To(Succeed())
		nsObj.Labels = map[string]string{"pod-security.kubernetes.io/enforce": "privileged"}
		Expect(k8sClient.Update(ctx, &nsObj)).To(Succeed())
		createTestNAD(ctx, ns, "boot-nad", bootdStaticNADConfig("192.0.2.2"))
		createTestSite(ctx, ns)

		subnet := testSubnet(ns)
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())

		r := newSubnetTestReconciler()
		key := types.NamespacedName{Name: subnet.Name, Namespace: ns}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var updated keziov1alpha1.Subnet
		Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())

		saCond := findCondition(updated.Status.Conditions, ConditionBootdServiceAccount)
		Expect(saCond).NotTo(BeNil())
		Expect(saCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(saCond.Reason).To(Equal("BootdServiceAccountMissing"))
		Expect(saCond.Message).To(ContainSubstring(bootdDefaultServiceAccountName))

		By("the PSA label check reports present, not the same cause")
		labelCond := findCondition(updated.Status.Conditions, ConditionBootdNamespacePSALabel)
		Expect(labelCond).NotTo(BeNil())
		Expect(labelCond.Status).To(Equal(metav1.ConditionTrue))

		readyCond := findCondition(updated.Status.Conditions, keziov1alpha1.ConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(readyCond.Reason).To(Equal("BootdServiceAccountMissing"), "Ready must name the missing ServiceAccount, not the generic BootdDeploymentUnavailable placeholder")

		By("neither prerequisite is in violationBlocks, so the Deployment must still be created")
		var deployments appsv1.DeploymentList
		Expect(k8sClient.List(ctx, &deployments, client.InNamespace(ns))).To(Succeed())
		Expect(deployments.Items).To(HaveLen(1), "a missing ServiceAccount must not withhold the bootd Deployment - it is reported, not blocking")
	})

	It("reports both prerequisite violations when neither is present, with Ready naming the earliest-appended one", func() {
		// checkBootdNamespacePrerequisites appends the PSA-label check
		// before the ServiceAccount check, so the earliest-appended
		// tie-break means Ready must name the PSA label.
		ns := createBareNamespace(ctx)
		createTestNAD(ctx, ns, "boot-nad", bootdStaticNADConfig("192.0.2.2"))
		createTestSite(ctx, ns)

		subnet := testSubnet(ns)
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())

		r := newSubnetTestReconciler()
		key := types.NamespacedName{Name: subnet.Name, Namespace: ns}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var updated keziov1alpha1.Subnet
		Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())

		labelCond := findCondition(updated.Status.Conditions, ConditionBootdNamespacePSALabel)
		Expect(labelCond).NotTo(BeNil())
		Expect(labelCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(labelCond.Reason).To(Equal("BootdNamespacePSALabelMissing"))

		saCond := findCondition(updated.Status.Conditions, ConditionBootdServiceAccount)
		Expect(saCond).NotTo(BeNil())
		Expect(saCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(saCond.Reason).To(Equal("BootdServiceAccountMissing"))

		readyCond := findCondition(updated.Status.Conditions, keziov1alpha1.ConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(readyCond.Reason).To(Equal("BootdNamespacePSALabelMissing"), "the PSA-label check is appended first in checkBootdNamespacePrerequisites, so it is the earliest-appended blocking Violation")

		By("neither prerequisite is in violationBlocks, so the Deployment must still be created")
		var deployments appsv1.DeploymentList
		Expect(k8sClient.List(ctx, &deployments, client.InNamespace(ns))).To(Succeed())
		Expect(deployments.Items).To(HaveLen(1), "both prerequisites missing at once must still not withhold the bootd Deployment")
	})

	It("bounds RequeueAfter while the PSA label is missing, and recovers once it is added", func() {
		ns := createBareNamespace(ctx)
		createBootdServiceAccount(ctx, ns)
		createTestNAD(ctx, ns, "boot-nad", bootdStaticNADConfig("192.0.2.2"))
		createTestSite(ctx, ns)

		subnet := testSubnet(ns)
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())

		r := newSubnetTestReconciler()
		key := types.NamespacedName{Name: subnet.Name, Namespace: ns}
		result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		By("neither Namespace nor ServiceAccount is watched, so onChange must bound the wait itself instead of relying on the manager's default resync")
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		Expect(result.RequeueAfter).To(BeNumerically("<=", bootdNamespacePrerequisiteRequeueAfter))

		var updated keziov1alpha1.Subnet
		Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())
		labelCond := findCondition(updated.Status.Conditions, ConditionBootdNamespacePSALabel)
		Expect(labelCond).NotTo(BeNil())
		Expect(labelCond.Status).To(Equal(metav1.ConditionFalse))

		var deployments appsv1.DeploymentList
		Expect(k8sClient.List(ctx, &deployments, client.InNamespace(ns))).To(Succeed())
		Expect(deployments.Items).To(HaveLen(1), "the Deployment must already exist - this prerequisite is non-blocking")

		By("adding the PSA label and reconciling again - as onChange's own RequeueAfter would eventually trigger")
		var nsObj corev1.Namespace
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ns}, &nsObj)).To(Succeed())
		nsObj.Labels = map[string]string{"pod-security.kubernetes.io/enforce": "privileged"}
		Expect(k8sClient.Update(ctx, &nsObj)).To(Succeed())

		result, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeZero(), "once both prerequisites are satisfied, onChange has no reason left to bound the wait")

		Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())
		labelCond = findCondition(updated.Status.Conditions, ConditionBootdNamespacePSALabel)
		Expect(labelCond).NotTo(BeNil())
		Expect(labelCond.Status).To(Equal(metav1.ConditionTrue))
	})

	It("bounds RequeueAfter while the ServiceAccount is missing, and recovers once it is created", func() {
		ns := createBareNamespace(ctx)
		var nsObj corev1.Namespace
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: ns}, &nsObj)).To(Succeed())
		nsObj.Labels = map[string]string{"pod-security.kubernetes.io/enforce": "privileged"}
		Expect(k8sClient.Update(ctx, &nsObj)).To(Succeed())
		createTestNAD(ctx, ns, "boot-nad", bootdStaticNADConfig("192.0.2.2"))
		createTestSite(ctx, ns)

		subnet := testSubnet(ns)
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())

		r := newSubnetTestReconciler()
		key := types.NamespacedName{Name: subnet.Name, Namespace: ns}
		result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		By("neither Namespace nor ServiceAccount is watched, so onChange must bound the wait itself instead of relying on the manager's default resync")
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		Expect(result.RequeueAfter).To(BeNumerically("<=", bootdNamespacePrerequisiteRequeueAfter))

		var updated keziov1alpha1.Subnet
		Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())
		saCond := findCondition(updated.Status.Conditions, ConditionBootdServiceAccount)
		Expect(saCond).NotTo(BeNil())
		Expect(saCond.Status).To(Equal(metav1.ConditionFalse))

		var deployments appsv1.DeploymentList
		Expect(k8sClient.List(ctx, &deployments, client.InNamespace(ns))).To(Succeed())
		Expect(deployments.Items).To(HaveLen(1), "the Deployment must already exist - this prerequisite is non-blocking")

		By("creating the ServiceAccount and reconciling again - as onChange's own RequeueAfter would eventually trigger")
		createBootdServiceAccount(ctx, ns)

		result, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeZero(), "once both prerequisites are satisfied, onChange has no reason left to bound the wait")

		Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())
		saCond = findCondition(updated.Status.Conditions, ConditionBootdServiceAccount)
		Expect(saCond).NotTo(BeNil())
		Expect(saCond.Status).To(Equal(metav1.ConditionTrue))
	})

	It("reports both prerequisites present, distinct from a nodeSelector-caused rollout failure surfaced from the Deployment's own conditions", func() {
		ns := createSubnetTestNamespace(ctx)
		createTestNAD(ctx, ns, "boot-nad", bootdStaticNADConfig("192.0.2.2"))

		subnet := testSubnet(ns)
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())

		r := newSubnetTestReconciler()
		key := types.NamespacedName{Name: subnet.Name, Namespace: ns}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var updated keziov1alpha1.Subnet
		Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())
		Expect(findCondition(updated.Status.Conditions, ConditionBootdNamespacePSALabel).Status).To(Equal(metav1.ConditionTrue))
		Expect(findCondition(updated.Status.Conditions, ConditionBootdServiceAccount).Status).To(Equal(metav1.ConditionTrue))

		By("simulating a nodeSelector matching no node: the real Deployment controller would report this as Progressing=False, ProgressDeadlineExceeded")
		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: bootdDeploymentName(subnet.Name)}, &dep)).To(Succeed())
		dep.Status.Conditions = []appsv1.DeploymentCondition{{
			Type:    appsv1.DeploymentProgressing,
			Status:  corev1.ConditionFalse,
			Reason:  "ProgressDeadlineExceeded",
			Message: "ReplicaSet \"kezio-bootd-rack-1-abc\" has timed out progressing.",
		}}
		Expect(k8sClient.Status().Update(ctx, &dep)).To(Succeed())

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())

		By("both namespace prerequisite conditions stay True - this is a third, distinct cause neither of them could have caught")
		Expect(findCondition(updated.Status.Conditions, ConditionBootdNamespacePSALabel).Status).To(Equal(metav1.ConditionTrue))
		Expect(findCondition(updated.Status.Conditions, ConditionBootdServiceAccount).Status).To(Equal(metav1.ConditionTrue))

		readyCond := findCondition(updated.Status.Conditions, keziov1alpha1.ConditionReady)
		Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(readyCond.Reason).To(Equal("ProgressDeadlineExceeded"), "the Deployment's own rollout-failure reason must reach Ready, not the generic placeholder nor either namespace-prerequisite reason")
		Expect(readyCond.Message).To(ContainSubstring("timed out progressing"))
	})

	It("reports SiteRefNotFound for a dangling siteRef without withholding the bootd Deployment, then clears once the Site is created", func() {
		ns := createSubnetTestNamespace(ctx)
		createTestNAD(ctx, ns, "boot-nad", bootdStaticNADConfig("192.0.2.2"))

		subnet := testSubnet(ns, func(s *keziov1alpha1.Subnet) {
			s.Spec.SiteRef = keziov1alpha1.NameRef{Name: "ghost"}
		})
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())

		r := newSubnetTestReconciler()
		key := types.NamespacedName{Name: subnet.Name, Namespace: ns}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var updated keziov1alpha1.Subnet
		Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())

		siteCond := findCondition(updated.Status.Conditions, ConditionSiteRefValid)
		Expect(siteCond).NotTo(BeNil())
		Expect(siteCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(siteCond.Reason).To(Equal("SiteRefNotFound"))
		Expect(siteCond.Message).To(ContainSubstring("ghost"))

		By("the bootd Deployment is still created - SiteRefValid does not withhold it")
		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: bootdDeploymentName(subnet.Name)}, &dep)).To(Succeed())

		By("Ready surfaces the dangling siteRef, per updateSubnetConditions's Violation-first precedence")
		readyCond := findCondition(updated.Status.Conditions, keziov1alpha1.ConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(readyCond.Reason).To(Equal("SiteRefNotFound"))

		By("creating the missing Site and reconciling again - the condition must clear promptly")
		site := &keziov1alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "ghost", Namespace: ns}}
		Expect(k8sClient.Create(ctx, site)).To(Succeed())

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())

		siteCond = findCondition(updated.Status.Conditions, ConditionSiteRefValid)
		Expect(siteCond).NotTo(BeNil())
		Expect(siteCond.Status).To(Equal(metav1.ConditionTrue))
		Expect(siteCond.Reason).To(Equal("SiteRefResolved"))
	})

	It("prunes DHCPLeaseRangeValid entirely, not merely staling it, once the Subnet moves off lease mode", func() {
		ns := createSubnetTestNamespace(ctx)
		createTestNAD(ctx, ns, "boot-nad", bootdStaticNADConfig("192.0.2.2"))

		subnet := testSubnet(ns, func(s *keziov1alpha1.Subnet) {
			s.Spec.DHCP = keziov1alpha1.SubnetDHCP{
				Mode:            keziov1alpha1.SubnetDHCPModeLease,
				LeaseRangeStart: "192.0.2.250",
				LeaseRangeEnd:   "192.0.2.10",
			}
		})
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())

		r := newSubnetTestReconciler()
		key := types.NamespacedName{Name: subnet.Name, Namespace: ns}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var updated keziov1alpha1.Subnet
		Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())
		leaseCond := findCondition(updated.Status.Conditions, ConditionDHCPLeaseRangeValid)
		Expect(leaseCond).NotTo(BeNil())
		Expect(leaseCond.Status).To(Equal(metav1.ConditionFalse))

		By("switching Mode to proxy - the lease range fields no longer apply")
		Expect(k8sClient.Get(ctx, key, subnet)).To(Succeed())
		subnet.Spec.DHCP = keziov1alpha1.SubnetDHCP{Mode: keziov1alpha1.SubnetDHCPModeProxy}
		Expect(k8sClient.Update(ctx, subnet)).To(Succeed())

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())

		Expect(findCondition(updated.Status.Conditions, ConditionDHCPLeaseRangeValid)).To(BeNil(), "the obsolete condition must be removed, not left frozen at its stale False verdict")

		By("stamping the now-created Deployment Available, as the real Deployment controller would, so Ready has nothing else left to withhold it")
		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: bootdDeploymentName(subnet.Name)}, &dep)).To(Succeed())
		dep.Status.Conditions = []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue}}
		Expect(k8sClient.Status().Update(ctx, &dep)).To(Succeed())

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())

		readyCond := findCondition(updated.Status.Conditions, keziov1alpha1.ConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionTrue), "Ready must recover once the only Violation is gone")
	})
})

// This spec runs a real, second manager (via SetupWithManager) to
// exercise the Watch + mapSiteToSubnets wiring that direct r.Reconcile
// calls elsewhere in this file never touch. The cache is scoped to one
// namespace so leftover Subnets/Sites from earlier specs (envtest never
// finalizes deleted namespaces) cannot race this manager's cluster-wide
// Lists.
var _ = Describe("Subnet Site watch wiring", func() {
	It("clears ConditionSiteRefValid once the Site is created, with no manual Reconcile call - proving the Watch and its mapper", func() {
		ctx := context.Background()
		ns := createBareNamespace(ctx)
		createBootdServiceAccount(ctx, ns)

		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme: k8sClient.Scheme(),
			Cache: cache.Options{
				DefaultNamespaces: map[string]cache.Config{ns: {}},
			},
			Metrics:                metricsserver.Options{BindAddress: "0"},
			LeaderElection:         false,
			HealthProbeBindAddress: "0",
		})
		Expect(err).NotTo(HaveOccurred())

		reconciler := &SubnetReconciler{
			Client: mgr.GetClient(),
			Scheme: mgr.GetScheme(),
			BootdDeployment: BootdDeploymentConfig{
				Image:              "bootd:test",
				BootArtifactsImage: "boot-artifacts:test",
			},
		}
		Expect(reconciler.SetupWithManager(mgr)).To(Succeed())

		mgrCtx, mgrCancel := context.WithCancel(ctx)
		DeferCleanup(mgrCancel)
		go func() {
			defer GinkgoRecover()
			Expect(mgr.Start(mgrCtx)).To(Succeed())
		}()
		Expect(mgr.GetCache().WaitForCacheSync(mgrCtx)).To(BeTrue())

		createTestNAD(ctx, ns, "boot-nad", bootdStaticNADConfig("192.0.2.2"))

		subnet := testSubnet(ns, func(s *keziov1alpha1.Subnet) {
			s.Spec.SiteRef = keziov1alpha1.NameRef{Name: "watch-site"}
		})
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())

		key := types.NamespacedName{Name: subnet.Name, Namespace: ns}
		var updated keziov1alpha1.Subnet
		Eventually(func() *metav1.Condition {
			if err := k8sClient.Get(ctx, key, &updated); err != nil {
				return nil
			}
			return findCondition(updated.Status.Conditions, ConditionSiteRefValid)
		}, "10s", "100ms").Should(And(
			Not(BeNil()),
			WithTransform(func(c *metav1.Condition) metav1.ConditionStatus { return c.Status }, Equal(metav1.ConditionFalse)),
		))

		By("creating the Site with no manual Reconcile call - only the Watch + mapSiteToSubnets can make this clear")
		site := &keziov1alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "watch-site", Namespace: ns}}
		Expect(k8sClient.Create(ctx, site)).To(Succeed())

		Eventually(func() metav1.ConditionStatus {
			if err := k8sClient.Get(ctx, key, &updated); err != nil {
				return metav1.ConditionUnknown
			}
			cond := findCondition(updated.Status.Conditions, ConditionSiteRefValid)
			if cond == nil {
				return metav1.ConditionUnknown
			}
			return cond.Status
		}, "10s", "100ms").Should(Equal(metav1.ConditionTrue))
	})
})
