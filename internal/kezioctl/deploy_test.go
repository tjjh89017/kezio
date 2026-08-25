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
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

func TestDeploy_SetsImageRef(t *testing.T) {
	machine := &keziov1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "node-1", Namespace: "default"}}
	c := fake.NewClientBuilder().WithScheme(Scheme).WithObjects(machine).Build()

	err := Deploy(context.Background(), c, DeployOptions{
		MachineName: "node-1",
		Namespace:   "default",
		ImageName:   "ubuntu-2404",
	})
	if err != nil {
		t.Fatalf("Deploy() error = %v", err)
	}

	stored := &keziov1alpha3.MachineClaim{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "node-1-claim"}, stored); err != nil {
		t.Fatalf("get MachineClaim: %v", err)
	}
	if stored.Spec.ImageRef == nil || stored.Spec.ImageRef.Name != "ubuntu-2404" {
		t.Errorf("Spec.ImageRef = %+v, want name ubuntu-2404", stored.Spec.ImageRef)
	}
	if stored.Spec.MachineName != "node-1" {
		t.Errorf("Spec.MachineName = %q, want node-1", stored.Spec.MachineName)
	}
}

func TestDeploy_SetsDataImagesPostHooksAndParams(t *testing.T) {
	machine := &keziov1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "node-1", Namespace: "default"}}
	c := fake.NewClientBuilder().WithScheme(Scheme).WithObjects(machine).Build()

	err := Deploy(context.Background(), c, DeployOptions{
		MachineName:  "node-1",
		Namespace:    "default",
		DataImages:   []keziov1alpha3.MachineDataImage{{ImageRef: keziov1alpha3.NameRef{Name: "scratch"}}},
		PostHookRefs: []keziov1alpha3.NameRef{{Name: "finalize"}},
		Params:       map[string]string{"hostname": "node-1"},
	})
	if err != nil {
		t.Fatalf("Deploy() error = %v", err)
	}

	stored := &keziov1alpha3.MachineClaim{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "node-1-claim"}, stored); err != nil {
		t.Fatalf("get MachineClaim: %v", err)
	}
	if len(stored.Spec.DataImages) != 1 || stored.Spec.DataImages[0].ImageRef.Name != "scratch" {
		t.Errorf("Spec.DataImages = %+v, want one entry named scratch", stored.Spec.DataImages)
	}
	if len(stored.Spec.PostHookRefs) != 1 || stored.Spec.PostHookRefs[0].Name != "finalize" {
		t.Errorf("Spec.PostHookRefs = %+v, want one entry named finalize", stored.Spec.PostHookRefs)
	}
	if stored.Spec.Params == nil || !strings.Contains(string(stored.Spec.Params.Raw), `"hostname":"node-1"`) {
		t.Errorf("Spec.Params = %v, want it to carry hostname=node-1", stored.Spec.Params)
	}
}

func TestDeploy_LeavesUnsetFieldsUntouched(t *testing.T) {
	machine := &keziov1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "node-1", Namespace: "default"}}
	claim := &keziov1alpha3.MachineClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1-claim", Namespace: "default"},
		Spec: keziov1alpha3.MachineClaimSpec{
			MachineName: "node-1",
			DataImages:  []keziov1alpha3.MachineDataImage{{ImageRef: keziov1alpha3.NameRef{Name: "kept"}}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(Scheme).WithObjects(machine, claim).Build()

	// Only --image was given: DataImages/PostHookRefs/Params were never
	// set on DeployOptions (all nil), and must survive unchanged.
	if err := Deploy(context.Background(), c, DeployOptions{
		MachineName: "node-1",
		Namespace:   "default",
		ImageName:   "ubuntu-2404",
	}); err != nil {
		t.Fatalf("Deploy() error = %v", err)
	}

	stored := &keziov1alpha3.MachineClaim{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "node-1-claim"}, stored); err != nil {
		t.Fatalf("get MachineClaim: %v", err)
	}
	if len(stored.Spec.DataImages) != 1 || stored.Spec.DataImages[0].ImageRef.Name != "kept" {
		t.Errorf("Spec.DataImages = %+v, want the pre-existing entry left untouched", stored.Spec.DataImages)
	}
}

func TestDeploy_MachineNotFound(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(Scheme).Build()

	err := Deploy(context.Background(), c, DeployOptions{
		MachineName: "missing",
		Namespace:   "default",
		ImageName:   "ubuntu-2404",
	})
	if err == nil {
		t.Fatal("expected an error for a missing Machine")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want it to mention not found", err.Error())
	}
}
