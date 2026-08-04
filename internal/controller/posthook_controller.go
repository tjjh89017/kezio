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

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
)

// PostHookReconciler reconciles a PostHook object
type PostHookReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=posthooks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=posthooks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=posthooks/finalizers,verbs=update

// Reconcile follows the same fetch / onDelete / onChange shape as the other
// kezio controllers: fetch the PostHook, dispatch to onDelete if it is
// being deleted, otherwise dispatch to onChange.
//
// PostHook owns no external resource, so — unlike Machine and Image —
// it carries no finalizer: there is nothing to clean up before the API
// server can delete the object, so ensuring a finalizer here would be
// ceremony rather than a pattern requirement. This is a deliberate
// deviation from the reference controller shape, which finalizes every
// kind uniformly.
//
// TODO(user): Modify onChange to compare the state specified by the
// PostHook object against the actual cluster state, and then perform
// operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.21.0/pkg/reconcile
func (r *PostHookReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	posthook := &keziov1alpha1.PostHook{}
	if err := r.Get(ctx, req.NamespacedName, posthook); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !posthook.DeletionTimestamp.IsZero() {
		return r.onDelete(ctx, req, posthook)
	}

	return r.onChange(ctx, req, posthook)
}

// onDelete is a trivial no-op: PostHook has no finalizer and owns nothing
// external, so there is no cleanup to perform before the API server
// finishes deleting the object.
func (r *PostHookReconciler) onDelete(ctx context.Context, _ ctrl.Request, _ *keziov1alpha1.PostHook) (ctrl.Result, error) {
	_ = logf.FromContext(ctx)
	return ctrl.Result{}, nil
}

// onChange is a no-op stub. TODO(user): your logic here.
func (r *PostHookReconciler) onChange(ctx context.Context, _ ctrl.Request, _ *keziov1alpha1.PostHook) (ctrl.Result, error) {
	_ = logf.FromContext(ctx)
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *PostHookReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&keziov1alpha1.PostHook{}).
		Named("posthook").
		Complete(r)
}
