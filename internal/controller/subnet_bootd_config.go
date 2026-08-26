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

// bootdDefaultServiceAccountName is the ServiceAccount name every bootd
// Deployment stamps as serviceAccountName unless BootdDeploymentConfig
// overrides it - config/bootd's namePrefix produces this exact name from
// its ServiceAccount's "bootd" base name.
const bootdDefaultServiceAccountName = "kezio-bootd"

// BootdDeploymentConfig configures how SubnetReconciler builds each
// Subnet's bootd Deployment. Its zero value (Image == "") disables bootd
// Deployment reconciliation entirely: NAD/subnet validation conditions
// are still computed and written regardless.
type BootdDeploymentConfig struct {
	// Image is the bootd container image reference.
	Image string
	// BootArtifactsImage is the boot-artifacts image the
	// fetch-boot-artifacts initContainer copies bootd.ShimFilename/
	// bootd.GrubFilename out of.
	BootArtifactsImage string
	// ServiceAccountName overrides bootdDefaultServiceAccountName. Empty
	// uses the default.
	ServiceAccountName string
	// AgentUpstreamURL, set, becomes every Subnet's bootd
	// BOOTD_AGENT_UPSTREAM_URL. Empty leaves the env var unset.
	AgentUpstreamURL string
	// BootUpstreamURL is BOOTD_BOOT_UPSTREAM_URL's counterpart.
	BootUpstreamURL string
	// HTTPBootURL, set, becomes every Subnet's bootd BOOTD_HTTP_BOOT_URL,
	// replacing the URL bootd derives itself. Empty keeps bootd's own
	// derivation.
	HTTPBootURL string
	// LeaseStorageClassName optionally sets the StorageClass of every
	// lease-mode Subnet's bootd lease PVC (buildBootdLeasePVC). Empty
	// leaves storageClassName unset, so the cluster's own default
	// StorageClass applies.
	LeaseStorageClassName string
}

// enabled reports whether bootd Deployments are configured: both Image
// and BootArtifactsImage must be set, mirroring
// PartitionContentPublishConfig.ready()'s all-or-nothing shape - a
// Deployment built with one but not the other would create a pod whose
// fetch-boot-artifacts initContainer can never succeed.
func (c BootdDeploymentConfig) enabled() bool {
	return c.Image != "" && c.BootArtifactsImage != ""
}

// serviceAccountName returns c.ServiceAccountName, falling back to
// bootdDefaultServiceAccountName when unset.
func (c BootdDeploymentConfig) serviceAccountName() string {
	if c.ServiceAccountName != "" {
		return c.ServiceAccountName
	}
	return bootdDefaultServiceAccountName
}
