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

package planbuild

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// clusterPodIP is the address Kubernetes reports for every seeder pod
// here - the one a machine on the provisioning segment cannot reach.
const clusterPodIP = "10.42.0.59"

func seederPod(networkStatus string) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "seeder", Namespace: "kezio"},
		Status:     corev1.PodStatus{PodIP: clusterPodIP},
	}
	if networkStatus != "" {
		pod.Annotations = map[string]string{multusNetworkStatusAnnotation: networkStatus}
	}
	return pod
}

func TestSeederPodIP(t *testing.T) {
	// Multus reports the cluster CNI first and the attached NAD second;
	// only the second is reachable from a provisioning segment.
	const bothInterfaces = `[
		{"name":"k8s-pod-network","interface":"eth0","ips":["10.42.0.59"],"default":true},
		{"name":"kezio/kezio-seeder-network","interface":"net1","ips":["192.0.2.5"]}
	]`

	for _, tc := range []struct {
		name      string
		pod       *corev1.Pod
		seederNet string
		want      string
	}{
		{
			name:      "the seeder network's address wins over the pod IP",
			pod:       seederPod(bothInterfaces),
			seederNet: "kezio/kezio-seeder-network",
			want:      "192.0.2.5",
		},
		{
			name:      "an unqualified network-status name still matches",
			pod:       seederPod(`[{"name":"kezio-seeder-network","ips":["192.0.2.5"]}]`),
			seederNet: "kezio/kezio-seeder-network",
			want:      "192.0.2.5",
		},
		{
			// No Subnet hosts seeders, so nothing is deploying over a
			// provisioning segment and the pod IP is the right answer.
			name:      "no seeder network falls back to the pod IP",
			pod:       seederPod(bothInterfaces),
			seederNet: "",
			want:      clusterPodIP,
		},
		{
			// The whole point of the fix: never hand out an address the
			// leecher cannot reach, even though one is sitting right there.
			name:      "a seeder network with no attachment yet reports no address",
			pod:       seederPod(""),
			seederNet: "kezio/kezio-seeder-network",
			want:      "",
		},
		{
			name:      "a different network's address is not used",
			pod:       seederPod(`[{"name":"kezio/other-network","ips":["198.51.100.7"]}]`),
			seederNet: "kezio/kezio-seeder-network",
			want:      "",
		},
		{
			name:      "unparsable network-status reports no address",
			pod:       seederPod("not json"),
			seederNet: "kezio/kezio-seeder-network",
			want:      "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := seederPodIP(tc.pod, tc.seederNet); got != tc.want {
				t.Fatalf("seederPodIP = %q, want %q", got, tc.want)
			}
		})
	}
}
