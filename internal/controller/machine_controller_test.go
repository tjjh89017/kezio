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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/deployer"
)

// newTestMachineSpec builds a minimal, valid MachineSpec for a test
// resource named name.
func newTestMachineSpec(name string) keziov1alpha1.MachineSpec {
	return keziov1alpha1.MachineSpec{
		BMC: keziov1alpha1.MachineBMC{
			Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
			CredentialsSecretRef: keziov1alpha1.SecretReference{Name: name + "-bmc"},
		},
		BootMACAddress: "aa:bb:cc:dd:ee:01",
	}
}

// reconcileUntil calls Reconcile repeatedly (bounded by maxSteps) until
// check reports true, and returns the last error Reconcile returned.
// Reconcile results carry backoff/poll delays as ctrl.Result.RequeueAfter,
// which envtest does not act on by itself, so tests drive the loop
// directly instead of waiting on the manager.
func reconcileUntil(ctx context.Context, r *MachineReconciler, key types.NamespacedName, maxSteps int, check func(m *keziov1alpha1.Machine) bool) (*keziov1alpha1.Machine, error) {
	machine := &keziov1alpha1.Machine{}
	var lastErr error
	for i := 0; i < maxSteps; i++ {
		_, lastErr = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		if lastErr != nil {
			return nil, lastErr
		}
		if err := k8sClient.Get(ctx, key, machine); err != nil {
			return nil, err
		}
		if check(machine) {
			return machine, nil
		}
	}
	return machine, lastErr
}

var _ = Describe("Machine Controller", func() {
	ctx := context.Background()

	Context("walking the full state machine", func() {
		It("drives Enrolling -> Inspecting -> Available -> Provisioning -> Provisioned", func() {
			const resourceName = "walk-machine"
			namespace := "default"
			key := types.NamespacedName{Name: resourceName, Namespace: namespace}

			By("creating the target Image")
			image := &keziov1alpha1.Image{
				ObjectMeta: metav1.ObjectMeta{Name: "walk-machine-image", Namespace: namespace},
				Spec: keziov1alpha1.ImageSpec{
					Source: keziov1alpha1.ImageSource{Format: keziov1alpha1.ImageFormatQCOW2},
				},
			}
			Expect(k8sClient.Create(ctx, image)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, image)).To(Succeed()) })

			By("creating the Machine")
			spec := newTestMachineSpec(resourceName)
			spec.ImageRef = &keziov1alpha1.NameRef{Name: image.Name}
			machine := &keziov1alpha1.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: namespace},
				Spec:       spec,
			}
			Expect(k8sClient.Create(ctx, machine)).To(Succeed())
			DeferCleanup(func() {
				m := &keziov1alpha1.Machine{}
				if err := k8sClient.Get(ctx, key, m); err == nil {
					Expect(k8sClient.Delete(ctx, m)).To(Succeed())
				}
			})

			r := &MachineReconciler{
				Client:          k8sClient,
				Scheme:          k8sClient.Scheme(),
				DeployerFactory: deployer.NewFactory().New,
			}

			By("reconciling until the finalizer is attached")
			result, err := reconcileUntil(ctx, r, key, 3, func(m *keziov1alpha1.Machine) bool {
				return controllerutil.ContainsFinalizer(m, keziov1alpha1.FinalizerName)
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(controllerutil.ContainsFinalizer(result, keziov1alpha1.FinalizerName)).To(BeTrue())

			By("reconciling to Enrolling")
			result, err = reconcileUntil(ctx, r, key, 3, func(m *keziov1alpha1.Machine) bool {
				return m.Status.State == keziov1alpha1.MachineStateEnrolling
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Status.State).To(Equal(keziov1alpha1.MachineStateEnrolling))

			By("reconciling to Inspecting")
			result, err = reconcileUntil(ctx, r, key, 3, func(m *keziov1alpha1.Machine) bool {
				return m.Status.State == keziov1alpha1.MachineStateInspecting
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Status.State).To(Equal(keziov1alpha1.MachineStateInspecting))

			By("reconciling to Available with hardware inventory recorded")
			result, err = reconcileUntil(ctx, r, key, 3, func(m *keziov1alpha1.Machine) bool {
				return m.Status.State == keziov1alpha1.MachineStateAvailable
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Status.State).To(Equal(keziov1alpha1.MachineStateAvailable))
			Expect(result.Status.Hardware).NotTo(BeNil())
			Expect(result.Status.Hardware.Disks).NotTo(BeEmpty())

			By("reconciling through Provisioning to Provisioned")
			result, err = reconcileUntil(ctx, r, key, 5, func(m *keziov1alpha1.Machine) bool {
				return m.Status.State == keziov1alpha1.MachineStateProvisioned
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Status.State).To(Equal(keziov1alpha1.MachineStateProvisioned))
			Expect(result.Status.Provisioning).NotTo(BeNil())
			Expect(result.Status.Provisioning.Image).NotTo(BeNil())
			Expect(result.Status.Provisioning.Image.ImageRef).To(Equal(*spec.ImageRef))
			Expect(result.Status.Provisioning.Image.TargetDisk).To(Equal("/dev/vda"))

			By("reporting a True Ready condition once Provisioned")
			readyCond := apimeta.FindStatusCondition(result.Status.Conditions, keziov1alpha1.ConditionReady)
			Expect(readyCond).NotTo(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))
			Expect(readyCond.Reason).To(Equal(reasonProvisioned))

			By("not re-triggering a deployment when spec has not changed")
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			steady := &keziov1alpha1.Machine{}
			Expect(k8sClient.Get(ctx, key, steady)).To(Succeed())
			Expect(steady.Status.State).To(Equal(keziov1alpha1.MachineStateProvisioned))
		})
	})

	Context("deleting a machine", func() {
		It("calls Deprovision and removes the finalizer", func() {
			const resourceName = "delete-machine"
			namespace := "default"
			key := types.NamespacedName{Name: resourceName, Namespace: namespace}

			machine := &keziov1alpha1.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: namespace},
				Spec:       newTestMachineSpec(resourceName),
			}
			Expect(k8sClient.Create(ctx, machine)).To(Succeed())

			r := &MachineReconciler{
				Client:          k8sClient,
				Scheme:          k8sClient.Scheme(),
				DeployerFactory: deployer.NewFactory().New,
			}

			By("attaching the finalizer")
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			withFinalizer := &keziov1alpha1.Machine{}
			Expect(k8sClient.Get(ctx, key, withFinalizer)).To(Succeed())
			Expect(controllerutil.ContainsFinalizer(withFinalizer, keziov1alpha1.FinalizerName)).To(BeTrue())

			By("deleting the Machine, which sets a deletion timestamp behind the finalizer")
			Expect(k8sClient.Delete(ctx, withFinalizer)).To(Succeed())

			By("reconciling the deletion, which drives Deprovision and removes the finalizer")
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() bool {
				gone := &keziov1alpha1.Machine{}
				err := k8sClient.Get(ctx, key, gone)
				return apierrors.IsNotFound(err)
			}).Should(BeTrue())
		})
	})

	Context("recording errors and backoff", func() {
		It("moves to Error with backoff on a failed phase, then recovers once the injected failure clears", func() {
			const resourceName = "error-machine"
			namespace := "default"
			key := types.NamespacedName{Name: resourceName, Namespace: namespace}
			nn := types.NamespacedName{Namespace: namespace, Name: resourceName}

			machine := &keziov1alpha1.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: namespace},
				Spec:       newTestMachineSpec(resourceName),
			}
			Expect(k8sClient.Create(ctx, machine)).To(Succeed())
			DeferCleanup(func() {
				m := &keziov1alpha1.Machine{}
				if err := k8sClient.Get(ctx, key, m); err == nil {
					Expect(k8sClient.Delete(ctx, m)).To(Succeed())
				}
			})

			failing := true
			factory := deployer.NewFactory()
			factory.Fail = func(machine types.NamespacedName, phase deployer.Phase) string {
				if machine == nn && phase == deployer.PhaseRegister && failing {
					return "injected register failure"
				}
				return ""
			}

			r := &MachineReconciler{
				Client:          k8sClient,
				Scheme:          k8sClient.Scheme(),
				DeployerFactory: factory.New,
			}

			By("attaching the finalizer, then reaching Enrolling")
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			enrolling := &keziov1alpha1.Machine{}
			Expect(k8sClient.Get(ctx, key, enrolling)).To(Succeed())
			Expect(enrolling.Status.State).To(Equal(keziov1alpha1.MachineStateEnrolling))

			By("failing Register and moving to Error with errorCount 1")
			result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))
			afterFirstError := &keziov1alpha1.Machine{}
			Expect(k8sClient.Get(ctx, key, afterFirstError)).To(Succeed())
			Expect(afterFirstError.Status.State).To(Equal(keziov1alpha1.MachineStateError))
			Expect(afterFirstError.Status.ErrorCount).To(Equal(int32(1)))
			Expect(afterFirstError.Status.ErrorMessage).To(Equal("injected register failure"))
			readyCond := apimeta.FindStatusCondition(afterFirstError.Status.Conditions, keziov1alpha1.ConditionReady)
			Expect(readyCond).NotTo(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
			Expect(readyCond.Reason).To(Equal(reasonRegisterFailed))

			By("failing again and increasing errorCount, with a growing backoff")
			result2, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			afterSecondError := &keziov1alpha1.Machine{}
			Expect(k8sClient.Get(ctx, key, afterSecondError)).To(Succeed())
			Expect(afterSecondError.Status.ErrorCount).To(Equal(int32(2)))
			Expect(result2.RequeueAfter).To(BeNumerically(">=", result.RequeueAfter/2))

			By("clearing the injected failure and retrying from the failed phase")
			failing = false
			result3, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(result3.RequeueAfter).To(BeZero())
			recovered := &keziov1alpha1.Machine{}
			Expect(k8sClient.Get(ctx, key, recovered)).To(Succeed())
			Expect(recovered.Status.State).To(Equal(keziov1alpha1.MachineStateInspecting))
			Expect(recovered.Status.ErrorCount).To(Equal(int32(0)))
			Expect(recovered.Status.ErrorMessage).To(BeEmpty())
		})
	})
})
