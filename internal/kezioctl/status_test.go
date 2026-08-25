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

package kezioctl

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

func TestStatus_NoDeployRecorded(t *testing.T) {
	machine := &keziov1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1", Namespace: "default"},
		Status:     keziov1alpha3.MachineStatus{State: keziov1alpha3.MachineStateAvailable},
	}
	c := fake.NewClientBuilder().WithScheme(Scheme).WithObjects(machine).Build()

	var out bytes.Buffer
	err := Status(context.Background(), c, StatusOptions{MachineName: "node-1", Namespace: "default", Out: &out})
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if !strings.Contains(out.String(), "no deploy recorded") {
		t.Errorf("output = %q, want it to report no deploy recorded", out.String())
	}
	if !strings.Contains(out.String(), keziov1alpha3.MachineStateAvailable) {
		t.Errorf("output = %q, want it to report the Machine's state", out.String())
	}
}

func TestStatus_PrintsCurrentDeployRunOnce(t *testing.T) {
	run := &keziov1alpha3.DeployRun{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1-abcde", Namespace: "default"},
		Spec:       keziov1alpha3.DeployRunSpec{MachineRef: keziov1alpha3.NameRef{Name: "node-1"}},
		Status:     keziov1alpha3.DeployRunStatus{Phase: keziov1alpha3.DeployRunPhaseWritingContent},
	}
	machine := &keziov1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1", Namespace: "default"},
		Status: keziov1alpha3.MachineStatus{
			State:         keziov1alpha3.MachineStateProvisioning,
			CurrentRunRef: &keziov1alpha3.NameRef{Name: "node-1-abcde"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(Scheme).WithObjects(machine, run).Build()

	var out bytes.Buffer
	if err := Status(context.Background(), c, StatusOptions{MachineName: "node-1", Namespace: "default", Out: &out}); err != nil {
		t.Fatalf("Status() error = %v", err)
	}

	got := out.String()
	if strings.Count(got, "\n") != 1 {
		t.Fatalf("output = %q, want exactly one line without --watch", got)
	}
	if !strings.Contains(got, "node-1-abcde") || !strings.Contains(got, keziov1alpha3.DeployRunPhaseWritingContent) {
		t.Errorf("output = %q, want it to name the DeployRun and its phase", got)
	}
}

// TestStatus_PrintsLastAttemptedDeployRun covers the report after a
// deployment did not succeed and the Machine no longer names a current
// run: the failed run is what an operator needs, so it must be reported
// in place of the older successful one.
func TestStatus_PrintsLastAttemptedDeployRun(t *testing.T) {
	failedRun := &keziov1alpha3.DeployRun{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1-failed", Namespace: "default"},
		Spec:       keziov1alpha3.DeployRunSpec{MachineRef: keziov1alpha3.NameRef{Name: "node-1"}},
		Status:     keziov1alpha3.DeployRunStatus{Phase: keziov1alpha3.DeployRunPhaseFailed},
	}
	successfulRun := &keziov1alpha3.DeployRun{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1-older", Namespace: "default"},
		Spec:       keziov1alpha3.DeployRunSpec{MachineRef: keziov1alpha3.NameRef{Name: "node-1"}},
		Status:     keziov1alpha3.DeployRunStatus{Phase: keziov1alpha3.DeployRunPhaseSucceeded},
	}
	machine := &keziov1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1", Namespace: "default"},
		Status: keziov1alpha3.MachineStatus{
			State:                keziov1alpha3.MachineStateEnrolling,
			LastAttemptedRunRef:  &keziov1alpha3.NameRef{Name: "node-1-failed"},
			LastSuccessfulRunRef: &keziov1alpha3.NameRef{Name: "node-1-older"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(Scheme).WithObjects(machine, failedRun, successfulRun).Build()

	var out bytes.Buffer
	if err := Status(context.Background(), c, StatusOptions{MachineName: "node-1", Namespace: "default", Out: &out}); err != nil {
		t.Fatalf("Status() error = %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "node-1-failed") {
		t.Errorf("output = %q, want it to name the last attempted DeployRun", got)
	}
	if strings.Contains(got, "node-1-older") {
		t.Errorf("output = %q, want the last attempt reported instead of the older successful run", got)
	}
}

func TestStatus_MachineNotFound(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(Scheme).Build()

	var out bytes.Buffer
	err := Status(context.Background(), c, StatusOptions{MachineName: "missing", Namespace: "default", Out: &out})
	if err == nil {
		t.Fatal("expected an error for a missing Machine")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want it to mention not found", err.Error())
	}
}

func TestStatus_RequiresOut(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(Scheme).Build()

	if err := Status(context.Background(), c, StatusOptions{MachineName: "node-1", Namespace: "default"}); err == nil {
		t.Fatal("expected an error when Out is nil")
	}
}

// TestStatus_WatchReachesTerminalState drives a DeployRun from
// WritingContent to Succeeded across polls, and asserts --watch keeps
// polling until it observes the terminal phase and then returns, printing
// one line per distinct phase (never a duplicate of the immediately
// preceding line).
func TestStatus_WatchReachesTerminalState(t *testing.T) {
	run := &keziov1alpha3.DeployRun{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1-abcde", Namespace: "default"},
		Spec:       keziov1alpha3.DeployRunSpec{MachineRef: keziov1alpha3.NameRef{Name: "node-1"}},
		Status:     keziov1alpha3.DeployRunStatus{Phase: keziov1alpha3.DeployRunPhaseWritingContent},
	}
	machine := &keziov1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1", Namespace: "default"},
		Status: keziov1alpha3.MachineStatus{
			State:         keziov1alpha3.MachineStateProvisioning,
			CurrentRunRef: &keziov1alpha3.NameRef{Name: "node-1-abcde"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(Scheme).WithObjects(machine, run).Build()

	var advance sync.Once
	go func() {
		time.Sleep(5 * time.Millisecond)
		advance.Do(func() {
			latest := &keziov1alpha3.DeployRun{}
			_ = c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "node-1-abcde"}, latest)
			latest.Status.Phase = keziov1alpha3.DeployRunPhaseSucceeded
			_ = c.Update(context.Background(), latest)
		})
	}()

	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := Status(ctx, c, StatusOptions{
		MachineName:  "node-1",
		Namespace:    "default",
		Watch:        true,
		PollInterval: time.Millisecond,
		Out:          &out,
	})
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}

	got := out.String()
	if !strings.Contains(got, keziov1alpha3.DeployRunPhaseWritingContent) {
		t.Errorf("output = %q, want it to have reported the initial phase", got)
	}
	if !strings.Contains(got, keziov1alpha3.DeployRunPhaseSucceeded) {
		t.Errorf("output = %q, want it to have reported the terminal phase", got)
	}
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	for i := 1; i < len(lines); i++ {
		if lines[i] == lines[i-1] {
			t.Errorf("output repeated an unchanged line: %q", lines[i])
		}
	}
}

func TestStatus_WatchCanceledByContext(t *testing.T) {
	run := &keziov1alpha3.DeployRun{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1-abcde", Namespace: "default"},
		Spec:       keziov1alpha3.DeployRunSpec{MachineRef: keziov1alpha3.NameRef{Name: "node-1"}},
		Status:     keziov1alpha3.DeployRunStatus{Phase: keziov1alpha3.DeployRunPhaseWritingContent},
	}
	machine := &keziov1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1", Namespace: "default"},
		Status: keziov1alpha3.MachineStatus{
			State:         keziov1alpha3.MachineStateProvisioning,
			CurrentRunRef: &keziov1alpha3.NameRef{Name: "node-1-abcde"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(Scheme).WithObjects(machine, run).Build()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()

	var out bytes.Buffer
	err := Status(ctx, c, StatusOptions{
		MachineName:  "node-1",
		Namespace:    "default",
		Watch:        true,
		PollInterval: time.Millisecond,
		Out:          &out,
	})
	if err == nil {
		t.Fatal("expected an error when the context is canceled before a terminal phase")
	}
}
