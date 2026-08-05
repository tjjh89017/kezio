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

package utils

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"

	. "github.com/onsi/ginkgo/v2" // nolint:revive,staticcheck
)

const (
	prometheusOperatorVersion = "v0.77.1"
	prometheusOperatorURL     = "https://github.com/prometheus-operator/prometheus-operator/" +
		"releases/download/%s/bundle.yaml"

	// certManagerURL always resolves to whatever cert-manager currently
	// calls "latest" rather than a version pinned in this repo. kezio's
	// webhooks only need cert-manager to mint and rotate a self-signed
	// serving certificate - a narrow, stable surface - so there is no
	// compatibility matrix worth pinning against, and pinning would just
	// mean a second place to remember to bump. This is a standing project
	// directive: never pin cert-manager's version here.
	certManagerURL = "https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml"

	// certManagerNamespace is the namespace the upstream manifest above
	// installs cert-manager into.
	certManagerNamespace = "cert-manager"

	// certManagerDeployTimeout bounds how long InstallCertManager waits for
	// each cert-manager Deployment to report Available.
	certManagerDeployTimeout = "5m"
)

// certManagerDeployments are the Deployments the upstream cert-manager
// manifest creates. InstallCertManager waits for all of them (not just
// cert-manager-webhook) to be Available: the controller and cainjector
// also gate whether a Certificate actually gets issued.
var certManagerDeployments = []string{
	"cert-manager",
	"cert-manager-webhook",
	"cert-manager-cainjector",
}

func warnError(err error) {
	_, _ = fmt.Fprintf(GinkgoWriter, "warning: %v\n", err)
}

// Run executes the provided command within this context
func Run(cmd *exec.Cmd) (string, error) {
	dir, _ := GetProjectDir()
	cmd.Dir = dir

	if err := os.Chdir(cmd.Dir); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "chdir dir: %q\n", err)
	}

	cmd.Env = append(os.Environ(), "GO111MODULE=on")
	command := strings.Join(cmd.Args, " ")
	_, _ = fmt.Fprintf(GinkgoWriter, "running: %q\n", command)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("%q failed with error %q: %w", command, string(output), err)
	}

	return string(output), nil
}

// InstallPrometheusOperator installs the prometheus Operator to be used to export the enabled metrics.
func InstallPrometheusOperator() error {
	url := fmt.Sprintf(prometheusOperatorURL, prometheusOperatorVersion)
	cmd := exec.Command("kubectl", "create", "-f", url)
	_, err := Run(cmd)
	return err
}

// UninstallPrometheusOperator uninstalls the prometheus
func UninstallPrometheusOperator() {
	url := fmt.Sprintf(prometheusOperatorURL, prometheusOperatorVersion)
	cmd := exec.Command("kubectl", "delete", "-f", url)
	if _, err := Run(cmd); err != nil {
		warnError(err)
	}
}

// IsPrometheusCRDsInstalled checks if any Prometheus CRDs are installed
// by verifying the existence of key CRDs related to Prometheus.
func IsPrometheusCRDsInstalled() bool {
	// List of common Prometheus CRDs
	prometheusCRDs := []string{
		"prometheuses.monitoring.coreos.com",
		"prometheusrules.monitoring.coreos.com",
		"prometheusagents.monitoring.coreos.com",
	}

	cmd := exec.Command("kubectl", "get", "crds", "-o", "custom-columns=NAME:.metadata.name")
	output, err := Run(cmd)
	if err != nil {
		return false
	}
	crdList := GetNonEmptyLines(output)
	for _, crd := range prometheusCRDs {
		for _, line := range crdList {
			if strings.Contains(line, crd) {
				return true
			}
		}
	}

	return false
}

// UninstallCertManager uninstalls the cert manager
func UninstallCertManager() {
	cmd := exec.Command("kubectl", "delete", "-f", certManagerURL)
	if _, err := Run(cmd); err != nil {
		warnError(err)
	}
}

// InstallCertManager installs cert-manager at whatever release the
// upstream project currently tags "latest" (see certManagerURL) and waits
// for every cert-manager Deployment to report Available before returning.
// Waiting on all three Deployments, not just cert-manager-webhook, avoids
// a race where kezio's webhook Certificate is created before the
// controller or cainjector is ready to issue and inject it.
func InstallCertManager() error {
	cmd := exec.Command("kubectl", "apply", "-f", certManagerURL)
	if _, err := Run(cmd); err != nil {
		return err
	}

	for _, name := range certManagerDeployments {
		cmd = exec.Command("kubectl", "wait", "deployment.apps/"+name,
			"--for", "condition=Available",
			"--namespace", certManagerNamespace,
			"--timeout", certManagerDeployTimeout,
		)
		if _, err := Run(cmd); err != nil {
			return fmt.Errorf("wait for %s to become available: %w", name, err)
		}
	}

	return nil
}

// IsCertManagerCRDsInstalled checks if any Cert Manager CRDs are installed
// by verifying the existence of key CRDs related to Cert Manager.
func IsCertManagerCRDsInstalled() bool {
	// List of common Cert Manager CRDs
	certManagerCRDs := []string{
		"certificates.cert-manager.io",
		"issuers.cert-manager.io",
		"clusterissuers.cert-manager.io",
		"certificaterequests.cert-manager.io",
		"orders.acme.cert-manager.io",
		"challenges.acme.cert-manager.io",
	}

	// Execute the kubectl command to get all CRDs
	cmd := exec.Command("kubectl", "get", "crds")
	output, err := Run(cmd)
	if err != nil {
		return false
	}

	// Check if any of the Cert Manager CRDs are present
	crdList := GetNonEmptyLines(output)
	for _, crd := range certManagerCRDs {
		for _, line := range crdList {
			if strings.Contains(line, crd) {
				return true
			}
		}
	}

	return false
}

// LoadImageToKindClusterWithName loads a local docker image to the kind cluster
func LoadImageToKindClusterWithName(name string) error {
	cluster := "kind"
	if v, ok := os.LookupEnv("KIND_CLUSTER"); ok {
		cluster = v
	}
	kindOptions := []string{"load", "docker-image", name, "--name", cluster}
	cmd := exec.Command("kind", kindOptions...)
	_, err := Run(cmd)
	return err
}

// LoadImageToRKE2Containerd imports a local docker image into the RKE2
// node's containerd image store. Used when the e2e suite targets a
// pre-provisioned single-node RKE2 cluster instead of Kind. Unlike k3s's
// "k3s ctr" wrapper (which defaults to the embedded socket and the k8s.io
// namespace on its own), RKE2 ships a plain "ctr" binary that needs both
// pointed at explicitly: -a selects RKE2's containerd socket (still under
// /run/k3s/containerd - RKE2 kept that path from its k3s lineage even
// though the cluster itself is RKE2, not k3s), and -n k8s.io selects the
// namespace the kubelet reads from, matching what "k3s ctr" did implicitly.
func LoadImageToRKE2Containerd(name string) error {
	// Stream the docker image tarball straight into RKE2's containerd
	// rather than buffering it to disk: docker save (stdout) -> ctr images
	// import (stdin).
	saveCmd := exec.Command("docker", "save", name)
	importCmd := exec.Command("sudo", "ctr", "-a", "/run/k3s/containerd/containerd.sock", "-n", "k8s.io", "images", "import", "-")

	pipe, err := saveCmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create docker save pipe: %w", err)
	}
	importCmd.Stdin = pipe

	var importOut bytes.Buffer
	importCmd.Stdout = &importOut
	importCmd.Stderr = &importOut

	if err := importCmd.Start(); err != nil {
		return fmt.Errorf("failed to start ctr images import: %w", err)
	}
	if err := saveCmd.Run(); err != nil {
		return fmt.Errorf("failed to run docker save %q: %w", name, err)
	}
	if err := importCmd.Wait(); err != nil {
		return fmt.Errorf("ctr images import failed: %q: %w", importOut.String(), err)
	}

	return nil
}

// GetNonEmptyLines converts given command output string into individual objects
// according to line breakers, and ignores the empty elements in it.
func GetNonEmptyLines(output string) []string {
	var res []string
	elements := strings.Split(output, "\n")
	for _, element := range elements {
		if element != "" {
			res = append(res, element)
		}
	}

	return res
}

// GetProjectDir will return the directory where the project is
func GetProjectDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return wd, fmt.Errorf("failed to get current working directory: %w", err)
	}
	wd = strings.ReplaceAll(wd, "/test/e2e", "")
	return wd, nil
}

// UncommentCode searches for target in the file and remove the comment prefix
// of the target content. The target content may span multiple lines.
func UncommentCode(filename, target, prefix string) error {
	// false positive
	// nolint:gosec
	content, err := os.ReadFile(filename)
	if err != nil {
		return fmt.Errorf("failed to read file %q: %w", filename, err)
	}
	strContent := string(content)

	idx := strings.Index(strContent, target)
	if idx < 0 {
		return fmt.Errorf("unable to find the code %q to be uncomment", target)
	}

	out := new(bytes.Buffer)
	_, err = out.Write(content[:idx])
	if err != nil {
		return fmt.Errorf("failed to write to output: %w", err)
	}

	scanner := bufio.NewScanner(bytes.NewBufferString(target))
	if !scanner.Scan() {
		return nil
	}
	for {
		if _, err = out.WriteString(strings.TrimPrefix(scanner.Text(), prefix)); err != nil {
			return fmt.Errorf("failed to write to output: %w", err)
		}
		// Avoid writing a newline in case the previous line was the last in target.
		if !scanner.Scan() {
			break
		}
		if _, err = out.WriteString("\n"); err != nil {
			return fmt.Errorf("failed to write to output: %w", err)
		}
	}

	if _, err = out.Write(content[idx+len(target):]); err != nil {
		return fmt.Errorf("failed to write to output: %w", err)
	}

	// false positive
	// nolint:gosec
	if err = os.WriteFile(filename, out.Bytes(), 0644); err != nil {
		return fmt.Errorf("failed to write file %q: %w", filename, err)
	}

	return nil
}
