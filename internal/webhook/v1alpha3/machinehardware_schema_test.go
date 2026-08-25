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

package v1alpha3

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

// These specs exercise the CRD schema (kubebuilder markers) through the
// real envtest apiserver, not the Go type definitions: OpenAPI validation
// only runs there.
var _ = Describe("MachineHardware CRD schema", func() {
	var hardwareCount int

	newMachineHardware := func() *keziov1alpha3.MachineHardware {
		hardwareCount++
		return &keziov1alpha3.MachineHardware{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("schema-test-hw-%d", hardwareCount),
				Namespace: "default",
			},
		}
	}

	It("admits an empty MachineHardware", func() {
		hw := newMachineHardware()
		Expect(k8sClient.Create(ctx, hw)).To(Succeed())
	})

	It("carries no status subresource", func() {
		hw := newMachineHardware()
		Expect(k8sClient.Create(ctx, hw)).To(Succeed())
		Expect(k8sClient.Status().Update(ctx, hw)).To(HaveOccurred())
	})

	It("admits a MachineHardware with a full disk and nic inventory", func() {
		hw := newMachineHardware()
		hw.Spec.CPUCount = 8
		hw.Spec.MemoryBytes = 17179869184
		hw.Spec.Disks = []keziov1alpha3.MachineHardwareDisk{{
			DeviceName:   "/dev/nvme0n1",
			SerialNumber: "S1",
			WWN:          "wwn-0x1",
			Model:        "model",
			Vendor:       "vendor",
			SizeBytes:    512110190592,
			HCTL:         "0:0:0:0",
		}}
		hw.Spec.Nics = []keziov1alpha3.MachineHardwareNIC{{
			Name:       "eth0",
			MACAddress: "aa:bb:cc:dd:ee:01",
		}}
		Expect(k8sClient.Create(ctx, hw)).To(Succeed())
	})

	It("rejects a disk with no deviceName", func() {
		hw := newMachineHardware()
		hw.Spec.Disks = []keziov1alpha3.MachineHardwareDisk{{}}
		Expect(k8sClient.Create(ctx, hw)).To(HaveOccurred())
	})

	It("rejects a nic MAC address that is not a MAC address", func() {
		hw := newMachineHardware()
		hw.Spec.Nics = []keziov1alpha3.MachineHardwareNIC{{
			Name:       "eth0",
			MACAddress: "not-a-mac",
		}}
		Expect(k8sClient.Create(ctx, hw)).To(HaveOccurred())
	})

	It("rejects a disk HCTL that does not match host:channel:target:lun", func() {
		hw := newMachineHardware()
		hw.Spec.Disks = []keziov1alpha3.MachineHardwareDisk{{
			DeviceName: "/dev/sda",
			HCTL:       "not-hctl",
		}}
		Expect(k8sClient.Create(ctx, hw)).To(HaveOccurred())
	})

	It("rejects more than 64 disks", func() {
		hw := newMachineHardware()
		for i := 0; i < 65; i++ {
			hw.Spec.Disks = append(hw.Spec.Disks, keziov1alpha3.MachineHardwareDisk{
				DeviceName: fmt.Sprintf("/dev/disk%d", i),
			})
		}
		Expect(k8sClient.Create(ctx, hw)).To(HaveOccurred())
	})
})
