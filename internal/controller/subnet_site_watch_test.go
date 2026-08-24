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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

var _ = Describe("Subnet Controller dangling siteRef", func() {
	ctx := context.Background()

	It("maps a Site event to reconcile requests for only the Subnets in its own namespace that reference it", func() {
		ns := createSubnetTestNamespace(ctx)
		site := testSite(ctx, ns, "site-watch")

		referencing := testSubnet(ns, func(s *keziov1alpha2.Subnet) {
			s.Name = "rack-referencing"
			s.Spec.SiteRef = keziov1alpha2.NameRef{Name: "site-watch"}
		})
		Expect(k8sClient.Create(ctx, referencing)).To(Succeed())
		other := testSubnet(ns, func(s *keziov1alpha2.Subnet) {
			s.Name = "rack-other"
			s.Spec.CIDR = "192.0.3.0/24"
			s.Spec.BootdServerIP = "192.0.3.2"
			s.Spec.SiteRef = keziov1alpha2.NameRef{Name: "hq"}
		})
		Expect(k8sClient.Create(ctx, other)).To(Succeed())

		r := newSubnetTestReconciler()
		// This is the mapping SetupWithManager's Site watch installs
		// (handler.EnqueueRequestsFromMapFunc(r.mapSiteToSubnets)) - called
		// directly against a Site event, the same way this package's other
		// map-function tests exercise their own watch's mapping
		// (mapMachineToPartitionContents, mapDeployRunToPartitionContents).
		requests := r.mapSiteToSubnets(ctx, site)
		Expect(requests).To(ConsistOf(reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: ns, Name: "rack-referencing"},
		}), "only the Subnet naming this Site in spec.siteRef must be requeued, and no unrelated write to it is involved")
	})

	It("flips Valid to False naming the missing Site once it is deleted, and back to True once it is recreated, touching no unrelated Subnet", func() {
		ns := createSubnetTestNamespace(ctx)
		site := testSite(ctx, ns, "site-vanish")
		createTestNAD(ctx, ns, "boot-nad", bootdStaticNADConfig("192.0.2.2"))
		createTestNAD(ctx, ns, "boot-nad-2", bootdStaticNADConfig("192.0.3.2"))

		affected := testSubnet(ns, func(s *keziov1alpha2.Subnet) {
			s.Name = "rack-affected"
			s.Spec.SiteRef = keziov1alpha2.NameRef{Name: "site-vanish"}
		})
		Expect(k8sClient.Create(ctx, affected)).To(Succeed())
		unrelated := testSubnet(ns, func(s *keziov1alpha2.Subnet) {
			s.Name = "rack-unrelated"
			s.Spec.CIDR = "192.0.3.0/24"
			s.Spec.BootdServerIP = "192.0.3.2"
			s.Spec.BootdNetworkRef = &keziov1alpha2.NameRef{Name: "boot-nad-2"}
			s.Spec.SiteRef = keziov1alpha2.NameRef{Name: "hq"}
		})
		Expect(k8sClient.Create(ctx, unrelated)).To(Succeed())

		r := newSubnetTestReconciler()
		affectedKey := types.NamespacedName{Name: affected.Name, Namespace: ns}
		unrelatedKey := types.NamespacedName{Name: unrelated.Name, Namespace: ns}

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: affectedKey})
		Expect(err).NotTo(HaveOccurred())
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: unrelatedKey})
		Expect(err).NotTo(HaveOccurred())

		var beforeDelete, unrelatedBefore keziov1alpha2.Subnet
		Expect(k8sClient.Get(ctx, affectedKey, &beforeDelete)).To(Succeed())
		Expect(findCondition(beforeDelete.Status.Conditions, keziov1alpha2.SubnetConditionValid).Status).To(Equal(metav1.ConditionTrue))
		Expect(k8sClient.Get(ctx, unrelatedKey, &unrelatedBefore)).To(Succeed())

		Expect(k8sClient.Delete(ctx, site)).To(Succeed())

		// mapSiteToSubnets is what would drive this reconcile from a real
		// watch; this directly reconciles the requests it would have
		// produced, the same convention this package's other watch-mapping
		// tests follow (see the mapping test above).
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: affectedKey})
		Expect(err).NotTo(HaveOccurred())

		var afterDelete keziov1alpha2.Subnet
		Expect(k8sClient.Get(ctx, affectedKey, &afterDelete)).To(Succeed())
		validCond := findCondition(afterDelete.Status.Conditions, keziov1alpha2.SubnetConditionValid)
		Expect(validCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(validCond.Reason).To(Equal("SiteNotFound"))
		Expect(validCond.Message).To(ContainSubstring("site-vanish"))

		readyCond := findCondition(afterDelete.Status.Conditions, keziov1alpha2.SubnetConditionReady)
		Expect(readyCond.Status).To(Equal(metav1.ConditionFalse), "Ready follows Valid down: bootd still PXEs machines on this segment, but the Subnet is misconfigured and must say so, the same way a non-blocking CheckBootdAddress mismatch already fails Ready without withholding the Deployment")
		Expect(readyCond.Reason).To(Equal("SiteNotFound"))

		// The unrelated Subnet, naming a different (still-existing) Site,
		// must not have been touched by reconciling the affected one.
		var unrelatedAfter keziov1alpha2.Subnet
		Expect(k8sClient.Get(ctx, unrelatedKey, &unrelatedAfter)).To(Succeed())
		Expect(unrelatedAfter.ResourceVersion).To(Equal(unrelatedBefore.ResourceVersion), "reconciling one Subnet must never write another")
		unrelatedValid := findCondition(unrelatedAfter.Status.Conditions, keziov1alpha2.SubnetConditionValid)
		Expect(unrelatedValid.Status).To(Equal(metav1.ConditionTrue))

		By("recreating the Site")
		recreated := &keziov1alpha2.Site{ObjectMeta: metav1.ObjectMeta{Name: "site-vanish", Namespace: ns}}
		Expect(k8sClient.Create(ctx, recreated)).To(Succeed())

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: affectedKey})
		Expect(err).NotTo(HaveOccurred())

		var afterRecreate keziov1alpha2.Subnet
		Expect(k8sClient.Get(ctx, affectedKey, &afterRecreate)).To(Succeed())
		Expect(findCondition(afterRecreate.Status.Conditions, keziov1alpha2.SubnetConditionValid).Status).To(Equal(metav1.ConditionTrue))
		// Ready also needs the bootd Deployment Available, which envtest
		// never sets on its own (no real Deployment controller runs here) -
		// stamp it, mirroring subnet_bootd_envtest_test.go's own pattern,
		// so Ready's own recovery is checked independently of that.
		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: bootdDeploymentName(affected.Name), Namespace: ns}, &dep)).To(Succeed())
		dep.Status.Conditions = []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue}}
		Expect(k8sClient.Status().Update(ctx, &dep)).To(Succeed())

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: affectedKey})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, affectedKey, &afterRecreate)).To(Succeed())
		Expect(findCondition(afterRecreate.Status.Conditions, keziov1alpha2.SubnetConditionReady).Status).To(Equal(metav1.ConditionTrue))
	})
})
