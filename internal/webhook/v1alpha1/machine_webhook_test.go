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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
)

var _ = Describe("Machine Webhook", func() {
	var (
		obj       *keziov1alpha1.Machine
		oldObj    *keziov1alpha1.Machine
		validator MachineCustomValidator
		defaulter MachineCustomDefaulter
	)

	validSpec := func() keziov1alpha1.MachineSpec {
		return keziov1alpha1.MachineSpec{
			BMC: keziov1alpha1.MachineBMC{
				Address:              "redfish://198.51.100.10/redfish/v1/Systems/1",
				CredentialsSecretRef: keziov1alpha1.SecretReference{Name: "bmc-creds"},
			},
			BootMACAddress: "aa:bb:cc:dd:ee:01",
			ImageRef:       &keziov1alpha1.NameRef{Name: "os-image"},
			TargetDisk:     &keziov1alpha1.TargetDiskHints{SerialNumber: "OS-DISK-1"},
			DataImages: []keziov1alpha1.MachineDataImage{
				{
					ImageRef:   keziov1alpha1.NameRef{Name: "data-image-a"},
					TargetDisk: &keziov1alpha1.TargetDiskHints{SerialNumber: "DATA-DISK-1"},
				},
				{
					ImageRef:   keziov1alpha1.NameRef{Name: "data-image-b"},
					TargetDisk: &keziov1alpha1.TargetDiskHints{SerialNumber: "DATA-DISK-2"},
				},
			},
		}
	}

	BeforeEach(func() {
		obj = &keziov1alpha1.Machine{Spec: validSpec()}
		oldObj = &keziov1alpha1.Machine{Spec: validSpec()}
		validator = MachineCustomValidator{}
		defaulter = MachineCustomDefaulter{}
		Expect(defaulter).NotTo(BeNil())
	})

	Context("When creating or updating Machine under Validating Webhook", func() {
		It("Should admit a fully valid spec on create", func() {
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Should admit a fully valid spec on update", func() {
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Should admit a spec with no targetDisk hints anywhere", func() {
			obj.Spec.TargetDisk = nil
			obj.Spec.DataImages[0].TargetDisk = nil
			obj.Spec.DataImages[1].TargetDisk = nil
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Should deny creation when the OS image and a dataImages entry target byte-identical disk hints", func() {
			obj.Spec.DataImages[0].TargetDisk = &keziov1alpha1.TargetDiskHints{SerialNumber: "OS-DISK-1"}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("byte-identical targetDisk hints"))
		})

		It("Should deny creation when two dataImages entries target byte-identical disk hints", func() {
			obj.Spec.DataImages[1].TargetDisk = obj.Spec.DataImages[0].TargetDisk
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("byte-identical targetDisk hints"))
		})

		It("Should deny update when two dataImages entries target byte-identical disk hints", func() {
			obj.Spec.DataImages[1].TargetDisk = obj.Spec.DataImages[0].TargetDisk
			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("byte-identical targetDisk hints"))
		})

		It("Should deny creation when two dataImages entries reference the same Image with identical (unset) hints", func() {
			obj.Spec.DataImages[0].TargetDisk = nil
			obj.Spec.DataImages[1].ImageRef = keziov1alpha1.NameRef{Name: "data-image-a"}
			obj.Spec.DataImages[1].TargetDisk = nil
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("both reference Image"))
		})

		It("Should admit two dataImages entries referencing the same Image with different hints", func() {
			obj.Spec.DataImages[1].ImageRef = keziov1alpha1.NameRef{Name: "data-image-a"}
			// TargetDisk hints already differ (DATA-DISK-1 vs DATA-DISK-2).
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Should deny a spec with an empty bmc block", func() {
			obj.Spec.BMC = keziov1alpha1.MachineBMC{}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec.bmc.address is required"))
		})

		It("Should deny a spec with an empty bmc address", func() {
			obj.Spec.BMC.Address = ""
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec.bmc.address is required"))
			Expect(err.Error()).NotTo(ContainSubstring("bmc-creds"))
		})

		It("Should admit an ipmi:// bmc address", func() {
			obj.Spec.BMC.Address = "ipmi://198.51.100.10"
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Should deny a bmc address with an unregistered scheme", func() {
			obj.Spec.BMC.Address = "http://198.51.100.10"
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("no registered BMC driver"))
		})

		It("Should deny an unparseable bmc address", func() {
			obj.Spec.BMC.Address = "://not a url"
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not a valid URL"))
		})

		It("Should deny a bmc address with no scheme", func() {
			obj.Spec.BMC.Address = "198.51.100.10/redfish/v1/Systems/1"
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("has no scheme"))
		})

		It("Should deny a bmc address set without a credentialsSecretRef", func() {
			obj.Spec.BMC.CredentialsSecretRef = keziov1alpha1.SecretReference{}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("credentialsSecretRef is required"))
		})

		It("Should redact userinfo embedded in the bmc address from the rejection message", func() {
			obj.Spec.BMC.Address = "http://admin:hunter2@198.51.100.10"
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).NotTo(ContainSubstring("hunter2"))
			Expect(err.Error()).To(ContainSubstring("admin:xxxxx@198.51.100.10"))
		})

		It("Should admit a bmc-insecure-skip-verify annotation value of \"true\"", func() {
			obj.Annotations = map[string]string{keziov1alpha1.AnnotationBMCInsecureSkipVerify: "true"}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Should admit a bmc-insecure-skip-verify annotation value of \"false\"", func() {
			obj.Annotations = map[string]string{keziov1alpha1.AnnotationBMCInsecureSkipVerify: "false"}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).NotTo(HaveOccurred())
		})

		It("Should deny a non-exact bmc-insecure-skip-verify annotation value", func() {
			for _, value := range []string{"1", "0", "T", "TRUE", "True", "t", "F", "FALSE", "False", "f", "maybe", "yes", ""} {
				obj.Annotations = map[string]string{keziov1alpha1.AnnotationBMCInsecureSkipVerify: value}
				_, err := validator.ValidateCreate(ctx, obj)
				Expect(err).To(HaveOccurred(), "value %q should be rejected", value)
				Expect(err.Error()).To(ContainSubstring("expected exactly"), "value %q should be rejected", value)
			}
		})

		It("Should skip validation on update when the object is being deleted", func() {
			now := metav1.Now()
			obj.DeletionTimestamp = &now
			obj.Finalizers = []string{"example.com/finalizer"}
			obj.Spec.DataImages[1].TargetDisk = obj.Spec.DataImages[0].TargetDisk

			_, err := validator.ValidateUpdate(ctx, oldObj, obj)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("When creating Machine under Defaulting Webhook", func() {
		It("Should not change the spec", func() {
			before := obj.Spec
			Expect(defaulter.Default(ctx, obj)).To(Succeed())
			Expect(obj.Spec).To(Equal(before))
		})
	})
})
