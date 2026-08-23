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
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

// reconcileSeeder reflects pc's real per-Site seeder placement into
// status.seeders[] and PartitionContentConditionSeederDegraded. It only
// ever runs once pc is Ready (see onChange). Unlike its earlier
// per-content-Deployment shape, this never creates, patches, or deletes
// anything: the seeder Deployment for a content now lives per (Image,
// Site), owned by whichever Image references this content
// (ImageReconciler.reconcileImageSeeder) - this reconciler only reads
// what already exists there and reflects it.
func (r *PartitionContentReconciler) reconcileSeeder(ctx context.Context, pc *keziov1alpha2.PartitionContent) (ctrl.Result, error) {
	images, err := r.imagesReferencing(ctx, pc)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("partitioncontent %q: listing referencing images: %w", pc.Name, err)
	}

	demand, err := r.resolveSeedDemand(ctx, pc)
	if err != nil {
		return ctrl.Result{}, err
	}

	sites, err := r.collectSeederSites(ctx, pc, images)
	if err != nil {
		return ctrl.Result{}, err
	}

	return r.recordSeederStatus(ctx, pc, sites, demand)
}

// collectSeederSites derives pc's status.seeders[]: one real entry per
// Site that has an available seeder Deployment among the Images
// referencing pc, with MachineCount taken from the same per-Site demand
// count reconcileImageSeeder itself groups by (seederDemandBySite) -
// deduplicated across every referencing Image, so a Machine referencing
// two Images that both reference pc is not counted twice.
func (r *PartitionContentReconciler) collectSeederSites(ctx context.Context, pc *keziov1alpha2.PartitionContent, images []keziov1alpha2.Image) ([]keziov1alpha2.PartitionContentSeederSite, error) {
	machines, err := r.demandMachinesForContent(ctx, pc, images)
	if err != nil {
		return nil, err
	}
	demand, _ := seederDemandBySite(ctx, r.Client, machines)

	available := make(map[string]bool)
	for i := range images {
		deployments, err := listImageSeederDeployments(ctx, r.Client, &images[i])
		if err != nil {
			return nil, fmt.Errorf("partitioncontent %q: listing seeder deployments for image %q: %w", pc.Name, images[i].Name, err)
		}
		for site, dep := range deployments {
			if dep.DeletionTimestamp.IsZero() && dep.Status.AvailableReplicas > 0 {
				available[site] = true
			}
		}
	}

	var sites []keziov1alpha2.PartitionContentSeederSite
	for site := range available {
		count := 0
		if d, ok := demand[site]; ok {
			count = d.count
		}
		sites = append(sites, keziov1alpha2.PartitionContentSeederSite{Site: site, MachineCount: int32(count)})
	}
	sort.Slice(sites, func(i, j int) bool { return sites[i].Site < sites[j].Site })
	return sites, nil
}

// demandMachinesForContent returns the deduplicated set of Machines that
// currently demand pc's content: every live Machine referencing any of
// images (machinesReferencingImage), plus - for the edge case where an
// active DeployRun's resolved snapshot names an Image that has since been
// deleted - the Machine each such active DeployRun names
// (activeDeployRunsReferencing already resolves that edge case for the
// deletion-blocking finalizer walk; this reuses it).
func (r *PartitionContentReconciler) demandMachinesForContent(ctx context.Context, pc *keziov1alpha2.PartitionContent, images []keziov1alpha2.Image) (map[client.ObjectKey]*keziov1alpha2.Machine, error) {
	seen := make(map[client.ObjectKey]*keziov1alpha2.Machine)

	for i := range images {
		live, err := machinesReferencingImage(ctx, r.Client, client.ObjectKey{Namespace: images[i].Namespace, Name: images[i].Name})
		if err != nil {
			return nil, fmt.Errorf("partitioncontent %q: listing machines referencing image %q: %w", pc.Name, images[i].Name, err)
		}
		for j := range live {
			seen[client.ObjectKeyFromObject(&live[j])] = &live[j]
		}
	}

	runs, err := r.activeDeployRunsReferencing(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("partitioncontent %q: listing referencing deploy runs: %w", pc.Name, err)
	}
	for i := range runs {
		run := &runs[i]
		ns := run.Spec.MachineRef.Namespace
		if ns == "" {
			ns = run.Namespace
		}
		key := client.ObjectKey{Namespace: ns, Name: run.Spec.MachineRef.Name}
		if _, ok := seen[key]; ok {
			continue
		}
		var m keziov1alpha2.Machine
		if err := r.Get(ctx, key, &m); err != nil {
			continue // deleted/unresolvable machine: no Site to attribute demand to
		}
		seen[key] = &m
	}

	return seen, nil
}

// recordSeederStatus writes sites into pc.Status.Seeders and sets
// PartitionContentConditionSeederDegraded from demand and sites together,
// then writes both through applyPartitionContentStatus.
//
// Seeders and SeederDegraded are deliberately independent: sites can
// still be non-empty while demand has just dropped (a Deployment still
// running out its grace period keeps counting), while SeederDegraded only
// ever reacts to demand asking for something no Site is currently
// providing - it is cleared entirely (not set False) when there is no
// demand, since "degraded" does not apply to something nobody asked for.
func (r *PartitionContentReconciler) recordSeederStatus(ctx context.Context, pc *keziov1alpha2.PartitionContent, sites []keziov1alpha2.PartitionContentSeederSite, demand bool) (ctrl.Result, error) {
	pc.Status.Seeders = sites
	available := len(sites) > 0

	switch {
	case !demand:
		meta.RemoveStatusCondition(&pc.Status.Conditions, keziov1alpha2.PartitionContentConditionSeederDegraded)
	case available:
		setPartitionContentSeederDegradedCondition(pc, metav1.ConditionFalse, "SeederAvailable",
			"at least one Site has a seeder deployment with an available replica")
	default:
		setPartitionContentSeederDegradedCondition(pc, metav1.ConditionTrue, "SeederUnavailable",
			"seeding is demanded but no Site has a seeder deployment with an available replica yet")
	}

	if err := r.applyPartitionContentStatus(ctx, pc); err != nil {
		return ctrl.Result{}, fmt.Errorf("partitioncontent %q: recording seeder status: %w", pc.Name, err)
	}
	return ctrl.Result{}, nil
}

// mapSeederDeploymentToPartitionContents maps a seeder Deployment event to
// a reconcile request per PartitionContent the Deployment's owning Image
// references: the Deployment is owned by an Image, not by any
// PartitionContent directly, so this is the indirection that lets a
// content's status.seeders[]/SeederDegraded catch up promptly when its
// seeder Deployment's availability changes.
func (r *PartitionContentReconciler) mapSeederDeploymentToPartitionContents(ctx context.Context, obj client.Object) []reconcile.Request {
	dep, ok := obj.(*appsv1.Deployment)
	if !ok || dep.Labels[partitionContentAppComponentLabel] != partitionContentSeederComponentValue {
		return nil
	}

	var imageName string
	for _, owner := range dep.OwnerReferences {
		if owner.Kind == "Image" {
			imageName = owner.Name
			break
		}
	}
	if imageName == "" {
		return nil
	}

	var image keziov1alpha2.Image
	if err := r.Get(ctx, client.ObjectKey{Namespace: dep.Namespace, Name: imageName}, &image); err != nil {
		return nil
	}

	names := imageContentRefNames(&image)
	requests := make([]reconcile.Request, 0, len(names))
	for _, name := range names {
		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKey{Namespace: image.Namespace, Name: name},
		})
	}
	return requests
}
