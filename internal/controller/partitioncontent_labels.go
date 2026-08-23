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

// Labels shared by every object the PartitionContent reconciler builds
// (the content PVC and the publish Job), plus the two labels every
// per-(Image, Site) seeder Deployment ImageReconciler builds also carries
// (partitionContentSeederComponentValue, partitionContentSeederSubnetLabel)
// - kept here rather than duplicated so SubnetReconciler.concurrentSeederDeployments
// and SiteReconciler.seederPlacementReady, which count seeder Deployments
// by these exact labels, share one definition with the code that sets
// them.
const (
	partitionContentAppNameLabel         = "app.kubernetes.io/name"
	partitionContentAppNameValue         = "kezio"
	partitionContentAppComponentLabel    = "app.kubernetes.io/component"
	partitionContentPVCComponentValue    = "partition-content"
	partitionContentJobComponentValue    = "partition-content-publish"
	partitionContentSeederComponentValue = "image-seeder"
	// partitionContentSeederSubnetLabel names the Subnet a seeder
	// Deployment was placed on, when sitederive resolved one -
	// SubnetReconciler.concurrentSeederDeployments and
	// SiteReconciler.seederPlacementReady filter on it to scope their
	// count to one Subnet's own seeder network. Absent (not set to "")
	// when no Subnet resolved placement, so a bare-label list still finds
	// only placed seeders.
	partitionContentSeederSubnetLabel = "kezio.kojuro.date/seeder-subnet"
)
