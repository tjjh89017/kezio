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
	"reflect"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
	"github.com/tjjh89017/kezio/internal/sitederive"
)

// seederSiteDemand is one Site's seed-demand count for one Image, plus
// the placement facts (sitederive.Resolution) to build or reflect a
// seeder Deployment there. Every Machine grouped into one seederSiteDemand
// resolved to the same Site, so they share one Resolution by construction
// - the first one observed is kept.
type seederSiteDemand struct {
	count      int
	resolution sitederive.Resolution
}

// machinesReferencingImage returns the live (not being deleted) Machines
// in image's own namespace whose bound MachineClaim's
// spec.imageRef/dataImages names image and whose deploy of it has not
// finished yet, via a client-side filter (resolveClaimIntent,
// claimImageRefs) rather than claimImageRefIndex: unlike
// PartitionContentReconciler's own resolveSeedDemand path, this is
// reachable from an ImageReconciler built directly against a plain,
// uncached client with no field indexer registered (see
// image_controller_test.go/image_ingest_test.go), so it must not depend
// on one being present. A Machine with no claimRef, or one whose claim
// cannot be resolved, contributes no demand: seeder placement is a
// property of a bound machine's Site, and an unbound machine names no
// intent at all.
//
// A Provisioned Machine's deploy has already finished, so it stops
// counting - see machineDeployPending. Every other state a bound claim
// can be observed in (Enrolling, Inspecting, Available, Provisioning)
// still counts: the deploy has not started yet, or is in flight.
func machinesReferencingImage(ctx context.Context, c client.Client, image client.ObjectKey) ([]keziov1alpha3.Machine, error) {
	var list keziov1alpha3.MachineList
	if err := c.List(ctx, &list, client.InNamespace(image.Namespace)); err != nil {
		return nil, err
	}
	live := make([]keziov1alpha3.Machine, 0, len(list.Items))
	for i := range list.Items {
		m := &list.Items[i]
		if !m.DeletionTimestamp.IsZero() {
			continue
		}
		claim, err := resolveClaimIntent(ctx, c, m)
		if err != nil {
			return nil, err
		}
		if claim == nil {
			continue
		}
		named := false
		for _, ref := range claimImageRefs(claim) {
			if ref == image {
				named = true
				break
			}
		}
		if !named {
			continue
		}
		if m.Status.State == keziov1alpha3.MachineStateProvisioned {
			pending, err := machineDeployPending(ctx, c, m, claim)
			if err != nil {
				return nil, err
			}
			if !pending {
				continue
			}
		}
		live = append(live, *m)
	}
	return live, nil
}

// machineDeployPending reports whether a Provisioned machine still has a
// re-provision pending: shouldProvision's own trigger (machine_controller.go)
// would fire again the next time reconcileIdle runs, because claim's intent
// no longer matches the last successful run's recorded spec, or no
// successful run is recorded at all. Such a Machine must keep counting as
// seed demand - its next reconcile starts a fresh Provisioning run against
// the very Image this checks.
//
// This mirrors shouldProvision/intentSubsetEqual's imageRef/dataImages
// comparison only, not hooksHash: seed-demand computation has no
// planbuild.Builder to resolve one. A hooks-only redeploy trigger (a
// PostHook/postHookRefs edit with the same imageRef/dataImages) is
// therefore invisible here, and such a Machine stops counting once
// Provisioned until its hooks-only run actually starts and moves it to
// Provisioning.
func machineDeployPending(ctx context.Context, c client.Client, machine *keziov1alpha3.Machine, claim *keziov1alpha3.MachineClaim) (bool, error) {
	if isEmptyDeployPayload(claim) {
		return false, nil
	}
	ref := machine.Status.LastSuccessfulRunRef
	if ref == nil {
		return true, nil
	}
	ns := ref.Namespace
	if ns == "" {
		ns = machine.Namespace
	}
	var run keziov1alpha3.DeployRun
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, &run); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	return !nameRefEqual(claim.Spec.ImageRef, run.Spec.ImageRef) || !reflect.DeepEqual(claim.Spec.DataImages, run.Spec.DataImages), nil
}

// seederDemandBySite resolves every Machine in machines to its seeder
// placement facts via sitederive.Resolve and groups them into per-Site
// demand counts. A Machine whose Site cannot be resolved (a dangling
// siteRef or seederSubnetRef - a user-facing misconfiguration, see
// sitederive.Resolve's own error classification) is logged and skipped,
// never allowed to block another Site's count.
//
// A Machine whose Site resolves but declares no seeder at all (HasSeeder
// false) contributes no demand count either, but its Site's identity is
// still collected into the second return value: unlike a dangling
// reference, "no seederSubnetRef" is not itself an error, yet a Machine
// there would otherwise wait forever with nothing pointing at the cause,
// so callers surface it distinctly (see ImageConditionSeederDegraded).
// The returned slice is sorted and deduplicated.
func seederDemandBySite(ctx context.Context, c client.Client, machines map[client.ObjectKey]*keziov1alpha3.Machine) (map[string]*seederSiteDemand, []string) {
	demand := make(map[string]*seederSiteDemand)
	noSeeder := make(map[string]bool)
	log := logf.FromContext(ctx)
	for key, machine := range machines {
		res, err := sitederive.Resolve(ctx, c, machine)
		if err != nil {
			log.Error(err, "skipping machine whose Site could not be resolved for seeder demand",
				"machine", key.String())
			continue
		}
		if !res.HasSeeder {
			noSeeder[res.SiteIdentity] = true
			continue
		}
		d, ok := demand[res.SiteIdentity]
		if !ok {
			d = &seederSiteDemand{resolution: res}
			demand[res.SiteIdentity] = d
		}
		d.count++
	}
	noSeederSites := make([]string, 0, len(noSeeder))
	for site := range noSeeder {
		noSeederSites = append(noSeederSites, site)
	}
	sort.Strings(noSeederSites)
	return demand, noSeederSites
}
