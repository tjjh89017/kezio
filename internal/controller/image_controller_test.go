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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
)

// reconcileImageUntil calls Reconcile repeatedly (bounded by maxSteps)
// until check reports true, and returns the last error Reconcile
// returned. Tests drive the loop directly instead of waiting on a
// manager, matching the pattern used for the Machine reconciler.
func reconcileImageUntil(ctx context.Context, r *ImageReconciler, key types.NamespacedName, maxSteps int, check func(i *keziov1alpha1.Image) bool) (*keziov1alpha1.Image, error) {
	image := &keziov1alpha1.Image{}
	var lastErr error
	for i := 0; i < maxSteps; i++ {
		_, lastErr = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		if lastErr != nil {
			return nil, lastErr
		}
		if err := k8sClient.Get(ctx, key, image); err != nil {
			return nil, err
		}
		if check(image) {
			return image, nil
		}
	}
	return image, lastErr
}

// deleteImageAndFinalize deletes image and reconciles it to completion,
// draining the finalizer this controller attaches. Tests use this
// instead of a bare k8sClient.Delete so a leftover, stuck-in-Terminating
// object never collides with a same-named object created by a later
// spec.
func deleteImageAndFinalize(ctx context.Context, r *ImageReconciler, key types.NamespacedName, image *keziov1alpha1.Image) {
	Expect(k8sClient.Delete(ctx, image)).To(Succeed())
	Eventually(func() bool {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		gone := &keziov1alpha1.Image{}
		return apierrors.IsNotFound(k8sClient.Get(ctx, key, gone))
	}).Should(BeTrue())
}

var _ = Describe("Image Controller", func() {
	ctx := context.Background()

	Context("a url-source image", func() {
		It("walks Pending -> Ingesting -> Ready with a True Ready condition", func() {
			const resourceName = "walk-image"
			namespace := "default"
			key := types.NamespacedName{Name: resourceName, Namespace: namespace}

			image := &keziov1alpha1.Image{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: namespace},
				Spec: keziov1alpha1.ImageSpec{
					Source: keziov1alpha1.ImageSource{
						URL:    "https://example.com/golden.qcow2",
						Format: keziov1alpha1.ImageFormatQCOW2,
					},
				},
			}
			Expect(k8sClient.Create(ctx, image)).To(Succeed())

			r := &ImageReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			DeferCleanup(func() { deleteImageAndFinalize(ctx, r, key, image) })

			By("reconciling to attach the finalizer")
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			By("reconciling to Ingesting")
			result, err := reconcileImageUntil(ctx, r, key, 2, func(i *keziov1alpha1.Image) bool {
				return i.Status.State == keziov1alpha1.ImageStateIngesting
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Status.State).To(Equal(keziov1alpha1.ImageStateIngesting))
			ingestingCond := apimeta.FindStatusCondition(result.Status.Conditions, keziov1alpha1.ConditionReady)
			Expect(ingestingCond).NotTo(BeNil())
			Expect(ingestingCond.Status).To(Equal(metav1.ConditionFalse))
			Expect(ingestingCond.Reason).To(Equal(reasonIngesting))

			By("reconciling to Ready")
			result, err = reconcileImageUntil(ctx, r, key, 2, func(i *keziov1alpha1.Image) bool {
				return i.Status.State == keziov1alpha1.ImageStateReady
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Status.State).To(Equal(keziov1alpha1.ImageStateReady))

			By("reporting a True Ready condition once Ready")
			readyCond := apimeta.FindStatusCondition(result.Status.Conditions, keziov1alpha1.ConditionReady)
			Expect(readyCond).NotTo(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))
			Expect(readyCond.Reason).To(Equal(reasonIngestComplete))

			By("staying Ready on a further reconcile")
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			steady := &keziov1alpha1.Image{}
			Expect(k8sClient.Get(ctx, key, steady)).To(Succeed())
			Expect(steady.Status.State).To(Equal(keziov1alpha1.ImageStateReady))
		})
	})

	Context("an image with no source URL", func() {
		It("stays Pending with an AwaitingUpload reason", func() {
			const resourceName = "upload-image"
			namespace := "default"
			key := types.NamespacedName{Name: resourceName, Namespace: namespace}

			image := &keziov1alpha1.Image{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: namespace},
				Spec: keziov1alpha1.ImageSpec{
					Source: keziov1alpha1.ImageSource{Format: keziov1alpha1.ImageFormatQCOW2},
				},
			}
			Expect(k8sClient.Create(ctx, image)).To(Succeed())

			r := &ImageReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			DeferCleanup(func() { deleteImageAndFinalize(ctx, r, key, image) })

			By("reconciling to attach the finalizer")
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			By("reconciling to Pending with AwaitingUpload")
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			result := &keziov1alpha1.Image{}
			Expect(k8sClient.Get(ctx, key, result)).To(Succeed())
			Expect(result.Status.State).To(Equal(keziov1alpha1.ImageStatePending))

			readyCond := apimeta.FindStatusCondition(result.Status.Conditions, keziov1alpha1.ConditionReady)
			Expect(readyCond).NotTo(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
			Expect(readyCond.Reason).To(Equal(reasonAwaitingUpload))
		})
	})

	Context("updating an image's spec", func() {
		It("is rejected by the CRD's immutability validation", func() {
			const resourceName = "immutable-image"
			namespace := "default"
			key := types.NamespacedName{Name: resourceName, Namespace: namespace}

			image := &keziov1alpha1.Image{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: namespace},
				Spec: keziov1alpha1.ImageSpec{
					Source: keziov1alpha1.ImageSource{
						URL:    "https://example.com/golden.qcow2",
						Format: keziov1alpha1.ImageFormatQCOW2,
					},
				},
			}
			Expect(k8sClient.Create(ctx, image)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, image)).To(Succeed()) })

			current := &keziov1alpha1.Image{}
			Expect(k8sClient.Get(ctx, key, current)).To(Succeed())
			current.Spec.Source.URL = "https://example.com/other.qcow2"

			err := k8sClient.Update(ctx, current)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("immutable"))
		})
	})

	Context("deleting an unreferenced image", func() {
		It("attaches a finalizer, then removes it and lets deletion proceed", func() {
			const resourceName = "delete-image"
			namespace := "default"
			key := types.NamespacedName{Name: resourceName, Namespace: namespace}

			image := &keziov1alpha1.Image{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: namespace},
				Spec: keziov1alpha1.ImageSpec{
					Source: keziov1alpha1.ImageSource{
						URL:    "https://example.com/golden.qcow2",
						Format: keziov1alpha1.ImageFormatQCOW2,
					},
				},
			}
			Expect(k8sClient.Create(ctx, image)).To(Succeed())

			r := &ImageReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

			By("attaching the finalizer")
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			withFinalizer := &keziov1alpha1.Image{}
			Expect(k8sClient.Get(ctx, key, withFinalizer)).To(Succeed())
			Expect(controllerutil.ContainsFinalizer(withFinalizer, keziov1alpha1.FinalizerName)).To(BeTrue())

			Expect(k8sClient.Delete(ctx, image)).To(Succeed())

			By("reconciling the deletion, which finds no referencing Machine and removes the finalizer")
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			gone := &keziov1alpha1.Image{}
			err = k8sClient.Get(ctx, key, gone)
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})
	})

	Context("deleting an image a Machine references", func() {
		It("keeps the finalizer and deletion timestamp until the Machine no longer references it", func() {
			const resourceName = "inuse-image"
			namespace := "default"
			key := types.NamespacedName{Name: resourceName, Namespace: namespace}

			image := &keziov1alpha1.Image{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: namespace},
				Spec: keziov1alpha1.ImageSpec{
					Source: keziov1alpha1.ImageSource{
						URL:    "https://example.com/golden.qcow2",
						Format: keziov1alpha1.ImageFormatQCOW2,
					},
				},
			}
			Expect(k8sClient.Create(ctx, image)).To(Succeed())

			machine := &keziov1alpha1.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: "inuse-machine", Namespace: namespace},
				Spec: keziov1alpha1.MachineSpec{
					BMC: &keziov1alpha1.MachineBMC{
						Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
						CredentialsSecretRef: keziov1alpha1.SecretReference{Name: "inuse-machine-bmc"},
					},
					BootMACAddress: "aa:bb:cc:dd:ee:02",
					ImageRef:       &keziov1alpha1.NameRef{Name: resourceName},
				},
			}
			Expect(k8sClient.Create(ctx, machine)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, machine)).To(Succeed()) })

			r := &ImageReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

			By("attaching the finalizer")
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Delete(ctx, image)).To(Succeed())

			By("reconciling the deletion while the Machine still references the Image")
			result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			By("staying present with a deletion timestamp and its finalizer")
			blocked := &keziov1alpha1.Image{}
			Expect(k8sClient.Get(ctx, key, blocked)).To(Succeed())
			Expect(blocked.DeletionTimestamp).NotTo(BeNil())
			Expect(controllerutil.ContainsFinalizer(blocked, keziov1alpha1.FinalizerName)).To(BeTrue())

			By("removing the Machine's reference")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: machine.Name, Namespace: namespace}, machine)).To(Succeed())
			machine.Spec.ImageRef = nil
			Expect(k8sClient.Update(ctx, machine)).To(Succeed())

			By("reconciling the deletion again, which now removes the finalizer")
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			gone := &keziov1alpha1.Image{}
			err = k8sClient.Get(ctx, key, gone)
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})
	})

	Context("deleting an image only referenced via a Machine's status.provisioning", func() {
		It("keeps the finalizer until the provisioning record no longer references it", func() {
			const resourceName = "provisioned-image"
			namespace := "default"
			key := types.NamespacedName{Name: resourceName, Namespace: namespace}

			image := &keziov1alpha1.Image{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: namespace},
				Spec: keziov1alpha1.ImageSpec{
					Source: keziov1alpha1.ImageSource{
						URL:    "https://example.com/golden.qcow2",
						Format: keziov1alpha1.ImageFormatQCOW2,
					},
				},
			}
			Expect(k8sClient.Create(ctx, image)).To(Succeed())

			machine := &keziov1alpha1.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: "provisioned-machine", Namespace: namespace},
				Spec: keziov1alpha1.MachineSpec{
					BMC: &keziov1alpha1.MachineBMC{
						Address:              "redfish://10.0.0.11/redfish/v1/Systems/1",
						CredentialsSecretRef: keziov1alpha1.SecretReference{Name: "provisioned-machine-bmc"},
					},
					BootMACAddress: "aa:bb:cc:dd:ee:03",
				},
			}
			Expect(k8sClient.Create(ctx, machine)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, machine)).To(Succeed()) })

			By("recording the Image as deployed in status.provisioning, with spec.imageRef left unset")
			machine.Status.Provisioning = &keziov1alpha1.MachineProvisioningStatus{
				Image: &keziov1alpha1.MachineProvisionedImage{
					ImageRef: keziov1alpha1.NameRef{Name: resourceName},
				},
			}
			Expect(k8sClient.Status().Update(ctx, machine)).To(Succeed())

			r := &ImageReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

			By("attaching the finalizer")
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Delete(ctx, image)).To(Succeed())

			By("reconciling the deletion while status.provisioning still references the Image")
			result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			By("staying present with a deletion timestamp and its finalizer")
			blocked := &keziov1alpha1.Image{}
			Expect(k8sClient.Get(ctx, key, blocked)).To(Succeed())
			Expect(blocked.DeletionTimestamp).NotTo(BeNil())
			Expect(controllerutil.ContainsFinalizer(blocked, keziov1alpha1.FinalizerName)).To(BeTrue())

			By("clearing the provisioning record")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: machine.Name, Namespace: namespace}, machine)).To(Succeed())
			machine.Status.Provisioning = nil
			Expect(k8sClient.Status().Update(ctx, machine)).To(Succeed())

			By("reconciling the deletion again, which now removes the finalizer")
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			gone := &keziov1alpha1.Image{}
			err = k8sClient.Get(ctx, key, gone)
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})
	})

	Context("deleting an image a cross-namespace Machine references", func() {
		It("keeps the finalizer until the referencing Machine in the other namespace releases it", func() {
			const resourceName = "cross-ns-image"
			const imageNamespace = "image-controller-cross-ns"
			const machineNamespace = "default"
			key := types.NamespacedName{Name: resourceName, Namespace: imageNamespace}

			By("creating the namespace that will hold the Image")
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: imageNamespace}}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())

			image := &keziov1alpha1.Image{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: imageNamespace},
				Spec: keziov1alpha1.ImageSpec{
					Source: keziov1alpha1.ImageSource{
						URL:    "https://example.com/golden.qcow2",
						Format: keziov1alpha1.ImageFormatQCOW2,
					},
				},
			}
			Expect(k8sClient.Create(ctx, image)).To(Succeed())

			machine := &keziov1alpha1.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: "cross-ns-machine", Namespace: machineNamespace},
				Spec: keziov1alpha1.MachineSpec{
					BMC: &keziov1alpha1.MachineBMC{
						Address:              "redfish://10.0.0.12/redfish/v1/Systems/1",
						CredentialsSecretRef: keziov1alpha1.SecretReference{Name: "cross-ns-machine-bmc"},
					},
					BootMACAddress: "aa:bb:cc:dd:ee:04",
					ImageRef:       &keziov1alpha1.NameRef{Name: resourceName, Namespace: imageNamespace},
				},
			}
			Expect(k8sClient.Create(ctx, machine)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, machine)).To(Succeed()) })

			r := &ImageReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

			By("attaching the finalizer")
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Delete(ctx, image)).To(Succeed())

			By("reconciling the deletion while the cross-namespace Machine still references the Image")
			result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			By("staying present with a deletion timestamp and its finalizer")
			blocked := &keziov1alpha1.Image{}
			Expect(k8sClient.Get(ctx, key, blocked)).To(Succeed())
			Expect(blocked.DeletionTimestamp).NotTo(BeNil())
			Expect(controllerutil.ContainsFinalizer(blocked, keziov1alpha1.FinalizerName)).To(BeTrue())

			By("removing the Machine's cross-namespace reference")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: machine.Name, Namespace: machineNamespace}, machine)).To(Succeed())
			machine.Spec.ImageRef = nil
			Expect(k8sClient.Update(ctx, machine)).To(Succeed())

			By("reconciling the deletion again, which now removes the finalizer")
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			gone := &keziov1alpha1.Image{}
			err = k8sClient.Get(ctx, key, gone)
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})
	})
})
