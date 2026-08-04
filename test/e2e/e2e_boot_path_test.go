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

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"time"

	. "github.com/onsi/ginkgo/v2" // nolint:revive,staticcheck
	. "github.com/onsi/gomega"    // nolint:revive,staticcheck

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/agentapi"
	"github.com/tjjh89017/kezio/test/utils"
)

// bootPathEnabled gates the "Boot path (control-plane wiring)" Context:
// it only registers its specs when E2E_BOOT_PATH=true, the same
// "same suite behind an env gate" shape imagePathEnabled uses (see
// e2e_image_path_test.go), so the default fast-lane run's timing and the
// image-path stage's DEPLOYER setting are both untouched unless this is
// set.
//
// Scope: this exercises the boot server (internal/bootserver) and the
// agent registration server (internal/agentserver) - real production
// HTTP handlers, a real minted single-use token, a real Machine status
// update - against a simulated agent: a plain HTTP client standing in
// for kezio-agent, POSTing a fabricated hardware inventory instead of a
// live-booted machine actually reporting one. It does NOT drive a real
// PXE boot, GRUB, or a live kernel/initrd/squashfs fetch: that end of
// the flow needs a NIC on a real L2 broadcast domain talking to
// kezio-bootd's proxyDHCP/TFTP responder and a UEFI firmware actually
// executing GRUB, none of which a GitHub-hosted runner's pod network can
// provide, and none of which this stage's "prove the control-plane
// wiring" goal needs - see the workflow's boot-path job doc comment for
// the fuller reasoning and for where a real KubeVirt VM PXE boot stage
// would plug in as a separate, heavier job.
var bootPathEnabled = os.Getenv("E2E_BOOT_PATH") == "true"

// bootPathMachineName and bootPathMAC identify the Machine this stage
// creates and the MAC address it registers with the boot config server.
const (
	bootPathMachineName = "e2e-boot-node-01"
	bootPathMAC         = "aa:bb:cc:dd:ee:03"
)

// bootPathDiskSerial is the disk serial number both the Machine's
// spec.targetDisk hint and the simulated agent's reported inventory use,
// so this stage also exercises the disk-serial-hint matching path the
// hint fields (api/v1alpha1.TargetDiskHints) exist for - even though
// this milestone stops at Available, before anything ever resolves that
// hint against a real deployment.
const bootPathDiskSerial = "KEZIOE2EBOOTPATH01"

// bootPathStateTimeout bounds how long the Machine takes to reach each
// state this stage waits on. The agent-driven Deployer's Inspect phase
// polls every agentInspectPollInterval (5s, internal/deployer/agent.go)
// once registration lands, so this only needs to be generous enough for
// a handful of reconciler ticks plus one webhook-admitted create - not
// as long as a real PXE + live boot cycle would need.
const bootPathStateTimeout = 2 * time.Minute

// registerBootPathContext adds the "Boot path (control-plane wiring)"
// Context as a Context INSIDE the same Describe("Manager", Ordered, ...)
// container e2e_test.go declares, the same nesting reasoning
// registerImagePathContext's doc comment explains (this Context needs
// the namespace/CRDs/controller-manager the outer BeforeAll creates, and
// must run after "Image and Machine lifecycle" so its DEPLOYER=agent
// switch on the shared controller-manager Deployment never runs
// concurrently with that Context's fake-deployer assertions).
func registerBootPathContext() {
	Context("Boot path (control-plane wiring)", func() {
		var (
			bootServerLocal  = "127.0.0.1:18090"
			agentServerLocal = "127.0.0.1:18091"
			portForwards     []*exec.Cmd
		)

		BeforeAll(func() {
			By("deploying the boot config and agent registration Services and switching to DEPLOYER=agent")
			runMake("deploy-boot-path", "IMG="+projectImage)

			By("waiting for the controller-manager to roll out under the new env")
			waitForRollout("kezio-controller-manager")

			By("port-forwarding the boot config and agent registration servers")
			portForwards = append(portForwards, mustPortForward("svc/kezio-boot-server", bootServerLocal, "8090"))
			portForwards = append(portForwards, mustPortForward("svc/kezio-agent-server", agentServerLocal, "8091"))
		})

		AfterAll(func() {
			By("stopping port-forwards")
			for _, pf := range portForwards {
				stopPortForward(pf)
			}

			By("deleting the boot-path Machine")
			deleteAndWait("machine", bootPathMachineName, time.Minute)
		})

		AfterEach(func() {
			if CurrentSpecReport().Failed() {
				dumpBootPathDiagnostics()
			}
		})

		It("mints a boot token, accepts a simulated agent registration, and reaches Available", func() {
			By("creating the Machine")
			applyManifest(fmt.Sprintf(`
apiVersion: kezio.kojuro.date/v1alpha1
kind: Machine
metadata:
  name: %s
  namespace: %s
spec:
  bootMACAddress: %q
  online: true
  targetDisk:
    serialNumber: %s
`, bootPathMachineName, namespace, bootPathMAC, bootPathDiskSerial))

			By("waiting for the Machine to reach state Inspecting (needs a net boot)")
			Eventually(func(g Gomega) {
				state, err := getJSONPath("machine", bootPathMachineName, "{.status.state}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(state).To(Equal(keziov1alpha1.MachineStateInspecting))
			}).WithTimeout(bootPathStateTimeout).Should(Succeed())

			By("fetching grub.cfg-<mac> from the boot config server, as GRUB would")
			grubConfig := mustGetBootServer(bootServerLocal, "/boot/grub.cfg-"+bootPathMAC)
			Expect(grubConfig).To(ContainSubstring("boot=live"),
				"expected a live-boot net-boot config now that the Machine needs to net boot")

			token := mustExtractBootToken(grubConfig)
			Expect(token).NotTo(BeEmpty(), "expected a kezio.token= value in the rendered grub.cfg")

			By("registering with the agent server, as kezio-agent would after booting the live environment")
			registerResp := mustRegisterAgent(agentServerLocal, token, bootPathHardware())
			Expect(registerResp.MachineName).To(Equal(bootPathMachineName))

			By("waiting for the Machine to reach state Available with the reported hardware recorded")
			Eventually(func(g Gomega) {
				state, err := getJSONPath("machine", bootPathMachineName, "{.status.state}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(state).To(Equal(keziov1alpha1.MachineStateAvailable))
			}).WithTimeout(bootPathStateTimeout).Should(Succeed())

			serial, err := getJSONPath("machine", bootPathMachineName, "{.status.hardware.disks[0].serialNumber}")
			Expect(err).NotTo(HaveOccurred())
			Expect(serial).To(Equal(bootPathDiskSerial),
				"expected the simulated agent's reported disk serial to match the targetDisk hint")

			nics, err := getJSONPath("machine", bootPathMachineName, "{.status.hardware.nics[*].macAddress}")
			Expect(err).NotTo(HaveOccurred())
			Expect(nics).To(ContainSubstring(bootPathMAC))

			memory, err := getJSONPath("machine", bootPathMachineName, "{.status.hardware.memoryBytes}")
			Expect(err).NotTo(HaveOccurred())
			Expect(memory).NotTo(BeEmpty())

			cpu, err := getJSONPath("machine", bootPathMachineName, "{.status.hardware.cpuCount}")
			Expect(err).NotTo(HaveOccurred())
			Expect(cpu).NotTo(BeEmpty())

			registeredStatus, err := getJSONPath("machine", bootPathMachineName,
				`{.status.conditions[?(@.type=="AgentRegistered")].status}`)
			Expect(err).NotTo(HaveOccurred())
			Expect(registeredStatus).To(Equal("True"))

			By("re-fetching grub.cfg-<mac>: an Available machine boots its local disk, not the live environment")
			grubConfigAfter := mustGetBootServer(bootServerLocal, "/boot/grub.cfg-"+bootPathMAC)
			Expect(grubConfigAfter).NotTo(ContainSubstring("boot=live"),
				"expected a boot-local response now that the Machine no longer needs a net boot")
		})
	})
}

// bootTokenPattern extracts the kezio.token= cmdline value from a
// rendered grub.cfg (see internal/bootserver.renderNetBootConfig): the
// token runs to the next whitespace, matching how the live kernel
// cmdline itself delimits it.
var bootTokenPattern = regexp.MustCompile(`kezio\.token=(\S+)`)

// mustExtractBootToken pulls the token out of a rendered grub.cfg body,
// failing the current spec if the expected pattern is not present.
func mustExtractBootToken(grubConfig string) string {
	m := bootTokenPattern.FindStringSubmatch(grubConfig)
	ExpectWithOffset(1, m).To(HaveLen(2), "grub.cfg did not contain a kezio.token= value:\n%s", grubConfig)
	return m[1]
}

// bootPathHardware builds the fake hardware inventory the simulated
// agent registers with: one disk carrying bootPathDiskSerial (the same
// serial the Machine's spec.targetDisk hint names) and one NIC carrying
// bootPathMAC, plus plausible memory/CPU counts.
func bootPathHardware() keziov1alpha1.MachineHardwareStatus {
	return keziov1alpha1.MachineHardwareStatus{
		Disks: []keziov1alpha1.MachineHardwareDisk{
			{
				DeviceName:   "/dev/sda",
				SerialNumber: bootPathDiskSerial,
				Model:        "KEZIO-E2E-DISK",
				Vendor:       "QEMU",
				SizeBytes:    16 << 30, // 16 GiB, a plausible blank data disk
			},
		},
		Nics: []keziov1alpha1.MachineHardwareNIC{
			{Name: "eth0", MACAddress: bootPathMAC},
		},
		MemoryBytes: 2 << 30, // 2 GiB
		CPUCount:    2,
	}
}

// mustGetBootServer issues an HTTP GET against the boot config server's
// port-forwarded local address and returns the response body as a
// string, failing the current spec on any error or non-200 response -
// bootserver's own contract (see internal/bootserver.Server's doc
// comment) is that every "not netbooting right now" or "unknown MAC"
// case still answers 200 with a boot-local body, so a non-200 here is
// always unexpected.
func mustGetBootServer(localAddr, path string) string {
	resp, err := http.Get("http://" + localAddr + path) //nolint:noctx // e2e helper, not production code
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "GET %s failed", path)
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, resp.StatusCode).To(Equal(http.StatusOK), "GET %s: unexpected status; body:\n%s", path, body)
	return string(body)
}

// mustRegisterAgent POSTs to /agent/register with token as the bearer
// credential and hardware as the reported inventory, mirroring what
// kezio-agent's real registration call does (internal/agent), and
// returns the decoded response, failing the current spec on any error or
// non-200 response.
func mustRegisterAgent(
	localAddr, token string, hardware keziov1alpha1.MachineHardwareStatus,
) agentapi.RegisterResponse {
	body, err := json.Marshal(agentapi.RegisterRequest{Hardware: hardware})
	ExpectWithOffset(1, err).NotTo(HaveOccurred())

	req, err := http.NewRequest(http.MethodPost, "http://"+localAddr+agentapi.RegisterPath, bytes.NewReader(body))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "POST %s failed", agentapi.RegisterPath)
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, resp.StatusCode).To(Equal(http.StatusOK),
		"POST %s: unexpected status; body:\n%s", agentapi.RegisterPath, respBody)

	var out agentapi.RegisterResponse
	ExpectWithOffset(1, json.Unmarshal(respBody, &out)).To(Succeed())
	return out
}

// dumpBootPathDiagnostics prints the Machine, the controller-manager
// logs, and cluster events to GinkgoWriter on failure - the same shape
// e2e_image_path_test.go's dumpImagePathDiagnostics uses.
func dumpBootPathDiagnostics() {
	By("dumping boot-path diagnostics")
	cmd := exec.Command("kubectl", "get", "machine", bootPathMachineName, "-n", namespace, "-o", "yaml")
	if out, err := utils.Run(cmd); err == nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "Machine %s:\n%s\n", bootPathMachineName, out)
	}

	cmd = exec.Command("kubectl", "logs", "deployment/kezio-controller-manager", "-n", namespace, "--tail=500")
	if out, err := utils.Run(cmd); err == nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "controller-manager logs:\n%s\n", out)
	}

	cmd = exec.Command("kubectl", "get", "events", "-n", namespace, "--sort-by=.lastTimestamp")
	if out, err := utils.Run(cmd); err == nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "events:\n%s\n", out)
	}
}
