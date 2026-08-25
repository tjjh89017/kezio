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
	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

// MachineSetDiskOptions configures MachineSetDisk.
type MachineSetDiskOptions struct {
	Name      string
	Namespace string
	// TargetDisk replaces the target MachineClaim's spec.targetDisk
	// wholesale - this command sets the hint set, it does not merge into
	// whatever was there before.
	TargetDisk keziov1alpha3.TargetDiskHints
}

// MachineSetDisk implements `kezioctl machine set-disk`: it replaces
// spec.targetDisk on the MachineClaim named after the target Machine
// (Name + "-claim") with opts.TargetDisk. The controller matches these
// hints against the agent-reported disk inventory at deploy time and
// requires exactly one match before any write; this command does not
// resolve or validate hints against real hardware itself.
func MachineSetDisk(ctx context.Context, c client.Client, opts MachineSetDiskOptions) error {
	machine := &keziov1alpha3.Machine{}
	machineKey := client.ObjectKey{Namespace: opts.Namespace, Name: opts.Name}
	if err := c.Get(ctx, machineKey, machine); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("machine %s/%s not found", opts.Namespace, opts.Name)
		}
		return fmt.Errorf("get Machine %s/%s: %w", opts.Namespace, opts.Name, err)
	}

	claimName := opts.Name + "-claim"
	claim := &keziov1alpha3.MachineClaim{}
	claimKey := client.ObjectKey{Namespace: opts.Namespace, Name: claimName}
	if err := c.Get(ctx, claimKey, claim); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("machineclaim %s/%s not found", opts.Namespace, claimName)
		}
		return fmt.Errorf("get MachineClaim %s/%s: %w", opts.Namespace, claimName, err)
	}

	hints := opts.TargetDisk
	claim.Spec.TargetDisk = &hints

	if err := c.Update(ctx, claim); err != nil {
		return fmt.Errorf("update MachineClaim %s/%s: %w", opts.Namespace, claimName, err)
	}
	return nil
}
