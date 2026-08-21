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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/posthookvalidate"
)

// PostHookReconciler reconciles a PostHook object.
//
// A PostHook carries no finalizer: it owns nothing (no PVC, Job, or
// Deployment), so there is nothing for garbage collection to leave
// behind - the same reasoning SubnetReconciler's doc comment gives for
// carrying none.
type PostHookReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=posthooks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=posthooks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kezio.kojuro.date,resources=posthooks/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconcile fetches the PostHook and folds spec validation plus
// reconcile-time source resolution into its Valid/Ready conditions.
func (r *PostHookReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ph keziov1alpha2.PostHook
	if err := r.Get(ctx, req.NamespacedName, &ph); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	return r.onChange(ctx, &ph)
}

// onChange runs posthookvalidate.Validate - the same spec-only checks the
// admission webhook applies - then, only once those pass, resolves every
// step's configMapRef/secretRef against this namespace: the webhook cannot
// check that a referenced key actually exists (the ConfigMap/Secret may
// not exist yet, or may be created after admission), so this is deferred
// to reconcile time. Valid is False on either failure; Ready mirrors Valid
// exactly, since a PostHook has nothing else to become ready.
func (r *PostHookReconciler) onChange(ctx context.Context, ph *keziov1alpha2.PostHook) (ctrl.Result, error) {
	if err := posthookvalidate.Validate(ph); err != nil {
		return r.recordStatus(ctx, ph, metav1.ConditionFalse, "InvalidSpec", err.Error())
	}

	missing, err := r.missingSources(ctx, ph)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(missing) > 0 {
		return r.recordStatus(ctx, ph, metav1.ConditionFalse, "SourceMissing", boundedMessage(missing))
	}

	return r.recordStatus(ctx, ph, metav1.ConditionTrue, "PostHookValid", "spec passes validation and every referenced source resolves")
}

// missingSources reports, for every script/chrootScript step's
// configMapRef/secretRef, a description of why it does not resolve in
// ph's own namespace - PostHookScriptSource carries no namespace field, so
// a ConfigMap/Secret it names always lives alongside the PostHook
// referencing it, mirroring ConfigMapKeyRef/SecretKeyRef's own doc
// comments. An inline script has nothing to resolve here.
func (r *PostHookReconciler) missingSources(ctx context.Context, ph *keziov1alpha2.PostHook) ([]string, error) {
	var missing []string
	for i, step := range ph.Spec.Steps {
		for _, kind := range [...]struct {
			path string
			src  *keziov1alpha2.PostHookScriptSource
		}{
			{fmt.Sprintf("spec.steps[%d].script", i), step.Script},
			{fmt.Sprintf("spec.steps[%d].chrootScript", i), step.ChrootScript},
		} {
			if kind.src == nil {
				continue
			}
			problem, err := r.checkSource(ctx, ph.Namespace, kind.path, *kind.src)
			if err != nil {
				return nil, err
			}
			if problem != "" {
				missing = append(missing, problem)
			}
		}
	}
	return missing, nil
}

// checkSource resolves one script source's configMapRef/secretRef in
// namespace, returning a non-empty description of the problem when the
// object or its key is missing. An inline script (SourceKind ==
// PostHookScriptSourceInline) has nothing to resolve and always returns
// "".
func (r *PostHookReconciler) checkSource(ctx context.Context, namespace, path string, src keziov1alpha2.PostHookScriptSource) (string, error) {
	switch src.SourceKind() {
	case keziov1alpha2.PostHookScriptSourceConfigMapRef:
		var cm corev1.ConfigMap
		switch err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: src.ConfigMapRef.Name}, &cm); {
		case apierrors.IsNotFound(err):
			return fmt.Sprintf("%s.configMapRef: ConfigMap %q not found", path, src.ConfigMapRef.Name), nil
		case err != nil:
			return "", fmt.Errorf("get configmap %s/%s: %w", namespace, src.ConfigMapRef.Name, err)
		}
		if _, ok := cm.Data[src.ConfigMapRef.Key]; !ok {
			return fmt.Sprintf("%s.configMapRef: ConfigMap %q has no key %q", path, src.ConfigMapRef.Name, src.ConfigMapRef.Key), nil
		}
	case keziov1alpha2.PostHookScriptSourceSecretRef:
		var secret corev1.Secret
		switch err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: src.SecretRef.Name}, &secret); {
		case apierrors.IsNotFound(err):
			return fmt.Sprintf("%s.secretRef: Secret %q not found", path, src.SecretRef.Name), nil
		case err != nil:
			return "", fmt.Errorf("get secret %s/%s: %w", namespace, src.SecretRef.Name, err)
		}
		if _, ok := secret.Data[src.SecretRef.Key]; !ok {
			return fmt.Sprintf("%s.secretRef: Secret %q has no key %q", path, src.SecretRef.Name, src.SecretRef.Key), nil
		}
	}
	return "", nil
}

// recordStatus writes Valid and Ready with status/reason/message,
// stamping status.observedGeneration and both conditions'
// observedGeneration from ph.Generation. Ready mirrors Valid's own
// status/reason/message on failure; on success it carries its own
// reason/message, since "valid" and "ready" are different claims even
// though a PostHook's Ready has no further condition of its own to add.
func (r *PostHookReconciler) recordStatus(ctx context.Context, ph *keziov1alpha2.PostHook, status metav1.ConditionStatus, reason, message string) (ctrl.Result, error) {
	ph.Status.ObservedGeneration = ph.Generation
	apimeta.SetStatusCondition(&ph.Status.Conditions, metav1.Condition{
		Type:               keziov1alpha2.PostHookConditionValid,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: ph.Generation,
	})

	readyStatus, readyReason, readyMessage := status, reason, message
	if status == metav1.ConditionTrue {
		readyReason, readyMessage = "PostHookReady", "PostHook is valid and ready"
	}
	apimeta.SetStatusCondition(&ph.Status.Conditions, metav1.Condition{
		Type:               keziov1alpha2.PostHookConditionReady,
		Status:             readyStatus,
		Reason:             readyReason,
		Message:            readyMessage,
		ObservedGeneration: ph.Generation,
	})

	if err := r.Status().Update(ctx, ph); err != nil {
		return ctrl.Result{}, fmt.Errorf("posthook %q: updating status: %w", ph.Name, err)
	}
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager. A status-only
// write (this reconciler's own Valid/Ready update) must not re-trigger
// itself, so For is restricted to a generation change, mirroring
// imageUpdatePredicate. The ConfigMap/Secret watches, mapped through
// ensurePostHookSourceIndexes' field indexes, let a PostHook stuck on
// SourceMissing recover the moment its referenced source is created,
// rather than waiting on the manager's default resync.
func (r *PostHookReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := ensurePostHookSourceIndexes(mgr); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&keziov1alpha2.PostHook{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.mapConfigMapToPostHooks)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.mapSecretToPostHooks)).
		Named("posthook").
		Complete(r)
}
