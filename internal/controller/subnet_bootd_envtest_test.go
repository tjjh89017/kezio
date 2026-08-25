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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
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

// newSubnetTestReconciler returns a SubnetReconciler with BootdDeployment
// enabled against fake images.
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

// createSubnetTestNamespace creates a fresh namespace for one spec, with
// the PSA label, the kezio-bootd ServiceAccount, and a bare Site named
// "hq" (testSubnet's default spec.siteRef) already present, so specs
// exercise the healthy case unless they deliberately withhold one.
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
	ExpectWithOffset(1, k8sClient.Create(ctx, &keziov1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "hq", Namespace: name}})).To(Succeed())
	return name
}

// createBootdServiceAccount creates the ServiceAccount buildBootdDeployment
// stamps as serviceAccountName, in namespace ns.
func createBootdServiceAccount(ctx context.Context, ns string) {
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: bootdDefaultServiceAccountName, Namespace: ns}}
	ExpectWithOffset(1, k8sClient.Create(ctx, sa)).To(Succeed())
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

	It("creates exactly one bootd Deployment, in the Subnet's own namespace, with the NAD annotation and required env, and Ready follows Deployment availability", func() {
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
		Expect(dep.Spec.Template.Spec.NodeSelector).To(BeNil())

		var bootdC *corev1.Container
		for i := range dep.Spec.Template.Spec.Containers {
			if dep.Spec.Template.Spec.Containers[i].Name == "bootd" {
				bootdC = &dep.Spec.Template.Spec.Containers[i]
			}
		}
		Expect(bootdC).NotTo(BeNil())
		Expect(bootdC.Env).To(ContainElement(corev1.EnvVar{Name: "BOOTD_SERVER_IP", Value: "192.0.2.2"}))
		Expect(bootdC.Env).To(ContainElement(corev1.EnvVar{Name: "BOOTD_PROVISIONING_CIDR", Value: "192.0.2.0/24"}))

		var afterCreate keziov1alpha3.Subnet
		Expect(k8sClient.Get(ctx, key, &afterCreate)).To(Succeed())
		validCond := findCondition(afterCreate.Status.Conditions, keziov1alpha3.SubnetConditionValid)
		Expect(validCond).NotTo(BeNil())
		Expect(validCond.Status).To(Equal(metav1.ConditionTrue))
		readyCond := findCondition(afterCreate.Status.Conditions, keziov1alpha3.SubnetConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionFalse), "the Deployment envtest just created never becomes Available - no real Deployment controller runs here")
		Expect(readyCond.Reason).To(Equal("BootdDeploymentUnavailable"))

		By("stamping the Deployment Available, as the real Deployment controller would")
		dep.Status.Conditions = []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue}}
		Expect(k8sClient.Status().Update(ctx, &dep)).To(Succeed())

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		var afterAvailable keziov1alpha3.Subnet
		Expect(k8sClient.Get(ctx, key, &afterAvailable)).To(Succeed())
		readyCond = findCondition(afterAvailable.Status.Conditions, keziov1alpha3.SubnetConditionReady)
		Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))
		Expect(readyCond.Reason).To(Equal("BootdReady"))

		By("re-reconciling does not create a second Deployment")
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.List(ctx, &deployments, client.InNamespace(ns))).To(Succeed())
		Expect(deployments.Items).To(HaveLen(1))
	})

	It("still creates the Deployment when bootdServerIP disagrees with the NAD's static address, but fails Valid/Ready", func() {
		ns := createSubnetTestNamespace(ctx)
		// NAD address (192.0.2.9) deliberately differs from bootdServerIP
		// (192.0.2.2) below - the mismatch CheckBootdAddress exists to
		// catch.
		createTestNAD(ctx, ns, "boot-nad", bootdStaticNADConfig("192.0.2.9"))

		subnet := testSubnet(ns)
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())

		r := newSubnetTestReconciler()
		key := types.NamespacedName{Name: subnet.Name, Namespace: ns}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var deployments appsv1.DeploymentList
		Expect(k8sClient.List(ctx, &deployments, client.InNamespace(ns))).To(Succeed())
		Expect(deployments.Items).To(HaveLen(1), "a non-blocking CheckBootdAddress Violation must not withhold the Deployment")

		var updated keziov1alpha3.Subnet
		Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())

		validCond := findCondition(updated.Status.Conditions, keziov1alpha3.SubnetConditionValid)
		Expect(validCond).NotTo(BeNil())
		Expect(validCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(validCond.Reason).To(Equal("BootdAddressMismatch"))

		readyCond := findCondition(updated.Status.Conditions, keziov1alpha3.SubnetConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(readyCond.Reason).To(Equal("BootdAddressMismatch"))
	})

	It("refuses the bootd Deployment and fails Valid/Ready when bootdServerIP is outside the Subnet's cidr, a blocking check", func() {
		ns := createSubnetTestNamespace(ctx)
		// NAD address matches bootdServerIP exactly, so only
		// CheckBootdServerIPInCIDR (not the bootd-address check) can catch
		// this out-of-cidr case.
		createTestNAD(ctx, ns, "boot-nad", bootdStaticNADConfig("203.0.113.5"))

		subnet := testSubnet(ns, func(s *keziov1alpha3.Subnet) {
			s.Spec.BootdServerIP = "203.0.113.5"
		})
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())

		r := newSubnetTestReconciler()
		key := types.NamespacedName{Name: subnet.Name, Namespace: ns}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var deployments appsv1.DeploymentList
		Expect(k8sClient.List(ctx, &deployments, client.InNamespace(ns))).To(Succeed())
		Expect(deployments.Items).To(BeEmpty(), "an out-of-cidr bootdServerIP must withhold the Deployment entirely")

		var updated keziov1alpha3.Subnet
		Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())
		validCond := findCondition(updated.Status.Conditions, keziov1alpha3.SubnetConditionValid)
		Expect(validCond).NotTo(BeNil())
		Expect(validCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(validCond.Reason).To(Equal("BootdServerIPOutsideCIDR"))

		readyCond := findCondition(updated.Status.Conditions, keziov1alpha3.SubnetConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(readyCond.Reason).To(Equal("BootdServerIPOutsideCIDR"))
	})

	// A lease-mode Subnet's gateway must survive the API server round
	// trip into the bootd container's env, empty string included: the
	// empty value is what tells bootd this segment has no exit, and
	// corev1.EnvVar.Value is omitempty, so only a real round trip proves
	// the variable is still set rather than dropped. The schema rules
	// themselves live with the rest of the CRD schema specs, in
	// internal/webhook/v1alpha3.
	DescribeTable("carries a lease-mode Subnet's gateway into the bootd container",
		func(gateway string) {
			ns := createSubnetTestNamespace(ctx)
			createTestNAD(ctx, ns, "boot-nad", bootdStaticNADConfig("192.0.2.2"))

			subnet := testSubnet(ns, func(s *keziov1alpha3.Subnet) {
				s.Spec.DHCP = &keziov1alpha3.SubnetDHCP{
					Mode:    keziov1alpha3.SubnetDHCPModeLease,
					Gateway: ptr.To(gateway),
				}
			})
			Expect(k8sClient.Create(ctx, subnet)).To(Succeed())

			r := newSubnetTestReconciler()
			key := types.NamespacedName{Name: subnet.Name, Namespace: ns}
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			var dep appsv1.Deployment
			depKey := types.NamespacedName{Name: bootdDeploymentName(subnet.Name), Namespace: ns}
			Expect(k8sClient.Get(ctx, depKey, &dep)).To(Succeed())

			var bootdC *corev1.Container
			for i := range dep.Spec.Template.Spec.Containers {
				if dep.Spec.Template.Spec.Containers[i].Name == "bootd" {
					bootdC = &dep.Spec.Template.Spec.Containers[i]
				}
			}
			Expect(bootdC).NotTo(BeNil())
			Expect(bootdC.Env).To(ContainElement(corev1.EnvVar{Name: "BOOTD_GATEWAY", Value: gateway}))
		},
		Entry("an address", "192.0.2.1"),
		Entry("the empty string, a segment with no exit", ""),
	)

	It("refuses both Subnets' bootd Deployments when two Subnets share the same bootdNetworkRef", func() {
		ns := createSubnetTestNamespace(ctx)
		createTestNAD(ctx, ns, "boot-nad", bootdStaticNADConfig("192.0.2.2"))

		first := testSubnet(ns)
		Expect(k8sClient.Create(ctx, first)).To(Succeed())

		second := testSubnet(ns, func(s *keziov1alpha3.Subnet) {
			s.Name = rack2SubnetName
			s.Spec.CIDR = rack2CIDR
			s.Spec.BootdServerIP = rack2BootdServerIP
			// Same bootdNetworkRef as first - the collision this check
			// exists to catch.
			s.Spec.BootdNetworkRef = &keziov1alpha3.NameRef{Name: "boot-nad"}
		})
		Expect(k8sClient.Create(ctx, second)).To(Succeed())

		r := newSubnetTestReconciler()
		firstKey := types.NamespacedName{Name: first.Name, Namespace: ns}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: firstKey})
		Expect(err).NotTo(HaveOccurred())
		secondKey := types.NamespacedName{Name: second.Name, Namespace: ns}
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: secondKey})
		Expect(err).NotTo(HaveOccurred())

		// The collision is symmetric - each Subnet's own reconcile sees
		// the other one sharing its bootdNetworkRef, so neither creates a
		// Deployment; there is no tie-break that lets one "win".
		var deployments appsv1.DeploymentList
		Expect(k8sClient.List(ctx, &deployments, client.InNamespace(ns))).To(Succeed())
		Expect(deployments.Items).To(BeEmpty(), "a bootdNetworkRef collision must withhold both Subnets' Deployments")

		for _, key := range []types.NamespacedName{firstKey, secondKey} {
			var updated keziov1alpha3.Subnet
			Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())
			validCond := findCondition(updated.Status.Conditions, keziov1alpha3.SubnetConditionValid)
			Expect(validCond).NotTo(BeNil())
			Expect(validCond.Status).To(Equal(metav1.ConditionFalse))
			Expect(validCond.Reason).To(Equal("BootdNetworkCollision"))
		}
	})

	It("reports Ready=False with a reason naming the missing image, but Valid=True, when no bootd Deployment image is configured", func() {
		ns := createSubnetTestNamespace(ctx)
		createTestNAD(ctx, ns, "boot-nad", bootdStaticNADConfig("192.0.2.2"))

		subnet := testSubnet(ns)
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())

		r := &SubnetReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()} // zero-value BootdDeployment: not enabled()
		key := types.NamespacedName{Name: subnet.Name, Namespace: ns}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var deployments appsv1.DeploymentList
		Expect(k8sClient.List(ctx, &deployments, client.InNamespace(ns))).To(Succeed())
		Expect(deployments.Items).To(BeEmpty(), "no Deployment image configured must never create a Deployment")

		var updated keziov1alpha3.Subnet
		Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())
		validCond := findCondition(updated.Status.Conditions, keziov1alpha3.SubnetConditionValid)
		Expect(validCond).NotTo(BeNil())
		Expect(validCond.Status).To(Equal(metav1.ConditionTrue), "an unconfigured manager is not a Subnet misconfiguration")

		readyCond := findCondition(updated.Status.Conditions, keziov1alpha3.SubnetConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(readyCond.Reason).To(Equal("BootdDeploymentImageUnconfigured"))
	})

	It("flags seeder static ipam as an Advisory that leaves Valid True and the Deployment in place", func() {
		ns := createSubnetTestNamespace(ctx)
		createTestNAD(ctx, ns, "boot-nad", bootdStaticNADConfig("192.0.2.2"))
		createTestNAD(ctx, ns, "seeder-nad", bootdStaticNADConfig("192.0.2.50"))

		subnet := testSubnet(ns, func(s *keziov1alpha3.Subnet) {
			s.Spec.SeederNetworkRef = &keziov1alpha3.NameRef{Name: "seeder-nad"}
		})
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())

		// Two Available seeder Deployments - concurrentSeederDeployments'
		// input - so CheckSeederStaticMultiImage sees concurrency > 1.
		for _, name := range []string{"seeder-a", "seeder-b"} {
			dep := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("kezio-%s-%s", name, rand.String(5)),
					Namespace: ns,
					Labels: map[string]string{
						partitionContentAppComponentLabel: partitionContentSeederComponentValue,
					},
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
						Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "busybox"}}},
					},
				},
			}
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())
			dep.Status.Replicas = 1
			dep.Status.ReadyReplicas = 1
			dep.Status.AvailableReplicas = 1
			Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())
		}

		r := newSubnetTestReconciler()
		key := types.NamespacedName{Name: subnet.Name, Namespace: ns}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var deployments appsv1.DeploymentList
		Expect(k8sClient.List(ctx, &deployments, client.InNamespace(ns), client.MatchingLabels{bootdAppComponentLabel: bootdComponentValue})).To(Succeed())
		Expect(deployments.Items).To(HaveLen(1), "an Advisory must not withhold the bootd Deployment")

		var updated keziov1alpha3.Subnet
		Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())
		validCond := findCondition(updated.Status.Conditions, keziov1alpha3.SubnetConditionValid)
		Expect(validCond).NotTo(BeNil())
		Expect(validCond.Status).To(Equal(metav1.ConditionTrue), "an Advisory verdict must never fail Valid")
	})

	It("creates no bootd Deployment and reaches Ready for a Subnet with no boot half", func() {
		ns := createSubnetTestNamespace(ctx)
		createTestNAD(ctx, ns, "seeder-nad", bootdStaticNADConfig("192.0.2.50"))

		subnet := testSubnet(ns, func(s *keziov1alpha3.Subnet) {
			s.Spec.BootdServerIP = ""
			s.Spec.BootdNetworkRef = nil
			s.Spec.DHCP = nil
			s.Spec.SeederNetworkRef = &keziov1alpha3.NameRef{Name: "seeder-nad"}
		})
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())

		r := newSubnetTestReconciler()
		key := types.NamespacedName{Name: subnet.Name, Namespace: ns}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var deployments appsv1.DeploymentList
		Expect(k8sClient.List(ctx, &deployments, client.InNamespace(ns), client.MatchingLabels{bootdAppComponentLabel: bootdComponentValue})).To(Succeed())
		Expect(deployments.Items).To(BeEmpty(), "a Subnet with no boot half must never get a bootd Deployment")

		var updated keziov1alpha3.Subnet
		Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())
		validCond := findCondition(updated.Status.Conditions, keziov1alpha3.SubnetConditionValid)
		Expect(validCond).NotTo(BeNil())
		Expect(validCond.Status).To(Equal(metav1.ConditionTrue))

		readyCond := findCondition(updated.Status.Conditions, keziov1alpha3.SubnetConditionReady)
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionTrue), "a data-plane-only Subnet has no Deployment to wait on, so it is Ready once Valid")
		Expect(readyCond.Reason).To(Equal("SubnetReady"))
	})
})
