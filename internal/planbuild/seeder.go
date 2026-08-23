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
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/tjjh89017/kezio/internal/seederdeploy"
	"github.com/tjjh89017/kezio/internal/sitederive"
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

	seederNet, err := b.seederNetworkName(ctx, ns)
	if err != nil {
		return "", err
	}

	for i := range pods.Items {
		ip := seederPodIP(&pods.Items[i], seederNet)
		if ip == "" {
			continue
		}
		host := net.JoinHostPort(ip, strconv.Itoa(int(seederdeploy.TorrentHTTPPort)))
		return fmt.Sprintf("http://%s/torrents/%s", host, hash.String()), nil
	}
	return "", &NotReadyError{Reason: fmt.Sprintf("no ready seeder pod yet for content %s", name)}
}

// seederNetworkName returns the namespace-qualified NetworkAttachmentDefinition
// seeder pods in ns attach through, or "" when no Subnet there hosts seeders.
func (b *Builder) seederNetworkName(ctx context.Context, ns string) (string, error) {
	res, ok, err := sitederive.ResolveNamespaceSeeder(ctx, b.Client, ns)
	if err != nil {
		return "", fmt.Errorf("resolving seeder network for namespace %s: %w", ns, err)
	}
	if !ok || res.SeederNetworkRef == nil {
		return "", nil
	}
	nadNS := res.SeederNetworkRef.Namespace
	if nadNS == "" {
		nadNS = res.Subnet.Namespace
	}
	return nadNS + "/" + res.SeederNetworkRef.Name, nil
}

// multusNetworkStatusAnnotation is where Multus records the interfaces it
// attached to a pod, including the address each one was assigned.
const multusNetworkStatusAnnotation = "k8s.v1.cni.cncf.io/network-status"

// multusNetworkStatus is the subset of one Multus network-status entry
// this package reads.
type multusNetworkStatus struct {
	Name string   `json:"name"`
	IPs  []string `json:"ips"`
}

// seederPodIP returns the address a leecher must use to reach pod, which is
// not the same thing as the address Kubernetes reports.
//
// When a Subnet designates a seeder network, that NAD is attached as a
// Multus *secondary* interface: Status.PodIP still holds the cluster CNI's
// address, which is by construction unreachable from the provisioning
// segment the target machine boots on. The reachable address lives only in
// Multus's network-status annotation, keyed by the NAD's own name.
//
// seederNet == "" means no Subnet hosts seeders (a Subnet-less lane, for
// example an image-only CI job), and Status.PodIP is then the only address
// there is - and the right one, since nothing is deploying over a
// provisioning segment in that topology.
//
// Returns "" when pod has no usable address yet, which the caller reports
// as not-ready rather than as a failure.
func seederPodIP(pod *corev1.Pod, seederNet string) string {
	if seederNet == "" {
		return pod.Status.PodIP
	}

	raw := pod.Annotations[multusNetworkStatusAnnotation]
	if raw == "" {
		return ""
	}
	var statuses []multusNetworkStatus
	if err := json.Unmarshal([]byte(raw), &statuses); err != nil {
		// Not this package's contract to police; treat an unreadable
		// annotation as "no address yet" so the caller retries.
		return ""
	}
	for _, status := range statuses {
		// Multus qualifies the name with the NAD's namespace, but has
		// not always done so; accept the bare name too.
		if status.Name != seederNet && status.Name != seederNet[strings.IndexByte(seederNet, '/')+1:] {
			continue
		}
		for _, ip := range status.IPs {
			if ip != "" {
				return ip
			}
		}
	}
	return ""
}
