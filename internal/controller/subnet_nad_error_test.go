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
	"errors"
	"fmt"
	"testing"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TestIsIndeterminateNADErr pins the error classification a NAD fetch
// depends on. The no-match case is the one a cluster without Multus
// produces: the NAD kind itself is absent, the client fails at the
// RESTMapper before any Get, and that error is not IsNotFound. If it is
// not folded into Indeterminate, Reconcile returns it forever and the
// Subnet's conditions are never written at all.
func TestIsIndeterminateNADErr(t *testing.T) {
	nadGK := schema.GroupKind{Group: "k8s.cni.cncf.io", Kind: "NetworkAttachmentDefinition"}
	notFound := kerrors.NewNotFound(schema.GroupResource{Group: nadGK.Group, Resource: "network-attachment-definitions"}, "boot-nad")
	noMatch := &apimeta.NoKindMatchError{GroupKind: nadGK}

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"object not found", notFound, true},
		{"kind absent from the cluster", noMatch, true},
		{"wrapped kind absent", fmt.Errorf("get nad: %w", noMatch), true},
		{"unreadable config", nadContentError{err: errors.New("spec.config missing")}, true},
		{"transient stays transient", errors.New("connection refused"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isIndeterminateNADErr(tc.err); got != tc.want {
				t.Fatalf("isIndeterminateNADErr(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}

	if r := indeterminateFromFetchErr("Bootd", "boot nad", noMatch); r.Reason != "BootdNADKindAbsent" {
		t.Fatalf("indeterminateFromFetchErr reason = %q, want BootdNADKindAbsent", r.Reason)
	}
}
