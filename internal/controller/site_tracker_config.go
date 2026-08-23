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

// TrackerDeploymentConfig configures how SiteReconciler builds each
// seeding Site's tracker Deployment. Its zero value (Image == "")
// disables tracker Deployment reconciliation entirely, mirroring
// BootdDeploymentConfig: status/conditions are still computed and written
// regardless.
type TrackerDeploymentConfig struct {
	// Image is the tracker container image reference (an opentracker
	// build; see config/opentracker/opentracker-deployment.yaml for the
	// image this shape was taken from).
	Image string
}

// enabled reports whether tracker Deployments are configured.
func (c TrackerDeploymentConfig) enabled() bool {
	return c.Image != ""
}
