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
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

func TestNewAbortDecider_MissingMachineAborts(t *testing.T) {
	c := newProgressTestClient(t)
	decide := NewAbortDecider(c)

	if !decide(context.Background(), "no-such-machine", "run1", "uid1") {
		t.Fatal("decide() = false, want true for a Machine that does not exist")
	}
}

func TestNewAbortDecider_NoCurrentRunRefAborts(t *testing.T) {
	machine := &keziov1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: "default"}}
	c := newProgressTestClient(t, machine)
	decide := NewAbortDecider(c)

	if !decide(context.Background(), "m1", "run1", "uid1") {
		t.Fatal("decide() = false, want true when the Machine has no currentRunRef")
	}
}

func TestNewAbortDecider_DifferentCurrentRunAborts(t *testing.T) {
	machine := &keziov1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: "default"},
		Status:     keziov1alpha3.MachineStatus{CurrentRunRef: &keziov1alpha3.NameRef{Name: "run2"}},
	}
	c := newProgressTestClient(t, machine)
	decide := NewAbortDecider(c)

	if !decide(context.Background(), "m1", "run1", "uid1") {
		t.Fatal("decide() = false, want true when runName is not the Machine's current run")
	}
}

func TestNewAbortDecider_MissingDeployRunAborts(t *testing.T) {
	machine := &keziov1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: "default"},
		Status:     keziov1alpha3.MachineStatus{CurrentRunRef: &keziov1alpha3.NameRef{Name: "run1"}},
	}
	c := newProgressTestClient(t, machine)
	decide := NewAbortDecider(c)

	if !decide(context.Background(), "m1", "run1", "uid1") {
		t.Fatal("decide() = false, want true when the current run's DeployRun does not exist")
	}
}

func TestNewAbortDecider_DeletingDeployRunAborts(t *testing.T) {
	machine := &keziov1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: "default"},
		Status:     keziov1alpha3.MachineStatus{CurrentRunRef: &keziov1alpha3.NameRef{Name: "run1"}},
	}
	run := &keziov1alpha3.DeployRun{
		ObjectMeta: metav1.ObjectMeta{
			Name: "run1", Namespace: "default",
			Finalizers:        []string{"kezio.kojuro.date/test-hold"},
			DeletionTimestamp: &metav1.Time{Time: metav1.Now().Time},
		},
	}
	c := newProgressTestClient(t, machine, run)
	decide := NewAbortDecider(c)

	if !decide(context.Background(), "m1", "run1", "uid1") {
		t.Fatal("decide() = false, want true when the current DeployRun has a deletionTimestamp")
	}
}

func TestNewAbortDecider_LiveCurrentRunContinues(t *testing.T) {
	machine := &keziov1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: "default"},
		Status:     keziov1alpha3.MachineStatus{CurrentRunRef: &keziov1alpha3.NameRef{Name: "run1"}},
	}
	run := &keziov1alpha3.DeployRun{ObjectMeta: metav1.ObjectMeta{Name: "run1", Namespace: "default"}}
	c := newProgressTestClient(t, machine, run)
	decide := NewAbortDecider(c)

	if decide(context.Background(), "m1", "run1", "uid1") {
		t.Fatal("decide() = true, want false for a live current run")
	}
}
