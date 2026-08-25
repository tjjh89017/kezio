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
	"fmt"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
	"github.com/tjjh89017/kezio/internal/bootd"
)

// Labels every bootd Deployment and its pods carry.
const (
	bootdAppNameLabel      = "app.kubernetes.io/name"
	bootdAppNameValue      = "kezio"
	bootdAppComponentLabel = "app.kubernetes.io/component"
	bootdComponentValue    = "bootd"
	// bootdDeploymentSubnetLabel names the Subnet a bootd Deployment was
	// built for, as an at-a-glance selector; the controller itself maps a
	// watch event back to its Subnet via the owner reference, not this
	// label.
	bootdDeploymentSubnetLabel = "kezio.kojuro.date/bootd-subnet"
)

// bootdDeploymentNamePrefix identifies a Deployment as a Subnet's bootd
// instance, at a glance in `kubectl get deployments`.
const bootdDeploymentNamePrefix = "kezio-bootd-"

// bootdMaxNameLength is the Kubernetes Deployment name limit, kept well
// inside the 63-character DNS-1035 limit the ReplicaSet/Pod names
// generated from it must also satisfy.
const bootdMaxNameLength = 63

// bootdDHCPInterface is the Multus secondary interface name every bootd
// pod's boot-segment attachment gets.
const bootdDHCPInterface = "net1"

// bootdTFTPDir is the in-pod directory the fetch-boot-artifacts
// initContainer populates and bootd's TFTP server reads from.
const bootdTFTPDir = "/tftp"

// bootdRunDirMount is the writable directory for bootd's rendered
// dnsmasq config, dhcp-hostsfile, and leasefile; required since the
// container's root filesystem is read-only.
const bootdRunDirMount = "/run/bootd"

// bootdTFTPVolumeName and bootdRunVolumeName name the two emptyDir
// volumes every bootd pod mounts.
const (
	bootdTFTPVolumeName = "tftp"
	bootdRunVolumeName  = "run"
)

// multusNetworksAnnotation is the Multus CNI pod annotation that adds a
// secondary network attachment alongside the pod's default network -
// bootd keeps its default network and only adds the boot L2 segment as
// net1.
const multusNetworksAnnotation = "k8s.v1.cni.cncf.io/networks"

// resolveNamespace returns ref.Namespace when set, otherwise
// defaultNamespace - the namespace default every NameRef in this package
// shares.
func resolveNamespace(ref keziov1alpha3.NameRef, defaultNamespace string) string {
	if ref.Namespace != "" {
		return ref.Namespace
	}
	return defaultNamespace
}

// bootdDeploymentName returns the deterministic Deployment name for
// subnetName's bootd instance, so SubnetReconciler stays idempotent
// across reconciles.
func bootdDeploymentName(subnetName string) string {
	name := bootdDeploymentNamePrefix + subnetName
	if len(name) <= bootdMaxNameLength {
		return name
	}

	sum := sha256.Sum256([]byte(subnetName))
	suffix := "-" + hex.EncodeToString(sum[:])[:8]
	maxBaseLen := bootdMaxNameLength - len(bootdDeploymentNamePrefix) - len(suffix)
	return bootdDeploymentNamePrefix + subnetName[:maxBaseLen] + suffix
}

// bootdPodAnnotations returns the pod template annotations attaching
// subnet's BootdNetworkRef as a secondary Multus network. A bare NAD
// name is qualified with subnet's own namespace rather than relying on a
// resolution default.
func bootdPodAnnotations(subnet *keziov1alpha3.Subnet) map[string]string {
	ref := *subnet.Spec.BootdNetworkRef
	ns := resolveNamespace(ref, subnet.Namespace)
	return map[string]string{multusNetworksAnnotation: ns + "/" + ref.Name}
}

// bootdEnv builds the bootd container's environment from SubnetSpec/
// SubnetDHCP and cfg's cluster-wide agent/boot upstream URLs when set.
//
// BOOTD_BOOT_CONFIG_URL is derived, not configured: once
// cfg.BootUpstreamURL is set, bootd's own reverse proxy forwards
// /boot/... to it, so the value is always this Subnet's own
// bootdServerIP on bootd.DefaultProxyPort. Left unset when
// cfg.BootUpstreamURL is empty, since nothing serves /boot/... then.
func bootdEnv(subnet *keziov1alpha3.Subnet, cfg BootdDeploymentConfig) []corev1.EnvVar {
	leaseMode := subnet.Spec.DHCP.Mode == keziov1alpha3.SubnetDHCPModeLease
	env := []corev1.EnvVar{
		{Name: "BOOTD_SERVER_IP", Value: subnet.Spec.BootdServerIP},
		{Name: "BOOTD_PROVISIONING_CIDR", Value: subnet.Spec.CIDR},
		{Name: "BOOTD_DHCP_INTERFACE", Value: bootdDHCPInterface},
		{Name: "BOOTD_TFTP_DIR", Value: bootdTFTPDir},
		{Name: "BOOTD_LEASE_MODE", Value: strconv.FormatBool(leaseMode)},
	}
	if leaseMode {
		// The CRD requires Gateway in lease mode, so the nil branch is
		// unreachable for an admitted Subnet. Stamping the variable with
		// an empty value would tell bootd "this segment has no exit",
		// which is a claim only the Subnet gets to make - so an absent
		// Gateway leaves the variable unset and bootd refuses to start,
		// rather than silently inventing the answer.
		if subnet.Spec.DHCP.Gateway != nil {
			env = append(env, corev1.EnvVar{Name: "BOOTD_GATEWAY", Value: *subnet.Spec.DHCP.Gateway})
		}
		if subnet.Spec.DHCP.LeaseRangeStart != "" {
			env = append(env, corev1.EnvVar{Name: "BOOTD_LEASE_RANGE_START", Value: subnet.Spec.DHCP.LeaseRangeStart})
		}
		if subnet.Spec.DHCP.LeaseRangeEnd != "" {
			env = append(env, corev1.EnvVar{Name: "BOOTD_LEASE_RANGE_END", Value: subnet.Spec.DHCP.LeaseRangeEnd})
		}
	}
	if cfg.AgentUpstreamURL != "" {
		env = append(env, corev1.EnvVar{Name: "BOOTD_AGENT_UPSTREAM_URL", Value: cfg.AgentUpstreamURL})
	}
	if cfg.BootUpstreamURL != "" {
		env = append(env, corev1.EnvVar{Name: "BOOTD_BOOT_UPSTREAM_URL", Value: cfg.BootUpstreamURL})
		env = append(env, corev1.EnvVar{
			Name:  "BOOTD_BOOT_CONFIG_URL",
			Value: fmt.Sprintf("http://%s:%d", subnet.Spec.BootdServerIP, bootd.DefaultProxyPort),
		})
	}
	if cfg.HTTPBootURL != "" {
		env = append(env, corev1.EnvVar{Name: "BOOTD_HTTP_BOOT_URL", Value: cfg.HTTPBootURL})
	}
	return env
}

// buildBootdDeployment constructs the (not yet created) bootd Deployment
// for subnet. The caller (reconcileBootdDeployment) sets the owner
// reference.
//
// subnet.Spec.NodeSelector becomes the pod template's NodeSelector:
// nothing in the cluster can otherwise tell which node is on this
// broadcast domain.
func buildBootdDeployment(subnet *keziov1alpha3.Subnet, cfg BootdDeploymentConfig) *appsv1.Deployment {
	replicas := int32(1)
	labels := map[string]string{
		bootdAppNameLabel:          bootdAppNameValue,
		bootdAppComponentLabel:     bootdComponentValue,
		bootdDeploymentSubnetLabel: subnet.Name,
	}
	trueVal := true
	falseVal := false

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      bootdDeploymentName(subnet.Name),
			Namespace: subnet.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			// One replica per broadcast domain: two bootd pods on the same
			// segment would both answer every DHCPDISCOVER with no way for
			// firmware to prefer one.
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: bootdPodAnnotations(subnet)},
				Spec: corev1.PodSpec{
					ServiceAccountName: cfg.serviceAccountName(),
					NodeSelector:       nodeSelectorOrNil(subnet.Spec.NodeSelector),
					SecurityContext: &corev1.PodSecurityContext{
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
						// Matches the boot-artifacts image's non-root gid;
						// the tftp emptyDir is otherwise root:root 0755 and
						// fetch-boot-artifacts's `cp` fails EACCES without
						// this.
						FSGroup: ptr.To(int64(65532)),
					},
					InitContainers: []corev1.Container{{
						Name:  "fetch-boot-artifacts",
						Image: cfg.BootArtifactsImage,
						Command: []string{
							"cp", "-a",
							"/boot-artifacts/" + bootd.ShimFilename,
							"/boot-artifacts/" + bootd.GrubFilename,
							"/dest",
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: bootdTFTPVolumeName, MountPath: "/dest"},
						},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &falseVal,
							ReadOnlyRootFilesystem:   &trueVal,
							RunAsNonRoot:             &trueVal,
							Capabilities: &corev1.Capabilities{
								Drop: []corev1.Capability{"ALL"},
							},
						},
					}},
					Containers: []corev1.Container{{
						Name:  "bootd",
						Image: cfg.Image,
						Env:   bootdEnv(subnet, cfg),
						Ports: []corev1.ContainerPort{
							{Name: "proxydhcp", ContainerPort: 67, Protocol: corev1.ProtocolUDP},
							{Name: "pxe", ContainerPort: 4011, Protocol: corev1.ProtocolUDP},
							{Name: "proxy-http", ContainerPort: bootd.DefaultProxyPort, Protocol: corev1.ProtocolTCP},
							{Name: bootdTFTPVolumeName, ContainerPort: 69, Protocol: corev1.ProtocolUDP},
						},
						SecurityContext: &corev1.SecurityContext{
							// Root, unlike every other kezio container:
							// Kubernetes only grants added capabilities to a
							// uid-0 process's permitted/effective sets.
							RunAsUser:                ptr.To(int64(0)),
							RunAsGroup:               ptr.To(int64(0)),
							RunAsNonRoot:             &falseVal,
							AllowPrivilegeEscalation: &falseVal,
							ReadOnlyRootFilesystem:   &trueVal,
							Capabilities: &corev1.Capabilities{
								Drop: []corev1.Capability{"ALL"},
								Add:  bootdCapabilities(),
							},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: bootdTFTPVolumeName, MountPath: bootdTFTPDir, ReadOnly: true},
							{Name: bootdRunVolumeName, MountPath: bootdRunDirMount},
						},
						Resources: corev1.ResourceRequirements{
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("1"),
								corev1.ResourceMemory: resource.MustParse("256Mi"),
							},
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("100m"),
								corev1.ResourceMemory: resource.MustParse("64Mi"),
							},
						},
						// No livenessProbe: bootd's protocols are all UDP
						// with no lightweight check to piggyback on.
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{Path: "/readyz", Port: intstr.FromInt32(bootd.DefaultHealthProbePort)},
							},
							InitialDelaySeconds: 1,
							PeriodSeconds:       5,
						},
					}},
					Volumes: []corev1.Volume{
						{Name: bootdTFTPVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: bootdRunVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					},
				},
			},
		},
	}
}

// bootdCapabilities converts bootd.DnsmasqCapabilities into the bootd
// container's Capabilities.Add, so the two never drift apart.
func bootdCapabilities() []corev1.Capability {
	caps := make([]corev1.Capability, len(bootd.DnsmasqCapabilities))
	for i, c := range bootd.DnsmasqCapabilities {
		caps[i] = corev1.Capability(c)
	}
	return caps
}

// nodeSelectorOrNil returns selector unchanged when non-empty, or nil
// when empty - so an unconstrained Subnet never stamps a pod template
// with a stray nodeSelector: {}.
func nodeSelectorOrNil(selector map[string]string) map[string]string {
	if len(selector) == 0 {
		return nil
	}
	return selector
}

// deploymentAvailable reports whether dep's DeploymentAvailable
// condition is True.
func deploymentAvailable(dep *appsv1.Deployment) bool {
	for _, c := range dep.Status.Conditions {
		if c.Type == appsv1.DeploymentAvailable {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// deploymentUnavailableReason derives a Subnet Ready reason/message from
// dep's own Conditions, for causes the namespace prerequisite checks
// cannot enumerate up front (e.g. a nodeSelector matching no node).
// ReplicaFailure is checked first as most specific, then a stalled
// Progressing condition; both carry dep's own Reason/Message verbatim.
// An ordinary rollout in progress trips neither, falling through to a
// placeholder.
func deploymentUnavailableReason(dep *appsv1.Deployment) (reason, message string) {
	for _, c := range dep.Status.Conditions {
		if c.Type == appsv1.DeploymentReplicaFailure && c.Status == corev1.ConditionTrue {
			return c.Reason, fmt.Sprintf("bootd Deployment could not create its pod: %s", c.Message)
		}
	}
	for _, c := range dep.Status.Conditions {
		if c.Type == appsv1.DeploymentProgressing && c.Status == corev1.ConditionFalse {
			return c.Reason, fmt.Sprintf("bootd Deployment stopped progressing: %s", c.Message)
		}
	}
	return "BootdDeploymentUnavailable", "bootd Deployment for this Subnet has not become Available yet"
}
