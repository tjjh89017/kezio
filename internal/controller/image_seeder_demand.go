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

package controller

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

// imageSeedDemandBySite groups image's current seed-demand Machines by
// their derived Site (seederDemandBySite). The demand source is unchanged
// from the per-content design - a live Machine naming image, or an active
// DeployRun whose resolved snapshot names it - only the grouping changes:
// by Site instead of collapsed into one namespace-wide boolean. The
// second return value names every Site among those Machines that
// declares no seederSubnetRef at all - see seederDemandBySite's doc
// comment.
func (r *ImageReconciler) imageSeedDemandBySite(ctx context.Context, image *keziov1alpha3.Image) (map[string]*seederSiteDemand, []string, error) {
	machines, err := r.demandMachinesForImage(ctx, image)
	if err != nil {
		return nil, nil, err
	}
	demand, noSeederSites := seederDemandBySite(ctx, r.Client, machines)
	return demand, noSeederSites, nil
}

// demandMachinesForImage returns the deduplicated set of Machines that
// currently demand image: every live Machine machineImageRefIndex names
// (spec.imageRef/dataImages), plus - for the edge case where an active
// DeployRun's resolved snapshot names image but the Machine's own current
// spec has since moved on - the Machine each such active DeployRun names.
// Mirrors PartitionContentReconciler's own resolveSeedDemand/
// activeDeployRunsReferencing shape, one level up (Image-direct rather
// than through a referenced PartitionContent).
func (r *ImageReconciler) demandMachinesForImage(ctx context.Context, image *keziov1alpha3.Image) (map[client.ObjectKey]*keziov1alpha3.Machine, error) {
	seen := make(map[client.ObjectKey]*keziov1alpha3.Machine)

	live, err := machinesReferencingImage(ctx, r.Client, client.ObjectKey{Namespace: image.Namespace, Name: image.Name})
	if err != nil {
		return nil, fmt.Errorf("image %q: listing machines referencing it: %w", image.Name, err)
	}
	for i := range live {
		seen[client.ObjectKeyFromObject(&live[i])] = &live[i]
	}

	var runs keziov1alpha3.DeployRunList
	if err := r.List(ctx, &runs, client.InNamespace(image.Namespace)); err != nil {
		return nil, fmt.Errorf("image %q: listing deploy runs: %w", image.Name, err)
	}
	for i := range runs.Items {
		run := &runs.Items[i]
		if !isDeployRunActive(run) || !deployRunNamesImage(run, image) {
			continue
		}
		machineNS := run.Spec.MachineRef.Namespace
		if machineNS == "" {
			machineNS = run.Namespace
		}
		key := client.ObjectKey{Namespace: machineNS, Name: run.Spec.MachineRef.Name}
		if _, ok := seen[key]; ok {
			continue
		}
		var m keziov1alpha3.Machine
		if err := r.Get(ctx, key, &m); err != nil {
			// Deleted/unresolvable machine: no Site to attribute demand
			// to, and nothing this reconcile can act on.
			continue
		}
		seen[key] = &m
	}

	return seen, nil
}

// deployRunNamesImage reports whether run's resolved snapshot
// (deployRunImageNames) names image.
func deployRunNamesImage(run *keziov1alpha3.DeployRun, image *keziov1alpha3.Image) bool {
	for _, key := range deployRunImageNames(run) {
		if key.Namespace == image.Namespace && key.Name == image.Name {
			return true
		}
	}
	return false
}
