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
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2" // nolint:revive,staticcheck
	. "github.com/onsi/gomega"    // nolint:revive,staticcheck

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/seeder"
	"github.com/tjjh89017/kezio/internal/store"
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

// Image tags for the three additional images this stage builds and
// imports locally, the same way BeforeSuite already does for the manager
// image (projectImage) - never pulled from a registry.
const (
	ingestImagePathImage       = "example.com/kezio-ingest:e2e"
	seederImagePathImage       = "example.com/kezio-seeder:e2e"
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

// imagePathTrackerURL is the opentracker announce URL the deployed
// controller-manager is told to use (via `make deploy-image-path`'s
// `kubectl set env SEEDER_TRACKER_URL=...`, which must stay in sync with
// this literal) and that this file uses locally to build the leecher's
// .torrent bytes with the same announce URL the seeder itself used.
const imagePathTrackerURL = "http://kezio-opentracker.kezio-system.svc.cluster.local:6969/announce"

// imagePathReadyTimeout bounds how long the Image takes to reach Ready:
// this exercises a real qemu-img convert, sfdisk dump, and partclone
// capture of a full Ubuntu cloud image inside the ingest Job, which is
// slower than the fake/stub path's near-instant transition but still
// bounded - a real hang here should fail loudly, not stall the job until
// its overall timeout.
const imagePathReadyTimeout = 15 * time.Minute

// imagePathLeechTimeout bounds how long the leecher Pod takes to finish
// downloading one partition's content over BitTorrent from the seeder.
const imagePathLeechTimeout = 5 * time.Minute

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
func registerImagePathContext() {
	Context("Image ingest and seeding (image-path)", func() {
		var (
			portForwards      []*exec.Cmd
			imageServiceLocal = "127.0.0.1:18080"
			seederGRPCLocal   = "127.0.0.1:15051"
			leecherGRPCLocal  = "127.0.0.1:15052"
			leecherPodName    = "kezio-e2e-leecher"
		)

		BeforeAll(func() {
			qcow2Path := os.Getenv("E2E_UBUNTU_QCOW2_PATH")
			Expect(qcow2Path).NotTo(BeEmpty(),
				"E2E_UBUNTU_QCOW2_PATH must name a downloaded Ubuntu cloud image when E2E_IMAGE_PATH=true")
			_, statErr := os.Stat(qcow2Path)
			Expect(statErr).NotTo(HaveOccurred(), "E2E_UBUNTU_QCOW2_PATH does not point at a readable file")

			By("building the ingest, seeder, and image-service images")
			buildAndImportImagePathImages()

			By("creating the image-service bearer token Secret")
			createImageServiceTokenSecret()

			By("deploying image-service, seeder, opentracker, the store, and wiring the manager for real ingest/seeding")
			runMake("deploy-image-path",
				"IMG="+projectImage,
				"IMAGE_SERVICE_IMG="+imageServiceImagePathImage,
				"INGEST_IMG="+ingestImagePathImage,
				"SEEDER_IMG="+seederImagePathImage,
			)

			By("waiting for every image-path Deployment to roll out")
			deployments := []string{
				"kezio-controller-manager", "kezio-image-service",
				"kezio-ezio-seeder", "kezio-opentracker",
			}
			for _, d := range deployments {
				waitForRollout(d)
			}

			By("building kezioctl")
			runMake("build-kezioctl")

			By("port-forwarding the image-service")
			portForwards = append(portForwards, mustPortForward("svc/kezio-image-service", imageServiceLocal, "8080"))
		})

		AfterAll(func() {
			By("stopping port-forwards and deleting the leecher Pod")
			for _, pf := range portForwards {
				stopPortForward(pf)
			}
			_, _ = utils.Run(exec.Command("kubectl", "delete", "pod", leecherPodName, "-n", namespace, "--ignore-not-found"))

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

		It("has the seeder report every Ready partition's content as an added torrent", func() {
			image := getImage(imagePathImageName)

			By("port-forwarding the seeder's gRPC control port")
			portForwards = append(portForwards, mustPortForward("svc/kezio-ezio-seeder", seederGRPCLocal, "50051"))

			By("polling GetTorrentStatus until every partition's info hash is present")
			Eventually(func(g Gomega) {
				present := seederTorrentHashes(g, seederGRPCLocal)
				for _, p := range image.Status.Partitions {
					if p.InfoHash == "" {
						continue
					}
					g.Expect(present).To(HaveKey(p.InfoHash),
						"expected the seeder to have added a torrent for partition %d (%s)", p.Number, p.Role)
				}
			}).WithTimeout(2 * time.Minute).WithPolling(2 * time.Second).Should(Succeed())
		})

		It("leeches the smallest partition's content from the seeder and byte-compares it against the store", func() {
			image := getImage(imagePathImageName)
			partition, err := utils.SmallestSeedablePartition(image.Status.Partitions)
			Expect(err).NotTo(HaveOccurred())
			By(fmt.Sprintf("selected partition %d (%s, %d bytes) as the smallest seedable content",
				partition.Number, partition.Role, partition.UsedBytes))

			By("reading torrent.info for that partition out of the store, via the seeder Deployment's read-only mount")
			info := loadTorrentInfoFromSeeder(partition.InfoHash)
			torrentBytes, err := store.BuildTorrentFile(info, imagePathTrackerURL)
			Expect(err).NotTo(HaveOccurred())

			By("starting a bare leecher Pod (the seeder image run as an ezio client, no seed_mode)")
			applyManifest(fmt.Sprintf(`
apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
  labels:
    app.kubernetes.io/name: kezio
    app.kubernetes.io/component: e2e-leecher
spec:
  restartPolicy: Never
  securityContext:
    runAsNonRoot: true
    # fsGroup: an emptyDir volume is created root:root 0755 by default,
    # which a non-root container (runAsUser 65532 below) can read and
    # traverse but not write into. The seeder Deployment never noticed
    # this (it mounts the store read-only, see
    # config/seeder/ezio-seeder-deployment.yaml), but this leecher does
    # need to write the downloaded pieces into /leech: the kubelet
    # chowns every volume's group ownership to fsGroup and grants it
    # group rwx, which - combined with every container in the pod
    # implicitly running with fsGroup as a supplementary group - is what
    # lets uid 65532 write here.
    fsGroup: 65532
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: leecher
      image: %s
      securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        runAsUser: 65532
        capabilities:
          drop: ["ALL"]
      volumeMounts:
        - name: leech
          mountPath: /leech
  volumes:
    - name: leech
      emptyDir: {}
`, leecherPodName, namespace, seederImagePathImage))
			waitForPodRunning(leecherPodName)

			By("port-forwarding the leecher's gRPC control port")
			portForwards = append(portForwards, mustPortForward("pod/"+leecherPodName, leecherGRPCLocal, "50051"))

			By("adding the torrent to the leecher without seed_mode, into an empty /leech")
			leecherClient, err := seeder.Dial(leecherGRPCLocal)
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = leecherClient.Close() }()
			Expect(leecherClient.AddTorrent(context.Background(), torrentBytes, "/leech", false,
				seeder.DefaultMaxUploads, seeder.DefaultMaxConnections)).To(Succeed())

			By("waiting for the leecher to report the torrent finished")
			// Completion itself is already the integrity proof: ezio
			// (libtorrent) verifies every downloaded piece's SHA-1
			// hash against torrent.info's, and a torrent only
			// reaches "finished" once every piece has verified. The
			// sha256sum comparison below is belt-and-suspenders on
			// top of that, not the primary integrity check.
			Eventually(func(g Gomega) {
				statuses, err := leecherClient.GetTorrentStatus(context.Background(), []string{partition.InfoHash})
				g.Expect(err).NotTo(HaveOccurred())
				t, ok := statuses[partition.InfoHash]
				g.Expect(ok).To(BeTrue(), "leecher does not know about the torrent yet")
				g.Expect(t.IsFinished).To(BeTrue(), "leecher torrent not finished yet")
			}).WithTimeout(imagePathLeechTimeout).WithPolling(3 * time.Second).Should(Succeed())

			By("byte-comparing every leeched extent file against the seeder's store copy " +
				"(belt-and-suspenders on top of piece-hash verification)")
			// Both paths go through store.ContentDataDir/ContentExtentPath
			// rather than a bare "<dir>/<name>" join: the torrent leecherClient
			// just downloaded is a BEP3 multi-file torrent named "content"
			// (store.BuildInfoDict), so every compliant client - the leecher
			// included - resolves each file entry as
			// "<save_path>/content/<name>", not "<save_path>/<name>" (see
			// internal/controller's addContent doc comment).
			infoHash := mustParseInfoHash(partition.InfoHash)
			for _, ext := range info.Extents {
				name := store.ExtentFileName(ext.Offset)
				leechedDigest := sha256sumInPod("pod/"+leecherPodName, store.ContentDataDir("/leech")+"/"+name)
				storeDigest := sha256sumInPod("deploy/kezio-ezio-seeder", store.ContentExtentPath("/store", infoHash, ext.Offset))
				Expect(leechedDigest).To(Equal(storeDigest), "extent file %s content mismatch between leecher and store", name)
			}
		})

		// registerSeedingBenchmarkSpec (e2e_benchmark_test.go) adds the
		// seeding throughput baseline as one more It() in this Context,
		// reusing the Image this BeforeAll already ingested and seeded -
		// but only when E2E_BENCHMARK=true, so the default image-path run
		// this Context otherwise supports never pays for the swarm.
		if benchmarkEnabled {
			registerSeedingBenchmarkSpec()
		}

		AfterEach(func() {
			if !CurrentSpecReport().Failed() {
				return
			}
			By("dumping image-path diagnostics: ingest Job, seeder, image-service, and the Image")
			dumpImagePathDiagnostics()
		})
	})
}

// buildAndImportImagePathImages builds the ingest, seeder, and
// image-service Docker images and imports them the same way BeforeSuite
// already does for the manager image: `ctr images import` against RKE2's
// containerd for a pre-provisioned RKE2 cluster, `kind load docker-image`
// otherwise. None of the three is ever pulled from a registry.
func buildAndImportImagePathImages() {
	runMake("docker-build-ingest", "INGEST_IMG="+ingestImagePathImage)
	runMake("docker-build-seeder", "SEEDER_IMG="+seederImagePathImage)
	runMake("docker-build-image-service", "IMAGE_SERVICE_IMG="+imageServiceImagePathImage)

	for _, img := range []string{ingestImagePathImage, seederImagePathImage, imageServiceImagePathImage} {
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

// waitForPodRunning waits for name in namespace to reach phase Running.
func waitForPodRunning(name string) {
	EventuallyWithOffset(1, func(g Gomega) {
		phase, err := getJSONPath("pod", name, "{.status.phase}")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(phase).To(Equal("Running"))
	}).WithTimeout(2 * time.Minute).WithPolling(2 * time.Second).Should(Succeed())
}

// getImage fetches and unmarshals the named Image in namespace. Every
// current caller (across e2e_image_path_test.go and e2e_benchmark_test.go)
// happens to pass imagePathImageName, but the parameter is kept: this is a
// generic kubectl-get-and-unmarshal helper, not one specific to that Image.
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

// seederTorrentHashes dials the seeder at grpcAddr and returns the set of
// info hashes it currently knows about.
func seederTorrentHashes(g Gomega, grpcAddr string) map[string]seeder.Torrent {
	c, err := seeder.Dial(grpcAddr)
	g.Expect(err).NotTo(HaveOccurred())
	defer func() { _ = c.Close() }()

	statuses, err := c.GetTorrentStatus(context.Background(), nil)
	g.Expect(err).NotTo(HaveOccurred())
	return statuses
}

// loadTorrentInfoFromSeeder reads and parses torrent.info for hash out of
// the store, via `kubectl exec` into the seeder Deployment (which mounts
// the store read-only at /store - see config/seeder/ezio-seeder-deployment.yaml).
// Reading it this way, rather than mounting the store into the test
// process, keeps the store PVC's only consumers exactly the ones a real
// deployment would have (ingest Job, seeder, and the manager for
// SEEDER_STORE_ROOT).
func loadTorrentInfoFromSeeder(hash string) *store.TorrentInfo {
	h := mustParseInfoHash(hash)
	path := store.ContentDirTorrentInfoPath(store.ContentDir("/store", h))
	cmd := exec.Command("kubectl", "exec", "deploy/kezio-ezio-seeder", "-n", namespace, "--", "cat", path)
	out, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "failed to read %s from the seeder", path)

	info, err := store.ParseTorrentInfo(bytes.NewReader([]byte(out)))
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "failed to parse torrent.info for %s", hash)
	return info
}

// sha256sumInPod runs `sha256sum <path>` inside target (a "pod/name" or
// "deploy/name" kubectl exec target) and returns the parsed hex digest.
func sha256sumInPod(target, path string) string {
	cmd := exec.Command("kubectl", "exec", target, "-n", namespace, "--", "sha256sum", path)
	out, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "sha256sum %s in %s failed", path, target)

	digest, err := utils.ParseSHA256Sum(out)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return digest
}

// mustParseInfoHash parses hash and fails the current spec on error.
func mustParseInfoHash(hash string) store.InfoHash {
	h, err := store.ParseInfoHash(hash)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return h
}

// dumpImagePathDiagnostics prints the ingest Job, seeder, image-service,
// and Image state for the image-path stage to GinkgoWriter, so a failing
// run points at which step broke without needing the workflow's own
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
		{"seeder logs", exec.Command("kubectl", "logs", "-n", namespace, "deploy/kezio-ezio-seeder", "--tail=500")},
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
