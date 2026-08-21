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

package agentserver

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/agentapi"
	"github.com/tjjh89017/kezio/internal/planbuild"
	"github.com/tjjh89017/kezio/internal/posthookdefaults"
)

const nextPlanTestManagerNamespace = "kezio-system"

// blankDataImage builds a Ready Image with a single mkfs data slot, cheap
// to resolve: no PartitionContent or seeder Deployment/Pod lookup.
func blankDataImage(name, ns string) *keziov1alpha2.Image {
	img := &keziov1alpha2.Image{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: keziov1alpha2.ImageSpec{
			Layout: keziov1alpha2.ImageDiskLayout{
				PartitionTable: keziov1alpha2.PartitionTableGPT,
				SfdiskJSON:     `{"partitiontable":{}}`,
				Slots:          []keziov1alpha2.ImageSlot{{Number: 1, Role: keziov1alpha2.PartitionRoleData, FSType: "ext4"}},
			},
		},
	}
	img.Status.State = keziov1alpha2.ImageStateReady
	return img
}

func mustCreateDefaultPostHook(t *testing.T, c client.Client) {
	t.Helper()
	ph := &keziov1alpha2.PostHook{
		ObjectMeta: metav1.ObjectMeta{Name: posthookdefaults.DefaultFinalizeHookName, Namespace: nextPlanTestManagerNamespace},
		Spec:       posthookdefaults.Spec(),
	}
	if err := c.Create(t.Context(), ph); err != nil {
		t.Fatalf("create default posthook: %v", err)
	}
	apimeta.SetStatusCondition(&ph.Status.Conditions, metav1.Condition{
		Type: keziov1alpha2.PostHookConditionValid, Status: metav1.ConditionTrue, Reason: "TestFixture", Message: "fixture",
	})
	if err := c.Status().Update(t.Context(), ph); err != nil {
		t.Fatalf("update default posthook status: %v", err)
	}
}

// newNextPlanTestMachine builds a Machine wired the same way
// newTestMachineWithSession does (a live session, so /agent/next
// authenticates), plus a resolvable deploy intent and a currentRunRef.
func newNextPlanTestMachine(now time.Time) *keziov1alpha2.Machine {
	m := newTestMachineWithSession(now)
	m.Spec.ImageRef = &keziov1alpha2.NameRef{Name: "img1"}
	m.Status.CurrentRunRef = &keziov1alpha2.NameRef{Name: "run1"}
	return m
}

func TestHandleNext_PendingRunAnswersDeployWithPlan(t *testing.T) {
	now := time.Now()
	machine := newNextPlanTestMachine(now)
	hw := &keziov1alpha2.MachineHardware{
		ObjectMeta: metav1.ObjectMeta{Name: machine.Name, Namespace: machine.Namespace},
		Spec:       keziov1alpha2.MachineHardwareSpec{Disks: []keziov1alpha2.MachineHardwareDisk{{DeviceName: "/dev/vda", SizeBytes: 32 << 30}}},
	}
	img := blankDataImage("img1", machine.Namespace)
	run := &keziov1alpha2.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "run1", Namespace: machine.Namespace}}
	run.Status.Phase = keziov1alpha2.DeployRunPhasePending

	s, c := newTestServer(t, now, machine, hw, img, run)
	mustCreateDefaultPostHook(t, c)
	s.PlanBuilder = &planbuild.Builder{Client: c, ManagerNamespace: nextPlanTestManagerNamespace}

	rec := doNext(s.Handler(), testSessionToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}
	var resp agentapi.NextResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Action != agentapi.ActionDeploy {
		t.Fatalf("Action = %q, want %q", resp.Action, agentapi.ActionDeploy)
	}
	if resp.Plan == nil {
		t.Fatal("Plan is nil, want the resolved DeployPlan")
	}
	if resp.Plan.RunName != run.Name {
		t.Errorf("Plan.RunName = %q, want %q", resp.Plan.RunName, run.Name)
	}
}

func TestHandleNext_NoCurrentRunAnswersWait(t *testing.T) {
	now := time.Now()
	machine := newTestMachineWithSession(now) // no CurrentRunRef
	s, c := newTestServer(t, now, machine)
	s.PlanBuilder = &planbuild.Builder{Client: c, ManagerNamespace: nextPlanTestManagerNamespace}

	rec := doNext(s.Handler(), testSessionToken)
	var resp agentapi.NextResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Action != agentapi.ActionWait {
		t.Fatalf("Action = %q, want %q", resp.Action, agentapi.ActionWait)
	}
}

func TestHandleNext_TerminalPhaseAnswersWait(t *testing.T) {
	now := time.Now()
	machine := newNextPlanTestMachine(now)
	run := &keziov1alpha2.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "run1", Namespace: machine.Namespace}}
	run.Status.Phase = keziov1alpha2.DeployRunPhaseSucceeded

	s, c := newTestServer(t, now, machine, run)
	s.PlanBuilder = &planbuild.Builder{Client: c, ManagerNamespace: nextPlanTestManagerNamespace}

	rec := doNext(s.Handler(), testSessionToken)
	var resp agentapi.NextResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Action != agentapi.ActionWait {
		t.Fatalf("Action = %q, want %q for a terminal-phase run", resp.Action, agentapi.ActionWait)
	}
}

func TestHandleNext_BuildErrorAnswersWait(t *testing.T) {
	now := time.Now()
	machine := newNextPlanTestMachine(now)
	// No MachineHardware, no Image: Build fails with a NotReadyError.
	run := &keziov1alpha2.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "run1", Namespace: machine.Namespace}}
	run.Status.Phase = keziov1alpha2.DeployRunPhasePending

	s, c := newTestServer(t, now, machine, run)
	s.PlanBuilder = &planbuild.Builder{Client: c, ManagerNamespace: nextPlanTestManagerNamespace}

	rec := doNext(s.Handler(), testSessionToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200 even though the build failed", rec.Code, rec.Body.String())
	}
	var resp agentapi.NextResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Action != agentapi.ActionWait {
		t.Fatalf("Action = %q, want %q when Build fails", resp.Action, agentapi.ActionWait)
	}
}

func TestHandleNext_NilPlanBuilderAnswersWait(t *testing.T) {
	now := time.Now()
	machine := newNextPlanTestMachine(now)
	run := &keziov1alpha2.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "run1", Namespace: machine.Namespace}}
	run.Status.Phase = keziov1alpha2.DeployRunPhasePending
	s, _ := newTestServer(t, now, machine, run) // s.PlanBuilder left nil

	rec := doNext(s.Handler(), testSessionToken)
	var resp agentapi.NextResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Action != agentapi.ActionWait {
		t.Fatalf("Action = %q, want %q with no PlanBuilder configured", resp.Action, agentapi.ActionWait)
	}
}
