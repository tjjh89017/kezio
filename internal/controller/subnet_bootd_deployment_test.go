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
	"fmt"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	keziov1alpha3 "github.com/tjjh89017/kezio/api/v1alpha3"
	"github.com/tjjh89017/kezio/internal/bootd"
)

// testSubnetName is every testSubnet fixture's fixed name: the envtest
// specs in subnet_bootd_envtest_test.go assert against the deterministic
// Deployment name it produces (bootdDeploymentName), so it is a shared
// constant rather than a per-call parameter.
const testSubnetName = "rack-1"

// testLeaseRangeStart and testLeaseRangeEnd are the lease bounds every
// lease-mode fixture in this file uses.
const (
	testLeaseRangeStart = "192.0.2.10"
	testLeaseRangeEnd   = "192.0.2.20"
)

// testGateway is the segment router every gateway fixture in this file
// uses - deliberately outside testLeaseRangeStart/End, as a real
// segment's router is.
const testGateway = "192.0.2.1"

func testSubnet(namespace string, mutate ...func(*keziov1alpha3.Subnet)) *keziov1alpha3.Subnet {
	subnet := &keziov1alpha3.Subnet{
		ObjectMeta: metav1.ObjectMeta{Name: testSubnetName, Namespace: namespace},
		Spec: keziov1alpha3.SubnetSpec{
			SiteRef:         keziov1alpha3.NameRef{Name: "hq"},
			CIDR:            "192.0.2.0/24",
			BootdServerIP:   "192.0.2.2",
			BootdNetworkRef: &keziov1alpha3.NameRef{Name: "boot-nad"},
			DHCP:            &keziov1alpha3.SubnetDHCP{Mode: keziov1alpha3.SubnetDHCPModeProxy},
		},
	}
	for _, m := range mutate {
		m(subnet)
	}
	return subnet
}

func envValue(env []corev1.EnvVar, name string) (string, bool) {
	for _, e := range env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

// envMust returns env's value for name, failing the test if it is absent.
func envMust(t *testing.T, env []corev1.EnvVar, name string) string {
	t.Helper()
	got, ok := envValue(env, name)
	if !ok {
		t.Fatalf("env %s is not set", name)
	}
	return got
}

// TestBuildBootdDeploymentNamespaceAndNaming checks that the Deployment
// lands in the Subnet's own namespace and is named deterministically
// from the Subnet's name.
func TestBuildBootdDeploymentNamespaceAndNaming(t *testing.T) {
	subnet := testSubnet("site-hq")
	cfg := BootdDeploymentConfig{Image: "bootd:test", BootArtifactsImage: "boot-artifacts:test"}

	dep := buildBootdDeployment(subnet, cfg)

	if dep.Namespace != "site-hq" {
		t.Errorf("Deployment namespace = %q, want Subnet's own namespace %q", dep.Namespace, "site-hq")
	}
	if dep.Name != "kezio-bootd-rack-1" {
		t.Errorf("Deployment name = %q, want %q", dep.Name, "kezio-bootd-rack-1")
	}

	// Must be deterministic - reconcileBootdDeployment's
	// Get-then-Create/Update depends on it.
	again := buildBootdDeployment(subnet, cfg)
	if again.Name != dep.Name {
		t.Errorf("buildBootdDeployment is not deterministic: %q != %q", again.Name, dep.Name)
	}
}

// TestBootdDeploymentNameTruncatesLongSubnetNames checks that an
// over-length Subnet name still produces a valid (<=63 character)
// Deployment name, using a stable hash suffix rather than a bare
// truncation so two long names sharing a prefix cannot collide.
func TestBootdDeploymentNameTruncatesLongSubnetNames(t *testing.T) {
	longName := strings.Repeat("a", 80)
	name := bootdDeploymentName(longName)

	if len(name) > 63 {
		t.Fatalf("bootdDeploymentName(%d chars) = %d chars, want <= 63", len(longName), len(name))
	}
	if !strings.HasPrefix(name, bootdDeploymentNamePrefix) {
		t.Errorf("bootdDeploymentName(%q) = %q, want prefix %q", longName, name, bootdDeploymentNamePrefix)
	}

	other := strings.Repeat("a", 80) + "-different-suffix-not-fitting"
	if bootdDeploymentName(other) == name {
		t.Errorf("two distinct over-length subnet names produced the same Deployment name %q", name)
	}
}

// TestBuildBootdDeploymentEnv checks the required env
// (BOOTD_SERVER_IP/BOOTD_PROVISIONING_CIDR straight from the Subnet, the
// controller-default BOOTD_DHCP_INTERFACE/BOOTD_TFTP_DIR) and that the
// agent/boot upstream URLs only appear when cfg sets them.
func TestBuildBootdDeploymentEnv(t *testing.T) {
	subnet := testSubnet("site-hq")

	t.Run("upstream URLs unset", func(t *testing.T) {
		cfg := BootdDeploymentConfig{Image: "bootd:test", BootArtifactsImage: "boot-artifacts:test"}
		env := bootdContainer(t, buildBootdDeployment(subnet, cfg)).Env

		wantEnv := map[string]string{
			"BOOTD_SERVER_IP":         "192.0.2.2",
			"BOOTD_PROVISIONING_CIDR": "192.0.2.0/24",
			"BOOTD_DHCP_INTERFACE":    "net1",
			"BOOTD_TFTP_DIR":          "/tftp",
			"BOOTD_LEASE_MODE":        "false",
		}
		for name, want := range wantEnv {
			got, ok := envValue(env, name)
			if !ok || got != want {
				t.Errorf("env %s = %q (present=%v), want %q", name, got, ok, want)
			}
		}
		for _, unwanted := range []string{
			"BOOTD_AGENT_UPSTREAM_URL", "BOOTD_BOOT_UPSTREAM_URL", "BOOTD_BOOT_CONFIG_URL",
			"BOOTD_LEASE_RANGE_START", "BOOTD_LEASE_RANGE_END",
		} {
			if _, ok := envValue(env, unwanted); ok {
				t.Errorf("env %s is set, want unset when not configured", unwanted)
			}
		}
	})

	t.Run("upstream URLs set", func(t *testing.T) {
		cfg := BootdDeploymentConfig{
			Image:              "bootd:test",
			BootArtifactsImage: "boot-artifacts:test",
			AgentUpstreamURL:   "http://kezio-agent-server.kezio-system.svc.cluster.local:8091",
			BootUpstreamURL:    "http://kezio-boot-server.kezio-system.svc.cluster.local:8090",
		}
		env := bootdContainer(t, buildBootdDeployment(subnet, cfg)).Env

		if got, _ := envValue(env, "BOOTD_AGENT_UPSTREAM_URL"); got != cfg.AgentUpstreamURL {
			t.Errorf("BOOTD_AGENT_UPSTREAM_URL = %q, want %q", got, cfg.AgentUpstreamURL)
		}
		if got, _ := envValue(env, "BOOTD_BOOT_UPSTREAM_URL"); got != cfg.BootUpstreamURL {
			t.Errorf("BOOTD_BOOT_UPSTREAM_URL = %q, want %q", got, cfg.BootUpstreamURL)
		}
		// Derived from bootdServerIP, since that's where bootd's own
		// reverse proxy (enabled by BootUpstreamURL) answers /boot/... .
		if got, want := envMust(t, env, "BOOTD_BOOT_CONFIG_URL"), fmt.Sprintf("http://192.0.2.2:%d", bootd.DefaultProxyPort); got != want {
			t.Errorf("BOOTD_BOOT_CONFIG_URL = %q, want %q", got, want)
		}
	})

	t.Run("boot upstream URL set without agent upstream still derives the config URL", func(t *testing.T) {
		cfg := BootdDeploymentConfig{
			Image:              "bootd:test",
			BootArtifactsImage: "boot-artifacts:test",
			BootUpstreamURL:    "http://kezio-boot-server.kezio-system.svc.cluster.local:8090",
		}
		env := bootdContainer(t, buildBootdDeployment(subnet, cfg)).Env

		if got, want := envMust(t, env, "BOOTD_BOOT_CONFIG_URL"), fmt.Sprintf("http://192.0.2.2:%d", bootd.DefaultProxyPort); got != want {
			t.Errorf("BOOTD_BOOT_CONFIG_URL = %q, want %q", got, want)
		}
		if _, ok := envValue(env, "BOOTD_AGENT_UPSTREAM_URL"); ok {
			t.Errorf("BOOTD_AGENT_UPSTREAM_URL is set, want unset when cfg leaves it empty")
		}
	})

	t.Run("HTTP Boot URL override passes through, and only when set", func(t *testing.T) {
		cfg := BootdDeploymentConfig{
			Image:              "bootd:test",
			BootArtifactsImage: "boot-artifacts:test",
			HTTPBootURL:        "http://192.0.2.2/boot/http/grubx64.efi",
		}
		env := bootdContainer(t, buildBootdDeployment(subnet, cfg)).Env

		if got, _ := envValue(env, "BOOTD_HTTP_BOOT_URL"); got != cfg.HTTPBootURL {
			t.Errorf("BOOTD_HTTP_BOOT_URL = %q, want %q", got, cfg.HTTPBootURL)
		}

		cfg.HTTPBootURL = ""
		env = bootdContainer(t, buildBootdDeployment(subnet, cfg)).Env
		// Unset must stay absent, not become empty: cmd/bootd treats the
		// variable's presence as the override, and an empty value would
		// silence its own derivation.
		if _, ok := envValue(env, "BOOTD_HTTP_BOOT_URL"); ok {
			t.Errorf("BOOTD_HTTP_BOOT_URL is set, want unset when cfg leaves it empty")
		}
	})

	t.Run("agent upstream URL alone does not derive the boot config URL", func(t *testing.T) {
		cfg := BootdDeploymentConfig{
			Image:              "bootd:test",
			BootArtifactsImage: "boot-artifacts:test",
			AgentUpstreamURL:   "http://kezio-agent-server.kezio-system.svc.cluster.local:8091",
		}
		env := bootdContainer(t, buildBootdDeployment(subnet, cfg)).Env

		// The reverse proxy only forwards /boot/... once BootUpstreamURL
		// is set, so BOOTD_BOOT_CONFIG_URL must stay unset here rather
		// than pointing a netbooting machine at a 404.
		if _, ok := envValue(env, "BOOTD_BOOT_CONFIG_URL"); ok {
			t.Errorf("BOOTD_BOOT_CONFIG_URL is set, want unset when BootUpstreamURL is not configured")
		}
	})

	// BOOTD_GATEWAY has to carry all three of the Subnet's states, and
	// "set to the empty string" must stay distinguishable from "not set":
	// cmd/bootd reads it with os.LookupEnv, where the first means "this
	// segment has no exit" and the second is a startup error.
	t.Run("lease mode carries all three gateway states", func(t *testing.T) {
		cfg := BootdDeploymentConfig{Image: "bootd:test", BootArtifactsImage: "boot-artifacts:test"}

		leaseSubnetWithGateway := func(gateway *string) *keziov1alpha3.Subnet {
			return testSubnet("site-hq", func(s *keziov1alpha3.Subnet) {
				s.Spec.DHCP = &keziov1alpha3.SubnetDHCP{
					Mode:    keziov1alpha3.SubnetDHCPModeLease,
					Gateway: gateway,
				}
			})
		}

		env := bootdContainer(t, buildBootdDeployment(leaseSubnetWithGateway(ptr.To(testGateway)), cfg)).Env
		if got := envMust(t, env, "BOOTD_GATEWAY"); got != testGateway {
			t.Errorf("BOOTD_GATEWAY = %q, want %q", got, testGateway)
		}

		env = bootdContainer(t, buildBootdDeployment(leaseSubnetWithGateway(ptr.To("")), cfg)).Env
		got, ok := envValue(env, "BOOTD_GATEWAY")
		if !ok {
			t.Errorf("BOOTD_GATEWAY is absent for a no-exit segment, want present and empty")
		}
		if got != "" {
			t.Errorf("BOOTD_GATEWAY = %q, want the empty string", got)
		}

		// The CRD rejects this, so it is unreachable for an admitted
		// Subnet; the builder must still not invent an answer, because an
		// empty value would claim the segment has no exit.
		env = bootdContainer(t, buildBootdDeployment(leaseSubnetWithGateway(nil), cfg)).Env
		if _, ok := envValue(env, "BOOTD_GATEWAY"); ok {
			t.Errorf("BOOTD_GATEWAY is set, want unset when the Subnet names no gateway at all")
		}
	})

	t.Run("lease mode carries the range bounds", func(t *testing.T) {
		leaseSubnet := testSubnet("site-hq", func(s *keziov1alpha3.Subnet) {
			s.Spec.DHCP = &keziov1alpha3.SubnetDHCP{
				Mode:            keziov1alpha3.SubnetDHCPModeLease,
				LeaseRangeStart: testLeaseRangeStart,
				LeaseRangeEnd:   testLeaseRangeEnd,
			}
		})
		cfg := BootdDeploymentConfig{Image: "bootd:test", BootArtifactsImage: "boot-artifacts:test"}
		env := bootdContainer(t, buildBootdDeployment(leaseSubnet, cfg)).Env

		if got, _ := envValue(env, "BOOTD_LEASE_MODE"); got != "true" {
			t.Errorf("BOOTD_LEASE_MODE = %q, want %q", got, "true")
		}
		if got, _ := envValue(env, "BOOTD_LEASE_RANGE_START"); got != testLeaseRangeStart {
			t.Errorf("BOOTD_LEASE_RANGE_START = %q, want %q", got, testLeaseRangeStart)
		}
		if got, _ := envValue(env, "BOOTD_LEASE_RANGE_END"); got != testLeaseRangeEnd {
			t.Errorf("BOOTD_LEASE_RANGE_END = %q, want %q", got, testLeaseRangeEnd)
		}
	})

	t.Run("proxy mode omits range bounds even if the Subnet somehow carries them", func(t *testing.T) {
		proxySubnet := testSubnet("site-hq", func(s *keziov1alpha3.Subnet) {
			s.Spec.DHCP = &keziov1alpha3.SubnetDHCP{
				Mode:            keziov1alpha3.SubnetDHCPModeProxy,
				LeaseRangeStart: testLeaseRangeStart,
				LeaseRangeEnd:   testLeaseRangeEnd,
			}
		})
		cfg := BootdDeploymentConfig{Image: "bootd:test", BootArtifactsImage: "boot-artifacts:test"}
		env := bootdContainer(t, buildBootdDeployment(proxySubnet, cfg)).Env
		if got, _ := envValue(env, "BOOTD_LEASE_MODE"); got != "false" {
			t.Errorf("BOOTD_LEASE_MODE = %q, want %q", got, "false")
		}
		if _, ok := envValue(env, "BOOTD_LEASE_RANGE_START"); ok {
			t.Errorf("BOOTD_LEASE_RANGE_START is set, want unset in proxy mode even when the Subnet carries it")
		}
		if _, ok := envValue(env, "BOOTD_LEASE_RANGE_END"); ok {
			t.Errorf("BOOTD_LEASE_RANGE_END is set, want unset in proxy mode even when the Subnet carries it")
		}
	})
}

// TestBuildBootdDeploymentEnvFullSet pins the exact env set a fully
// configured Subnet/cfg pair produces, rather than spot-checking
// individual keys, so a silently omitted variable shows up as a missing
// entry.
func TestBuildBootdDeploymentEnvFullSet(t *testing.T) {
	subnet := testSubnet("site-hq", func(s *keziov1alpha3.Subnet) {
		s.Spec.DHCP = &keziov1alpha3.SubnetDHCP{
			Mode:            keziov1alpha3.SubnetDHCPModeLease,
			LeaseRangeStart: testLeaseRangeStart,
			LeaseRangeEnd:   testLeaseRangeEnd,
		}
	})
	cfg := BootdDeploymentConfig{
		Image:              "bootd:test",
		BootArtifactsImage: "boot-artifacts:test",
		AgentUpstreamURL:   "http://kezio-agent-server.kezio-system.svc.cluster.local:8091",
		BootUpstreamURL:    "http://kezio-boot-server.kezio-system.svc.cluster.local:8090",
	}

	want := []corev1.EnvVar{
		{Name: "BOOTD_SERVER_IP", Value: "192.0.2.2"},
		{Name: "BOOTD_PROVISIONING_CIDR", Value: "192.0.2.0/24"},
		{Name: "BOOTD_DHCP_INTERFACE", Value: "net1"},
		{Name: "BOOTD_TFTP_DIR", Value: "/tftp"},
		{Name: "BOOTD_LEASE_MODE", Value: "true"},
		{Name: "BOOTD_LEASE_RANGE_START", Value: testLeaseRangeStart},
		{Name: "BOOTD_LEASE_RANGE_END", Value: testLeaseRangeEnd},
		{Name: "BOOTD_AGENT_UPSTREAM_URL", Value: cfg.AgentUpstreamURL},
		{Name: "BOOTD_BOOT_UPSTREAM_URL", Value: cfg.BootUpstreamURL},
		{Name: "BOOTD_BOOT_CONFIG_URL", Value: fmt.Sprintf("http://192.0.2.2:%d", bootd.DefaultProxyPort)},
	}

	got := bootdContainer(t, buildBootdDeployment(subnet, cfg)).Env
	if !reflect.DeepEqual(got, want) {
		t.Errorf("bootd container env =\n%#v\nwant\n%#v", got, want)
	}
}

// TestBuildBootdDeploymentNADAnnotation checks that the pod template's
// Multus networks annotation names subnet's own BootdNetworkRef,
// qualified with the Subnet's namespace.
func TestBuildBootdDeploymentNADAnnotation(t *testing.T) {
	subnet := testSubnet("site-hq", func(s *keziov1alpha3.Subnet) {
		s.Spec.BootdNetworkRef = &keziov1alpha3.NameRef{Name: "boot-nad"}
	})
	cfg := BootdDeploymentConfig{Image: "bootd:test", BootArtifactsImage: "boot-artifacts:test"}

	dep := buildBootdDeployment(subnet, cfg)
	want := "site-hq/boot-nad"
	if got := dep.Spec.Template.Annotations[multusNetworksAnnotation]; got != want {
		t.Errorf("pod annotation %s = %q, want %q", multusNetworksAnnotation, got, want)
	}
}

// TestBuildBootdDeploymentServiceAccount checks the default and override
// paths for the pod's ServiceAccountName.
func TestBuildBootdDeploymentServiceAccount(t *testing.T) {
	subnet := testSubnet("site-hq")

	dep := buildBootdDeployment(subnet, BootdDeploymentConfig{Image: "bootd:test", BootArtifactsImage: "boot-artifacts:test"})
	if got := dep.Spec.Template.Spec.ServiceAccountName; got != "kezio-bootd" {
		t.Errorf("default ServiceAccountName = %q, want %q (config/bootd/kustomization.yaml's namePrefix: kezio- "+
			"applied to rbac.yaml's \"bootd\" ServiceAccount)", got, "kezio-bootd")
	}

	dep = buildBootdDeployment(subnet, BootdDeploymentConfig{
		Image: "bootd:test", BootArtifactsImage: "boot-artifacts:test", ServiceAccountName: "custom-bootd-sa",
	})
	if got := dep.Spec.Template.Spec.ServiceAccountName; got != "custom-bootd-sa" {
		t.Errorf("overridden ServiceAccountName = %q, want %q", got, "custom-bootd-sa")
	}
}

// TestBuildBootdDeploymentReplicasAndSelector checks that exactly one
// replica is requested and the Deployment's own selector matches its pod
// template's labels - the "exactly one responder per broadcast domain"
// invariant this reconciler must not silently break.
func TestBuildBootdDeploymentReplicasAndSelector(t *testing.T) {
	subnet := testSubnet("site-hq")
	dep := buildBootdDeployment(subnet, BootdDeploymentConfig{Image: "bootd:test", BootArtifactsImage: "boot-artifacts:test"})

	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 1 {
		t.Errorf("Replicas = %v, want 1", dep.Spec.Replicas)
	}
	for k, v := range dep.Spec.Selector.MatchLabels {
		if dep.Spec.Template.Labels[k] != v {
			t.Errorf("selector label %s=%q not present on pod template labels %v", k, v, dep.Spec.Template.Labels)
		}
	}
}

// TestBuildBootdDeploymentNodeSelector checks that the pod template's
// NodeSelector comes straight from the Subnet's own NodeSelector, and
// that an empty or nil selector leaves the pod unconstrained rather than
// stamping an empty map.
func TestBuildBootdDeploymentNodeSelector(t *testing.T) {
	cfg := BootdDeploymentConfig{Image: "bootd:test", BootArtifactsImage: "boot-artifacts:test"}

	t.Run("propagates the Subnet's own NodeSelector", func(t *testing.T) {
		subnet := testSubnet("site-hq", func(s *keziov1alpha3.Subnet) {
			s.Spec.NodeSelector = map[string]string{"segment": "rack-1-vlan"}
		})
		dep := buildBootdDeployment(subnet, cfg)
		got := dep.Spec.Template.Spec.NodeSelector
		want := map[string]string{"segment": "rack-1-vlan"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("pod NodeSelector = %#v, want %#v", got, want)
		}
	})

	t.Run("empty NodeSelector leaves the pod unconstrained, not an empty map", func(t *testing.T) {
		subnet := testSubnet("site-hq", func(s *keziov1alpha3.Subnet) {
			s.Spec.NodeSelector = map[string]string{}
		})
		dep := buildBootdDeployment(subnet, cfg)
		if got := dep.Spec.Template.Spec.NodeSelector; got != nil {
			t.Errorf("pod NodeSelector = %#v, want nil (unconstrained), not an empty map", got)
		}
	})

	t.Run("nil NodeSelector leaves the pod unconstrained", func(t *testing.T) {
		subnet := testSubnet("site-hq")
		dep := buildBootdDeployment(subnet, cfg)
		if got := dep.Spec.Template.Spec.NodeSelector; got != nil {
			t.Errorf("pod NodeSelector = %#v, want nil for an unset NodeSelector", got)
		}
	})
}

// TestBuildBootdDeploymentContainerSecurityContext pins the bootd
// container's SecurityContext, including Capabilities, against
// bootd.DnsmasqCapabilities rather than a second hardcoded literal, so
// drift between the two fails here instead of producing a
// CrashLoopBackOff (dnsmasq needs NET_ADMIN/NET_RAW to start).
func TestBuildBootdDeploymentContainerSecurityContext(t *testing.T) {
	subnet := testSubnet("site-hq")
	cfg := BootdDeploymentConfig{Image: "bootd:test", BootArtifactsImage: "boot-artifacts:test"}

	got := bootdContainer(t, buildBootdDeployment(subnet, cfg)).SecurityContext

	wantCaps := make([]corev1.Capability, len(bootd.DnsmasqCapabilities))
	for i, c := range bootd.DnsmasqCapabilities {
		wantCaps[i] = corev1.Capability(c)
	}

	trueVal, falseVal := true, false
	want := &corev1.SecurityContext{
		RunAsUser:                ptr.To(int64(0)),
		RunAsGroup:               ptr.To(int64(0)),
		RunAsNonRoot:             &falseVal,
		AllowPrivilegeEscalation: &falseVal,
		ReadOnlyRootFilesystem:   &trueVal,
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
			Add:  wantCaps,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("bootd container SecurityContext =\n%#v\nwant\n%#v", got, want)
	}
	if !reflect.DeepEqual(wantCaps, []corev1.Capability{"NET_BIND_SERVICE", "NET_ADMIN", "NET_RAW"}) {
		t.Errorf("bootd.DnsmasqCapabilities = %#v, want exactly NET_BIND_SERVICE, NET_ADMIN, NET_RAW", wantCaps)
	}
}

// TestBuildBootdDeploymentContainerPorts pins the bootd container's exact
// ContainerPort set (proxyDHCP, PXE, reverse proxy, TFTP), so
// NetworkPolicy/kubectl tooling sees the pod's real surface.
func TestBuildBootdDeploymentContainerPorts(t *testing.T) {
	subnet := testSubnet("site-hq")
	cfg := BootdDeploymentConfig{Image: "bootd:test", BootArtifactsImage: "boot-artifacts:test"}

	got := bootdContainer(t, buildBootdDeployment(subnet, cfg)).Ports

	want := []corev1.ContainerPort{
		{Name: "proxydhcp", ContainerPort: 67, Protocol: corev1.ProtocolUDP},
		{Name: "pxe", ContainerPort: 4011, Protocol: corev1.ProtocolUDP},
		{Name: "proxy-http", ContainerPort: bootd.DefaultProxyPort, Protocol: corev1.ProtocolTCP},
		{Name: "tftp", ContainerPort: 69, Protocol: corev1.ProtocolUDP},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("bootd container Ports =\n%#v\nwant\n%#v", got, want)
	}
}

// TestBuildBootdDeploymentReadinessProbe pins the ReadinessProbe and
// checks no LivenessProbe is set: dnsmasq already restarts itself with
// backoff, so a liveness probe would fight that supervisor. The port is
// checked against bootd.DefaultHealthProbePort rather than a literal, so
// a changed default fails here instead of `kubectl rollout status`
// reporting success before bootd is actually listening.
func TestBuildBootdDeploymentReadinessProbe(t *testing.T) {
	subnet := testSubnet("site-hq")
	cfg := BootdDeploymentConfig{Image: "bootd:test", BootArtifactsImage: "boot-artifacts:test"}

	c := bootdContainer(t, buildBootdDeployment(subnet, cfg))

	want := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{Path: "/readyz", Port: intstr.FromInt32(bootd.DefaultHealthProbePort)},
		},
		InitialDelaySeconds: 1,
		PeriodSeconds:       5,
	}
	if !reflect.DeepEqual(c.ReadinessProbe, want) {
		t.Errorf("bootd container ReadinessProbe =\n%#v\nwant\n%#v", c.ReadinessProbe, want)
	}
	if c.LivenessProbe != nil {
		t.Errorf("bootd container LivenessProbe = %#v, want nil", c.LivenessProbe)
	}
}

// TestBuildBootdDeploymentResources pins the bootd container's resource
// limits and requests, since a silently dropped entry would not fail any
// other test.
func TestBuildBootdDeploymentResources(t *testing.T) {
	subnet := testSubnet("site-hq")
	cfg := BootdDeploymentConfig{Image: "bootd:test", BootArtifactsImage: "boot-artifacts:test"}

	got := bootdContainer(t, buildBootdDeployment(subnet, cfg)).Resources

	want := corev1.ResourceRequirements{
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("bootd container Resources =\n%#v\nwant\n%#v", got, want)
	}
}

// TestBuildBootdDeploymentInitContainer pins the fetch-boot-artifacts
// initContainer's image, command, mount and SecurityContext: a broken
// command or wrong mount path would leave the "tftp" emptyDir empty,
// which the TFTP server degrades to a per-file error rather than a crash.
func TestBuildBootdDeploymentInitContainer(t *testing.T) {
	subnet := testSubnet("site-hq")
	cfg := BootdDeploymentConfig{Image: "bootd:test", BootArtifactsImage: "boot-artifacts:test"}

	dep := buildBootdDeployment(subnet, cfg)
	if len(dep.Spec.Template.Spec.InitContainers) != 1 {
		t.Fatalf("InitContainers = %#v, want exactly 1", dep.Spec.Template.Spec.InitContainers)
	}
	got := dep.Spec.Template.Spec.InitContainers[0]

	trueVal, falseVal := true, false
	want := corev1.Container{
		Name:  "fetch-boot-artifacts",
		Image: "boot-artifacts:test",
		Command: []string{
			"cp", "-a",
			"/boot-artifacts/" + bootd.ShimFilename,
			"/boot-artifacts/" + bootd.GrubFilename,
			"/dest",
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "tftp", MountPath: "/dest"},
		},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: &falseVal,
			ReadOnlyRootFilesystem:   &trueVal,
			RunAsNonRoot:             &trueVal,
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("fetch-boot-artifacts initContainer =\n%#v\nwant\n%#v", got, want)
	}
}

// TestBuildBootdDeploymentPodSecurityContext pins the pod-level
// SecurityContext: the tftp emptyDir is created root:root 0755, and
// fetch-boot-artifacts's `cp` fails EACCES without the matching FSGroup -
// a failure envtest cannot observe, since it never runs a real kubelet.
func TestBuildBootdDeploymentPodSecurityContext(t *testing.T) {
	subnet := testSubnet("site-hq")
	cfg := BootdDeploymentConfig{Image: "bootd:test", BootArtifactsImage: "boot-artifacts:test"}

	got := buildBootdDeployment(subnet, cfg).Spec.Template.Spec.SecurityContext

	want := &corev1.PodSecurityContext{
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		FSGroup:        ptr.To(int64(65532)),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pod SecurityContext =\n%#v\nwant\n%#v", got, want)
	}
}

// TestBuildBootdDeploymentImage checks the bootd container's Image comes
// from cfg.Image, not cfg.BootArtifactsImage: envtest never pulls or runs
// an image, so a swapped reference here would only fail once a real pod
// tried to run the wrong binary.
func TestBuildBootdDeploymentImage(t *testing.T) {
	subnet := testSubnet("site-hq")
	cfg := BootdDeploymentConfig{Image: "bootd:test", BootArtifactsImage: "boot-artifacts:test"}

	got := bootdContainer(t, buildBootdDeployment(subnet, cfg)).Image
	if got != cfg.Image {
		t.Errorf("bootd container Image = %q, want cfg.Image %q", got, cfg.Image)
	}
}

// TestBuildBootdDeploymentVolumesAndMounts pins the pod's Volumes and the
// bootd container's VolumeMounts: dropping the "run" volume stays
// admission-valid, but with ReadOnlyRootFilesystem: true bootd then has
// nowhere to write its dnsmasq config/hostsfile/leasefile, a
// runtime-only failure invisible to any envtest spec.
func TestBuildBootdDeploymentVolumesAndMounts(t *testing.T) {
	subnet := testSubnet("site-hq")
	cfg := BootdDeploymentConfig{Image: "bootd:test", BootArtifactsImage: "boot-artifacts:test"}

	dep := buildBootdDeployment(subnet, cfg)

	wantVolumes := []corev1.Volume{
		{Name: "tftp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "run", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
	if got := dep.Spec.Template.Spec.Volumes; !reflect.DeepEqual(got, wantVolumes) {
		t.Errorf("pod Volumes =\n%#v\nwant\n%#v", got, wantVolumes)
	}

	wantMounts := []corev1.VolumeMount{
		{Name: "tftp", MountPath: "/tftp", ReadOnly: true},
		{Name: "run", MountPath: "/run/bootd"},
	}
	if got := bootdContainer(t, dep).VolumeMounts; !reflect.DeepEqual(got, wantMounts) {
		t.Errorf("bootd container VolumeMounts =\n%#v\nwant\n%#v", got, wantMounts)
	}
}

// TestBuildBootdDeploymentLabelValues checks label values on dep.Labels,
// dep.Spec.Selector.MatchLabels and dep.Spec.Template.Labels using two
// distinct Subnets, to prove the values genuinely diverge: two Subnets
// producing identical selectors would let their Deployments fight over
// the same pods.
func TestBuildBootdDeploymentLabelValues(t *testing.T) {
	cfg := BootdDeploymentConfig{Image: "bootd:test", BootArtifactsImage: "boot-artifacts:test"}

	rack1 := testSubnet("site-hq")
	rack2 := testSubnet("site-hq", func(s *keziov1alpha3.Subnet) { s.Name = "rack-2" })

	dep1 := buildBootdDeployment(rack1, cfg)
	dep2 := buildBootdDeployment(rack2, cfg)

	check := func(t *testing.T, dep *appsv1.Deployment, subnetName string) {
		t.Helper()
		want := map[string]string{
			bootdAppNameLabel:          bootdAppNameValue,
			bootdAppComponentLabel:     bootdComponentValue,
			bootdDeploymentSubnetLabel: subnetName,
		}
		if got := dep.Labels; !reflect.DeepEqual(got, want) {
			t.Errorf("Deployment Labels =\n%#v\nwant\n%#v", got, want)
		}
		if got := dep.Spec.Selector.MatchLabels; !reflect.DeepEqual(got, want) {
			t.Errorf("Selector.MatchLabels =\n%#v\nwant\n%#v", got, want)
		}
		if got := dep.Spec.Template.Labels; !reflect.DeepEqual(got, want) {
			t.Errorf("Template.Labels =\n%#v\nwant\n%#v", got, want)
		}
	}
	check(t, dep1, testSubnetName)
	check(t, dep2, "rack-2")

	if reflect.DeepEqual(dep1.Spec.Selector.MatchLabels, dep2.Spec.Selector.MatchLabels) {
		t.Errorf("two distinct Subnets produced identical selectors %#v: their Deployments would fight over the same pods",
			dep1.Spec.Selector.MatchLabels)
	}
}

// bootdContainer returns dep's "bootd" container, failing the test if it
// is not found.
func bootdContainer(t *testing.T, dep *appsv1.Deployment) corev1.Container {
	t.Helper()
	for _, c := range dep.Spec.Template.Spec.Containers {
		if c.Name == bootdComponentValue {
			return c
		}
	}
	t.Fatalf("no %q container found in Deployment %s/%s", bootdComponentValue, dep.Namespace, dep.Name)
	return corev1.Container{}
}
