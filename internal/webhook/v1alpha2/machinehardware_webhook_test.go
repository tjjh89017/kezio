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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

// MachineHardware's validator is a deliberate no-op (see
// machinehardware_webhook.go), so this suite only proves the webhook is
// registered and serves admission requests.
var _ = Describe("MachineHardware Webhook", func() {
	Context("admission round-trip through the webhook server", func() {
		It("admits a valid MachineHardware", func() {
			created := &keziov1alpha2.MachineHardware{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "machinehardware-admission-roundtrip",
					Namespace: "default",
				},
			}
			Expect(k8sClient.Create(ctx, created)).To(Succeed())
			Expect(k8sClient.Delete(ctx, created)).To(Succeed())
		})
	})
})
