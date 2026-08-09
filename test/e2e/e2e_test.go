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
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/tjjh89017/kezio/test/utils"
)

// namespace where the project is deployed in
const namespace = "kezio-system"

// serviceAccountName created for the project
const serviceAccountName = "kezio-controller-manager"

// metricsServiceName is the name of the metrics service of the project
const metricsServiceName = "kezio-controller-manager-metrics-service"

// metricsRoleBindingName is the name of the RBAC that will be created to allow get the metrics data
const metricsRoleBindingName = "kezio-metrics-binding"

// e2eImageName and e2eMachineName identify the Image and Machine created by
// the "Image and Machine lifecycle" context. They are declared here (rather
// than local to that Context) so AfterAll can clean them up regardless of
// which spec, if any, failed.
//
// e2eSiteName and e2eSubnetName back the Machine's now-required
// spec.subnetRef (Machine.Spec.NetworkSite is gone; subnetRef is
// mandatory). The referenced NAD (e2eBootNADName) is never created: with
// BOOTD_DEPLOYMENT_IMAGE unset in this lane, SubnetReconciler never
// attempts a bootd Deployment, and a missing NAD only ever yields an
// Indeterminate condition on the Subnet, never blocking the Machine's own
// lifecycle under the fake deployer.
const (
	e2eImageName    = "e2e-golden"
	e2eMachineName  = "e2e-node-01"
	e2eSiteName     = "e2e-site"
	e2eSubnetName   = "e2e-subnet"
	e2eBootNADName  = "e2e-boot-net"
	e2eSubnetCIDR   = "192.0.2.0/24"
	e2eSubnetBootIP = "192.0.2.10"
)

var _ = Describe("Manager", Ordered, func() {
	var controllerPodName string

	// Before running the tests, set up the environment by creating the namespace,
	// enforce the restricted security policy to the namespace, installing CRDs,
	// and deploying the controller.
	BeforeAll(func() {
		By("creating manager namespace")
		cmd := exec.Command("kubectl", "create", "ns", namespace)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create namespace")

		By("labeling the namespace to enforce the restricted security policy")
		cmd = exec.Command("kubectl", "label", "--overwrite", "ns", namespace,
			"pod-security.kubernetes.io/enforce=restricted")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to label namespace with restricted policy")

		By("installing CRDs")
		cmd = exec.Command("make", "install")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")

		By("deploying the controller-manager")
		cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", projectImage))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")
	})

	// After all tests have been executed, clean up by undeploying the controller, uninstalling CRDs,
	// and deleting the namespace.
	AfterAll(func() {
		By("cleaning up the curl pod for metrics")
		cmd := exec.Command("kubectl", "delete", "pod", "curl-metrics", "-n", namespace)
		_, _ = utils.Run(cmd)

		// The Image controller holds a finalizer on the Image while any
		// Machine references it, and only the running controller-manager
		// removes it. "make undeploy" below tears the controller-manager
		// down, so the Machine and Image must be deleted (and confirmed
		// gone) before that happens - otherwise the Image is left with a
		// finalizer nothing can ever clear, and namespace/CRD deletion
		// hangs forever. This must run even if an earlier spec in this
		// suite failed partway through, so it tolerates the objects
		// already being deleted or never having existed.
		By("deleting the e2e Machine and Image before undeploying the controller-manager")
		deleteAndWait("machine", e2eMachineName, time.Minute)
		deleteAndWait("image", e2eImageName, 2*time.Minute)

		By("deleting the e2e Subnet and Site")
		_, _ = utils.Run(exec.Command("kubectl", "delete", "subnet", e2eSubnetName, "-n", namespace, "--ignore-not-found"))
		_, _ = utils.Run(exec.Command("kubectl", "delete", "site", e2eSiteName, "-n", namespace, "--ignore-not-found"))

		By("undeploying the controller-manager")
		cmd = exec.Command("make", "undeploy")
		_, _ = utils.Run(cmd)

		By("uninstalling CRDs")
		cmd = exec.Command("make", "uninstall")
		_, _ = utils.Run(cmd)

		By("removing manager namespace")
		cmd = exec.Command("kubectl", "delete", "ns", namespace)
		_, _ = utils.Run(cmd)
	})

	// After each test, check for failures and collect logs, events,
	// and pod descriptions for debugging.
	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			By("Fetching controller manager pod logs")
			cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
			controllerLogs, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n %s", controllerLogs)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Controller logs: %s", err)
			}

			By("Fetching Kubernetes events")
			cmd = exec.Command("kubectl", "get", "events", "-n", namespace, "--sort-by=.lastTimestamp")
			eventsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Kubernetes events:\n%s", eventsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Kubernetes events: %s", err)
			}

			By("Fetching curl-metrics logs")
			cmd = exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
			metricsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Metrics logs:\n %s", metricsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get curl-metrics logs: %s", err)
			}

			By("Fetching controller manager pod description")
			cmd = exec.Command("kubectl", "describe", "pod", controllerPodName, "-n", namespace)
			podDescription, err := utils.Run(cmd)
			if err == nil {
				fmt.Println("Pod description:\n", podDescription)
			} else {
				fmt.Println("Failed to describe controller pod")
			}
		}
	})

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	Context("Manager", func() {
		It("should run successfully", func() {
			By("validating that the controller-manager pod is running as expected")
			verifyControllerUp := func(g Gomega) {
				// Get the name of the controller-manager pod
				cmd := exec.Command("kubectl", "get",
					"pods", "-l", "control-plane=controller-manager",
					"-o", "go-template={{ range .items }}"+
						"{{ if not .metadata.deletionTimestamp }}"+
						"{{ .metadata.name }}"+
						"{{ \"\\n\" }}{{ end }}{{ end }}",
					"-n", namespace,
				)

				podOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve controller-manager pod information")
				podNames := utils.GetNonEmptyLines(podOutput)
				g.Expect(podNames).To(HaveLen(1), "expected 1 controller pod running")
				controllerPodName = podNames[0]
				g.Expect(controllerPodName).To(ContainSubstring("controller-manager"))

				// Validate the pod's status
				cmd = exec.Command("kubectl", "get",
					"pods", controllerPodName, "-o", "jsonpath={.status.phase}",
					"-n", namespace,
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"), "Incorrect controller-manager pod status")
			}
			Eventually(verifyControllerUp).Should(Succeed())
		})

		It("should ensure the metrics endpoint is serving metrics", func() {
			By("creating a ClusterRoleBinding for the service account to allow access to metrics")
			cmd := exec.Command("kubectl", "create", "clusterrolebinding", metricsRoleBindingName,
				"--clusterrole=kezio-metrics-reader",
				fmt.Sprintf("--serviceaccount=%s:%s", namespace, serviceAccountName),
			)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ClusterRoleBinding")

			By("validating that the metrics service is available")
			cmd = exec.Command("kubectl", "get", "service", metricsServiceName, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Metrics service should exist")

			By("getting the service account token")
			token, err := serviceAccountToken()
			Expect(err).NotTo(HaveOccurred())
			Expect(token).NotTo(BeEmpty())

			By("waiting for the metrics endpoint to be ready")
			verifyMetricsEndpointReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "endpoints", metricsServiceName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("8443"), "Metrics endpoint is not ready")
			}
			Eventually(verifyMetricsEndpointReady).Should(Succeed())

			By("verifying that the controller manager is serving the metrics server")
			verifyMetricsServerStarted := func(g Gomega) {
				cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("controller-runtime.metrics\tServing metrics server"),
					"Metrics server not yet started")
			}
			Eventually(verifyMetricsServerStarted).Should(Succeed())

			By("creating the curl-metrics pod to access the metrics endpoint")
			cmd = exec.Command("kubectl", "run", "curl-metrics", "--restart=Never",
				"--namespace", namespace,
				"--image=curlimages/curl:latest",
				"--overrides",
				fmt.Sprintf(`{
					"spec": {
						"containers": [{
							"name": "curl",
							"image": "curlimages/curl:latest",
							"command": ["/bin/sh", "-c"],
							"args": ["curl -v -k -H 'Authorization: Bearer %s' https://%s.%s.svc.cluster.local:8443/metrics"],
							"securityContext": {
								"allowPrivilegeEscalation": false,
								"capabilities": {
									"drop": ["ALL"]
								},
								"runAsNonRoot": true,
								"runAsUser": 1000,
								"seccompProfile": {
									"type": "RuntimeDefault"
								}
							}
						}],
						"serviceAccount": "%s"
					}
				}`, token, metricsServiceName, namespace, serviceAccountName))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create curl-metrics pod")

			By("waiting for the curl-metrics pod to complete.")
			verifyCurlUp := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "curl-metrics",
					"-o", "jsonpath={.status.phase}",
					"-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Succeeded"), "curl pod in wrong status")
			}
			Eventually(verifyCurlUp, 5*time.Minute).Should(Succeed())

			By("getting the metrics by checking curl-metrics logs")
			metricsOutput := getMetricsOutput()
			Expect(metricsOutput).To(ContainSubstring(
				"controller_runtime_reconcile_total",
			))
		})

		// +kubebuilder:scaffold:e2e-webhooks-checks
	})

	Context("Image and Machine lifecycle", func() {
		const (
			imageName   = e2eImageName
			machineName = e2eMachineName
		)

		It("creates an Image with a url source and waits for it to reach Ready", func() {
			By("creating the Image")
			applyManifest(fmt.Sprintf(`
apiVersion: kezio.kojuro.date/v1alpha1
kind: Image
metadata:
  name: %s
  namespace: %s
spec:
  source:
    url: https://example.invalid/kezio-e2e-golden.raw
    format: raw
  bootable: true
  osFamily: Linux
`, imageName, namespace))

			By("waiting for the Image to reach state Ready")
			Eventually(func(g Gomega) {
				state, err := getJSONPath("image", imageName, "{.status.state}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(state).To(Equal("Ready"))
			}).Should(Succeed())
		})

		It("creates a Machine referencing the Image and drives it through the state machine to Provisioned", func() {
			By("creating the Site and Subnet the Machine's spec.subnetRef requires")
			applyManifest(fmt.Sprintf(`
apiVersion: kezio.kojuro.date/v1alpha1
kind: Site
metadata:
  name: %s
  namespace: %s
`, e2eSiteName, namespace))
			applyManifest(fmt.Sprintf(`
apiVersion: kezio.kojuro.date/v1alpha1
kind: Subnet
metadata:
  name: %s
  namespace: %s
spec:
  siteRef:
    name: %s
  cidr: "%s"
  bootdServerIP: "%s"
  bootdNetworkRef:
    name: %s
  dhcp:
    mode: proxy
`, e2eSubnetName, namespace, e2eSiteName, e2eSubnetCIDR, e2eSubnetBootIP, e2eBootNADName))

			By("creating the Machine")
			applyManifest(fmt.Sprintf(`
apiVersion: kezio.kojuro.date/v1alpha1
kind: Machine
metadata:
  name: %s
  namespace: %s
spec:
  bmc:
    address: redfish://198.51.100.10/redfish/v1/Systems/1
    credentialsSecretRef:
      name: %s-bmc
  bootMACAddress: "aa:bb:cc:dd:ee:02"
  online: true
  subnetRef:
    name: %s
  imageRef:
    name: %s
`, machineName, namespace, machineName, e2eSubnetName, imageName))

			By("waiting for the Machine to reach state Provisioned")
			Eventually(func(g Gomega) {
				state, err := getJSONPath("machine", machineName, "{.status.state}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(state).To(Equal("Provisioned"),
					"expected the Machine to walk Enrolling -> Inspecting -> Available -> "+
						"Provisioning -> Provisioned on the fake deployer")
			}).Should(Succeed())

			By("verifying the Ready condition and the recorded provisioning status")
			readyStatus, err := getJSONPath("machine", machineName,
				`{.status.conditions[?(@.type=="Ready")].status}`)
			Expect(err).NotTo(HaveOccurred())
			Expect(readyStatus).To(Equal("True"))

			targetDisk, err := getJSONPath("machine", machineName, "{.status.provisioning.image.targetDisk}")
			Expect(err).NotTo(HaveOccurred())
			Expect(targetDisk).NotTo(BeEmpty(), "expected the fake deployer to record a resolved target disk")

			disks, err := getJSONPath("machine", machineName, "{.status.hardware.disks[*].deviceName}")
			Expect(err).NotTo(HaveOccurred())
			Expect(disks).NotTo(BeEmpty(), "expected Inspecting to have populated status.hardware")
		})

		It("deletes the Machine and completes cleanup through the finalizer", func() {
			By("deleting the Machine")
			cmd := exec.Command("kubectl", "delete", "machine", machineName, "-n", namespace, "--timeout=60s")
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to delete the Machine")

			By("verifying the Machine object is gone")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "machine", machineName, "-n", namespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred(), "expected the Machine to be gone after its finalizer ran")
			}).Should(Succeed())
		})
	})

	// registerImagePathContext (e2e_image_path_test.go) adds the "Image
	// ingest and seeding" Context as a sibling of "Manager" and "Image and
	// Machine lifecycle" above, inside this same Describe("Manager",
	// Ordered, ...) - not as a separate top-level Describe, which Ginkgo's
	// default top-level randomization could run before this container's
	// own BeforeAll (see that function's doc comment). It only runs when
	// E2E_IMAGE_PATH=true.
	if imagePathEnabled {
		registerImagePathContext()
	}

	// registerSiteSubnetContext (e2e_sitesubnet_test.go) adds the
	// "Site/Subnet network model" Context as a further sibling, for the
	// same reason and under the same nesting rule as
	// registerImagePathContext above - it is not gated by an env var, so
	// it always runs.
	registerSiteSubnetContext()
})

// serviceAccountToken returns a token for the specified service account in the given namespace.
// It uses the Kubernetes TokenRequest API to generate a token by directly sending a request
// and parsing the resulting token from the API response.
func serviceAccountToken() (string, error) {
	const tokenRequestRawString = `{
		"apiVersion": "authentication.k8s.io/v1",
		"kind": "TokenRequest"
	}`

	// Temporary file to store the token request
	secretName := fmt.Sprintf("%s-token-request", serviceAccountName)
	tokenRequestFile := filepath.Join("/tmp", secretName)
	err := os.WriteFile(tokenRequestFile, []byte(tokenRequestRawString), os.FileMode(0o644))
	if err != nil {
		return "", err
	}

	var out string
	verifyTokenCreation := func(g Gomega) {
		// Execute kubectl command to create the token
		cmd := exec.Command("kubectl", "create", "--raw", fmt.Sprintf(
			"/api/v1/namespaces/%s/serviceaccounts/%s/token",
			namespace,
			serviceAccountName,
		), "-f", tokenRequestFile)

		output, err := cmd.CombinedOutput()
		g.Expect(err).NotTo(HaveOccurred())

		// Parse the JSON output to extract the token
		var token tokenRequest
		err = json.Unmarshal(output, &token)
		g.Expect(err).NotTo(HaveOccurred())

		out = token.Status.Token
	}
	Eventually(verifyTokenCreation).Should(Succeed())

	return out, err
}

// getMetricsOutput retrieves and returns the logs from the curl pod used to access the metrics endpoint.
func getMetricsOutput() string {
	By("getting the curl-metrics logs")
	cmd := exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
	metricsOutput, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to retrieve logs from curl pod")
	Expect(metricsOutput).To(ContainSubstring("< HTTP/1.1 200 OK"))
	return metricsOutput
}

// tokenRequest is a simplified representation of the Kubernetes TokenRequest API response,
// containing only the token field that we need to extract.
type tokenRequest struct {
	Status struct {
		Token string `json:"token"`
	} `json:"status"`
}

// applyManifest writes the given YAML to a temporary file and applies it
// with kubectl. It fails the current spec on error.
func applyManifest(yaml string) {
	tmpFile, err := os.CreateTemp("", "kezio-e2e-*.yaml")
	Expect(err).NotTo(HaveOccurred(), "Failed to create a temporary manifest file")
	defer func() { _ = os.Remove(tmpFile.Name()) }()

	_, err = tmpFile.WriteString(yaml)
	Expect(err).NotTo(HaveOccurred(), "Failed to write the manifest")
	Expect(tmpFile.Close()).To(Succeed())

	cmd := exec.Command("kubectl", "apply", "-f", tmpFile.Name())
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to apply manifest:\n%s", yaml)
}

// getJSONPath returns the value of jsonPath on the named object of the
// given kind, in the operator's namespace.
func getJSONPath(kind, name, jsonPath string) (string, error) {
	cmd := exec.Command("kubectl", "get", kind, name, "-n", namespace, "-o", fmt.Sprintf("jsonpath=%s", jsonPath))
	return utils.Run(cmd)
}

// deleteAndWait issues a best-effort delete of the named object and waits
// for it to be fully removed before returning. It is safe to call when the
// object is already gone (or never existed): the delete uses
// --ignore-not-found, and the wait loop treats "kubectl get" failing as
// success.
//
// If the object is not gone within gracePeriod - e.g. because an earlier
// spec failed mid-flow and left dangling references that keep a finalizer
// from clearing - its finalizers are force-removed via kubectl patch as a
// last resort, so teardown never deadlocks CI. That path logs loudly since
// it papers over whatever prevented graceful deletion.
func deleteAndWait(kind, name string, gracePeriod time.Duration) {
	cmd := exec.Command("kubectl", "delete", kind, name, "-n", namespace, "--ignore-not-found", "--wait=false")
	_, _ = utils.Run(cmd)

	const pollInterval = 2 * time.Second
	deadline := time.Now().Add(gracePeriod)
	for time.Now().Before(deadline) {
		cmd := exec.Command("kubectl", "get", kind, name, "-n", namespace)
		if _, err := utils.Run(cmd); err != nil {
			return // gone (or never existed)
		}
		time.Sleep(pollInterval)
	}

	_, _ = fmt.Fprintf(GinkgoWriter,
		"WARNING: %s %q was not deleted gracefully within %s; force-removing its finalizers "+
			"so teardown can proceed. This usually means an earlier spec failed mid-flow.\n",
		kind, name, gracePeriod)

	cmd = exec.Command("kubectl", "patch", kind, name, "-n", namespace,
		"--type=merge", "-p", `{"metadata":{"finalizers":[]}}`)
	_, _ = utils.Run(cmd)

	cmd = exec.Command("kubectl", "wait", fmt.Sprintf("%s/%s", kind, name),
		"-n", namespace, "--for=delete", "--timeout=30s")
	if _, err := utils.Run(cmd); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter,
			"WARNING: %s %q still present after force-removing finalizers: %v\n", kind, name, err)
	}
}
