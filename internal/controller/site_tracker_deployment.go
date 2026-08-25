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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
)

// Labels every tracker Deployment and its pods carry.
const (
	trackerAppNameLabel      = "app.kubernetes.io/name"
	trackerAppNameValue      = "kezio"
	trackerAppComponentLabel = "app.kubernetes.io/component"
	trackerComponentValue    = "tracker"
	// trackerDeploymentSiteLabel names the Site a tracker Deployment was
	// built for, as an at-a-glance selector; the controller itself maps a
	// watch event back to its Site via the owner reference, not this label.
	trackerDeploymentSiteLabel = "kezio.kojuro.date/tracker-site"
)

// trackerDeploymentNamePrefix identifies a Deployment as a Site's tracker
// instance, at a glance in `kubectl get deployments`.
const trackerDeploymentNamePrefix = "kezio-tracker-"

// trackerMaxNameLength mirrors bootdMaxNameLength: comfortably inside the
// 63-character DNS-1035 limit the ReplicaSet/Pod names generated from it
// must also satisfy.
const trackerMaxNameLength = 63

// trackerAnnouncePort is opentracker's announce port, both TCP and UDP.
// Pinned (not configurable) since it is baked into every .torrent this
// Site's seeders serve via Site.Status.TrackerURL.
const trackerAnnouncePort = 6969

// trackerDeploymentName returns the deterministic Deployment name for
// siteName's tracker instance, so SiteReconciler stays idempotent across
// reconciles. Mirrors bootdDeploymentName's hash-suffix fallback for
// over-length names.
func trackerDeploymentName(siteName string) string {
	name := trackerDeploymentNamePrefix + siteName
	if len(name) <= trackerMaxNameLength {
		return name
	}

	sum := sha256.Sum256([]byte(siteName))
	suffix := "-" + hex.EncodeToString(sum[:])[:8]
	maxBaseLen := trackerMaxNameLength - len(trackerDeploymentNamePrefix) - len(suffix)
	return trackerDeploymentNamePrefix + siteName[:maxBaseLen] + suffix
}

// trackerNetworkSelectionElement is the subset of Multus's
// NetworkSelectionElement that both k8s.v1.cni.cncf.io/networks and
// v1.multus-cni.io/default-network parse the same way. IPs requests a
// specific address from the NAD's IPAM plugin instead of letting it
// allocate the next free one - the tracker needs exactly
// Site.Spec.Tracker.IP, the address baked into every .torrent this Site's
// seeder serves, not whatever the seeding NAD's pool would hand out next.
type trackerNetworkSelectionElement struct {
	Name      string   `json:"name"`
	Namespace string   `json:"namespace,omitempty"`
	IPs       []string `json:"ips,omitempty"`
}

// trackerIPWithPrefix returns ip in CIDR notation using subnetCIDR's own
// prefix length. Multus hands the bridge CNI plugin the ips selection
// element verbatim, and that plugin rejects a bare address ("the 'ip'
// field is expected to be in CIDR notation") - so ip needs a prefix, and
// the seeding Subnet's own is the only one that describes the broadcast
// domain the tracker pod actually joins.
func trackerIPWithPrefix(subnetCIDR, ip string) (string, error) {
	_, network, err := net.ParseCIDR(subnetCIDR)
	if err != nil {
		return "", fmt.Errorf("seeding Subnet cidr %q: %w", subnetCIDR, err)
	}
	ones, _ := network.Mask.Size()
	return fmt.Sprintf("%s/%d", ip, ones), nil
}

// trackerPodAnnotations returns the pod template annotations placing
// seedingSubnet's SeederNetworkRef as the pod's default (and only)
// network, pinned to ip in seedingSubnet's own CIDR notation - or nil
// when seedingSubnet carries no SeederNetworkRef at all (SiteReconciler.
// onChange returns a SeederNetworkRefMissing failure before this is ever
// called in that case, so callers never actually see the nil return
// today; it stays defensive rather than assuming that check can never
// change). An error means seedingSubnet.Spec.CIDR itself does not
// parse; the caller surfaces that as a Site misconfiguration rather
// than falling back to a bare address the bridge CNI plugin would
// reject.
//
// Single-homed via multusDefaultNetworkAnnotation, not the additive one,
// for the same no-NAT reason seederPodAnnotations is: a peer connects to
// the address the tracker's announce response advertises, which must be
// the address the tracker pod actually listens on.
func trackerPodAnnotations(seedingSubnet *keziov1alpha2.Subnet, ip string) (map[string]string, error) {
	if seedingSubnet.Spec.SeederNetworkRef == nil {
		return nil, nil
	}
	ipWithPrefix, err := trackerIPWithPrefix(seedingSubnet.Spec.CIDR, ip)
	if err != nil {
		return nil, err
	}
	ref := *seedingSubnet.Spec.SeederNetworkRef
	ns := resolveNamespace(ref, seedingSubnet.Namespace)
	elements := []trackerNetworkSelectionElement{{Name: ref.Name, Namespace: ns, IPs: []string{ipWithPrefix}}}
	// Marshaling a fixed struct of strings never fails.
	encoded, _ := json.Marshal(elements)
	return map[string]string{multusDefaultNetworkAnnotation: string(encoded)}, nil
}

// buildTrackerDeployment constructs the (not yet created) tracker
// Deployment for site, placed on seedingSubnet. The caller
// (reconcileTrackerDeployment) sets the owner reference.
//
// No Service fronts this Deployment: peers reach the tracker at ip
// directly, and a ClusterIP would DNAT that address, breaking the
// announce/reachability consistency BitTorrent's peer-to-peer connections
// depend on (see multusDefaultNetworkAnnotation's own doc comment).
//
// An error means seedingSubnet.Spec.CIDR does not parse (see
// trackerPodAnnotations); the caller surfaces that as a Site
// misconfiguration instead of creating a Deployment.
func buildTrackerDeployment(site *keziov1alpha2.Site, seedingSubnet *keziov1alpha2.Subnet, cfg TrackerDeploymentConfig) (*appsv1.Deployment, error) {
	annotations, err := trackerPodAnnotations(seedingSubnet, site.Spec.Tracker.IP)
	if err != nil {
		return nil, err
	}

	replicas := int32(1)
	labels := map[string]string{
		trackerAppNameLabel:        trackerAppNameValue,
		trackerAppComponentLabel:   trackerComponentValue,
		trackerDeploymentSiteLabel: site.Name,
	}
	trueVal, falseVal := true, false

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      trackerDeploymentName(site.Name),
			Namespace: site.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			// One replica: a second tracker pod would need its own pinned
			// address, and Site carries only one.
			Replicas: &replicas,
			// Recreate, not the default RollingUpdate: that one surges a
			// second pod before it deletes the outgoing one, and the pinned
			// address both pods request cannot be held by two pods at once -
			// the replacement pod's sandbox fails until the outgoing pod's
			// CNI DEL frees the address.
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: annotations,
				},
				Spec: corev1.PodSpec{
					NodeSelector: nodeSelectorOrNil(seedingSubnet.Spec.NodeSelector),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   &trueVal,
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:  "tracker",
						Image: cfg.Image,
						// The image sets CMD but no ENTRYPOINT (see
						// config/opentracker/opentracker-deployment.yaml), so
						// Command points at the binary directly and Args
						// passes flags to it.
						Command: []string{"/bin/opentracker"},
						Args:    []string{"-p", strconv.Itoa(trackerAnnouncePort)},
						Ports: []corev1.ContainerPort{
							{Name: "announce-tcp", ContainerPort: trackerAnnouncePort, Protocol: corev1.ProtocolTCP},
							{Name: "announce-udp", ContainerPort: trackerAnnouncePort, Protocol: corev1.ProtocolUDP},
						},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &falseVal,
							ReadOnlyRootFilesystem:   &trueVal,
							// The image ships no USER metadata; an arbitrary
							// non-root uid/gid satisfies runAsNonRoot since
							// opentracker needs no privilege beyond binding
							// its own unprivileged port.
							RunAsUser:  ptr.To(int64(65534)),
							RunAsGroup: ptr.To(int64(65534)),
							Capabilities: &corev1.Capabilities{
								Drop: []corev1.Capability{"ALL"},
							},
						},
						Resources: corev1.ResourceRequirements{
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("250m"),
								corev1.ResourceMemory: resource.MustParse("128Mi"),
							},
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("50m"),
								corev1.ResourceMemory: resource.MustParse("32Mi"),
							},
						},
					}},
				},
			},
		},
	}, nil
}

// trackerAnnounceURL builds the announce URL Site.Status.TrackerURL
// carries for a Site running its own tracker Deployment, bound to ip on
// trackerAnnouncePort.
func trackerAnnounceURL(ip string) string {
	return fmt.Sprintf("http://%s:%d/announce", ip, trackerAnnouncePort)
}

// trackerDeploymentUnavailableReason derives a Site Ready reason/message
// from dep's own Conditions. Mirrors deploymentUnavailableReason but with
// tracker-flavored text; that function's own messages name bootd
// specifically and would be misleading here.
func trackerDeploymentUnavailableReason(dep *appsv1.Deployment) (reason, message string) {
	for _, c := range dep.Status.Conditions {
		if c.Type == appsv1.DeploymentReplicaFailure && c.Status == corev1.ConditionTrue {
			return c.Reason, fmt.Sprintf("tracker Deployment could not create its pod: %s", c.Message)
		}
	}
	for _, c := range dep.Status.Conditions {
		if c.Type == appsv1.DeploymentProgressing && c.Status == corev1.ConditionFalse {
			return c.Reason, fmt.Sprintf("tracker Deployment stopped progressing: %s", c.Message)
		}
	}
	return "TrackerDeploymentUnavailable", "tracker Deployment for this Site has not become Available yet"
}
