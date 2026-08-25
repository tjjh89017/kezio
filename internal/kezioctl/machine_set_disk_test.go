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

func TestMachineSetDisk_SetsTargetDisk(t *testing.T) {
	machine := &keziov1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "node-1", Namespace: "default"}}
	c := fake.NewClientBuilder().WithScheme(Scheme).WithObjects(machine).Build()

	minSize := int64(100)
	err := MachineSetDisk(context.Background(), c, MachineSetDiskOptions{
		Name:      "node-1",
		Namespace: "default",
		TargetDisk: keziov1alpha3.TargetDiskHints{
			SerialNumber:     "SN123",
			MinSizeGigabytes: &minSize,
		},
	})
	if err != nil {
		t.Fatalf("MachineSetDisk() error = %v", err)
	}

	stored := &keziov1alpha3.Machine{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "node-1"}, stored); err != nil {
		t.Fatalf("get Machine: %v", err)
	}
	if stored.Spec.TargetDisk == nil {
		t.Fatal("Spec.TargetDisk is nil, want it set")
	}
	if stored.Spec.TargetDisk.SerialNumber != "SN123" {
		t.Errorf("TargetDisk.SerialNumber = %q, want SN123", stored.Spec.TargetDisk.SerialNumber)
	}
	if stored.Spec.TargetDisk.MinSizeGigabytes == nil || *stored.Spec.TargetDisk.MinSizeGigabytes != 100 {
		t.Errorf("TargetDisk.MinSizeGigabytes = %v, want 100", stored.Spec.TargetDisk.MinSizeGigabytes)
	}
}

func TestMachineSetDisk_ReplacesExistingHints(t *testing.T) {
	machine := &keziov1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1", Namespace: "default"},
		Spec: keziov1alpha3.MachineSpec{
			TargetDisk: &keziov1alpha3.TargetDiskHints{SerialNumber: "OLD"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(Scheme).WithObjects(machine).Build()

	err := MachineSetDisk(context.Background(), c, MachineSetDiskOptions{
		Name:       "node-1",
		Namespace:  "default",
		TargetDisk: keziov1alpha3.TargetDiskHints{WWN: "NEW-WWN"},
	})
	if err != nil {
		t.Fatalf("MachineSetDisk() error = %v", err)
	}

	stored := &keziov1alpha3.Machine{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "node-1"}, stored); err != nil {
		t.Fatalf("get Machine: %v", err)
	}
	if stored.Spec.TargetDisk.SerialNumber != "" {
		t.Errorf("TargetDisk.SerialNumber = %q, want cleared by the replacing set-disk call", stored.Spec.TargetDisk.SerialNumber)
	}
	if stored.Spec.TargetDisk.WWN != "NEW-WWN" {
		t.Errorf("TargetDisk.WWN = %q, want NEW-WWN", stored.Spec.TargetDisk.WWN)
	}
}

func TestMachineSetDisk_MachineNotFound(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(Scheme).Build()

	err := MachineSetDisk(context.Background(), c, MachineSetDiskOptions{
		Name:       "missing",
		Namespace:  "default",
		TargetDisk: keziov1alpha3.TargetDiskHints{WWN: "x"},
	})
	if err == nil {
		t.Fatal("expected an error for a missing Machine")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want it to mention not found", err.Error())
	}
}
