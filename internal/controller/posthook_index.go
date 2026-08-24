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
	"sync"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

// posthookConfigMapRefIndex and posthookSecretRefIndex are the field index
// names registered on PostHook for the configMapRef/secretRef names its
// steps carry: PostHookReconciler's mapConfigMapToPostHooks/
// mapSecretToPostHooks reverse lookups list against these instead of
// scanning every PostHook in the namespace.
const (
	posthookConfigMapRefIndex = "spec.steps.script.configMapRef.name"
	posthookSecretRefIndex    = "spec.steps.script.secretRef.name"
)

// posthookSourceIndexesOnce guards ensurePostHookSourceIndexes: only
// PostHookReconciler needs these indexes today, but the once guard mirrors
// ensureImageContentRefIndex in case a future reconciler also watches
// PostHook by referenced source.
var (
	posthookSourceIndexesOnce sync.Once
	posthookSourceIndexesErr  error
)

// ensurePostHookSourceIndexes registers posthookConfigMapRefIndex and
// posthookSecretRefIndex on the manager's field indexer exactly once.
func ensurePostHookSourceIndexes(mgr ctrl.Manager) error {
	posthookSourceIndexesOnce.Do(func() {
		if err := mgr.GetFieldIndexer().IndexField(context.Background(), &keziov1alpha2.PostHook{}, posthookConfigMapRefIndex, indexPostHookConfigMapRefs); err != nil {
			posthookSourceIndexesErr = err
			return
		}
		posthookSourceIndexesErr = mgr.GetFieldIndexer().IndexField(context.Background(), &keziov1alpha2.PostHook{}, posthookSecretRefIndex, indexPostHookSecretRefs)
	})
	return posthookSourceIndexesErr
}

// indexPostHookConfigMapRefs extracts every ConfigMap name obj's script
// steps reference, for posthookConfigMapRefIndex.
func indexPostHookConfigMapRefs(obj client.Object) []string {
	ph, ok := obj.(*keziov1alpha2.PostHook)
	if !ok {
		return nil
	}
	seen := make(map[string]bool)
	names := make([]string, 0, len(ph.Spec.Steps))
	for _, step := range ph.Spec.Steps {
		src := step.Script
		if src == nil || src.ConfigMapRef == nil || seen[src.ConfigMapRef.Name] {
			continue
		}
		seen[src.ConfigMapRef.Name] = true
		names = append(names, src.ConfigMapRef.Name)
	}
	return names
}

// indexPostHookSecretRefs extracts every Secret name obj's script steps
// reference, for posthookSecretRefIndex.
func indexPostHookSecretRefs(obj client.Object) []string {
	ph, ok := obj.(*keziov1alpha2.PostHook)
	if !ok {
		return nil
	}
	seen := make(map[string]bool)
	names := make([]string, 0, len(ph.Spec.Steps))
	for _, step := range ph.Spec.Steps {
		src := step.Script
		if src == nil || src.SecretRef == nil || seen[src.SecretRef.Name] {
			continue
		}
		seen[src.SecretRef.Name] = true
		names = append(names, src.SecretRef.Name)
	}
	return names
}

// mapConfigMapToPostHooks maps a ConfigMap event to a reconcile request
// per PostHook, in the same namespace, whose steps reference it - the
// watch that lets a PostHook stuck on SourceMissing recover the moment the
// ConfigMap it references is created, without waiting for the manager's
// default resync.
func (r *PostHookReconciler) mapConfigMapToPostHooks(ctx context.Context, obj client.Object) []reconcile.Request {
	cm, ok := obj.(*corev1.ConfigMap)
	if !ok {
		return nil
	}
	var posthooks keziov1alpha2.PostHookList
	if err := r.List(ctx, &posthooks, client.InNamespace(cm.Namespace), client.MatchingFields{posthookConfigMapRefIndex: cm.Name}); err != nil {
		return nil
	}
	return postHookRequests(posthooks.Items)
}

// mapSecretToPostHooks is mapConfigMapToPostHooks' Secret counterpart.
func (r *PostHookReconciler) mapSecretToPostHooks(ctx context.Context, obj client.Object) []reconcile.Request {
	secret, ok := obj.(*corev1.Secret)
	if !ok {
		return nil
	}
	var posthooks keziov1alpha2.PostHookList
	if err := r.List(ctx, &posthooks, client.InNamespace(secret.Namespace), client.MatchingFields{posthookSecretRefIndex: secret.Name}); err != nil {
		return nil
	}
	return postHookRequests(posthooks.Items)
}

// postHookRequests builds one reconcile.Request per PostHook in items.
func postHookRequests(items []keziov1alpha2.PostHook) []reconcile.Request {
	requests := make([]reconcile.Request, 0, len(items))
	for _, ph := range items {
		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKey{Namespace: ph.Namespace, Name: ph.Name},
		})
	}
	return requests
}
