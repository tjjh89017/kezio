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
	"fmt"
	"os"
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2" // nolint:revive,staticcheck
	. "github.com/onsi/gomega"    // nolint:revive,staticcheck

	"github.com/tjjh89017/kezio/internal/controller"
	"github.com/tjjh89017/kezio/test/utils"
)

// This RKE2 lane runs with Multus disabled, so no CNI honours the Multus
// annotations these Deployments carry. Every assertion below targets
// object fields, not pod networking or rollout success.

// siteSubnetNADCRDPath is the vendored NetworkAttachmentDefinition CRD,
// reused from internal/controller's envtest suite since this lane has no
// real Multus install either.
const siteSubnetNADCRDPath = "internal/controller/testdata/multus-crds/networkattachmentdefinitions.yaml"

// siteSubnetNADCRDName is the CustomResourceDefinition object name
// siteSubnetNADCRDPath declares.
const siteSubnetNADCRDName = "network-attachment-definitions.k8s.cni.cncf.io"

const (
	siteSubnetSiteName          = "e2efast-site"
	siteSubnetSubnetName        = "e2efast-subnet"
	siteSubnetCollideSubnetName = "e2efast-subnet-collide"
	siteSubnetBootNADName       = "kezio-e2efast-boot-net"
	siteSubnetSeederNADName     = "kezio-e2efast-seeder-net"
	siteSubnetImageName         = "e2efast-image"
	siteSubnetMachineName       = "e2efast-machine"
	siteSubnetProbeMachineName  = "e2efast-probe"

	// TEST-NET-3 (RFC 5737): documentation-only, never routable.
	siteSubnetCIDR      = "203.0.113.0/24"
	siteSubnetBootdIP   = "203.0.113.10"
	siteSubnetSeederIP  = "203.0.113.20"
	siteSubnetCollideIP = "203.0.113.99"
)

// siteSubnetBootdDeploymentName is the bootd Deployment name
// SubnetReconciler derives for siteSubnetSubnetName: "kezio-bootd-" plus
// the Subnet's own name (bootdDeploymentName in
// internal/controller/bootd_deployment.go).
const siteSubnetBootdDeploymentName = "kezio-bootd-" + siteSubnetSubnetName

const controllerManagerDeploymentName = "kezio-controller-manager"

// registerSiteSubnetContext adds the "Site/Subnet network model" Context
// as a sibling inside e2e_test.go's Describe("Manager", Ordered, ...).
// It runs unconditionally on every PR, unlike the env-gated image-path
// Context.
func registerSiteSubnetContext() {
	Context("Site/Subnet network model", func() {
		BeforeAll(func() {
			By("installing the NetworkAttachmentDefinition CRD (no Multus on this lane)")
			_, err := utils.Run(exec.Command("kubectl", "apply", "-f", siteSubnetNADCRDPath))
			Expect(err).NotTo(HaveOccurred(), "failed to install the NetworkAttachmentDefinition CRD")
			_, err = utils.Run(exec.Command("kubectl", "wait", "--for=condition=Established",
				"crd/"+siteSubnetNADCRDName, "--timeout=60s"))
			Expect(err).NotTo(HaveOccurred(), "NetworkAttachmentDefinition CRD never became Established")

			// BOOTD_DEPLOYMENT_IMAGE/SEEDER_DEPLOYMENT_IMAGE start unset, which
			// yields the zero, inert Config and no Deployment; opting in here
			// is what lets this Context assert on real objects without them
			// ever needing to actually run. INGEST_IMAGE- clears any value
			// left by the image-path Context so this Context's fake
			// https://example.invalid/... Image hits image_controller.go's
			// Ingest.Image == "" stub path instead of a real, failing ingest
			// Job; clearing an unset var is a no-op in the plain RKE2 lane.
			By("configuring the controller-manager for bootd and seeder Deployment reconciliation")
			_, err = utils.Run(exec.Command("kubectl", "set", "env",
				"deployment/"+controllerManagerDeploymentName, "-n", namespace,
				"BOOTD_DEPLOYMENT_IMAGE=example.com/kezio-bootd:e2e-fast-stub",
				"BOOTD_DEPLOYMENT_BOOT_ARTIFACTS_IMAGE=example.com/kezio-boot-artifacts:e2e-fast-stub",
				"SEEDER_DEPLOYMENT_IMAGE=example.com/kezio-seeder:e2e-fast-stub",
				"INGEST_IMAGE-",
			))
			Expect(err).NotTo(HaveOccurred(), "failed to set BOOTD_DEPLOYMENT_*/SEEDER_DEPLOYMENT_* on the controller-manager")
			waitForRollout(controllerManagerDeploymentName)
		})

		AfterAll(func() {
			By("deleting the site-subnet-model Machine and Image")
			deleteAndWait("machine", siteSubnetMachineName, time.Minute)
			deleteAndWait("machine", siteSubnetProbeMachineName, time.Minute)
			deleteAndWait("image", siteSubnetImageName, 2*time.Minute)

			By("deleting the Subnets, Site, and NetworkAttachmentDefinitions")
			for _, obj := range [][2]string{
				{"subnet", siteSubnetCollideSubnetName},
				{"subnet", siteSubnetSubnetName},
				{"site", siteSubnetSiteName},
				{"networkattachmentdefinition", siteSubnetSeederNADName},
				{"networkattachmentdefinition", siteSubnetBootNADName},
			} {
				_, _ = utils.Run(exec.Command("kubectl", "delete", obj[0], obj[1], "-n", namespace, "--ignore-not-found"))
			}

			By("removing the NetworkAttachmentDefinition CRD")
			_, _ = utils.Run(exec.Command("kubectl", "delete", "-f", siteSubnetNADCRDPath, "--ignore-not-found"))
		})

		// This fast lane once accepted a manifest missing the required
		// spec.subnetRef and drove the Machine to Provisioned (unexplained,
		// unrepeated). If apply unexpectedly succeeds, the block below dumps
		// the live schema and stored object to the CI log.
		It("rejects a Machine manifest missing spec.subnetRef", func() {
			yaml := fmt.Sprintf(`
apiVersion: kezio.kojuro.date/v1alpha1
kind: Machine
metadata:
  name: %s
  namespace: %s
spec:
  bmc:
    address: redfish://198.51.100.20/redfish/v1/Systems/1
    credentialsSecretRef:
      name: %s-bmc
  bootMACAddress: "aa:bb:cc:dd:ee:20"
`, siteSubnetProbeMachineName, namespace, siteSubnetProbeMachineName)

			tmpFile, err := os.CreateTemp("", "kezio-e2e-probe-*.yaml")
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = os.Remove(tmpFile.Name()) }()
			_, err = tmpFile.WriteString(yaml)
			Expect(err).NotTo(HaveOccurred())
			Expect(tmpFile.Close()).To(Succeed())

			out, applyErr := utils.Run(exec.Command("kubectl", "apply", "-f", tmpFile.Name()))
			if applyErr == nil {
				By("PROBE ANOMALY: apply unexpectedly succeeded - dumping the live schema and stored object")
				schema, _ := utils.Run(exec.Command("kubectl", "get", "crd", "machines.kezio.kojuro.date", "-o",
					"jsonpath={.spec.versions[0].schema.openAPIV3Schema.properties.spec.required}"))
				_, _ = fmt.Fprintf(GinkgoWriter, "machines CRD spec.required: %s\n", schema)
				stored, _ := utils.Run(exec.Command("kubectl", "get", "machine", siteSubnetProbeMachineName,
					"-n", namespace, "-o", "yaml"))
				_, _ = fmt.Fprintf(GinkgoWriter, "stored Machine object:\n%s\n", stored)
			}
			Expect(applyErr).To(HaveOccurred(), "expected the API server to reject a Machine with no spec.subnetRef")
			Expect(out).To(ContainSubstring("subnetRef"))
		})

		It("creates a per-Subnet bootd Deployment from a Site, Subnet, and NAD", func() {
			By("creating the boot and seeder NetworkAttachmentDefinitions")
			applyManifest(fmt.Sprintf(`
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: %s
  namespace: %s
spec:
  config: |
    {
      "cniVersion": "0.3.1",
      "name": "%s",
      "plugins": [
        {
          "type": "bridge",
          "bridge": "kezio-e2efast0",
          "ipam": {
            "type": "static",
            "addresses": [
              {"address": "%s/24"}
            ]
          }
        }
      ]
    }
`, siteSubnetBootNADName, namespace, siteSubnetBootNADName, siteSubnetBootdIP))

			applyManifest(fmt.Sprintf(`
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: %s
  namespace: %s
spec:
  config: |
    {
      "cniVersion": "0.3.1",
      "name": "%s",
      "plugins": [
        {
          "type": "bridge",
          "bridge": "kezio-e2efast0",
          "ipam": {
            "type": "static",
            "addresses": [
              {"address": "%s/24"}
            ]
          }
        }
      ]
    }
`, siteSubnetSeederNADName, namespace, siteSubnetSeederNADName, siteSubnetSeederIP))

			By("creating the Site")
			applyManifest(fmt.Sprintf(`
apiVersion: kezio.kojuro.date/v1alpha1
kind: Site
metadata:
  name: %s
  namespace: %s
spec:
  seederSubnetRef:
    name: %s
`, siteSubnetSiteName, namespace, siteSubnetSubnetName))

			By("creating the Subnet in proxyDHCP mode")
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
  seederNetworkRef:
    name: %s
  nodeSelector:
    kubernetes.io/os: linux
  dhcp:
    mode: proxy
`, siteSubnetSubnetName, namespace, siteSubnetSiteName, siteSubnetCIDR, siteSubnetBootdIP,
				siteSubnetBootNADName, siteSubnetSeederNADName))

			By("waiting for the bootd Deployment to appear")
			Eventually(func(g Gomega) {
				name, err := getJSONPath("deployment", siteSubnetBootdDeploymentName, "{.metadata.name}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(name).To(Equal(siteSubnetBootdDeploymentName))
			}).Should(Succeed())

			By("verifying the Multus networks annotation names the boot NAD")
			netsAnno, err := getJSONPath("deployment", siteSubnetBootdDeploymentName,
				`{.spec.template.metadata.annotations.k8s\.v1\.cni\.cncf\.io/networks}`)
			Expect(err).NotTo(HaveOccurred())
			Expect(netsAnno).To(Equal(namespace + "/" + siteSubnetBootNADName))

			By("verifying bootd's env carries proxyDHCP mode and this Subnet's server IP")
			leaseMode, err := getJSONPath("deployment", siteSubnetBootdDeploymentName,
				`{.spec.template.spec.containers[?(@.name=="bootd")].env[?(@.name=="BOOTD_LEASE_MODE")].value}`)
			Expect(err).NotTo(HaveOccurred())
			Expect(leaseMode).To(Equal("false"), "expected BOOTD_LEASE_MODE=false for dhcp.mode: proxy")

			serverIP, err := getJSONPath("deployment", siteSubnetBootdDeploymentName,
				`{.spec.template.spec.containers[?(@.name=="bootd")].env[?(@.name=="BOOTD_SERVER_IP")].value}`)
			Expect(err).NotTo(HaveOccurred())
			Expect(serverIP).To(Equal(siteSubnetBootdIP))

			By("verifying the Subnet's declared nodeSelector reached the bootd pod template")
			nodeSelector, err := getJSONPath("deployment", siteSubnetBootdDeploymentName,
				`{.spec.template.spec.nodeSelector.kubernetes\.io/os}`)
			Expect(err).NotTo(HaveOccurred())
			Expect(nodeSelector).To(Equal("linux"))

			// This lane's kezio-system namespace never gets the privileged
			// PSA label bootd's NET_ADMIN capability needs, so status is
			// expected False here even though the Deployment still exists.
			By("verifying ConditionReady reports the missing bootd namespace prerequisites this lane never provisions")
			Eventually(func(g Gomega) {
				status, err := getJSONPath("subnet", siteSubnetSubnetName,
					`{.status.conditions[?(@.type=="BootdNamespacePSALabel")].status}`)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(status).To(Equal("False"))
			}).Should(Succeed())
		})

		It("creates a per-(Image,Site) seeder Deployment once a Machine demands one", func() {
			By("creating the Image")
			applyManifest(fmt.Sprintf(`
apiVersion: kezio.kojuro.date/v1alpha1
kind: Image
metadata:
  name: %s
  namespace: %s
spec:
  source:
    url: https://example.invalid/kezio-e2efast-golden.raw
    format: raw
  bootable: true
  osFamily: Linux
`, siteSubnetImageName, namespace))

			Eventually(func(g Gomega) {
				state, err := getJSONPath("image", siteSubnetImageName, "{.status.state}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(state).To(Equal("Ready"))
			}).Should(Succeed())

			By("creating the Machine, referencing the Subnet through spec.subnetRef")
			applyManifest(fmt.Sprintf(`
apiVersion: kezio.kojuro.date/v1alpha1
kind: Machine
metadata:
  name: %s
  namespace: %s
spec:
  bmc:
    address: redfish://198.51.100.21/redfish/v1/Systems/1
    credentialsSecretRef:
      name: %s-bmc
  bootMACAddress: "aa:bb:cc:dd:ee:21"
  online: true
  subnetRef:
    name: %s
  imageRef:
    name: %s
`, siteSubnetMachineName, namespace, siteSubnetMachineName, siteSubnetSubnetName, siteSubnetImageName))

			By("waiting for the Machine to reach state Provisioned on the fake deployer")
			Eventually(func(g Gomega) {
				state, err := getJSONPath("machine", siteSubnetMachineName, "{.status.state}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(state).To(Equal("Provisioned"))
			}).Should(Succeed())

			// The Deployment persists for a 5m grace period after demand
			// drops, outlasting this suite's two-minute Eventually window.
			By("waiting for the per-(Image,Site) seeder Deployment to appear")
			var seederDeploymentName string
			Eventually(func(g Gomega) {
				out, err := utils.Run(exec.Command("kubectl", "get", "deployment", "-n", namespace,
					"-l", fmt.Sprintf("%s=%s", controller.SeederDeploymentImageLabel, siteSubnetImageName),
					"-o", "jsonpath={.items[0].metadata.name}"))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).NotTo(BeEmpty())
				seederDeploymentName = out
			}).Should(Succeed())

			By("verifying the seeder pod's default-network annotation names the seeder Subnet's NAD")
			netAnno, err := getJSONPath("deployment", seederDeploymentName,
				`{.spec.template.metadata.annotations.v1\.multus-cni\.io/default-network}`)
			Expect(err).NotTo(HaveOccurred())
			Expect(netAnno).To(Equal(namespace + "/" + siteSubnetSeederNADName))

			By("verifying the seeder pod template inherited the seeder Subnet's nodeSelector")
			nodeSelector, err := getJSONPath("deployment", seederDeploymentName,
				`{.spec.template.spec.nodeSelector.kubernetes\.io/os}`)
			Expect(err).NotTo(HaveOccurred())
			Expect(nodeSelector).To(Equal("linux"))

			By("verifying the seeder Deployment records the Site it serves")
			siteAnno, err := getJSONPath("deployment", seederDeploymentName,
				fmt.Sprintf(`{.metadata.annotations.%s}`, jsonPathEscapeAnnotation(controller.SeederDeploymentSiteAnnotation)))
			Expect(err).NotTo(HaveOccurred())
			Expect(siteAnno).To(Equal(namespace + "/" + siteSubnetSiteName))
		})

		It("surfaces a bootd network collision and withholds the second Deployment", func() {
			By("creating a second Subnet colliding on the same bootdNetworkRef")
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
`, siteSubnetCollideSubnetName, namespace, siteSubnetSiteName, siteSubnetCIDR,
				siteSubnetCollideIP, siteSubnetBootNADName))

			By("waiting for the colliding Subnet to report BootdNetworkCollision")
			Eventually(func(g Gomega) {
				status, err := getJSONPath("subnet", siteSubnetCollideSubnetName,
					`{.status.conditions[?(@.type=="BootdNetworkCollision")].status}`)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(status).To(Equal("False"))

				readyStatus, err := getJSONPath("subnet", siteSubnetCollideSubnetName,
					`{.status.conditions[?(@.type=="Ready")].reason}`)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(readyStatus).To(Equal("BootdNetworkCollision"))
			}).Should(Succeed())

			By("verifying no bootd Deployment was created for the colliding Subnet")
			collideDeploymentName := "kezio-bootd-" + siteSubnetCollideSubnetName
			_, err := getJSONPath("deployment", collideDeploymentName, "{.metadata.name}")
			Expect(err).To(HaveOccurred(), "expected no bootd Deployment for the colliding Subnet")

			By("verifying the original Subnet's own bootd Deployment is untouched")
			_, err = getJSONPath("deployment", siteSubnetBootdDeploymentName, "{.metadata.name}")
			Expect(err).NotTo(HaveOccurred(), "expected the original Subnet's bootd Deployment to still exist")
		})
	})
}

// jsonPathEscapeAnnotation escapes every "." in key for use as a bare
// (unbracketed) kubectl jsonpath field segment - the same style
// .github/actions/deploy-bootd/action.yml already uses for the Multus
// network-status annotation.
func jsonPathEscapeAnnotation(key string) string {
	escaped := ""
	for _, r := range key {
		if r == '.' {
			escaped += `\.`
			continue
		}
		escaped += string(r)
	}
	return escaped
}
