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
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/tjjh89017/kezio/test/utils"
)

var (
	// e2eCluster selects the target cluster flavor and must stay in sync
	// with the Makefile's E2E_CLUSTER variable. The default (empty or
	// "kind") builds and `kind load`s the manager image into a throwaway
	// Kind cluster. Set E2E_CLUSTER=k3s to target a pre-provisioned
	// single-node k3s cluster (the CI job stands one up and sets
	// KUBECONFIG itself), in which case the image is imported into the
	// node's containerd instead, and no Kind cluster is created or torn
	// down around the run.
	e2eCluster = os.Getenv("E2E_CLUSTER")

	// projectImage is the name of the image which will be build and loaded
	// with the code source changes to be tested.
	projectImage = "example.com/kezio:v0.0.1"

	// skipCertManagerInstall skips installing cert-manager when the
	// CERT_MANAGER_INSTALL_SKIP env var is "true". Useful when cert-manager
	// is already present on the target cluster (e.g. a developer's local
	// cluster), so the suite does not fight over managing it.
	skipCertManagerInstall = os.Getenv("CERT_MANAGER_INSTALL_SKIP") == "true"

	// isCertManagerAlreadyInstalled records whether cert-manager's CRDs
	// were found on the cluster before this suite tried to install it, so
	// AfterSuite only tears down an installation this suite itself made.
	isCertManagerAlreadyInstalled = false
)

// TestE2E runs the end-to-end (e2e) test suite for the project. These tests execute in an isolated,
// temporary environment to validate project changes with the purposed to be used in CI jobs.
// The default setup requires Kind and builds/loads the Manager Docker image locally.
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting kezio integration test suite\n")
	RunSpecs(t, "e2e suite")
}

var _ = BeforeSuite(func() {
	// kezio's Image, Machine, and PostHook CRDs each carry a defaulting
	// and a validating webhook, so the controller-manager cannot start
	// serving admission requests (and "make deploy" in e2e_test.go's
	// BeforeAll cannot succeed) until cert-manager has minted the webhook
	// serving certificate. Install cert-manager first, before anything
	// that depends on it.
	if !skipCertManagerInstall {
		By("checking if cert-manager is already installed")
		isCertManagerAlreadyInstalled = utils.IsCertManagerCRDsInstalled()
		if !isCertManagerAlreadyInstalled {
			By("installing cert-manager")
			Expect(utils.InstallCertManager()).To(Succeed(), "Failed to install cert-manager")
		} else {
			_, _ = fmt.Fprintf(GinkgoWriter, "WARNING: cert-manager is already installed. Skipping installation...\n")
		}
	}

	By("building the manager(Operator) image")
	cmd := exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", projectImage))
	_, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the manager(Operator) image")

	// Make the freshly built image available to the target cluster. Kind
	// loads it directly; a pre-provisioned k3s cluster (E2E_CLUSTER=k3s)
	// imports it into the node's containerd instead, since there is no
	// Kind cluster to load it into.
	if e2eCluster == "k3s" {
		By("importing the manager(Operator) image into the k3s node containerd")
		err = utils.LoadImageToK3sContainerd(projectImage)
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to import the manager(Operator) image into k3s containerd")
	} else {
		By("loading the manager(Operator) image on Kind")
		err = utils.LoadImageToKindClusterWithName(projectImage)
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the manager(Operator) image into Kind")
	}
})

var _ = AfterSuite(func() {
	// Teardown cert-manager after the suite if not skipped and if this
	// suite is the one that installed it.
	if !skipCertManagerInstall && !isCertManagerAlreadyInstalled {
		By("uninstalling cert-manager")
		utils.UninstallCertManager()
	}
})
