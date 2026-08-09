/*
Copyright 2026 Date Huang.

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
	"errors"
	"testing"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/nadvalidate"
)

// testSubnetForNADChecks returns a minimal Subnet with only the fields
// runNADChecks reads: BootdNetworkRef (always resolved) and
// BootdServerIP. It has no SeederNetworkRef, so runNADChecks returns
// after the bootd NAD fetch alone - exactly what these tests exercise.
func testSubnetForNADChecks() *keziov1alpha1.Subnet {
	return &keziov1alpha1.Subnet{
		ObjectMeta: metav1.ObjectMeta{Name: "rack-1", Namespace: "default"},
		Spec: keziov1alpha1.SubnetSpec{
			BootdServerIP:   "10.0.0.1",
			BootdNetworkRef: keziov1alpha1.NameRef{Name: "bootd-net"},
		},
	}
}

// isNADUnstructuredGet reports whether a Get call targets the
// NetworkAttachmentDefinition GVK fetchNADConfig reads, the same test
// of obj's GVK subnet_bootd_envtest_test.go's createTestNAD relies on
// production code using.
func isNADUnstructuredGet(obj client.Object) bool {
	return obj.GetObjectKind().GroupVersionKind() == networkAttachmentDefinitionGVK
}

// TestRunNADChecks_TransientFetchErrorPropagates checks that a
// non-NotFound Get error (timeout, throttling, RBAC misconfiguration)
// is returned so Reconcile requeues with backoff, rather than folded
// into a durable Indeterminate condition nothing would ever clear.
func TestRunNADChecks_TransientFetchErrorPropagates(t *testing.T) {
	wantErr := errors.New("injected transient failure")
	c := fake.NewClientBuilder().
		WithScheme(newPartitionStorageTestScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cli client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if isNADUnstructuredGet(obj) {
					return wantErr
				}
				return cli.Get(ctx, key, obj, opts...)
			},
		}).
		Build()

	r := &SubnetReconciler{Client: c, Scheme: c.Scheme()}
	checks, err := r.runNADChecks(context.Background(), testSubnetForNADChecks())
	if !errors.Is(err, wantErr) {
		t.Fatalf("runNADChecks() error = %v, want it to wrap %v", err, wantErr)
	}
	if checks != nil {
		t.Errorf("runNADChecks() checks = %+v, want nil on error", checks)
	}
}

// TestRunNADChecks_NotFoundStaysIndeterminate checks the other side: a
// genuinely missing bootd NAD (IsNotFound) is folded into an
// Indeterminate BootdAddressValid condition instead of being returned
// as a reconcile error.
func TestRunNADChecks_NotFoundStaysIndeterminate(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(newPartitionStorageTestScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cli client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if isNADUnstructuredGet(obj) {
					return kerrors.NewNotFound(schema.GroupResource{Group: networkAttachmentDefinitionGVK.Group, Resource: "network-attachment-definitions"}, key.Name)
				}
				return cli.Get(ctx, key, obj, opts...)
			},
		}).
		Build()

	r := &SubnetReconciler{Client: c, Scheme: c.Scheme()}
	checks, err := r.runNADChecks(context.Background(), testSubnetForNADChecks())
	if err != nil {
		t.Fatalf("runNADChecks() error = %v, want nil (NotFound must not requeue)", err)
	}
	if len(checks) != 1 {
		t.Fatalf("runNADChecks() checks = %+v, want exactly one (bootd) check", checks)
	}
	got := checks[0]
	if got.conditionType != ConditionBootdAddressValid {
		t.Errorf("checks[0].conditionType = %q, want %q", got.conditionType, ConditionBootdAddressValid)
	}
	if got.result.Verdict != nadvalidate.Indeterminate {
		t.Errorf("checks[0].result.Verdict = %v, want Indeterminate", got.result.Verdict)
	}
	if got.result.Reason != "BootdNADNotFound" {
		t.Errorf("checks[0].result.Reason = %q, want %q", got.result.Reason, "BootdNADNotFound")
	}
}
