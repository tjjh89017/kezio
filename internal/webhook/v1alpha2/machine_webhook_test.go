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

package v1alpha2

import (
	"context"
	"net/url"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/bmc"
)

// webhookTestDriver is a stub bmc.Driver registered under the
// "webhooktest" scheme purely so the webhook's scheme check has something
// to recognize; nothing in this suite ever calls it.
func webhookTestDriver(context.Context, *url.URL, bmc.Credentials, bmc.Options) (bmc.BMC, error) {
	return nil, nil
}

const unregisteredSchemeAddress = "unregistered-scheme://10.0.0.1"

var _ = Describe("Machine Webhook", func() {
	var (
		obj       *keziov1alpha2.Machine
		oldObj    *keziov1alpha2.Machine
		validator MachineCustomValidator
	)

	BeforeEach(func() {
		obj = &keziov1alpha2.Machine{
			Spec: keziov1alpha2.MachineSpec{
				BMC: keziov1alpha2.MachineBMC{
					Address:              "webhooktest://10.0.0.1",
					CredentialsSecretRef: keziov1alpha2.SecretReference{Name: "bmc-creds"},
				},
				BootMACAddress: "aa:bb:cc:dd:ee:01",
				SubnetRef:      keziov1alpha2.NameRef{Name: "subnet"},
			},
		}
		oldObj = obj.DeepCopy()
		validator = MachineCustomValidator{}
		Expect(validator).NotTo(BeNil(), "Expected validator to be initialized")
		Expect(oldObj).NotTo(BeNil(), "Expected oldObj to be initialized")
		Expect(obj).NotTo(BeNil(), "Expected obj to be initialized")

		if !bmc.IsSchemeRegistered("webhooktest") {
			bmc.Register("webhooktest", webhookTestDriver)
		}
	})

	Context("spec.bmc.address scheme", func() {
		It("admits an address whose scheme is registered", func() {
			Expect(validator.ValidateCreate(ctx, obj)).Error().NotTo(HaveOccurred())
		})

		It("denies an address with no scheme", func() {
			obj.Spec.BMC.Address = "10.0.0.1"
			Expect(validator.ValidateCreate(ctx, obj)).Error().To(HaveOccurred())
		})

		It("denies an address whose scheme has no registered driver", func() {
			obj.Spec.BMC.Address = unregisteredSchemeAddress
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("unregistered-scheme"))
		})

		It("denies an unregistered scheme without echoing embedded credentials", func() {
			obj.Spec.BMC.Address = "unregistered-scheme://user:s3cr3t@10.0.0.1"
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).NotTo(ContainSubstring("s3cr3t"))
		})

		It("checks the scheme again on update", func() {
			obj.Spec.BMC.Address = unregisteredSchemeAddress
			Expect(validator.ValidateUpdate(ctx, oldObj, obj)).Error().To(HaveOccurred())
		})

		It("skips validation for an update racing object deletion", func() {
			obj.Spec.BMC.Address = unregisteredSchemeAddress
			now := metav1.Now()
			obj.DeletionTimestamp = &now
			obj.Finalizers = []string{"kezio.kojuro.date/test"}
			Expect(validator.ValidateUpdate(ctx, oldObj, obj)).Error().NotTo(HaveOccurred())
		})
	})

	Context("annotation \"kezio.kojuro.date/inspect-disable\"", func() {
		It("admits a Machine with the annotation and a boot MAC address", func() {
			obj.Annotations = map[string]string{AnnotationInspectDisable: "true"}
			Expect(validator.ValidateCreate(ctx, obj)).Error().NotTo(HaveOccurred())
		})

		It("denies the annotation set to a value other than \"true\"", func() {
			obj.Annotations = map[string]string{AnnotationInspectDisable: "yes"}
			Expect(validator.ValidateCreate(ctx, obj)).Error().To(HaveOccurred())
		})

		It("denies the annotation without a boot MAC address", func() {
			obj.Annotations = map[string]string{AnnotationInspectDisable: "true"}
			obj.Spec.BootMACAddress = ""
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("bootMACAddress"))
		})

		It("admits a missing boot MAC address when the annotation is absent", func() {
			// api/v1alpha2 requires bootMACAddress unconditionally at the CRD
			// schema level today; this direct validator call bypasses that
			// schema layer to exercise the webhook's own conditional check.
			obj.Spec.BootMACAddress = ""
			Expect(validator.ValidateCreate(ctx, obj)).Error().NotTo(HaveOccurred())
		})
	})
})
