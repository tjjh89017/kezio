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

package deployer

import (
	"context"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/agentapi"
)

// newAgentTestClient builds a fake client seeded with machine, with
// generation and the status subresource behaving the way a real API
// server's do: Update bumps Generation on a spec change, Status().Update
// never does. Both matter to agentDeployer.Provision's staleness check.
func newAgentTestClient(t *testing.T, machine *keziov1alpha1.Machine) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := keziov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&keziov1alpha1.Machine{}).
		WithObjects(machine).
		Build()
}

// setProgressCondition writes the ProvisioningProgress condition directly
// (standing in for agentserver's real handler), with ObservedGeneration
// pinned so tests can exercise Provision's staleness check independently
// of the machine's actual generation.
func setProgressCondition(t *testing.T, c client.Client, key types.NamespacedName, reason, message string, observedGeneration int64) {
	t.Helper()
	ctx := context.Background()
	machine := &keziov1alpha1.Machine{}
	if err := c.Get(ctx, key, machine); err != nil {
		t.Fatalf("Get: %v", err)
	}
	apimeta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
		Type:               keziov1alpha1.MachineConditionProvisioningProgress,
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: observedGeneration,
	})
	if err := c.Status().Update(ctx, machine); err != nil {
		t.Fatalf("Status().Update: %v", err)
	}
}

func newProvisioningMachine() *keziov1alpha1.Machine {
	m := newTestMachine("default", "node-01")
	m.Status.Provisioning = &keziov1alpha1.MachineProvisioningStatus{
		Image: &keziov1alpha1.MachineProvisionedImage{
			ImageRef:   keziov1alpha1.NameRef{Name: "os-image"},
			TargetDisk: "/dev/nvme0n1",
		},
		DataImages: []keziov1alpha1.MachineProvisionedImage{
			{ImageRef: keziov1alpha1.NameRef{Name: "data-image"}, TargetDisk: "/dev/sda"},
		},
	}
	return m
}

// No ProvisioningProgress condition at all must poll, not fail or succeed.
func TestAgentDeployer_Provision_NoConditionYetKeepsPolling(t *testing.T) {
	machine := newProvisioningMachine()
	c := newAgentTestClient(t, machine)
	dep := &agentDeployer{client: c, key: types.NamespacedName{Namespace: "default", Name: "node-01"}}

	result, err := dep.Provision(context.Background(), &ProvisionData{ImageRef: &keziov1alpha1.NameRef{Name: "os-image"}})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if result.ErrorMessage != "" {
		t.Fatalf("ErrorMessage = %q, want empty", result.ErrorMessage)
	}
	if result.RequeueAfter == 0 {
		t.Fatal("RequeueAfter = 0, want a poll interval while no progress has been reported")
	}
}

// Every intermediate whole-plan step (plus older-agent fallback values and
// "Unknown") must keep polling, not be treated as success or failure.
func TestAgentDeployer_Provision_IntermediateStepKeepsPolling(t *testing.T) {
	for _, reason := range []string{
		agentapi.DeployStepPartitioning,
		agentapi.DeployStepWritingContent,
		agentapi.DeployStepRunningPostHook,
		agentapi.DeployStepFinalizing,
		agentapi.PartitionPhaseSeeding,
		"Unknown",
	} {
		t.Run(reason, func(t *testing.T) {
			machine := newProvisioningMachine()
			c := newAgentTestClient(t, machine)
			key := types.NamespacedName{Namespace: "default", Name: "node-01"}
			setProgressCondition(t, c, key, reason, "in progress", machine.Generation)
			dep := &agentDeployer{client: c, key: key}

			result, err := dep.Provision(context.Background(), &ProvisionData{ImageRef: &keziov1alpha1.NameRef{Name: "os-image"}})
			if err != nil {
				t.Fatalf("Provision: %v", err)
			}
			if result.ErrorMessage != "" {
				t.Fatalf("ErrorMessage = %q, want empty for intermediate reason %q", result.ErrorMessage, reason)
			}
			if result.RequeueAfter == 0 {
				t.Fatalf("RequeueAfter = 0, want a poll interval for intermediate reason %q", reason)
			}
		})
	}
}

// DeployStepRebootingToDisk must complete the deployment and echo
// ResolvedTargetDisk/ResolvedDataImageDisks from status.provisioning
// rather than re-deriving them.
func TestAgentDeployer_Provision_TerminalSuccessEchoesResolvedDisks(t *testing.T) {
	machine := newProvisioningMachine()
	c := newAgentTestClient(t, machine)
	key := types.NamespacedName{Namespace: "default", Name: "node-01"}
	setProgressCondition(t, c, key, agentapi.DeployStepRebootingToDisk, "rebooting", machine.Generation)
	dep := &agentDeployer{client: c, key: key}

	data := &ProvisionData{
		ImageRef:   &keziov1alpha1.NameRef{Name: "os-image"},
		DataImages: []keziov1alpha1.MachineDataImage{{ImageRef: keziov1alpha1.NameRef{Name: "data-image"}}},
	}
	result, err := dep.Provision(context.Background(), data)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if result.ErrorMessage != "" {
		t.Fatalf("ErrorMessage = %q, want empty on the terminal success step", result.ErrorMessage)
	}
	if result.RequeueAfter != 0 {
		t.Fatalf("RequeueAfter = %v, want 0 (complete) on the terminal success step", result.RequeueAfter)
	}
	if !result.Dirty {
		t.Fatal("Dirty = false, want true on the terminal success step")
	}
	if data.ResolvedTargetDisk != "/dev/nvme0n1" {
		t.Fatalf("ResolvedTargetDisk = %q, want the disk already recorded in status.provisioning.image.targetDisk (/dev/nvme0n1)", data.ResolvedTargetDisk)
	}
	if len(data.ResolvedDataImageDisks) != 1 || data.ResolvedDataImageDisks[0] != "/dev/sda" {
		t.Fatalf("ResolvedDataImageDisks = %v, want [/dev/sda] echoed from status.provisioning.dataImages", data.ResolvedDataImageDisks)
	}
}

// DeployStepFailed must report Result.ErrorMessage, not keep polling or
// silently succeed.
func TestAgentDeployer_Provision_FailedStepReportsError(t *testing.T) {
	machine := newProvisioningMachine()
	c := newAgentTestClient(t, machine)
	key := types.NamespacedName{Namespace: "default", Name: "node-01"}
	setProgressCondition(t, c, key, agentapi.DeployStepFailed, "sfdisk exited 1", machine.Generation)
	dep := &agentDeployer{client: c, key: key}

	result, err := dep.Provision(context.Background(), &ProvisionData{ImageRef: &keziov1alpha1.NameRef{Name: "os-image"}})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if result.ErrorMessage == "" {
		t.Fatal("ErrorMessage = \"\", want a non-empty message on the terminal failure step")
	}
	if result.RequeueAfter != 0 {
		t.Fatalf("RequeueAfter = %v, want 0 alongside an ErrorMessage", result.RequeueAfter)
	}
}

// A condition whose ObservedGeneration lags a just-bumped Generation
// (redeploy racing a stale terminal-success left from the prior
// deployment) must not be trusted; Provision must keep polling.
func TestAgentDeployer_Provision_StaleGenerationKeepsPolling(t *testing.T) {
	machine := newProvisioningMachine()
	c := newAgentTestClient(t, machine)
	key := types.NamespacedName{Namespace: "default", Name: "node-01"}

	// A prior deployment's terminal success, observed at generation 1.
	setProgressCondition(t, c, key, agentapi.DeployStepRebootingToDisk, "rebooting", 1)

	// A redeploy bumps Generation via a real spec update.
	current := &keziov1alpha1.Machine{}
	if err := c.Get(context.Background(), key, current); err != nil {
		t.Fatalf("Get: %v", err)
	}
	current.Spec.ImageRef = &keziov1alpha1.NameRef{Name: "new-os-image"}
	if err := c.Update(context.Background(), current); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if current.Generation == 1 {
		t.Fatal("test setup: Generation did not change on a spec update; the fake client no longer models this")
	}

	dep := &agentDeployer{client: c, key: key}
	result, err := dep.Provision(context.Background(), &ProvisionData{ImageRef: &keziov1alpha1.NameRef{Name: "new-os-image"}})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if result.ErrorMessage != "" {
		t.Fatalf("ErrorMessage = %q, want empty for a stale condition", result.ErrorMessage)
	}
	if result.RequeueAfter == 0 {
		t.Fatal("RequeueAfter = 0, want Provision to keep polling rather than trust a stale-generation condition")
	}
}

// Deprovision is a no-op reporting success (see its doc comment).
func TestAgentDeployer_Deprovision_ReportsSuccess(t *testing.T) {
	machine := newTestMachine("default", "node-01")
	c := newAgentTestClient(t, machine)
	dep := &agentDeployer{client: c, key: types.NamespacedName{Namespace: "default", Name: "node-01"}}

	result, err := dep.Deprovision(context.Background(), &DeprovisionData{})
	if err != nil {
		t.Fatalf("Deprovision: %v", err)
	}
	if result.ErrorMessage != "" {
		t.Fatalf("ErrorMessage = %q, want empty", result.ErrorMessage)
	}
	if !result.Dirty {
		t.Fatal("Dirty = false, want true on success")
	}
}
