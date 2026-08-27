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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

// trackerTestSeedingSubnet returns a seeding Subnet carrying a
// SeederNetworkRef, so buildTrackerDeployment produces the pinned-address
// annotation rather than withholding it.
func trackerTestSeedingSubnet(namespace string) *keziov1alpha3.Subnet {
	return testSubnet(namespace, func(s *keziov1alpha3.Subnet) {
		s.Spec.SeederNetworkRef = &keziov1alpha3.NameRef{Name: "seeder-nad"}
	})
}

// TestBuildTrackerDeploymentStrategyIsRecreate checks the Deployment asks
// for Recreate. The default RollingUpdate surges a second pod before the
// outgoing one is deleted, and both would request the same pinned
// address, which the ipam plugin can only hand to one pod at a time.
func TestBuildTrackerDeploymentStrategyIsRecreate(t *testing.T) {
	site := &keziov1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "hq", Namespace: "site-hq"},
		Spec:       keziov1alpha3.SiteSpec{Tracker: keziov1alpha3.SiteTracker{IP: "192.0.2.60"}},
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

func trackerTestPod(annotations map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "site-hq", Name: "kezio-tracker-hq-abc12", Annotations: annotations},
	}
}

func TestTrackerPodNetworkIssue_NoAnnotationReportsMissing(t *testing.T) {
	pod := trackerTestPod(nil)

	reason, message := trackerPodNetworkIssue(pod, "site-hq", "seeder-nad", "192.0.2.60")
	if reason != "TrackerNetworkMissing" {
		t.Errorf("reason = %q, want TrackerNetworkMissing", reason)
	}
	if message == "" {
		t.Error("message = \"\", want a non-empty explanation")
	}
}

func TestTrackerPodNetworkIssue_UnparseableAnnotationReportsMissing(t *testing.T) {
	pod := trackerTestPod(map[string]string{trackerNetworkStatusAnnotation: "not json"})

	reason, _ := trackerPodNetworkIssue(pod, "site-hq", "seeder-nad", "192.0.2.60")
	if reason != "TrackerNetworkMissing" {
		t.Errorf("reason = %q, want TrackerNetworkMissing", reason)
	}
}

func TestTrackerPodNetworkIssue_NoMatchingNADEntryReportsMissing(t *testing.T) {
	pod := trackerTestPod(map[string]string{
		trackerNetworkStatusAnnotation: `[{"name":"kube-system/calico","ips":["10.244.0.7"]}]`,
	})

	reason, _ := trackerPodNetworkIssue(pod, "site-hq", "seeder-nad", "192.0.2.60")
	if reason != "TrackerNetworkMissing" {
		t.Errorf("reason = %q, want TrackerNetworkMissing", reason)
	}
}

func TestTrackerPodNetworkIssue_WrongIPReportsMismatch(t *testing.T) {
	pod := trackerTestPod(map[string]string{
		trackerNetworkStatusAnnotation: `[{"name":"site-hq/seeder-nad","ips":["192.0.2.99"]}]`,
	})

	reason, message := trackerPodNetworkIssue(pod, "site-hq", "seeder-nad", "192.0.2.60")
	if reason != "TrackerNetworkIPMismatch" {
		t.Errorf("reason = %q, want TrackerNetworkIPMismatch", reason)
	}
	if message == "" {
		t.Error("message = \"\", want a non-empty explanation")
	}
}

func TestTrackerPodNetworkIssue_MatchingIPReportsOK(t *testing.T) {
	pod := trackerTestPod(map[string]string{
		trackerNetworkStatusAnnotation: `[{"name":"kube-system/calico","ips":["10.244.0.7"]},` +
			`{"name":"site-hq/seeder-nad","ips":["192.0.2.60"]}]`,
	})

	reason, message := trackerPodNetworkIssue(pod, "site-hq", "seeder-nad", "192.0.2.60")
	if reason != "" || message != "" {
		t.Errorf("got (%q, %q), want (\"\", \"\")", reason, message)
	}
}

func TestSelectTrackerPod_PrefersRunningOverPending(t *testing.T) {
	pending := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pending"},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}
	running := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "running"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}

	got := selectTrackerPod([]corev1.Pod{pending, running})
	if got == nil || got.Name != "running" {
		t.Errorf("selectTrackerPod = %v, want the Running pod", got)
	}
}

func TestSelectTrackerPod_SkipsTerminating(t *testing.T) {
	now := metav1.Now()
	terminating := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "terminating", DeletionTimestamp: &now},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	pending := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pending"},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}

	got := selectTrackerPod([]corev1.Pod{terminating, pending})
	if got == nil || got.Name != "pending" {
		t.Errorf("selectTrackerPod = %v, want the live pending pod, not the terminating Running one", got)
	}
}

func TestSelectTrackerPod_NoneLiveReturnsNil(t *testing.T) {
	now := metav1.Now()
	terminating := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "terminating", DeletionTimestamp: &now}}

	if got := selectTrackerPod([]corev1.Pod{terminating}); got != nil {
		t.Errorf("selectTrackerPod = %v, want nil", got)
	}
}
