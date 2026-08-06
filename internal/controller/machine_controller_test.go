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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/agentapi"
	"github.com/tjjh89017/kezio/internal/deployer"
)

// signalDeployer wraps a fake deployer.Deployer, overriding only
// Provision to poll MachineConditionProvisioningProgress instead of
// completing immediately: the same signal internal/agentserver's
// progress handler sets from an agent's own whole-plan step reports
// (agentapi.DeployStep* - see agentapi.DeployStepRebootingToDisk's doc
// comment for why that particular Reason means the deploy succeeded).
// This models the seam a later work item's real, agent-driven Provision
// implementation fills in (internal/deployer/agent.go's Provision doc
// comment), so this package's reconcileProvisioning logic - which is
// what actually sets status.provisioning.image and drives
// Provisioning -> Provisioned - is exercised the same way it will be
// once that real implementation lands, without this test depending on
// not-yet-implemented production code.
type signalDeployer struct {
	deployer.Deployer
	client ctrlclient.Client
	key    types.NamespacedName
}

func (d *signalDeployer) Provision(ctx context.Context, data *deployer.ProvisionData) (deployer.Result, error) {
	machine := &keziov1alpha1.Machine{}
	if err := d.client.Get(ctx, d.key, machine); err != nil {
		return deployer.Result{}, err
	}

	cond := apimeta.FindStatusCondition(machine.Status.Conditions, keziov1alpha1.MachineConditionProvisioningProgress)
	if cond == nil || cond.Reason != agentapi.DeployStepRebootingToDisk {
		return deployer.Result{RequeueAfter: time.Millisecond}, nil
	}

	if data.ImageRef != nil {
		data.ResolvedTargetDisk = "/dev/vda"
	}
	return deployer.Result{Dirty: true}, nil
}

// stallingInspectDeployer wraps a fake deployer.Deployer, overriding only
// Inspect to always ask the reconciler to poll again instead of ever
// completing - the same seam pattern signalDeployer gives Provision,
// applied to Inspect so a test can exercise reconcileInspecting's
// stuck-detection (pollInspecting) without a real registration ever
// landing.
type stallingInspectDeployer struct {
	deployer.Deployer
}

func (d *stallingInspectDeployer) Inspect(context.Context, *deployer.InspectData) (deployer.Result, error) {
	return deployer.Result{RequeueAfter: time.Millisecond}, nil
}

// powerOnMismatchDeployer wraps a fake deployer.Deployer, overriding only
// PowerOn to report success while observing the machine as still off -
// simulating a BMC whose PowerOn command is acknowledged but never
// actually lands (see deployer.Result.PoweredOn's doc comment for what a
// real agentDeployer does with GetPowerState's read-back in this case).
type powerOnMismatchDeployer struct {
	deployer.Deployer
}

func (d *powerOnMismatchDeployer) PowerOn(context.Context) (deployer.Result, error) {
	off := false
	return deployer.Result{Dirty: true, PoweredOn: &off}, nil
}

// laggingPowerOffDeployer wraps a fake deployer.Deployer, simulating a BMC
// whose PowerOff is a graceful shutdown still in progress: the first call
// observes the machine as still on, and every call after that observes it
// correctly off - the same shape a real BMC's GetPowerState read-back
// takes once an orderly shutdown actually completes. It also counts
// PowerOn calls, so a test can assert PowerOn still fires once
// spec.online flips back to true instead of being skipped by the first
// call's stale observation.
type laggingPowerOffDeployer struct {
	deployer.Deployer
	powerOffCalls *int
	powerOnCalls  *int
}

func (d *laggingPowerOffDeployer) PowerOff(context.Context) (deployer.Result, error) {
	*d.powerOffCalls++
	still := *d.powerOffCalls == 1
	return deployer.Result{Dirty: true, PoweredOn: &still}, nil
}

func (d *laggingPowerOffDeployer) PowerOn(context.Context) (deployer.Result, error) {
	*d.powerOnCalls++
	on := true
	return deployer.Result{Dirty: true, PoweredOn: &on}, nil
}

// newTestMachineSpec builds a minimal, valid MachineSpec for a test
// resource named name.
func newTestMachineSpec(name string) keziov1alpha1.MachineSpec {
	return keziov1alpha1.MachineSpec{
		BMC: &keziov1alpha1.MachineBMC{
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

		It("re-provisions a Provisioned Machine when spec.imageRef changes to a different Image", func() {
			const resourceName = "reprovision-machine"
			namespace := "default"
			key := types.NamespacedName{Name: resourceName, Namespace: namespace}

			By("creating the original and replacement target Images")
			image := &keziov1alpha1.Image{
				ObjectMeta: metav1.ObjectMeta{Name: "reprovision-machine-image", Namespace: namespace},
				Spec: keziov1alpha1.ImageSpec{
					Source: keziov1alpha1.ImageSource{Format: keziov1alpha1.ImageFormatQCOW2},
				},
			}
			Expect(k8sClient.Create(ctx, image)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, image)).To(Succeed()) })

			imageV2 := &keziov1alpha1.Image{
				ObjectMeta: metav1.ObjectMeta{Name: "reprovision-machine-image-v2", Namespace: namespace},
				Spec: keziov1alpha1.ImageSpec{
					Source: keziov1alpha1.ImageSource{Format: keziov1alpha1.ImageFormatQCOW2},
				},
			}
			Expect(k8sClient.Create(ctx, imageV2)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, imageV2)).To(Succeed()) })

			By("creating the Machine referencing the original image")
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

			By("driving the Machine to Provisioned with the original image")
			result, err := reconcileUntil(ctx, r, key, 20, func(m *keziov1alpha1.Machine) bool {
				return m.Status.State == keziov1alpha1.MachineStateProvisioned
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Status.State).To(Equal(keziov1alpha1.MachineStateProvisioned))
			Expect(result.Status.Provisioning).NotTo(BeNil())
			Expect(result.Status.Provisioning.Image).NotTo(BeNil())
			Expect(result.Status.Provisioning.Image.ImageRef).To(Equal(*spec.ImageRef))

			By("updating spec.imageRef to the replacement image")
			toUpdate := &keziov1alpha1.Machine{}
			Expect(k8sClient.Get(ctx, key, toUpdate)).To(Succeed())
			newRef := keziov1alpha1.NameRef{Name: imageV2.Name}
			toUpdate.Spec.ImageRef = &newRef
			Expect(k8sClient.Update(ctx, toUpdate)).To(Succeed())

			By("reconciling once, which must detect the ref change and move back to Provisioning")
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			reprovisioning := &keziov1alpha1.Machine{}
			Expect(k8sClient.Get(ctx, key, reprovisioning)).To(Succeed())
			Expect(reprovisioning.Status.State).To(Equal(keziov1alpha1.MachineStateProvisioning))

			By("reconciling through to Provisioned again, with a fresh deploy recorded for the new image")
			result, err = reconcileUntil(ctx, r, key, 20, func(m *keziov1alpha1.Machine) bool {
				return m.Status.State == keziov1alpha1.MachineStateProvisioned
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Status.State).To(Equal(keziov1alpha1.MachineStateProvisioned))
			Expect(result.Status.Provisioning).NotTo(BeNil())
			Expect(result.Status.Provisioning.Image).NotTo(BeNil())
			Expect(result.Status.Provisioning.Image.ImageRef).To(Equal(newRef))
			Expect(result.Status.Provisioning.Image.ImageRef).NotTo(Equal(*spec.ImageRef))
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

	Context("resolving the target disk", func() {
		// bothSSD gives the machine two same-shaped disks, so a hint
		// that only says "not spinning" cannot tell them apart.
		bothSSD := func(machineName string) *keziov1alpha1.MachineHardwareStatus {
			rotational := false
			return &keziov1alpha1.MachineHardwareStatus{
				Disks: []keziov1alpha1.MachineHardwareDisk{
					{
						DeviceName:   "/dev/nvme0n1",
						SerialNumber: fmt.Sprintf("%s-disk0", machineName),
						Rotational:   &rotational,
						SizeBytes:    256 * 1024 * 1024 * 1024,
					},
					{
						DeviceName:   "/dev/nvme1n1",
						SerialNumber: fmt.Sprintf("%s-disk1", machineName),
						Rotational:   &rotational,
						SizeBytes:    256 * 1024 * 1024 * 1024,
					},
				},
				Nics: []keziov1alpha1.MachineHardwareNIC{
					{Name: "eth0", MACAddress: "aa:bb:cc:dd:ee:01"},
				},
				MemoryBytes: 16 * 1024 * 1024 * 1024,
				CPUCount:    4,
			}
		}

		It("resolves a serial-number hint and records status.provisioning.targetDisk before Provision runs", func() {
			const resourceName = "diskmatch-machine"
			namespace := "default"
			key := types.NamespacedName{Name: resourceName, Namespace: namespace}
			nn := types.NamespacedName{Namespace: namespace, Name: resourceName}

			image := &keziov1alpha1.Image{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName + "-image", Namespace: namespace},
				Spec: keziov1alpha1.ImageSpec{
					Source: keziov1alpha1.ImageSource{Format: keziov1alpha1.ImageFormatQCOW2},
				},
			}
			Expect(k8sClient.Create(ctx, image)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, image)).To(Succeed()) })

			spec := newTestMachineSpec(resourceName)
			spec.ImageRef = &keziov1alpha1.NameRef{Name: image.Name}
			spec.TargetDisk = &keziov1alpha1.TargetDiskHints{
				SerialNumber: fmt.Sprintf("%s-disk1", resourceName),
			}
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

			// Provision itself is made to fail (an unrelated, injected
			// failure) so the reconcile stops right after resolution,
			// letting the test observe status.provisioning.targetDisk
			// independent of whether the deploy action it names ever
			// succeeds.
			provisionFails := true
			factory := deployer.NewFactory()
			factory.Fail = func(m types.NamespacedName, phase deployer.Phase) string {
				if m == nn && phase == deployer.PhaseProvision && provisionFails {
					return "injected provision failure"
				}
				return ""
			}

			r := &MachineReconciler{
				Client:          k8sClient,
				Scheme:          k8sClient.Scheme(),
				DeployerFactory: factory.New,
			}

			By("reconciling to Available")
			result, err := reconcileUntil(ctx, r, key, 10, func(m *keziov1alpha1.Machine) bool {
				return m.Status.State == keziov1alpha1.MachineStateAvailable
			})
			Expect(err).NotTo(HaveOccurred())

			By("overriding the fake single-disk inventory with two candidate disks")
			result.Status.Hardware = bothSSD(resourceName)
			Expect(k8sClient.Status().Update(ctx, result)).To(Succeed())

			By("reconciling into Provisioning; resolution succeeds and is recorded even though Provision then fails")
			result, err = reconcileUntil(ctx, r, key, 10, func(m *keziov1alpha1.Machine) bool {
				return m.Status.State == keziov1alpha1.MachineStateError
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Status.State).To(Equal(keziov1alpha1.MachineStateError))
			Expect(result.Status.ErrorMessage).To(Equal("injected provision failure"))
			Expect(result.Status.Provisioning).NotTo(BeNil())
			Expect(result.Status.Provisioning.Image).NotTo(BeNil())
			Expect(result.Status.Provisioning.Image.TargetDisk).To(Equal("/dev/nvme1n1"))

			By("clearing the injected failure so Provision can complete")
			provisionFails = false
			result, err = reconcileUntil(ctx, r, key, 10, func(m *keziov1alpha1.Machine) bool {
				return m.Status.State == keziov1alpha1.MachineStateProvisioned
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Status.State).To(Equal(keziov1alpha1.MachineStateProvisioned))
		})

		It("moves to Error with a clear message when the hints match more than one disk", func() {
			const resourceName = "diskmatch-conflict-machine"
			namespace := "default"
			key := types.NamespacedName{Name: resourceName, Namespace: namespace}

			image := &keziov1alpha1.Image{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName + "-image", Namespace: namespace},
				Spec: keziov1alpha1.ImageSpec{
					Source: keziov1alpha1.ImageSource{Format: keziov1alpha1.ImageFormatQCOW2},
				},
			}
			Expect(k8sClient.Create(ctx, image)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, image)).To(Succeed()) })

			spec := newTestMachineSpec(resourceName)
			spec.ImageRef = &keziov1alpha1.NameRef{Name: image.Name}
			// No hint at all: with two reported disks this is ambiguous
			// on its own, exercising the "no hint, multiple disks" rule
			// as well as the general ambiguous-match error path.
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

			By("reconciling to Available")
			result, err := reconcileUntil(ctx, r, key, 10, func(m *keziov1alpha1.Machine) bool {
				return m.Status.State == keziov1alpha1.MachineStateAvailable
			})
			Expect(err).NotTo(HaveOccurred())

			By("overriding the fake single-disk inventory with two indistinguishable disks")
			result.Status.Hardware = bothSSD(resourceName)
			Expect(k8sClient.Status().Update(ctx, result)).To(Succeed())

			By("reconciling into Provisioning, which fails to resolve a unique disk")
			result, err = reconcileUntil(ctx, r, key, 10, func(m *keziov1alpha1.Machine) bool {
				return m.Status.State == keziov1alpha1.MachineStateError
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Status.State).To(Equal(keziov1alpha1.MachineStateError))
			Expect(result.Status.ErrorMessage).To(ContainSubstring("targetDisk hint is required"))
			Expect(result.Status.ErrorCount).To(BeNumerically(">=", 1))
			readyCond := apimeta.FindStatusCondition(result.Status.Conditions, keziov1alpha1.ConditionReady)
			Expect(readyCond).NotTo(BeNil())
			Expect(readyCond.Reason).To(Equal(reasonProvisionFailed))

			By("never having recorded a resolved target disk, since the match failed before any deploy action")
			Expect(result.Status.Provisioning).To(BeNil())
		})

		It("moves to Error naming the Image when the referenced Image reaches ImageStateFailed, then resumes once it recovers", func() {
			const resourceName = "image-failed-machine"
			namespace := "default"
			key := types.NamespacedName{Name: resourceName, Namespace: namespace}

			image := &keziov1alpha1.Image{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName + "-image", Namespace: namespace},
				Spec: keziov1alpha1.ImageSpec{
					Source: keziov1alpha1.ImageSource{Format: keziov1alpha1.ImageFormatQCOW2},
				},
			}
			Expect(k8sClient.Create(ctx, image)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, image)).To(Succeed()) })

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

			By("reconciling to Available")
			_, err := reconcileUntil(ctx, r, key, 10, func(m *keziov1alpha1.Machine) bool {
				return m.Status.State == keziov1alpha1.MachineStateAvailable
			})
			Expect(err).NotTo(HaveOccurred())

			By("driving the referenced Image to ImageStateFailed, as a checksum mismatch would")
			image.Status.State = keziov1alpha1.ImageStateFailed
			Expect(k8sClient.Status().Update(ctx, image)).To(Succeed())

			By("reconciling into Provisioning, which stalls forever on the failed Image without this check")
			result, err := reconcileUntil(ctx, r, key, 10, func(m *keziov1alpha1.Machine) bool {
				return m.Status.State == keziov1alpha1.MachineStateError
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Status.State).To(Equal(keziov1alpha1.MachineStateError))
			Expect(result.Status.ErrorMessage).To(ContainSubstring(image.Name))
			Expect(result.Status.ErrorCount).To(BeNumerically(">=", 1))
			readyCond := apimeta.FindStatusCondition(result.Status.Conditions, keziov1alpha1.ConditionReady)
			Expect(readyCond).NotTo(BeNil())
			Expect(readyCond.Reason).To(Equal(reasonProvisionFailed))

			By("recovering the Image back to Ready so the existing Error-state retry path can proceed")
			image.Status.State = keziov1alpha1.ImageStateReady
			Expect(k8sClient.Status().Update(ctx, image)).To(Succeed())

			result, err = reconcileUntil(ctx, r, key, 10, func(m *keziov1alpha1.Machine) bool {
				return m.Status.State == keziov1alpha1.MachineStateProvisioned
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Status.State).To(Equal(keziov1alpha1.MachineStateProvisioned))
		})
	})

	Context("reacting to an agent-reported deploy success", func() {
		It("stalls in Provisioning while sub-step reasons progress, then records status.provisioning.image and advances to Provisioned once the terminal step lands", func() {
			const resourceName = "agent-signal-machine"
			namespace := "default"
			key := types.NamespacedName{Name: resourceName, Namespace: namespace}

			image := &keziov1alpha1.Image{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName + "-image", Namespace: namespace},
				Spec: keziov1alpha1.ImageSpec{
					Source: keziov1alpha1.ImageSource{Format: keziov1alpha1.ImageFormatQCOW2},
				},
			}
			Expect(k8sClient.Create(ctx, image)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, image)).To(Succeed()) })

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

			base := deployer.NewFactory()
			r := &MachineReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				DeployerFactory: func(m *keziov1alpha1.Machine) (deployer.Deployer, error) {
					fake, err := base.New(m)
					if err != nil {
						return nil, err
					}
					return &signalDeployer{Deployer: fake, client: k8sClient, key: key}, nil
				},
			}

			By("reconciling to Provisioning")
			_, err := reconcileUntil(ctx, r, key, 10, func(m *keziov1alpha1.Machine) bool {
				return m.Status.State == keziov1alpha1.MachineStateProvisioning
			})
			Expect(err).NotTo(HaveOccurred())

			By("simulating the agent's WritingContent step report - the reconciler must stay in Provisioning")
			Expect(setProvisioningProgressReasonForTest(ctx, key, agentapi.DeployStepWritingContent, "42%")).To(Succeed())
			stalled, err := reconcileUntil(ctx, r, key, 3, func(m *keziov1alpha1.Machine) bool {
				return m.Status.State == keziov1alpha1.MachineStateProvisioned
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(stalled.Status.State).To(Equal(keziov1alpha1.MachineStateProvisioning))
			cond := apimeta.FindStatusCondition(stalled.Status.Conditions, keziov1alpha1.MachineConditionProvisioningProgress)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Reason).To(Equal(agentapi.DeployStepWritingContent))

			By("simulating the agent's terminal RebootingToDisk report")
			Expect(setProvisioningProgressReasonForTest(ctx, key, agentapi.DeployStepRebootingToDisk, "deploy finalized; rebooting")).To(Succeed())

			By("reconciling through to Provisioned, with status.provisioning.image recorded")
			result, err := reconcileUntil(ctx, r, key, 10, func(m *keziov1alpha1.Machine) bool {
				return m.Status.State == keziov1alpha1.MachineStateProvisioned
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Status.State).To(Equal(keziov1alpha1.MachineStateProvisioned))
			Expect(result.Status.Provisioning).NotTo(BeNil())
			Expect(result.Status.Provisioning.Image).NotTo(BeNil())
			Expect(result.Status.Provisioning.Image.ImageRef).To(Equal(*spec.ImageRef))
			Expect(result.Status.Provisioning.Image.TargetDisk).To(Equal("/dev/vda"))
		})
	})

	Context("managing power state", func() {
		// newPowerOnlyMachineSpec builds a Machine spec with no ImageRef
		// and no DataImages, so needsProvisioning is always false and the
		// machine reaches Available directly, letting reconcilePower run
		// on the very first Available reconcile instead of being
		// preceded by a deployment.
		newPowerOnlyMachineSpec := func(name string, online bool) keziov1alpha1.MachineSpec {
			spec := newTestMachineSpec(name)
			spec.Online = &online
			return spec
		}

		It("calls PowerOn and records status.poweredOn=true when spec.online is true", func() {
			const resourceName = "power-on-machine"
			namespace := "default"
			key := types.NamespacedName{Name: resourceName, Namespace: namespace}
			nn := types.NamespacedName{Namespace: namespace, Name: resourceName}

			machine := &keziov1alpha1.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: namespace},
				Spec:       newPowerOnlyMachineSpec(resourceName, true),
			}
			Expect(k8sClient.Create(ctx, machine)).To(Succeed())
			DeferCleanup(func() {
				m := &keziov1alpha1.Machine{}
				if err := k8sClient.Get(ctx, key, m); err == nil {
					Expect(k8sClient.Delete(ctx, m)).To(Succeed())
				}
			})

			var powerOnCalls, powerOffCalls int
			factory := deployer.NewFactory()
			factory.Fail = func(m types.NamespacedName, phase deployer.Phase) string {
				if m == nn {
					switch phase {
					case deployer.PhasePowerOn:
						powerOnCalls++
					case deployer.PhasePowerOff:
						powerOffCalls++
					}
				}
				return ""
			}

			r := &MachineReconciler{
				Client:          k8sClient,
				Scheme:          k8sClient.Scheme(),
				DeployerFactory: factory.New,
			}

			By("reconciling to Available, where reconcilePower matches status.poweredOn to spec.online")
			result, err := reconcileUntil(ctx, r, key, 10, func(m *keziov1alpha1.Machine) bool {
				return m.Status.State == keziov1alpha1.MachineStateAvailable && m.Status.PoweredOn != nil
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Status.PoweredOn).NotTo(BeNil())
			Expect(*result.Status.PoweredOn).To(BeTrue())
			Expect(powerOnCalls).To(Equal(1))
			Expect(powerOffCalls).To(BeZero())

			By("not calling PowerOn again once status.poweredOn already matches spec.online")
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			steady := &keziov1alpha1.Machine{}
			Expect(k8sClient.Get(ctx, key, steady)).To(Succeed())
			Expect(*steady.Status.PoweredOn).To(BeTrue())
			Expect(powerOnCalls).To(Equal(1))
		})

		It("records status.poweredOn from the driver-observed state, not spec.online, when they disagree", func() {
			const resourceName = "power-on-mismatch-machine"
			namespace := "default"
			key := types.NamespacedName{Name: resourceName, Namespace: namespace}

			machine := &keziov1alpha1.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: namespace},
				Spec:       newPowerOnlyMachineSpec(resourceName, true),
			}
			Expect(k8sClient.Create(ctx, machine)).To(Succeed())
			DeferCleanup(func() {
				m := &keziov1alpha1.Machine{}
				if err := k8sClient.Get(ctx, key, m); err == nil {
					Expect(k8sClient.Delete(ctx, m)).To(Succeed())
				}
			})

			base := deployer.NewFactory()
			r := &MachineReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				DeployerFactory: func(m *keziov1alpha1.Machine) (deployer.Deployer, error) {
					fake, err := base.New(m)
					if err != nil {
						return nil, err
					}
					return &powerOnMismatchDeployer{Deployer: fake}, nil
				},
			}

			By("reconciling to Available, where spec.online=true but the driver reports the machine still off")
			result, err := reconcileUntil(ctx, r, key, 10, func(m *keziov1alpha1.Machine) bool {
				return m.Status.State == keziov1alpha1.MachineStateAvailable && m.Status.PoweredOn != nil
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Status.PoweredOn).NotTo(BeNil())
			Expect(*result.Status.PoweredOn).To(BeFalse(), "status.poweredOn must reflect what the driver observed, not the commanded/desired state")
		})

		It("requeues at backoffBaseDelay when PowerOff succeeds but the machine has not actually gone off yet, then still calls PowerOn once it does", func() {
			// Regression test for a stale post-PowerOff observation
			// (for example a graceful shutdown that is still in
			// progress when GetPowerState is read back) getting cached
			// into status.poweredOn as if it were confirmed, then
			// wrongly matching a later spec.online=true and causing
			// reconcilePower to skip PowerOn entirely.
			const resourceName = "power-off-lagging-machine"
			namespace := "default"
			key := types.NamespacedName{Name: resourceName, Namespace: namespace}
			nn := types.NamespacedName{Namespace: namespace, Name: resourceName}

			machine := &keziov1alpha1.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: namespace},
				Spec:       newPowerOnlyMachineSpec(resourceName, false),
			}
			Expect(k8sClient.Create(ctx, machine)).To(Succeed())
			DeferCleanup(func() {
				m := &keziov1alpha1.Machine{}
				if err := k8sClient.Get(ctx, key, m); err == nil {
					Expect(k8sClient.Delete(ctx, m)).To(Succeed())
				}
			})

			var powerOffCalls, powerOnCalls int
			base := deployer.NewFactory()
			r := &MachineReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				DeployerFactory: func(m *keziov1alpha1.Machine) (deployer.Deployer, error) {
					fake, err := base.New(m)
					if err != nil {
						return nil, err
					}
					if m.Name != nn.Name {
						return fake, nil
					}
					return &laggingPowerOffDeployer{Deployer: fake, powerOffCalls: &powerOffCalls, powerOnCalls: &powerOnCalls}, nil
				},
			}

			By("reconciling to Available, before reconcilePower has run at all")
			_, err := reconcileUntil(ctx, r, key, 10, func(m *keziov1alpha1.Machine) bool {
				return m.Status.State == keziov1alpha1.MachineStateAvailable
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(powerOffCalls).To(BeZero())

			By("reconciling once, where the first PowerOff still observes the machine as on and requeues at backoffBaseDelay")
			firstPowerResult, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(firstPowerResult.RequeueAfter).To(Equal(backoffBaseDelay))

			mismatched := &keziov1alpha1.Machine{}
			Expect(k8sClient.Get(ctx, key, mismatched)).To(Succeed())
			Expect(*mismatched.Status.PoweredOn).To(BeTrue(), "the honest, still-mismatched observation must be recorded")
			Expect(powerOffCalls).To(Equal(1))

			By("reconciling once more, which retries PowerOff and this time actually observes off")
			secondPowerResult, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(secondPowerResult.RequeueAfter).To(BeZero())

			settled := &keziov1alpha1.Machine{}
			Expect(k8sClient.Get(ctx, key, settled)).To(Succeed())
			Expect(*settled.Status.PoweredOn).To(BeFalse(), "the retry must have observed the machine actually off by now")
			Expect(powerOffCalls).To(Equal(2))

			By("flipping spec.online back to true: PowerOn must still fire, not be skipped by the earlier mismatch")
			Expect(k8sClient.Get(ctx, key, settled)).To(Succeed())
			online := true
			settled.Spec.Online = &online
			Expect(k8sClient.Update(ctx, settled)).To(Succeed())

			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(powerOnCalls).To(Equal(1), "PowerOn must be called once spec.online flips back to true")
		})

		It("calls PowerOff and records status.poweredOn=false when spec.online is false", func() {
			const resourceName = "power-off-machine"
			namespace := "default"
			key := types.NamespacedName{Name: resourceName, Namespace: namespace}
			nn := types.NamespacedName{Namespace: namespace, Name: resourceName}

			machine := &keziov1alpha1.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: namespace},
				Spec:       newPowerOnlyMachineSpec(resourceName, false),
			}
			Expect(k8sClient.Create(ctx, machine)).To(Succeed())
			DeferCleanup(func() {
				m := &keziov1alpha1.Machine{}
				if err := k8sClient.Get(ctx, key, m); err == nil {
					Expect(k8sClient.Delete(ctx, m)).To(Succeed())
				}
			})

			var powerOffCalls int
			factory := deployer.NewFactory()
			factory.Fail = func(m types.NamespacedName, phase deployer.Phase) string {
				if m == nn && phase == deployer.PhasePowerOff {
					powerOffCalls++
				}
				return ""
			}

			r := &MachineReconciler{
				Client:          k8sClient,
				Scheme:          k8sClient.Scheme(),
				DeployerFactory: factory.New,
			}

			By("reconciling to Available, where reconcilePower matches status.poweredOn to spec.online")
			result, err := reconcileUntil(ctx, r, key, 10, func(m *keziov1alpha1.Machine) bool {
				return m.Status.State == keziov1alpha1.MachineStateAvailable && m.Status.PoweredOn != nil
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Status.PoweredOn).NotTo(BeNil())
			Expect(*result.Status.PoweredOn).To(BeFalse())
			Expect(powerOffCalls).To(Equal(1))
		})

		It("requeues at backoffBaseDelay without touching state or errorCount when the power call fails", func() {
			const resourceName = "power-error-machine"
			namespace := "default"
			key := types.NamespacedName{Name: resourceName, Namespace: namespace}
			nn := types.NamespacedName{Namespace: namespace, Name: resourceName}

			machine := &keziov1alpha1.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: namespace},
				Spec:       newPowerOnlyMachineSpec(resourceName, true),
			}
			Expect(k8sClient.Create(ctx, machine)).To(Succeed())
			DeferCleanup(func() {
				m := &keziov1alpha1.Machine{}
				if err := k8sClient.Get(ctx, key, m); err == nil {
					Expect(k8sClient.Delete(ctx, m)).To(Succeed())
				}
			})

			factory := deployer.NewFactory()
			factory.Fail = func(m types.NamespacedName, phase deployer.Phase) string {
				if m == nn && phase == deployer.PhasePowerOn {
					return "injected power-on failure"
				}
				return ""
			}

			r := &MachineReconciler{
				Client:          k8sClient,
				Scheme:          k8sClient.Scheme(),
				DeployerFactory: factory.New,
			}

			By("reconciling to Available, where the injected PowerOn failure blocks reconcilePower")
			result, err := reconcileUntil(ctx, r, key, 10, func(m *keziov1alpha1.Machine) bool {
				return m.Status.State == keziov1alpha1.MachineStateAvailable
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Status.PoweredOn).To(BeNil())

			By("reconciling once more, which drives reconcilePower and hits the injected failure")
			powerResult, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(powerResult.RequeueAfter).To(Equal(backoffBaseDelay))

			By("leaving state and errorCount untouched, since a power drift is not a phase failure")
			afterFailure := &keziov1alpha1.Machine{}
			Expect(k8sClient.Get(ctx, key, afterFailure)).To(Succeed())
			Expect(afterFailure.Status.State).To(Equal(keziov1alpha1.MachineStateAvailable))
			Expect(afterFailure.Status.ErrorCount).To(Equal(int32(0)))
			Expect(afterFailure.Status.PoweredOn).To(BeNil())
		})
	})

	Context("deploying data images only, without an OS image", func() {
		It("deploys DataImages, leaves status.provisioning.image nil, and powers off after PowerOff AfterDeploy", func() {
			const resourceName = "data-only-machine"
			namespace := "default"
			key := types.NamespacedName{Name: resourceName, Namespace: namespace}
			nn := types.NamespacedName{Namespace: namespace, Name: resourceName}

			dataImage := &keziov1alpha1.Image{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName + "-data-image", Namespace: namespace},
				Spec: keziov1alpha1.ImageSpec{
					Source: keziov1alpha1.ImageSource{Format: keziov1alpha1.ImageFormatQCOW2},
				},
			}
			Expect(k8sClient.Create(ctx, dataImage)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, dataImage)).To(Succeed()) })

			spec := newTestMachineSpec(resourceName)
			spec.DataImages = []keziov1alpha1.MachineDataImage{
				{ImageRef: keziov1alpha1.NameRef{Name: dataImage.Name}},
			}
			spec.AfterDeploy = keziov1alpha1.AfterDeployPowerOff
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

			var powerOffCalls int
			factory := deployer.NewFactory()
			factory.Fail = func(m types.NamespacedName, phase deployer.Phase) string {
				if m == nn && phase == deployer.PhasePowerOff {
					powerOffCalls++
				}
				return ""
			}

			r := &MachineReconciler{
				Client:          k8sClient,
				Scheme:          k8sClient.Scheme(),
				DeployerFactory: factory.New,
			}

			By("reconciling through Provisioning to Provisioned")
			result, err := reconcileUntil(ctx, r, key, 10, func(m *keziov1alpha1.Machine) bool {
				return m.Status.State == keziov1alpha1.MachineStateProvisioned
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Status.State).To(Equal(keziov1alpha1.MachineStateProvisioned))

			By("recording DataImages and no OS image in status.provisioning")
			Expect(result.Status.Provisioning).NotTo(BeNil())
			Expect(result.Status.Provisioning.Image).To(BeNil())
			Expect(result.Status.Provisioning.DataImages).To(HaveLen(1))
			Expect(result.Status.Provisioning.DataImages[0].ImageRef).To(Equal(spec.DataImages[0].ImageRef))

			By("powering off after the data-only deploy, per AfterDeployPowerOff")
			Expect(result.Status.PoweredOn).NotTo(BeNil())
			Expect(*result.Status.PoweredOn).To(BeFalse())
			Expect(powerOffCalls).To(Equal(1))
		})

		It("power-cycles the machine instead of leaving it to the agent when AfterDeploy is Reboot", func() {
			const resourceName = "data-only-reboot-machine"
			namespace := "default"
			key := types.NamespacedName{Name: resourceName, Namespace: namespace}
			nn := types.NamespacedName{Namespace: namespace, Name: resourceName}

			dataImage := &keziov1alpha1.Image{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName + "-data-image", Namespace: namespace},
				Spec: keziov1alpha1.ImageSpec{
					Source: keziov1alpha1.ImageSource{Format: keziov1alpha1.ImageFormatQCOW2},
				},
			}
			Expect(k8sClient.Create(ctx, dataImage)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, dataImage)).To(Succeed()) })

			spec := newTestMachineSpec(resourceName)
			spec.DataImages = []keziov1alpha1.MachineDataImage{
				{ImageRef: keziov1alpha1.NameRef{Name: dataImage.Name}},
			}
			spec.AfterDeploy = keziov1alpha1.AfterDeployReboot
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

			var powerCycleCalls, powerOffCalls int
			factory := deployer.NewFactory()
			factory.Fail = func(m types.NamespacedName, phase deployer.Phase) string {
				if m == nn {
					switch phase {
					case deployer.PhasePowerCycle:
						powerCycleCalls++
					case deployer.PhasePowerOff:
						powerOffCalls++
					}
				}
				return ""
			}

			r := &MachineReconciler{
				Client:          k8sClient,
				Scheme:          k8sClient.Scheme(),
				DeployerFactory: factory.New,
			}

			By("reconciling through Provisioning to Provisioned")
			result, err := reconcileUntil(ctx, r, key, 10, func(m *keziov1alpha1.Machine) bool {
				return m.Status.State == keziov1alpha1.MachineStateProvisioned
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Status.State).To(Equal(keziov1alpha1.MachineStateProvisioned))

			By("routing the AfterDeploy=Reboot handoff through dep.PowerCycle (BMC-driven), not dep.PowerOff")
			Expect(powerCycleCalls).To(Equal(1))
			Expect(powerOffCalls).To(BeZero())
		})

		It("moves to Error with reasonProvisionFailed when the AfterDeploy power-off call fails", func() {
			const resourceName = "data-only-poweroff-fail-machine"
			namespace := "default"
			key := types.NamespacedName{Name: resourceName, Namespace: namespace}
			nn := types.NamespacedName{Namespace: namespace, Name: resourceName}

			dataImage := &keziov1alpha1.Image{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName + "-data-image", Namespace: namespace},
				Spec: keziov1alpha1.ImageSpec{
					Source: keziov1alpha1.ImageSource{Format: keziov1alpha1.ImageFormatQCOW2},
				},
			}
			Expect(k8sClient.Create(ctx, dataImage)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, dataImage)).To(Succeed()) })

			spec := newTestMachineSpec(resourceName)
			spec.DataImages = []keziov1alpha1.MachineDataImage{
				{ImageRef: keziov1alpha1.NameRef{Name: dataImage.Name}},
			}
			spec.AfterDeploy = keziov1alpha1.AfterDeployPowerOff
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

			factory := deployer.NewFactory()
			factory.Fail = func(m types.NamespacedName, phase deployer.Phase) string {
				if m == nn && phase == deployer.PhasePowerOff {
					return "injected power-off failure"
				}
				return ""
			}

			r := &MachineReconciler{
				Client:          k8sClient,
				Scheme:          k8sClient.Scheme(),
				DeployerFactory: factory.New,
			}

			By("reconciling into Provisioning, which fails at the AfterDeploy power-off handoff")
			result, err := reconcileUntil(ctx, r, key, 10, func(m *keziov1alpha1.Machine) bool {
				return m.Status.State == keziov1alpha1.MachineStateError
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Status.State).To(Equal(keziov1alpha1.MachineStateError))
			Expect(result.Status.ErrorMessage).To(Equal("injected power-off failure"))
			readyCond := apimeta.FindStatusCondition(result.Status.Conditions, keziov1alpha1.ConditionReady)
			Expect(readyCond).NotTo(BeNil())
			Expect(readyCond.Reason).To(Equal(reasonProvisionFailed))
		})

		It("moves to Error with reasonProvisionFailed when the AfterDeploy power-cycle call fails", func() {
			const resourceName = "data-only-reboot-fail-machine"
			namespace := "default"
			key := types.NamespacedName{Name: resourceName, Namespace: namespace}
			nn := types.NamespacedName{Namespace: namespace, Name: resourceName}

			dataImage := &keziov1alpha1.Image{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName + "-data-image", Namespace: namespace},
				Spec: keziov1alpha1.ImageSpec{
					Source: keziov1alpha1.ImageSource{Format: keziov1alpha1.ImageFormatQCOW2},
				},
			}
			Expect(k8sClient.Create(ctx, dataImage)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, dataImage)).To(Succeed()) })

			spec := newTestMachineSpec(resourceName)
			spec.DataImages = []keziov1alpha1.MachineDataImage{
				{ImageRef: keziov1alpha1.NameRef{Name: dataImage.Name}},
			}
			spec.AfterDeploy = keziov1alpha1.AfterDeployReboot
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

			factory := deployer.NewFactory()
			factory.Fail = func(m types.NamespacedName, phase deployer.Phase) string {
				if m == nn && phase == deployer.PhasePowerCycle {
					return "injected power-cycle failure"
				}
				return ""
			}

			r := &MachineReconciler{
				Client:          k8sClient,
				Scheme:          k8sClient.Scheme(),
				DeployerFactory: factory.New,
			}

			By("reconciling into Provisioning, which fails at the AfterDeploy power-cycle handoff")
			result, err := reconcileUntil(ctx, r, key, 10, func(m *keziov1alpha1.Machine) bool {
				return m.Status.State == keziov1alpha1.MachineStateError
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Status.State).To(Equal(keziov1alpha1.MachineStateError))
			Expect(result.Status.ErrorMessage).To(Equal("injected power-cycle failure"))
			readyCond := apimeta.FindStatusCondition(result.Status.Conditions, keziov1alpha1.ConditionReady)
			Expect(readyCond).NotTo(BeNil())
			Expect(readyCond.Reason).To(Equal(reasonProvisionFailed))
		})
	})

	Context("detecting a stuck inspection", func() {
		// stallingInspectDeployer wraps a fake deployer.Deployer,
		// overriding Inspect to always ask the reconciler to poll again
		// instead of ever completing, so a test can exercise
		// reconcileInspecting's stuck-detection (pollInspecting) without
		// a real registration ever landing.
		newStallingInspectFactory := func(base *deployer.FakeFactory) deployer.Factory {
			return func(m *keziov1alpha1.Machine) (deployer.Deployer, error) {
				fake, err := base.New(m)
				if err != nil {
					return nil, err
				}
				return &stallingInspectDeployer{Deployer: fake}, nil
			}
		}

		It("keeps polling while within inspectingStuckThreshold", func() {
			const resourceName = "stuck-inspect-machine"
			namespace := "default"
			key := types.NamespacedName{Name: resourceName, Namespace: namespace}

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

			factory := deployer.NewFactory()
			r := &MachineReconciler{
				Client:          k8sClient,
				Scheme:          k8sClient.Scheme(),
				DeployerFactory: newStallingInspectFactory(factory),
			}

			By("reconciling to Inspecting, where the stalling Inspect never completes")
			_, err := reconcileUntil(ctx, r, key, 10, func(m *keziov1alpha1.Machine) bool {
				return m.Status.State == keziov1alpha1.MachineStateInspecting && m.Status.InspectingSince != nil
			})
			Expect(err).NotTo(HaveOccurred())

			By("polling once more well within the threshold - still Inspecting")
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			stillInspecting := &keziov1alpha1.Machine{}
			Expect(k8sClient.Get(ctx, key, stillInspecting)).To(Succeed())
			Expect(stillInspecting.Status.State).To(Equal(keziov1alpha1.MachineStateInspecting))
		})

		It("moves to Error, without taking any power action, once inspectingStuckThreshold has elapsed", func() {
			// Regression test: reconcileInspecting used to force a BMC
			// power-cycle once a machine sat in Inspecting past
			// inspectingStuckThreshold, on the theory that this could
			// recover a machine whose PXE boot silently failed. In
			// practice nothing observable at this layer can tell that
			// case apart from kezio-agent being alive and still working
			// (for example, still fetching the live environment's
			// squashfs over a slow link) - so the power-cycle was just as
			// likely to interrupt real progress, and since interrupting
			// it does not make the next attempt any more likely to
			// register in time, it could repeat forever. pollInspecting
			// must instead report the failure through recordPhaseError
			// and take no automatic recovery action, leaving it to a
			// human who can see what the machine is actually doing.
			const resourceName = "stuck-inspect-error-machine"
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

			var powerCalls int
			factory := deployer.NewFactory()
			factory.Fail = func(m types.NamespacedName, phase deployer.Phase) string {
				if m == nn && (phase == deployer.PhasePowerCycle || phase == deployer.PhasePowerOn || phase == deployer.PhasePowerOff) {
					powerCalls++
				}
				return ""
			}

			r := &MachineReconciler{
				Client:          k8sClient,
				Scheme:          k8sClient.Scheme(),
				DeployerFactory: newStallingInspectFactory(factory),
			}

			By("reconciling to Inspecting, where the stalling Inspect never completes")
			inspecting, err := reconcileUntil(ctx, r, key, 10, func(m *keziov1alpha1.Machine) bool {
				return m.Status.State == keziov1alpha1.MachineStateInspecting && m.Status.InspectingSince != nil
			})
			Expect(err).NotTo(HaveOccurred())
			powerCallsAfterRegister := powerCalls

			By("backdating status.inspectingSince past inspectingStuckThreshold")
			stale := metav1.NewTime(inspecting.Status.InspectingSince.Add(-2 * inspectingStuckThreshold))
			inspecting.Status.InspectingSince = &stale
			Expect(k8sClient.Status().Update(ctx, inspecting)).To(Succeed())

			By("reconciling once more, which must give up instead of taking any power action")
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(powerCalls).To(Equal(powerCallsAfterRegister), "pollInspecting must not call any power phase")

			afterGivingUp := &keziov1alpha1.Machine{}
			Expect(k8sClient.Get(ctx, key, afterGivingUp)).To(Succeed())
			Expect(afterGivingUp.Status.State).To(Equal(keziov1alpha1.MachineStateError))
			readyCond := apimeta.FindStatusCondition(afterGivingUp.Status.Conditions, keziov1alpha1.ConditionReady)
			Expect(readyCond).NotTo(BeNil())
			Expect(readyCond.Reason).To(Equal(reasonInspectFailed))
			Expect(readyCond.Message).To(ContainSubstring("did not register"))

			By("retrying from Error immediately - the same reconcile a Machine watch fires right after " +
				"the status update above - must not trip the stuck check again")
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			afterImmediateRetry := &keziov1alpha1.Machine{}
			Expect(k8sClient.Get(ctx, key, afterImmediateRetry)).To(Succeed())
			Expect(afterImmediateRetry.Status.State).To(Equal(keziov1alpha1.MachineStateError),
				"still stuck, so the retry must stay in Error rather than silently drop back to Inspecting")
			Expect(afterImmediateRetry.Status.ErrorCount).To(Equal(int32(1)),
				"pollInspecting must have restarted inspectingSince's clock when it first gave up, "+
					"so an immediate retry observes a fresh window instead of re-tripping the stale one")
			Expect(powerCalls).To(Equal(powerCallsAfterRegister))

			By("backdating status.inspectingSince past inspectingStuckThreshold a second time")
			afterImmediateRetry.Status.InspectingSince.Time = afterImmediateRetry.Status.InspectingSince.Add(-2 * inspectingStuckThreshold)
			Expect(k8sClient.Status().Update(ctx, afterImmediateRetry)).To(Succeed())

			By("reconciling once more, which must report the failure again now that a full window has genuinely elapsed")
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			afterSecondError := &keziov1alpha1.Machine{}
			Expect(k8sClient.Get(ctx, key, afterSecondError)).To(Succeed())
			Expect(afterSecondError.Status.State).To(Equal(keziov1alpha1.MachineStateError))
			Expect(afterSecondError.Status.ErrorCount).To(Equal(int32(2)),
				"errorCount grows by one per inspectingStuckThreshold window, not per reconcile")
			Expect(powerCalls).To(Equal(powerCallsAfterRegister))
		})
	})
})

// setProvisioningProgressReasonForTest sets key's
// MachineConditionProvisioningProgress condition to (reason, message),
// the same shape internal/agentserver's setProvisioningProgressCondition
// writes from a real agent's progress report - standing in for that
// out-of-band HTTP call so this test can drive the reconciler's reaction
// to it without spinning up an agentserver.Server.
func setProvisioningProgressReasonForTest(ctx context.Context, key types.NamespacedName, reason, message string) error {
	m := &keziov1alpha1.Machine{}
	if err := k8sClient.Get(ctx, key, m); err != nil {
		return err
	}
	apimeta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
		Type:               keziov1alpha1.MachineConditionProvisioningProgress,
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: m.Generation,
	})
	return k8sClient.Status().Update(ctx, m)
}
