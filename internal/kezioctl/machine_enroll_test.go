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
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

func TestMachineEnroll_CreatesTheMachine(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(Scheme).Build()

	machine, err := MachineEnroll(context.Background(), c, MachineEnrollOptions{
		Name:                 "node-1",
		Namespace:            "default",
		BMCAddress:           "redfish://10.0.0.1",
		BMCCredentialsSecret: "node-1-bmc",
		BootMACAddress:       "aa:bb:cc:dd:ee:01",
		SubnetName:           "provisioning",
	})
	if err != nil {
		t.Fatalf("MachineEnroll() error = %v", err)
	}
	if machine.Spec.BMC.Address != "redfish://10.0.0.1" {
		t.Errorf("BMC.Address = %q, want redfish://10.0.0.1", machine.Spec.BMC.Address)
	}
	if machine.Spec.BMC.CredentialsSecretRef.Name != "node-1-bmc" {
		t.Errorf("CredentialsSecretRef.Name = %q, want node-1-bmc", machine.Spec.BMC.CredentialsSecretRef.Name)
	}
	if machine.Spec.SubnetRef.Name != "provisioning" {
		t.Errorf("SubnetRef.Name = %q, want provisioning", machine.Spec.SubnetRef.Name)
	}

	stored := &keziov1alpha3.Machine{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "node-1"}, stored); err != nil {
		t.Fatalf("get created Machine: %v", err)
	}
	if stored.Spec.BootMACAddress != "aa:bb:cc:dd:ee:01" {
		t.Errorf("stored BootMACAddress = %q, want aa:bb:cc:dd:ee:01", stored.Spec.BootMACAddress)
	}
}

func TestMachineEnroll_AlreadyExists(t *testing.T) {
	existing := &keziov1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "dup", Namespace: "default"}}
	c := fake.NewClientBuilder().WithScheme(Scheme).WithObjects(existing).Build()

	_, err := MachineEnroll(context.Background(), c, MachineEnrollOptions{
		Name:                 "dup",
		Namespace:            "default",
		BMCAddress:           "redfish://10.0.0.1",
		BMCCredentialsSecret: "creds",
		SubnetName:           "provisioning",
	})
	if err == nil {
		t.Fatal("expected an error when the Machine already exists")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error = %q, want it to mention the Machine already exists", err.Error())
	}
}

// TestMachineEnroll_SurfacesWebhookRejectionReadably simulates the Machine
// webhook's real rejection of a spec.subnetRef naming a Subnet with no
// boot half (see validateSubnetRef in
// internal/webhook/v1alpha3/machine_webhook.go) by having the fake
// client's Create return exactly that message, the way a real API server
// would after webhook admission. kezioctl talks to a real apiserver in
// production - this asserts MachineEnroll passes that rejection through
// unmodified rather than swallowing or reformatting it, without needing an
// envtest webhook server for a CLI-level test.
func TestMachineEnroll_SurfacesWebhookRejectionReadably(t *testing.T) {
	const webhookMessage = `admission webhook "vmachine-v1alpha3.kb.io" denied the request: ` +
		`spec.subnetRef names Subnet "data-plane-only", which has no boot half ` +
		`(bootdServerIP/bootdNetworkRef/dhcp): a Machine cannot PXE-boot from a data-plane-only Subnet`

	c := fake.NewClientBuilder().WithScheme(Scheme).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*keziov1alpha3.Machine); ok {
				return errors.New(webhookMessage)
			}
			return c.Create(ctx, obj, opts...)
		},
	}).Build()

	_, err := MachineEnroll(context.Background(), c, MachineEnrollOptions{
		Name:                 "node-1",
		Namespace:            "default",
		BMCAddress:           "redfish://10.0.0.1",
		BMCCredentialsSecret: "creds",
		SubnetName:           "data-plane-only",
	})
	if err == nil {
		t.Fatal("expected an error for a rejected Machine")
	}
	if !strings.Contains(err.Error(), "no boot half") {
		t.Errorf("error = %q, want it to contain the webhook's own message", err.Error())
	}
	if strings.Contains(err.Error(), "already exists") {
		t.Errorf("error = %q, must not be misclassified as already-exists", err.Error())
	}
}
