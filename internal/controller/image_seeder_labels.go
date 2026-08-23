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

// imageSeederInstanceLabel carries the exact (Image, Site) identity a
// seeder Deployment is scoped to - its own deterministic name
// (seederdeploy.Name), which is unique per (Image, Site) by construction.
// It is set on both the Deployment's Selector.MatchLabels and its pod
// template labels, so two seeder Deployments can never select each
// other's pods: before this label existed, every seeder Deployment in a
// namespace shared the same two-label selector
// (partitionContentAppNameLabel/partitionContentSeederComponentValue) and
// could adopt or list pods that did not hold the content it was built
// for.
const imageSeederInstanceLabel = "kezio.kojuro.date/seeder-instance"

// imageSeederSiteAnnotation records the (namespace-qualified) Site
// identity a seeder Deployment was built for - sitederive.Resolution's
// own SiteIdentity string. A Deployment annotation, not a label: Site
// identity carries a "/" (namespace/name), which a label value cannot
// hold. This is what lets a reader (PartitionContentReconciler's own
// status derivation, SiteReconciler) recover which Site a given
// Image-owned seeder Deployment serves without re-resolving it.
const imageSeederSiteAnnotation = "kezio.kojuro.date/seeder-site"
