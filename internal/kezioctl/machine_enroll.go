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
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

// MachineEnrollOptions configures MachineEnroll. It corresponds directly
// to `kezioctl machine enroll`'s flags.
type MachineEnrollOptions struct {
	// Name is both the Kubernetes object name and the Machine's identity.
	Name string
	// Namespace is the namespace the Machine is created in.
	Namespace string
	// BMCAddress is the BMC endpoint URL (MachineSpec.BMC.Address).
	BMCAddress string
	// BMCCredentialsSecret names the Secret holding the BMC username and
	// password, in the same namespace as the Machine.
	BMCCredentialsSecret string
	// BootMACAddress is the MAC address of the NIC this machine network
	// boots from. Normally left empty and discovered by inspection.
	BootMACAddress string
	// SubnetName names the Subnet this machine network boots through.
	SubnetName string
	// SubnetNamespace is SubnetName's namespace, if different from the
	// Machine's own.
	SubnetNamespace string
}

// MachineEnroll implements `kezioctl machine enroll`: it creates the
// Machine CR from the given facts. It performs no admission checks of its
// own - the Machine webhook (MachineCustomValidator) rejects a
// spec.subnetRef naming a Subnet with no boot half, a spec.bmc.address
// whose scheme has no registered driver, and a few other rules - so this
// command does not duplicate any of that and simply returns whatever the
// API server rejects it with.
func MachineEnroll(ctx context.Context, c client.Client, opts MachineEnrollOptions) (*keziov1alpha2.Machine, error) {
	machine := &keziov1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      opts.Name,
			Namespace: opts.Namespace,
		},
		Spec: keziov1alpha2.MachineSpec{
			BMC: keziov1alpha2.MachineBMC{
				Address:              opts.BMCAddress,
				CredentialsSecretRef: keziov1alpha2.SecretReference{Name: opts.BMCCredentialsSecret},
			},
			BootMACAddress: opts.BootMACAddress,
			SubnetRef: keziov1alpha2.NameRef{
				Name:      opts.SubnetName,
				Namespace: opts.SubnetNamespace,
			},
		},
	}

	if err := c.Create(ctx, machine); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("machine %s/%s already exists", opts.Namespace, opts.Name)
		}
		return nil, fmt.Errorf("create Machine %s/%s: %w", opts.Namespace, opts.Name, err)
	}
	return machine, nil
}
