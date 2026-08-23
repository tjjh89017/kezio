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

	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
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
// in image's own namespace whose spec.imageRef/dataImages names image,
// via a client-side filter (machineImageRefs) rather than
// machineImageRefIndex: unlike PartitionContentReconciler's own
// resolveSeedDemand path, this is reachable from an ImageReconciler built
// directly against a plain, uncached client with no field indexer
// registered (see image_controller_test.go/image_ingest_test.go), so it
// must not depend on one being present.
func machinesReferencingImage(ctx context.Context, c client.Client, image client.ObjectKey) ([]keziov1alpha2.Machine, error) {
	var list keziov1alpha2.MachineList
	if err := c.List(ctx, &list, client.InNamespace(image.Namespace)); err != nil {
		return nil, err
	}
	live := make([]keziov1alpha2.Machine, 0, len(list.Items))
	for i := range list.Items {
		m := &list.Items[i]
		if !m.DeletionTimestamp.IsZero() {
			continue
		}
		for _, ref := range machineImageRefs(m) {
			if ref == image {
				live = append(live, *m)
				break
			}
		}
	}
	return live, nil
}

// seederDemandBySite resolves every Machine in machines to its seeder
// placement facts via sitederive.Resolve and groups them into per-Site
// demand counts. A Machine whose Site cannot be resolved (a dangling
// siteRef or seederSubnetRef - a user-facing misconfiguration, see
// sitederive.Resolve's own error classification) is logged and skipped,
// never allowed to block another Site's count; likewise a Machine whose
// Site resolves but declares no seeder at all (HasSeeder false) is
// skipped, since there is nothing to count demand toward.
func seederDemandBySite(ctx context.Context, c client.Client, machines map[client.ObjectKey]*keziov1alpha2.Machine) map[string]*seederSiteDemand {
	demand := make(map[string]*seederSiteDemand)
	log := logf.FromContext(ctx)
	for key, machine := range machines {
		res, err := sitederive.Resolve(ctx, c, machine)
		if err != nil {
			log.Error(err, "skipping machine whose Site could not be resolved for seeder demand",
				"machine", key.String())
			continue
		}
		if !res.HasSeeder {
			continue
		}
		d, ok := demand[res.SiteIdentity]
		if !ok {
			d = &seederSiteDemand{resolution: res}
			demand[res.SiteIdentity] = d
		}
		d.count++
	}
	return demand
}
