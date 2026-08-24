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
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

// trackerTestSeedingSubnet returns a seeding Subnet carrying a
// SeederNetworkRef, so buildTrackerDeployment produces the pinned-address
// annotation rather than withholding it.
func trackerTestSeedingSubnet(namespace string) *keziov1alpha2.Subnet {
	return testSubnet(namespace, func(s *keziov1alpha2.Subnet) {
		s.Spec.SeederNetworkRef = &keziov1alpha2.NameRef{Name: "seeder-nad"}
	})
}

// TestBuildTrackerDeploymentStrategyIsRecreate checks the Deployment asks
// for Recreate. The default RollingUpdate surges a second pod before the
// outgoing one is deleted, and both would request the same pinned
// address, which the ipam plugin can only hand to one pod at a time.
func TestBuildTrackerDeploymentStrategyIsRecreate(t *testing.T) {
	site := &keziov1alpha2.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "hq", Namespace: "site-hq"},
		Spec:       keziov1alpha2.SiteSpec{Tracker: keziov1alpha2.SiteTracker{IP: "192.0.2.60"}},
	}
	subnet := trackerTestSeedingSubnet("site-hq")

	dep, err := buildTrackerDeployment(site, subnet, TrackerDeploymentConfig{Image: "tracker:test"})
	if err != nil {
		t.Fatalf("buildTrackerDeployment returned an error: %v", err)
	}

	if got := dep.Spec.Strategy.Type; got != appsv1.RecreateDeploymentStrategyType {
		t.Errorf("Deployment strategy = %q, want %q", got, appsv1.RecreateDeploymentStrategyType)
	}
	if dep.Spec.Strategy.RollingUpdate != nil {
		t.Errorf("Deployment RollingUpdate = %#v, want nil under a Recreate strategy", dep.Spec.Strategy.RollingUpdate)
	}
}
