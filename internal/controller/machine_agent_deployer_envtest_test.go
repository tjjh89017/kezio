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
	"net/url"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
	"github.com/tjjh89017/kezio/internal/bmc"
	"github.com/tjjh89017/kezio/internal/deployer"
)

// machineBMCTestDriverScheme is registered with internal/bmc's registry
// (see init) so a Machine pointing spec.bmc.address at
// "kezio-testbmc-controller://..." gets machineBMCTestDriver back instead
// of real hardware.
const machineBMCTestDriverScheme = "kezio-testbmc-controller"

func init() {
	bmc.Register(machineBMCTestDriverScheme, machineBMCTestDriverConnect)
}

// machineBMCTestDriver is a no-op bmc.BMC: this suite only exercises the
// Machine walk up to and through Inspecting, so every action it drives
// (arming PXE, powering on) just needs to succeed silently.
type machineBMCTestDriver struct{}

func (machineBMCTestDriver) PowerOn(context.Context) error       { return nil }
func (machineBMCTestDriver) PowerOff(context.Context) error      { return nil }
func (machineBMCTestDriver) ForcePowerOff(context.Context) error { return nil }
func (machineBMCTestDriver) PowerCycle(context.Context) error    { return nil }
func (machineBMCTestDriver) GetPowerState(context.Context) (bmc.PowerState, error) {
	return bmc.PowerStateOff, nil
}
func (machineBMCTestDriver) SetOneTimePXEBoot(context.Context) error { return nil }

func machineBMCTestDriverConnect(context.Context, *url.URL, bmc.Credentials, bmc.Options) (bmc.BMC, error) {
	return machineBMCTestDriver{}, nil
}

var _ = Describe("Machine reconciliation with AgentDeployer", func() {
	ctx := context.Background()

	It("walks Enrolling to Inspecting and stays Continuing until the live agent registers, then reaches Available", func() {
		name := fmt.Sprintf("agent-deployer-%d", GinkgoRandomSeed())
		secretName := name + "-bmc"
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: "default"},
			Type:       corev1.SecretTypeBasicAuth,
			Data: map[string][]byte{
				"username": []byte("admin"),
				"password": []byte("hunter2"),
			},
		}
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())
		DeferCleanup(func() {
			Expect(k8sClient.Delete(ctx, secret)).To(Succeed())
		})

		machine := &keziov1alpha3.Machine{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: keziov1alpha3.MachineSpec{
				BMC: keziov1alpha3.MachineBMC{
					Address:              machineBMCTestDriverScheme + "://" + name,
					CredentialsSecretRef: keziov1alpha3.SecretReference{Name: secretName},
				},
				BootMACAddress: "aa:bb:cc:dd:ee:03",
				SubnetRef:      keziov1alpha3.NameRef{Name: "default"},
			},
		}
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())
		DeferCleanup(func() {
			Expect(k8sClient.Delete(ctx, machine)).To(Succeed())
		})

		reconciler := &MachineReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Deployer: &deployer.AgentDeployer{Client: k8sClient},
		}
		key := types.NamespacedName{Name: name, Namespace: "default"}

		By("reconciling until the finalizer is added")
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		By("reconciling through Enrolling into Inspecting")
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var afterInspectStart keziov1alpha3.Machine
		Expect(k8sClient.Get(ctx, key, &afterInspectStart)).To(Succeed())
		Expect(afterInspectStart.Status.State).To(Equal(keziov1alpha3.MachineStateInspecting))

		By("Inspect arming PXE and powering on: the machine stays Inspecting/OK, not Failed")
		result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))

		var afterArm keziov1alpha3.Machine
		Expect(k8sClient.Get(ctx, key, &afterArm)).To(Succeed())
		Expect(afterArm.Status.State).To(Equal(keziov1alpha3.MachineStateInspecting))
		Expect(afterArm.Status.OperationalStatus).To(Equal(keziov1alpha3.MachineOperationalStatusOK))

		By("polling again without a registered agent: still Continuing, not Failed")
		result, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		var stillInspecting keziov1alpha3.Machine
		Expect(k8sClient.Get(ctx, key, &stillInspecting)).To(Succeed())
		Expect(stillInspecting.Status.State).To(Equal(keziov1alpha3.MachineStateInspecting))
		Expect(stillInspecting.Status.OperationalStatus).To(Equal(keziov1alpha3.MachineOperationalStatusOK))

		By("simulating the live agent's registration: MachineHardware plus AgentRegistered=True")
		hw := &keziov1alpha3.MachineHardware{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}
		Expect(k8sClient.Create(ctx, hw)).To(Succeed())

		var registering keziov1alpha3.Machine
		Expect(k8sClient.Get(ctx, key, &registering)).To(Succeed())
		apimeta.SetStatusCondition(&registering.Status.Conditions, metav1.Condition{
			Type:               keziov1alpha3.MachineConditionAgentRegistered,
			Status:             metav1.ConditionTrue,
			Reason:             "AgentRegistered",
			Message:            "kezio-agent registered",
			ObservedGeneration: registering.Generation,
		})
		Expect(k8sClient.Status().Update(ctx, &registering)).To(Succeed())

		By("reconciling once more: Inspect observes registration and hardware, machine reaches Available")
		_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var final keziov1alpha3.Machine
		Expect(k8sClient.Get(ctx, key, &final)).To(Succeed())
		Expect(final.Status.State).To(Equal(keziov1alpha3.MachineStateAvailable))
		Expect(final.Status.OperationalStatus).To(Equal(keziov1alpha3.MachineOperationalStatusOK))
	})

	It("deletes a Machine through the real Deprovision step instead of stalling in Deprovisioning forever", func() {
		name := fmt.Sprintf("agent-deployer-delete-%d", GinkgoRandomSeed())
		secretName := name + "-bmc"
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: "default"},
			Type:       corev1.SecretTypeBasicAuth,
			Data: map[string][]byte{
				"username": []byte("admin"),
				"password": []byte("hunter2"),
			},
		}
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())
		DeferCleanup(func() {
			Expect(k8sClient.Delete(ctx, secret)).To(Succeed())
		})

		machine := &keziov1alpha3.Machine{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: keziov1alpha3.MachineSpec{
				BMC: keziov1alpha3.MachineBMC{
					Address:              machineBMCTestDriverScheme + "://" + name,
					CredentialsSecretRef: keziov1alpha3.SecretReference{Name: secretName},
				},
				BootMACAddress: "aa:bb:cc:dd:ee:04",
				SubnetRef:      keziov1alpha3.NameRef{Name: "default"},
			},
		}
		Expect(k8sClient.Create(ctx, machine)).To(Succeed())

		reconciler := &MachineReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Deployer: &deployer.AgentDeployer{Client: k8sClient},
		}
		key := types.NamespacedName{Name: name, Namespace: "default"}

		By("reconciling until the finalizer is added")
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		By("deleting the Machine")
		var toDelete keziov1alpha3.Machine
		Expect(k8sClient.Get(ctx, key, &toDelete)).To(Succeed())
		Expect(k8sClient.Delete(ctx, &toDelete)).To(Succeed())

		By("reconciling the delete walk to completion: a stubbed Deprovision would stall here forever")
		var gone bool
		var machine2 keziov1alpha3.Machine
		for i := 0; i < 10; i++ {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			if err := k8sClient.Get(ctx, key, &machine2); apierrors.IsNotFound(err) {
				gone = true
				break
			}
		}
		Expect(gone).To(BeTrue(), "the Machine must be gone once the real AgentDeployer's delete walk finishes")
	})
})
