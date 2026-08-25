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

package posthookdefaults

import (
	"context"
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
)

// FieldManager is the Server-Side Apply field manager identity Ensurer
// writes the default PostHook's spec under.
const FieldManager = "kezio-manager"

// applyBody is the Server-Side Apply request body ensure sends: identity
// plus spec, mirroring machineStatusApplyBody's shape in
// internal/controller (a hand-written body rather than a generated
// ApplyConfiguration type, for the same reason: this repo has no
// applyconfiguration-gen setup).
type applyBody struct {
	metav1.TypeMeta `json:",inline"`
	Metadata        applyBodyMetadata          `json:"metadata,omitempty"`
	Spec            keziov1alpha3.PostHookSpec `json:"spec"`
}

// applyBodyMetadata mirrors the JSON shape of the metav1.ObjectMeta fields
// applyBody actually sets.
type applyBodyMetadata struct {
	Name      string `json:"name,omitempty"`
	Namespace string `json:"namespace,omitempty"`
}

// Ensurer is a manager.Runnable that creates or updates the shipped
// kezio-default-finalize PostHook in the manager's own namespace on start,
// through Server-Side Apply with forced ownership under FieldManager: a
// user who edits spec.steps has that edit overwritten on the next manager
// start, while metadata they add (labels, annotations) - fields this
// apply body never sets - survives untouched.
type Ensurer struct {
	Client client.Client
}

var _ manager.Runnable = (*Ensurer)(nil)

// Start implements manager.Runnable. POD_NAMESPACE unset (a local `make
// run` outside a Pod, where the default PostHook has no namespace to live
// in) logs and returns nil rather than failing manager startup.
func (e *Ensurer) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("posthookdefaults")

	ns, ok := Namespace()
	if !ok {
		log.Info("POD_NAMESPACE not set; skipping the default PostHook")
		return nil
	}

	if err := e.ensure(ctx, ns); err != nil {
		return fmt.Errorf("ensuring PostHook %q in namespace %q: %w", DefaultFinalizeHookName, ns, err)
	}
	log.Info("ensured the default PostHook", "namespace", ns, "name", DefaultFinalizeHookName)
	return nil
}

// ensure server-side-applies the shipped spec onto the PostHook named
// DefaultFinalizeHookName in namespace, creating it if absent.
func (e *Ensurer) ensure(ctx context.Context, namespace string) error {
	body := applyBody{
		TypeMeta: metav1.TypeMeta{APIVersion: keziov1alpha3.GroupVersion.String(), Kind: "PostHook"},
		Metadata: applyBodyMetadata{Name: DefaultFinalizeHookName, Namespace: namespace},
		Spec:     Spec(),
	}
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encoding apply body: %w", err)
	}

	ph := &keziov1alpha3.PostHook{
		ObjectMeta: metav1.ObjectMeta{Name: DefaultFinalizeHookName, Namespace: namespace},
	}
	patch := client.RawPatch(types.ApplyPatchType, data)
	return e.Client.Patch(ctx, ph, patch, client.FieldOwner(FieldManager), client.ForceOwnership)
}
