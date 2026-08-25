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

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

// seederDeploymentFor builds the seeder Deployment shape
// SiteReconciler.seederPlacementReady counts and
// mapSeederDeploymentToSite maps: the two placement labels
// ImageReconciler sets, plus the Site identity annotation.
func seederDeploymentFor(ns, name, subnetName, siteIdentity string) *appsv1.Deployment {
	podLabels := map[string]string{imageSeederInstanceLabel: name}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				partitionContentAppComponentLabel: partitionContentSeederComponentValue,
				partitionContentSeederSubnetLabel: subnetName,
			},
			Annotations: map[string]string{imageSeederSiteAnnotation: siteIdentity},
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: podLabels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "ezio", Image: "seeder:test"}},
				},
			},
		},
	}
}

var _ = Describe("Site Controller seeder watch", func() {
	ctx := context.Background()

	It("maps a seeder Deployment event to a reconcile request for the Site it names", func() {
		r := newSiteTestReconciler()

		requests := r.mapSeederDeploymentToSite(ctx,
			seederDeploymentFor("images", "kezio-seeder-img-0badcafe", "rack-1", "sites/site-seeder-watch"))

		Expect(requests).To(ConsistOf(reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: "sites", Name: "site-seeder-watch"},
		}), "the seeder Deployment is owned by the Image, so only this mapping can requeue the Site it serves")
	})

	It("maps no request for a Deployment that carries no seeder Site identity", func() {
		r := newSiteTestReconciler()

		bare := seederDeploymentFor("images", "kezio-tracker-site", "rack-1", "")
		delete(bare.Annotations, imageSeederSiteAnnotation)

		Expect(r.mapSeederDeploymentToSite(ctx, bare)).To(BeEmpty())
	})

	It("reports seederReady=true once a seeder Deployment on the seeding Subnet has an available replica", func() {
		ns := createSubnetTestNamespace(ctx)
		site := testSite(ctx, ns, "site-seeder-ready")

		subnet := testSubnet(ns, func(s *keziov1alpha3.Subnet) {
			s.Spec.SiteRef = keziov1alpha3.NameRef{Name: "site-seeder-ready"}
			s.Spec.SeederNetworkRef = &keziov1alpha3.NameRef{Name: "seeder-nad"}
		})
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())
		createTestNAD(ctx, ns, "seeder-nad", `{"ipam":{}}`)
		setSeederSubnetRef(ctx, site, subnet.Name, keziov1alpha3.SiteTracker{IP: "192.0.2.60"})

		r := newSiteTestReconciler()
		key := types.NamespacedName{Name: site.Name, Namespace: ns}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var beforeSeeder keziov1alpha3.Site
		Expect(k8sClient.Get(ctx, key, &beforeSeeder)).To(Succeed())
		Expect(beforeSeeder.Status.SeederReady).To(BeFalse(), "no seeder Deployment exists yet")

		dep := seederDeploymentFor(ns, "kezio-seeder-img-0badcafe", subnet.Name, ns+"/"+site.Name)
		Expect(k8sClient.Create(ctx, dep)).To(Succeed())
		// envtest runs no Deployment controller, so availability is stamped
		// here the same way this package's other Deployment tests do.
		dep.Status.Replicas = 1
		dep.Status.ReadyReplicas = 1
		dep.Status.AvailableReplicas = 1
		Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())

		// The Deployment event reaches the Site only through this mapping.
		requests := r.mapSeederDeploymentToSite(ctx, dep)
		Expect(requests).To(ConsistOf(reconcile.Request{NamespacedName: key}))
		_, err = r.Reconcile(ctx, requests[0])
		Expect(err).NotTo(HaveOccurred())

		var updated keziov1alpha3.Site
		Expect(k8sClient.Get(ctx, key, &updated)).To(Succeed())
		Expect(updated.Status.SeederReady).To(BeTrue())
	})
})
