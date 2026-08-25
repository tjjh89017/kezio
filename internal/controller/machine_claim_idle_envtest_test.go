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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
	"github.com/tjjh89017/kezio/internal/deployer"
)

var _ = Describe("Machine reconciliation without a bound claim", func() {
	ctx := context.Background()

	BeforeEach(func() {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "bmc-creds", Namespace: "default"},
			Type:       corev1.SecretTypeBasicAuth,
			Data: map[string][]byte{
				"username": []byte("admin"),
				"password": []byte("hunter2"),
			},
		}
		if err := k8sClient.Create(ctx, secret); err != nil && !apierrors.IsAlreadyExists(err) {
			Expect(err).NotTo(HaveOccurred())
		}
	})

	newBareTestMachine := func(name string) *keziov1alpha3.Machine {
		machine := &keziov1alpha3.Machine{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: keziov1alpha3.MachineSpec{
				BMC: keziov1alpha3.MachineBMC{
					Address:              "redfish://10.0.0.10/redfish/v1/Systems/1",
					CredentialsSecretRef: keziov1alpha3.SecretReference{Name: "bmc-creds"},
				},
				BootMACAddress: "aa:bb:cc:dd:ee:40",
				SubnetRef:      keziov1alpha3.NameRef{Name: "default"},
			},
		}
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())
		return machine
	}

	It("never creates a DeployRun for a machine with no claimRef", func() {
		name := fmt.Sprintf("no-claim-%d", GinkgoRandomSeed())
		machine := newBareTestMachine(name)
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, machine)).To(Succeed()) })

		reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: &deployer.FakeDeployer{Client: k8sClient}}
		key := types.NamespacedName{Name: name, Namespace: "default"}

		for range 10 {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
		}

		var final keziov1alpha3.Machine
		Expect(k8sClient.Get(ctx, key, &final)).To(Succeed())
		Expect(final.Status.State).To(Equal(keziov1alpha3.MachineStateAvailable))

		var runs keziov1alpha3.DeployRunList
		Expect(k8sClient.List(ctx, &runs, client.InNamespace("default"))).To(Succeed())
		for _, run := range runs.Items {
			Expect(run.Spec.MachineRef.Name).NotTo(Equal(name))
		}
	})

	It("moves a Released machine back to Available through the re-inspect annotation", func() {
		name := fmt.Sprintf("released-%d", GinkgoRandomSeed())
		machine := newBareTestMachine(name)
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, machine)).To(Succeed()) })

		reconciler := &MachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Deployer: &deployer.FakeDeployer{Client: k8sClient}, Recorder: record.NewFakeRecorder(20)}
		key := types.NamespacedName{Name: name, Namespace: "default"}

		By("adding the finalizer")
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		By("forcing the machine straight to Released, as a real release would leave it")
		var released keziov1alpha3.Machine
		Expect(k8sClient.Get(ctx, key, &released)).To(Succeed())
		released.Status.State = keziov1alpha3.MachineStateReleased
		Expect(k8sClient.Status().Update(ctx, &released)).To(Succeed())

		Expect(k8sClient.Get(ctx, key, &released)).To(Succeed())
		released.Annotations = map[string]string{keziov1alpha3.MachineAnnotationReInspect: "true"}
		Expect(k8sClient.Update(ctx, &released)).To(Succeed())

		By("reconciling: the re-inspect annotation is accepted, the machine moves to Inspecting")
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		var inspecting keziov1alpha3.Machine
		Expect(k8sClient.Get(ctx, key, &inspecting)).To(Succeed())
		Expect(inspecting.Status.State).To(Equal(keziov1alpha3.MachineStateInspecting))
		_, hasAnnotation := inspecting.Annotations[keziov1alpha3.MachineAnnotationReInspect]
		Expect(hasAnnotation).To(BeFalse())

		By("reconciling again: Inspect completes, the machine reaches Available")
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		var available keziov1alpha3.Machine
		Expect(k8sClient.Get(ctx, key, &available)).To(Succeed())
		Expect(available.Status.State).To(Equal(keziov1alpha3.MachineStateAvailable))
	})
})
