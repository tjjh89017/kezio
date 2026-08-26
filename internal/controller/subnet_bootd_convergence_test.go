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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// These specs cover the two ways a bootd Deployment must converge back to
// the desired state: it must be recreated once deleted (the owner-reference
// watch this Subnet's SetupWithManager registers is what triggers that
// reconcile in production; here Reconcile is driven directly, exercising
// reconcileBootdDeployment's create-on-missing branch the same way that
// watch-driven reconcile would), and its image must catch up once the
// manager's configured BootdDeploymentConfig.Image changes, without ever
// needing a second, unrelated reconcile to notice.
var _ = Describe("Subnet bootd Deployment convergence", func() {
	ctx := context.Background()

	It("recreates the bootd Deployment once it is deleted", func() {
		ns := createSubnetTestNamespace(ctx)
		createTestNAD(ctx, ns, "boot-nad", bootdStaticNADConfig("192.0.2.2"))

		subnet := testSubnet(ns)
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())

		r := newSubnetTestReconciler()
		key := types.NamespacedName{Name: subnet.Name, Namespace: ns}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		depKey := types.NamespacedName{Name: bootdDeploymentName(subnet.Name), Namespace: ns}
		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, depKey, &dep)).To(Succeed())
		firstUID := dep.UID

		Expect(k8sClient.Delete(ctx, &dep)).To(Succeed())
		Eventually(func() error {
			return k8sClient.Get(ctx, depKey, &appsv1.Deployment{})
		}).ShouldNot(Succeed(), "the Deployment must actually be gone before the next reconcile")

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, depKey, &dep)).To(Succeed())
		Expect(dep.UID).NotTo(Equal(firstUID), "a genuinely new object, not a stale read of the deleted one")
	})

	It("converges the bootd Deployment's image once BootdDeploymentConfig.Image changes", func() {
		ns := createSubnetTestNamespace(ctx)
		createTestNAD(ctx, ns, "boot-nad", bootdStaticNADConfig("192.0.2.2"))

		subnet := testSubnet(ns)
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())

		r := newSubnetTestReconciler()
		key := types.NamespacedName{Name: subnet.Name, Namespace: ns}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		depKey := types.NamespacedName{Name: bootdDeploymentName(subnet.Name), Namespace: ns}
		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, depKey, &dep)).To(Succeed())
		bootdC := mustFindContainer(&dep, "bootd")
		Expect(bootdC.Image).To(Equal("bootd:test"))

		// Simulate the manager restarting with a new BOOTD_DEPLOYMENT_IMAGE:
		// a fresh reconciler with the changed config, same running Subnet.
		r.BootdDeployment.Image = "bootd:new"
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, depKey, &dep)).To(Succeed())
		bootdC = mustFindContainer(&dep, "bootd")
		Expect(bootdC.Image).To(Equal("bootd:new"), "an existing bootd Deployment must converge to the newly configured image, not keep the one it was created with")
	})
})
