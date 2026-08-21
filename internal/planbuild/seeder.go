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
	"context"
	"fmt"
	"net"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/tjjh89017/kezio/internal/seederdeploy"
	"github.com/tjjh89017/kezio/internal/store"
)

// resolveTorrentURL finds hash's seeder Deployment in ns (the content's
// own namespace - seeding is not yet site-aware, so there is exactly one
// Deployment per content, named by seederdeploy.Name) and a ready pod
// within it, and returns the URL that pod serves hash's .torrent from
// (see cmd/seeder's HTTP server). Neither a missing Deployment nor one
// with no pod that has reported a PodIP is a configuration mistake - a
// seeder is created on demand and takes a moment to schedule - so both
// are reported as NotReadyError rather than a hard failure.
func (b *Builder) resolveTorrentURL(ctx context.Context, ns string, hash store.InfoHash) (string, error) {
	name := seederdeploy.Name(hash)

	dep := &appsv1.Deployment{}
	if err := b.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, dep); err != nil {
		if apierrors.IsNotFound(err) {
			return "", &NotReadyError{Reason: fmt.Sprintf("no seeder deployment yet for content %s", name)}
		}
		return "", fmt.Errorf("get seeder deployment %s/%s: %w", ns, name, err)
	}
	if dep.Spec.Selector == nil {
		return "", fmt.Errorf("seeder deployment %s/%s has no pod selector", ns, name)
	}

	pods := &corev1.PodList{}
	if err := b.Client.List(ctx, pods,
		client.InNamespace(ns),
		client.MatchingLabels(dep.Spec.Selector.MatchLabels),
	); err != nil {
		return "", fmt.Errorf("list pods for seeder deployment %s/%s: %w", ns, name, err)
	}

	for i := range pods.Items {
		if ip := pods.Items[i].Status.PodIP; ip != "" {
			host := net.JoinHostPort(ip, strconv.Itoa(int(seederdeploy.TorrentHTTPPort)))
			return fmt.Sprintf("http://%s/torrents/%s", host, hash.String()), nil
		}
	}
	return "", &NotReadyError{Reason: fmt.Sprintf("no ready seeder pod yet for content %s", name)}
}
