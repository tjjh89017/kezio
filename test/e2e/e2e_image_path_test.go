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
	"net"
	"os"
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2" // nolint:revive,staticcheck
	. "github.com/onsi/gomega"    // nolint:revive,staticcheck

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/controller"
	"github.com/tjjh89017/kezio/internal/ingest"
	"github.com/tjjh89017/kezio/test/utils"
)

// imagePathEnabled gates the "Image ingest and seeding" Context: it only
// registers its specs when E2E_IMAGE_PATH=true. This keeps the default
// (fast, stub-ingest/fake-deployer) e2e run's timing untouched - the extra
// Context below is not merely skipped at runtime, it is never declared at
// all unless this is set, matching the "same suite behind an env gate"
// shape test/e2e_image_path_test.go's work item called for over a second
// ginkgo binary or a hand-rolled label filter.
var imagePathEnabled = os.Getenv("E2E_IMAGE_PATH") == "true"

// Image tags for the two images this stage builds and imports locally,
// the same way BeforeSuite already does for the manager image
// (projectImage) - never pulled from a registry. No seeder image is
// built here (see registerImagePathContext's doc comment: there is no
// Machine, so no per-Image seeder Deployment is ever created).
const (
	ingestImagePathImage       = "example.com/kezio-ingest:e2e"
	imageServiceImagePathImage = "example.com/kezio-image-service:e2e"
)

// imagePathImageName is the Image CR this stage's upload creates.
const imagePathImageName = "e2e-image-path-ubuntu"

// imageServiceTokenSecretName and imageServiceToken authenticate
// kezioctl's upload against the image-service Deployment this stage
// applies (see config/image-service/deployment.yaml: it requires a
// pre-existing Secret with this exact name). A fixed value is fine here:
// this token only ever protects a throwaway, single-run CI cluster.
const (
	//nolint:gosec // not a credential, a fixed e2e-only test fixture name
	imageServiceTokenSecretName = "kezio-image-service-token"
	imageServiceToken           = "kezio-e2e-image-path-token"
)

// imagePathReadyTimeout bounds how long the Image takes to reach Ready:
// this exercises a real qemu-img convert, sfdisk dump, and partclone
// capture of a full Ubuntu cloud image inside the ingest Job, which is
// slower than the fake/stub path's near-instant transition but still
// bounded - a real hang here should fail loudly, not stall the job until
// its overall timeout.
const imagePathReadyTimeout = 15 * time.Minute

// registerImagePathContext adds the "Image ingest and seeding" Context as
// a Context INSIDE the same Describe("Manager", Ordered, ...) container
// e2e_test.go declares (see this file's call site there, guarded by
// imagePathEnabled) - not as a second, separate top-level Describe. That
// distinction matters: Ginkgo randomizes the run order of top-level
// containers by default, so a second `Describe("Manager", ...)` here
// would be a different container with no ordering guarantee relative to
// the original one, even sharing its name. This Context needs the
// namespace/CRDs/controller-manager the original Describe's BeforeAll
// creates to already exist, and its own AfterAll needs to run before that
// Describe's AfterAll tears the controller-manager down (see this
// function's own AfterAll doc comment) - both of which only hold if this
// Context is registered as a true nested sibling within that same
// container.
//
// This stage exercises real ingest (qemu-img/sfdisk/partclone) through
// image-service and the per-partition-PVC/publish path only. It runs no
// seeder: seeding is demand-driven by a Machine deploying the Image (see
// internal/controller/seeder_deployment.go), and this stage never creates
// a Machine, so a per-Image seeder Deployment is never expected to exist
// for it - proving exactly that absence is one of this stage's own
// assertions below. End-to-end seeding proof (a real per-Image seeder
// Deployment actually serving a leecher) lives in the deploy-path e2e
// lane, which does drive a Machine through a real deploy.
func registerImagePathContext() {
	Context("Image ingest and seeding (image-path)", func() {
		var (
			portForwards      []*exec.Cmd
			imageServiceLocal = "127.0.0.1:18080"
		)

		BeforeAll(func() {
			qcow2Path := os.Getenv("E2E_UBUNTU_QCOW2_PATH")
			Expect(qcow2Path).NotTo(BeEmpty(),
				"E2E_UBUNTU_QCOW2_PATH must name a downloaded Ubuntu cloud image when E2E_IMAGE_PATH=true")
			_, statErr := os.Stat(qcow2Path)
			Expect(statErr).NotTo(HaveOccurred(), "E2E_UBUNTU_QCOW2_PATH does not point at a readable file")

			By("building the ingest and image-service images")
			buildAndImportImagePathImages()

			By("creating the image-service bearer token Secret")
			createImageServiceTokenSecret()

			By("deploying image-service and wiring the manager for real ingest")
			runMake("deploy-image-path",
				"IMG="+projectImage,
				"IMAGE_SERVICE_IMG="+imageServiceImagePathImage,
				"INGEST_IMG="+ingestImagePathImage,
			)

			By("waiting for every image-path Deployment to roll out")
			for _, d := range []string{"kezio-controller-manager", "kezio-image-service"} {
				waitForRollout(d)
			}

			By("building kezioctl")
			runMake("build-kezioctl")

			By("port-forwarding the image-service")
			portForwards = append(portForwards, mustPortForward("svc/kezio-image-service", imageServiceLocal, "8080"))
		})

		AfterAll(func() {
			By("stopping port-forwards")
			for _, pf := range portForwards {
				stopPortForward(pf)
			}

			// Delete the Image this stage created (and wait for it)
			// before the outer AfterAll runs "make undeploy": that
			// tears down the controller-manager, which is the only
			// thing that can clear this Image's finalizer - see
			// deleteAndWait's doc comment for why this ordering
			// matters.
			By("deleting the image-path Image")
			deleteAndWait("image", imagePathImageName, 2*time.Minute)
		})

		It("ingests an uploaded Ubuntu cloud image to Ready with real partclone/qemu-img/sfdisk", func() {
			By("uploading the Ubuntu cloud image through kezioctl")
			cmd := exec.Command("bin/kezioctl", "image", "upload", os.Getenv("E2E_UBUNTU_QCOW2_PATH"),
				"--name", imagePathImageName,
				"--namespace", namespace,
				"--server", "http://"+imageServiceLocal,
				"--token", imageServiceToken,
				// The Ubuntu cloud image is fetched with a ".img"
				// filename despite actually being qcow2, and
				// DetectFormatFromFilename now treats ".img" as
				// ambiguous (see internal/kezioctl/format.go) rather
				// than guessing raw. Pass the real format explicitly
				// so the Image CR's declared format matches what
				// qemu-img reports during ingest.
				"--format", "qcow2",
			)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "kezioctl image upload failed")

			By("waiting for the Image to reach state Ready (real ingest: qemu-img, sfdisk, partclone)")
			Eventually(func(g Gomega) {
				state, err := getJSONPath("image", imagePathImageName, "{.status.state}")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(state).To(Equal(keziov1alpha1.ImageStateReady))
			}).WithTimeout(imagePathReadyTimeout).WithPolling(5 * time.Second).Should(Succeed())

			By("asserting status.disk and status.partitions were populated with info hashes")
			image := getImage(imagePathImageName)
			Expect(image.Status.Disk).NotTo(BeNil(), "expected ingest to populate status.disk")
			Expect(image.Status.Disk.SizeBytes).To(BeNumerically(">", 0))
			Expect(image.Status.Partitions).NotTo(BeEmpty(), "expected ingest to populate status.partitions")
			for _, p := range image.Status.Partitions {
				if p.Role == keziov1alpha1.PartitionRoleSwap {
					continue // restored by UUID, not content - see ImageLayoutSlot.IsBlank
				}
				Expect(p.InfoHash).NotTo(BeEmpty(), "expected partition %d (%s) to have an infoHash", p.Number, p.Role)
			}
		})

		It("publishes every content partition into its own PVC", func() {
			image := getImage(imagePathImageName)

			out, err := utils.Run(exec.Command("kubectl", "get", "pvc", "-n", namespace,
				"-l", controller.AppComponentLabel+"=partition-content", "-o", "json"))
			Expect(err).NotTo(HaveOccurred())

			var pvcList struct {
				Items []struct {
					Metadata struct {
						OwnerReferences []struct {
							Name string `json:"name"`
						} `json:"ownerReferences"`
					} `json:"metadata"`
					Status struct {
						Phase string `json:"phase"`
					} `json:"status"`
				} `json:"items"`
			}
			Expect(json.Unmarshal([]byte(out), &pvcList)).To(Succeed())

			var ownedBound int
			for _, pvc := range pvcList.Items {
				owned := false
				for _, ref := range pvc.Metadata.OwnerReferences {
					if ref.Name == imagePathImageName {
						owned = true
					}
				}
				if !owned {
					continue
				}
				Expect(pvc.Status.Phase).To(Equal("Bound"), "expected every partition PVC to be Bound")
				ownedBound++
			}

			wantContentPartitions := 0
			for _, p := range image.Status.Partitions {
				if p.InfoHash != "" {
					wantContentPartitions++
				}
			}
			Expect(ownedBound).To(Equal(wantContentPartitions),
				"expected one Bound partition-content PVC per content partition")
		})

		It("writes the Image's ImageLayout", func() {
			layoutName := ingest.ImageLayoutName(imagePathImageName)
			_, err := getJSONPath("imagelayout", layoutName, "{.metadata.name}")
			Expect(err).NotTo(HaveOccurred(), "expected imagelayout %s to exist", layoutName)
		})

		It("creates no per-Image seeder Deployment or Pod (no Machine ever demands one)", func() {
			out, err := utils.Run(exec.Command("kubectl", "get", "deployment", "-n", namespace,
				"-l", fmt.Sprintf("%s=%s", controller.SeederDeploymentImageLabel, imagePathImageName),
				"-o", "name"))
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(BeEmpty(), "expected no seeder Deployment for an Image with no deploying Machine")

			out, err = utils.Run(exec.Command("kubectl", "get", "pods", "-n", namespace,
				"-l", fmt.Sprintf("%s=%s,%s=%s",
					"app.kubernetes.io/name", "kezio", controller.AppComponentLabel, controller.SeederComponentValue),
				"-o", "name"))
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(BeEmpty(), "expected no seeder pod for an Image with no deploying Machine")
		})

		AfterEach(func() {
			if !CurrentSpecReport().Failed() {
				return
			}
			By("dumping image-path diagnostics: ingest Job, image-service, and the Image")
			dumpImagePathDiagnostics()
		})
	})
}

// buildAndImportImagePathImages builds the ingest and image-service
// Docker images and imports them the same way BeforeSuite already does
// for the manager image: `ctr images import` against RKE2's containerd
// for a pre-provisioned RKE2 cluster, `kind load docker-image` otherwise.
// Neither is ever pulled from a registry.
func buildAndImportImagePathImages() {
	// CI sets E2E_SKIP_IMAGE_PATH_IMAGE_BUILD=true once it has already
	// imported both images from the "images" job's artifacts (see
	// .github/workflows/main.yaml's e2e-image-path job) - never set by a
	// local dev run.
	if os.Getenv("E2E_SKIP_IMAGE_PATH_IMAGE_BUILD") == "true" {
		_, _ = fmt.Fprintf(GinkgoWriter, "E2E_SKIP_IMAGE_PATH_IMAGE_BUILD=true: reusing the already-imported images\n")
		return
	}

	runMake("docker-build-ingest", "INGEST_IMG="+ingestImagePathImage)
	runMake("docker-build-image-service", "IMAGE_SERVICE_IMG="+imageServiceImagePathImage)

	for _, img := range []string{ingestImagePathImage, imageServiceImagePathImage} {
		var err error
		if e2eCluster == "rke2" {
			err = utils.LoadImageToRKE2Containerd(img)
		} else {
			err = utils.LoadImageToKindClusterWithName(img)
		}
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), "failed to import %s", img)
	}
}

// createImageServiceTokenSecret creates the bearer-token Secret
// config/image-service/deployment.yaml requires to already exist. The
// image-path stage's namespace is created fresh by the outer BeforeAll, so
// there is nothing to clean up first.
func createImageServiceTokenSecret() {
	cmd := exec.Command("kubectl", "create", "secret", "generic", imageServiceTokenSecretName,
		"-n", namespace, "--from-literal=token="+imageServiceToken)
	_, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "failed to create the image-service token Secret")
}

// runMake runs `make <target> <args...>` from the project root and fails
// the current spec on error, the same shape every existing e2e setup step
// (make docker-build, make deploy, ...) already uses via utils.Run.
func runMake(target string, args ...string) {
	cmd := exec.Command("make", append([]string{target}, args...)...)
	_, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "make %s failed", target)
}

// waitForRollout waits for deployment/name in namespace to finish rolling
// out.
func waitForRollout(name string) {
	cmd := exec.Command("kubectl", "rollout", "status", "deployment/"+name, "-n", namespace, "--timeout=180s")
	_, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "deployment/%s did not roll out", name)
}

// getImage fetches and unmarshals the named Image in namespace. Every
// current caller happens to pass imagePathImageName, but this is a
// generic kubectl-get-and-unmarshal helper, not one specific to that
// Image.
//
//nolint:unparam // see doc comment: deliberately generic, not accidentally single-value
func getImage(name string) *keziov1alpha1.Image {
	cmd := exec.Command("kubectl", "get", "image", name, "-n", namespace, "-o", "json")
	out, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())

	image := &keziov1alpha1.Image{}
	ExpectWithOffset(1, json.Unmarshal([]byte(out), image)).To(Succeed())
	return image
}

// mustPortForward starts `kubectl port-forward` from target's remotePort
// to localAddr (host:port) in the background and waits for the local port
// to accept connections before returning. The caller is responsible for
// passing the returned *exec.Cmd to stopPortForward during cleanup.
func mustPortForward(target, localAddr, remotePort string) *exec.Cmd {
	_, localPort, err := net.SplitHostPort(localAddr)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())

	cmd := exec.Command("kubectl", "port-forward", "-n", namespace, target, localPort+":"+remotePort)
	cmd.Stdout = GinkgoWriter
	cmd.Stderr = GinkgoWriter
	ExpectWithOffset(1, cmd.Start()).To(Succeed(), "failed to start kubectl port-forward for %s", target)

	EventuallyWithOffset(1, func() error {
		conn, dialErr := net.DialTimeout("tcp", localAddr, time.Second)
		if dialErr != nil {
			return dialErr
		}
		return conn.Close()
	}).WithTimeout(30*time.Second).WithPolling(500*time.Millisecond).Should(Succeed(),
		"port-forward to %s on %s never became reachable", target, localAddr)

	return cmd
}

// stopPortForward terminates a port-forward started by mustPortForward.
func stopPortForward(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}

// dumpImagePathDiagnostics prints the ingest Job, image-service, and
// Image state for the image-path stage to GinkgoWriter, so a failing run
// points at which step broke without needing the workflow's own
// failure-log-dump step (see .github/workflows/test-e2e.yml's
// test-e2e-image-path job) to be re-run.
func dumpImagePathDiagnostics() {
	dumps := []struct {
		label string
		cmd   *exec.Cmd
	}{
		{"image-path Image", exec.Command("kubectl", "get", "image", imagePathImageName, "-n", namespace, "-o", "yaml")},
		{"ingest Jobs", exec.Command(
			"kubectl", "get", "jobs", "-n", namespace, "-l", "kezio.kojuro.date/image", "-o", "wide")},
		{"ingest Job logs", exec.Command(
			"kubectl", "logs", "-n", namespace, "-l", "kezio.kojuro.date/image", "--all-containers", "--tail=500")},
		{"partition PVCs", exec.Command(
			"kubectl", "get", "pvc", "-n", namespace, "-l", controller.AppComponentLabel+"=partition-content", "-o", "wide")},
		{"image-service logs", exec.Command(
			"kubectl", "logs", "-n", namespace, "deploy/kezio-image-service", "--tail=500")},
		{"pods", exec.Command("kubectl", "get", "pods", "-n", namespace, "-o", "wide")},
	}
	for _, d := range dumps {
		out, err := utils.Run(d.cmd)
		if err != nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "%s: failed to fetch: %v\n", d.label, err)
			continue
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "===== %s =====\n%s\n", d.label, out)
	}
}
