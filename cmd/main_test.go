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

package main

import "testing"

// clearBootServerEnv unsets every environment variable
// bootServerConfigFromEnv reads, so each test starts from the
// "server off" baseline regardless of what the process environment (or
// an earlier test in this file) happens to carry.
func clearBootServerEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"BOOT_SERVER_ADDR", "BOOT_ARTIFACTS_DIR", "BOOT_SERVER_URL",
		"BOOT_AGENT_SERVER_URL", "BOOT_KERNEL_PATH", "BOOT_INITRD_PATH",
		"BOOT_EFI_DIR", "BOOT_TOKEN_TTL",
	} {
		t.Setenv(key, "")
	}
}

// TestBootServerConfigFromEnv_AgentServerURLDistinctFromServerURL pins
// the fix for the registration handshake that never completed: kezio.server=
// (renderNetBootConfig's cmdline value the agent registers against) must
// come from BOOT_AGENT_SERVER_URL, not silently collapse onto
// BOOT_SERVER_URL - internal/agentserver and internal/bootserver are two
// different Services on two different container ports fronting the same
// controller-manager Pod (config/bootserver/service.yaml:8090,
// config/agentserver/service.yaml:8091), so an agent told to register at
// the boot server's address gets a 404 from a mux that never mounts
// internal/agentserver's routes, forever, with nothing surfacing
// server-side.
func TestBootServerConfigFromEnv_AgentServerURLDistinctFromServerURL(t *testing.T) {
	clearBootServerEnv(t)
	t.Setenv("BOOT_SERVER_ADDR", ":8090")
	t.Setenv("BOOT_ARTIFACTS_DIR", "/boot-artifacts")
	t.Setenv("BOOT_SERVER_URL", "http://kezio-boot-server.kezio-system.svc.cluster.local:8090")
	t.Setenv("BOOT_AGENT_SERVER_URL", "http://kezio-agent-server.kezio-system.svc.cluster.local:8091")

	cfg, err := bootServerConfigFromEnv()
	if err != nil {
		t.Fatalf("bootServerConfigFromEnv() error = %v", err)
	}
	if cfg == nil {
		t.Fatal("bootServerConfigFromEnv() = nil, want a Config since BOOT_SERVER_ADDR is set")
	}
	if cfg.ServerURL != "http://kezio-boot-server.kezio-system.svc.cluster.local:8090" {
		t.Errorf("cfg.ServerURL = %q, want the boot server's own address", cfg.ServerURL)
	}
	if cfg.AgentServerURL != "http://kezio-agent-server.kezio-system.svc.cluster.local:8091" {
		t.Errorf("cfg.AgentServerURL = %q, want the agent server's own address, distinct from ServerURL", cfg.AgentServerURL)
	}
}

// TestBootServerConfigFromEnv_AgentServerURLUnsetLeftEmpty proves
// bootServerConfigFromEnv itself does not paper over a missing
// BOOT_AGENT_SERVER_URL by defaulting it to BOOT_SERVER_URL - that
// fallback belongs to bootserver.Config.withDefaults, which a deployment
// that genuinely shares one address for both servers can still rely on,
// but this function's job is only to read the environment faithfully.
func TestBootServerConfigFromEnv_AgentServerURLUnsetLeftEmpty(t *testing.T) {
	clearBootServerEnv(t)
	t.Setenv("BOOT_SERVER_ADDR", ":8090")
	t.Setenv("BOOT_ARTIFACTS_DIR", "/boot-artifacts")
	t.Setenv("BOOT_SERVER_URL", "http://kezio-boot-server.kezio-system.svc.cluster.local:8090")

	cfg, err := bootServerConfigFromEnv()
	if err != nil {
		t.Fatalf("bootServerConfigFromEnv() error = %v", err)
	}
	if cfg == nil {
		t.Fatal("bootServerConfigFromEnv() = nil, want a Config since BOOT_SERVER_ADDR is set")
	}
	if cfg.AgentServerURL != "" {
		t.Errorf("cfg.AgentServerURL = %q, want empty when BOOT_AGENT_SERVER_URL is unset", cfg.AgentServerURL)
	}
}

// TestBootServerConfigFromEnv_ServerOff proves the inert-by-default gate
// (BOOT_SERVER_ADDR unset) still returns a nil Config untouched by this
// change.
func TestBootServerConfigFromEnv_ServerOff(t *testing.T) {
	clearBootServerEnv(t)

	cfg, err := bootServerConfigFromEnv()
	if err != nil {
		t.Fatalf("bootServerConfigFromEnv() error = %v", err)
	}
	if cfg != nil {
		t.Errorf("bootServerConfigFromEnv() = %+v, want nil when BOOT_SERVER_ADDR is unset", cfg)
	}
}
