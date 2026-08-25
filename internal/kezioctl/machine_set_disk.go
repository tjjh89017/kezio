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
	// TargetDisk replaces the Machine's spec.targetDisk wholesale - this
	// command sets the hint set, it does not merge into whatever was
	// there before.
	TargetDisk keziov1alpha3.TargetDiskHints
}

// MachineSetDisk implements `kezioctl machine set-disk`: it replaces the
// named Machine's spec.targetDisk with opts.TargetDisk. The controller
// matches these hints against the agent-reported disk inventory at deploy
// time and requires exactly one match before any write; this command does
// not resolve or validate hints against real hardware itself.
func MachineSetDisk(ctx context.Context, c client.Client, opts MachineSetDiskOptions) error {
	machine := &keziov1alpha3.Machine{}
	key := client.ObjectKey{Namespace: opts.Namespace, Name: opts.Name}
	if err := c.Get(ctx, key, machine); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("machine %s/%s not found", opts.Namespace, opts.Name)
		}
		return fmt.Errorf("get Machine %s/%s: %w", opts.Namespace, opts.Name, err)
	}

	hints := opts.TargetDisk
	machine.Spec.TargetDisk = &hints

	if err := c.Update(ctx, machine); err != nil {
		return fmt.Errorf("update Machine %s/%s: %w", opts.Namespace, opts.Name, err)
	}
	return nil
}
