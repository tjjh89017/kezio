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

// Package controller holds every reconciler kezio's manager runs, one per
// CRD: Site, Subnet, Machine, DeployRun, Image, ImageImport,
// PartitionContent, and PostHook.
//
// Which reconciler owns which Kubernetes object is the fact that decides
// how every other one has to watch it:
//
//   - SiteReconciler owns the per-Site tracker Deployment.
//   - SubnetReconciler owns the per-Subnet bootd Deployment.
//   - ImageReconciler owns the seeder Deployment - one per (Image, Site),
//     created only once the Image is Ready and only where live Machines
//     demand it, and torn down after a grace period once they stop.
//   - PartitionContentReconciler owns the content PVC and the publish
//     Job, and carries PartitionContentFinalizer. It creates no seeder
//     Deployment of its own; it only reflects the Image-owned ones into
//     status.
//   - ImageImportReconciler owns the ingest Job and its scratch PVC. The
//     PartitionContent and Image objects it creates are deliberately not
//     owner-referenced to it: they outlive the one-shot request that
//     captured them.
//   - MachineReconciler owns the DeployRun of each provisioning pass;
//     DeployRunReconciler only garbage-collects them.
//   - PostHookReconciler owns nothing and writes conditions only.
//
// A seeder Deployment is therefore owned by the Image, never by the Site
// it runs at. Any reconciler other than ImageReconciler that must react
// to one has to Watches it with an explicit mapping, because Owns sees
// only objects carrying an owner reference back to the object being
// reconciled. SiteReconciler derives status.seederReady from that
// Deployment's availability, so relying on Owns there left seederReady
// stale until something else happened to write the Site - hence
// mapSeederDeploymentToSite (site_controller.go). PartitionContent
// reaches its own status.seeders[] the same way, hopping through the
// Deployment's Image owner reference
// (mapSeederDeploymentToPartitionContents).
//
// Every reconciler that creates a named Deployment refuses to adopt or
// overwrite one already occupying that name without an owner reference
// back to the reconciled object (metav1.IsControlledBy). Such a foreign
// object - hand-applied, or a survivor of a deleted-and-recreated
// same-named parent - is read for its ownership and then left entirely
// alone: never patched, never deleted. The collision is surfaced instead
// (Site/Subnet not Ready, Image SeederDegraded). Since it carries no
// owner reference, no Owns watch will ever see it disappear either, so
// the Site and Subnet reconcilers poll on a bounded RequeueAfter rather
// than wait for an event that cannot arrive.
//
// A Machine never names its Site. The Site is derived, always through
// internal/sitederive: Machine.spec.subnetRef -> Subnet ->
// Subnet.spec.siteRef -> Site, and from there the Site's own
// spec.seederSubnetRef, which is the Subnet a seeder and tracker are
// actually placed on. A Machine's broadcast domain and its seeder's are
// allowed to differ as long as one Site holds both, so nothing here may
// shortcut that chain by treating a Machine's own Subnet as the seeding
// one.
package controller
