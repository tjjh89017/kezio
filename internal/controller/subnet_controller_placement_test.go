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
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apimachineryruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

// concurrentSeederDeploymentsTestClient builds a fake client seeded with
// deployments, its scheme carrying both apps/v1 and kezio.kojuro.date/v1alpha3
// - concurrentSeederDeployments never reads a kezio type directly, but
// SubnetReconciler's own embedded client.Client expects the full scheme.
func concurrentSeederDeploymentsTestClient(t *testing.T, deployments ...client.Object) client.Client {
	t.Helper()
	scheme := apimachineryruntime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("appsv1.AddToScheme() error = %v", err)
	}
	if err := keziov1alpha3.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployments...).WithStatusSubresource(&appsv1.Deployment{}).Build()
}

// availableSeederDeployment builds a seeder Deployment carrying the labels
// concurrentSeederDeployments filters on, with AvailableReplicas already
// set - the fake client's status subresource means Status must be set via
// a separate Status().Update after Create, so tests build the desired
// status up front and let the fixture helper apply it.
func availableSeederDeployment(namespace, name, subnetLabel string) *appsv1.Deployment {
	labels := map[string]string{partitionContentAppComponentLabel: partitionContentSeederComponentValue}
	if subnetLabel != "" {
		labels[partitionContentSeederSubnetLabel] = subnetLabel
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "stub", Image: "stub:test"}},
				},
			},
		},
		Status: appsv1.DeploymentStatus{AvailableReplicas: 1},
	}
}

// TestConcurrentSeederDeploymentsScopesToSubnetNetwork checks that
// concurrentSeederDeployments counts only seeder Deployments carrying
// subnet's own seeder-subnet label in subnet's own namespace, ignoring an
// available seeder placed on a different Subnet, one with no placement
// label at all, and one in a different namespace with the same label
// value.
func TestConcurrentSeederDeploymentsScopesToSubnetNetwork(t *testing.T) {
	subnet := &keziov1alpha3.Subnet{ObjectMeta: metav1.ObjectMeta{Name: "rack-1", Namespace: "ns1"}}

	onSubnet := availableSeederDeployment("ns1", "seeder-a", "rack-1")
	otherSubnet := availableSeederDeployment("ns1", "seeder-b", "rack-2")
	unplaced := availableSeederDeployment("ns1", "seeder-c", "")
	otherNamespace := availableSeederDeployment("ns2", "seeder-d", "rack-1")

	c := concurrentSeederDeploymentsTestClient(t, onSubnet, otherSubnet, unplaced, otherNamespace)
	r := &SubnetReconciler{Client: c, Scheme: c.Scheme()}

	got, err := r.concurrentSeederDeployments(context.Background(), subnet)
	if err != nil {
		t.Fatalf("concurrentSeederDeployments() error = %v", err)
	}
	if got != 1 {
		t.Errorf("concurrentSeederDeployments() = %d, want 1 (only seeder-a matches subnet's namespace and seeder-subnet label)", got)
	}
}
