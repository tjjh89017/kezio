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

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/sitederive"
	"github.com/tjjh89017/kezio/internal/store"
)

// partitionContentSeederSubnetLabel names the Subnet a seeder Deployment
// was placed on, when sitederive resolved one - the SubnetReconciler's
// concurrentSeederDeployments filters on it to scope its count to one
// Subnet's own seeder network. Absent (not set to "") when no Subnet
// resolved placement, so a bare-label list still finds only placed
// seeders.
const partitionContentSeederSubnetLabel = "kezio.kojuro.date/seeder-subnet"

// resolveSeederPlacement resolves pc's seeder Deployment placement facts
// through sitederive.ResolveNamespaceSeeder: the Subnet (if any) in pc's
// own namespace that hosts seeders. No matching Subnet resolves to the
// zero Resolution, so a seeder Deployment built from it carries no Multus
// annotation and no nodeSelector - the shape every Subnet-less
// environment (for example an image-only CI lane) has always built.
func (r *PartitionContentReconciler) resolveSeederPlacement(ctx context.Context, pc *keziov1alpha2.PartitionContent) (sitederive.Resolution, error) {
	res, ok, err := sitederive.ResolveNamespaceSeeder(ctx, r.Client, pc.Namespace)
	if err != nil {
		return sitederive.Resolution{}, fmt.Errorf("partitioncontent %q: resolving seeder placement: %w", pc.Name, err)
	}
	if !ok {
		return sitederive.Resolution{}, nil
	}
	return res, nil
}

// multusDefaultNetworkAnnotation is the Multus CNI pod annotation that
// REPLACES a pod's default network attachment, unlike
// multusNetworksAnnotation, which only adds a second one alongside it.
//
// A seeder must be single-homed on its Subnet's network, not dual-homed:
// leechers reach it at Status.PodIP, and a pod that keeps the cluster CNI
// as its default reports that cluster address there - the one address a
// machine on the provisioning segment cannot route to. Single-homing also
// keeps BitTorrent's own peer discovery honest, since the address a peer
// announces is then the address it actually listens on, with no NAT in
// between.
const multusDefaultNetworkAnnotation = "v1.multus-cni.io/default-network"

// seederPodAnnotations returns the pod template annotations placing
// res.SeederNetworkRef as the pod's default (and only) network, or nil
// when res carries no SeederNetworkRef. A bare NAD name defaults against
// the resolved Subnet's own namespace rather than pc's.
//
// Unlike bootdPodAnnotations, this replaces the default network rather
// than adding to it - see multusDefaultNetworkAnnotation. Both seeder
// containers talk to each other over the pod-local loopback, so nothing
// in the pod needs the cluster network it gives up.
func seederPodAnnotations(res sitederive.Resolution) map[string]string {
	if res.SeederNetworkRef == nil {
		return nil
	}
	ns := resolveNamespace(*res.SeederNetworkRef, res.Subnet.Namespace)
	return map[string]string{multusDefaultNetworkAnnotation: ns + "/" + res.SeederNetworkRef.Name}
}

// ensureSeederPlacement patches dep's placement (the Multus annotation,
// nodeSelector, and the seeder-subnet label) in place when it differs
// from res, and leaves dep untouched otherwise - so a reconcile with no
// placement drift never issues a write. The seeder is stateless, so the
// rollout a pod template patch triggers is harmless; no other field
// (image, replicas, ...) is touched here.
func (r *PartitionContentReconciler) ensureSeederPlacement(ctx context.Context, pc *keziov1alpha2.PartitionContent, hash store.InfoHash, dep *appsv1.Deployment, res sitederive.Resolution) (*appsv1.Deployment, error) {
	desired := r.buildSeederDeployment(pc, hash, res)

	wantAnnotations := desired.Spec.Template.Annotations
	wantNodeSelector := desired.Spec.Template.Spec.NodeSelector
	wantLabels := desired.Labels

	if equality.Semantic.DeepEqual(dep.Spec.Template.Annotations, wantAnnotations) &&
		equality.Semantic.DeepEqual(dep.Spec.Template.Spec.NodeSelector, wantNodeSelector) &&
		equality.Semantic.DeepEqual(dep.Labels, wantLabels) {
		return dep, nil
	}

	patch := client.MergeFrom(dep.DeepCopy())
	dep.Spec.Template.Annotations = wantAnnotations
	dep.Spec.Template.Spec.NodeSelector = wantNodeSelector
	dep.Labels = wantLabels
	if err := r.Patch(ctx, dep, patch); err != nil {
		return nil, fmt.Errorf("partitioncontent %q: updating seeder deployment placement %q: %w", pc.Name, dep.Name, err)
	}
	return dep, nil
}

// mapSubnetToPartitionContents maps a Subnet event to a reconcile request
// per PartitionContent in the Subnet's own namespace: seeder placement is
// resolved per-namespace (resolveSeederPlacement), so any Subnet change
// in that namespace can change what any PartitionContent there should
// resolve to, including one whose seeder Deployment already exists.
func (r *PartitionContentReconciler) mapSubnetToPartitionContents(ctx context.Context, obj client.Object) []reconcile.Request {
	subnet, ok := obj.(*keziov1alpha2.Subnet)
	if !ok {
		return nil
	}
	var contents keziov1alpha2.PartitionContentList
	if err := r.List(ctx, &contents, client.InNamespace(subnet.Namespace)); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0, len(contents.Items))
	for i := range contents.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(&contents.Items[i]),
		})
	}
	return requests
}
