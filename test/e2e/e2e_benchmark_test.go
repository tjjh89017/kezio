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
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2" // nolint:revive,staticcheck
	. "github.com/onsi/gomega"    // nolint:revive,staticcheck

	"github.com/tjjh89017/kezio/internal/seeder"
	"github.com/tjjh89017/kezio/internal/store"
	"github.com/tjjh89017/kezio/test/utils"
)

// benchmarkEnabled gates the seeding throughput baseline spec registered by
// registerSeedingBenchmarkSpec below. It is checked on top of, not instead
// of, imagePathEnabled (see e2e_image_path_test.go): the benchmark leeches
// against the same Image the image-path stage already ingested and seeded,
// so it is registered as one more It() inside that stage's Context rather
// than duplicating BeforeAll's image-service/seeder/opentracker setup. A
// dispatch of this benchmark therefore always sets both E2E_IMAGE_PATH=true
// and E2E_BENCHMARK=true - see .github/workflows/benchmark-seeding.yml.
var benchmarkEnabled = os.Getenv("E2E_BENCHMARK") == "true"

const (
	// benchmarkDefaultLeechers is how many leecher Pods swarm the seeder
	// when E2E_BENCHMARK_LEECHERS is unset - "10+" per this stage's own
	// baseline goal. benchmarkRequestedLeecherCount below still runs this
	// count (or an env override) through benchmarkAvailableDiskBytes'
	// disk-headroom cap before actually launching Pods; see that
	// function's doc comment for why a free CI runner may end up running
	// fewer than this.
	benchmarkDefaultLeechers = 10

	// benchmarkDiskSafetyFraction is the fraction of the runner's free
	// disk that must stay unused regardless of leecher count - headroom
	// for the OS, the four already-built container images, and the store
	// PVC's own copy of the content this benchmark leeches, all of which
	// already occupy this same disk on the single-node RKE2 cluster this
	// stage runs on. See utils.CapLeecherCount.
	benchmarkDiskSafetyFraction = 0.2

	// benchmarkLeechTimeout bounds how long any one leecher may take to
	// finish before it counts as a benchmark failure (not merely a slow
	// sample): a real hang here should fail loudly rather than let the
	// whole spec stall until the job's overall timeout.
	benchmarkLeechTimeout = 10 * time.Minute

	// benchmarkMetricsSampleInterval is how often the seeder's CPU/memory
	// is sampled via `kubectl top pod` while the swarm runs.
	benchmarkMetricsSampleInterval = 3 * time.Second

	// benchmarkLeecherPodNamePrefix names every benchmark leecher Pod
	// "<prefix><index>", distinct from the single "kezio-e2e-leecher" Pod
	// the image-path stage's own leech spec uses (e2e_image_path_test.go)
	// so the two specs never collide even when both run in the same
	// suite invocation.
	benchmarkLeecherPodNamePrefix = "kezio-e2e-bench-leecher-"

	// benchmarkLeecherGRPCBasePort is the first local port `kubectl
	// port-forward` binds each leecher's gRPC control port to; leecher i
	// gets basePort+i, so N leechers need N-1 free ports above this one.
	benchmarkLeecherGRPCBasePort = 15100

	// benchmarkSeederComponentLabel selects the seeder Pod for `kubectl
	// top pod`, matching config/seeder/ezio-seeder-deployment.yaml's
	// template labels.
	benchmarkSeederComponentLabel = "app.kubernetes.io/component=ezio-seeder"
)

// registerSeedingBenchmarkSpec adds one more It() to the enclosing
// Context("Image ingest and seeding (image-path)", ...) - see this
// function's call site in e2e_image_path_test.go, guarded by
// benchmarkEnabled. It reuses that Context's already-ingested,
// already-seeded imagePathImageName Image; it does not upload or ingest
// anything itself.
func registerSeedingBenchmarkSpec() {
	It("records a seeding throughput baseline against N concurrent leechers (benchmark, not a gate)", func() {
		image := getImage(imagePathImageName)
		partition, err := utils.LargestSeedablePartition(image.Status.Partitions)
		Expect(err).NotTo(HaveOccurred())
		By(fmt.Sprintf("selected partition %d (%s, %d bytes) as the largest seedable content (the rootfs)",
			partition.Number, partition.Role, partition.UsedBytes))

		info := loadTorrentInfoFromSeeder(partition.InfoHash)
		torrentBytes, err := store.BuildTorrentFile(info, imagePathTrackerURL)
		Expect(err).NotTo(HaveOccurred())

		requested := benchmarkRequestedLeecherCount()
		available := benchmarkAvailableDiskBytes()
		n, err := utils.CapLeecherCount(requested, partition.UsedBytes, available, benchmarkDiskSafetyFraction)
		Expect(err).NotTo(HaveOccurred())
		if n < 1 {
			Skip(fmt.Sprintf(
				"not enough free disk to run the benchmark: %d bytes available, need at least one "+
					"%d-byte content copy plus a %.0f%% safety margin - rerun on a bigger runner or against "+
					"a smaller partition", available, partition.UsedBytes, benchmarkDiskSafetyFraction*100))
		}
		if n < requested {
			_, _ = fmt.Fprintf(GinkgoWriter,
				"WARNING: capping the benchmark to %d leechers (requested %d): only %d bytes free disk for "+
					"%d-byte content copies with a %.0f%% safety margin. Run E2E_BENCHMARK_LEECHERS=%d on a "+
					"bigger (e.g. self-hosted) runner for the full count.\n",
				n, requested, available, partition.UsedBytes, benchmarkDiskSafetyFraction*100, requested)
		}

		By(fmt.Sprintf("starting %d leecher Pods", n))
		names := make([]string, n)
		for i := 0; i < n; i++ {
			names[i] = benchmarkLeecherPodName(i)
			applyBenchmarkLeecherPod(names[i], seederImagePathImage)
		}
		defer func() {
			By("deleting the benchmark leecher Pods")
			for _, name := range names {
				_, _ = utils.Run(exec.Command("kubectl", "delete", "pod", name, "-n", namespace, "--ignore-not-found"))
			}
		}()
		for _, name := range names {
			waitForPodRunning(name)
		}

		By("port-forwarding every leecher's gRPC control port")
		leechers := make([]benchmarkLeecher, n)
		pfs := make([]*exec.Cmd, 0, n)
		defer func() {
			for _, pf := range pfs {
				stopPortForward(pf)
			}
		}()
		for i, name := range names {
			localAddr := fmt.Sprintf("127.0.0.1:%d", benchmarkLeecherGRPCBasePort+i)
			pfs = append(pfs, mustPortForward("pod/"+name, localAddr, "50051"))
			client, dialErr := seeder.Dial(localAddr)
			Expect(dialErr).NotTo(HaveOccurred())
			leechers[i] = benchmarkLeecher{name: name, client: client}
		}
		defer func() {
			for _, l := range leechers {
				_ = l.client.Close()
			}
		}()

		By("sampling the seeder's CPU/memory while the swarm runs")
		sampler := newSeederMetricsSampler()
		stopSampling := make(chan struct{})
		var samplerWG sync.WaitGroup
		samplerWG.Add(1)
		go func() {
			defer samplerWG.Done()
			sampler.run(benchmarkMetricsSampleInterval, stopSampling)
		}()

		By(fmt.Sprintf("adding the torrent to all %d leechers and timing completion", n))
		resultsCh := make(chan benchmarkLeechResult, n)
		swarmStart := time.Now()
		var wg sync.WaitGroup
		for _, l := range leechers {
			wg.Add(1)
			go func() {
				defer GinkgoRecover()
				defer wg.Done()
				resultsCh <- leechOneTorrent(l.name, l.client, torrentBytes, partition.InfoHash, swarmStart)
			}()
		}
		wg.Wait()
		close(resultsCh)
		close(stopSampling)
		samplerWG.Wait()
		wallTime := time.Since(swarmStart)

		var durations []time.Duration
		var failures []string
		for r := range resultsCh {
			if r.err != nil {
				failures = append(failures, fmt.Sprintf("%s: %v", r.name, r.err))
				continue
			}
			durations = append(durations, r.duration)
		}
		Expect(failures).To(BeEmpty(), "%d/%d leechers failed to finish: %s", len(failures), n, strings.Join(failures, "; "))

		summary, err := utils.SummarizeDurations(durations)
		Expect(err).NotTo(HaveOccurred())
		totalBytes := int64(n) * partition.UsedBytes
		throughput, err := utils.ThroughputMBps(totalBytes, wallTime)
		Expect(err).NotTo(HaveOccurred())

		peakCPUMilli, peakMemBytes, sampleCount, degraded, degradeReason := sampler.summary()

		report := formatBenchmarkReport(benchmarkReport{
			leecherCount:    n,
			requestedCount:  requested,
			contentBytes:    partition.UsedBytes,
			contentRole:     partition.Role,
			durations:       summary,
			wallTime:        wallTime,
			throughputMBps:  throughput,
			peakCPUMilli:    peakCPUMilli,
			peakMemBytes:    peakMemBytes,
			metricsSamples:  sampleCount,
			metricsDegraded: degraded,
			degradeReason:   degradeReason,
		})

		fmt.Println(report) //nolint:forbidigo // deliberate: this is the benchmark's log output, not debug noise
		_, _ = fmt.Fprint(GinkgoWriter, report)
		writeStepSummary(report)
	})
}

// benchmarkLeecher pairs one leecher Pod's name with its dialed gRPC
// client.
type benchmarkLeecher struct {
	name   string
	client *seeder.Client
}

// benchmarkLeechResult is one leecher's outcome: either the wall-clock
// duration from swarm start to torrent completion, or the error that kept
// it from finishing.
type benchmarkLeechResult struct {
	name     string
	duration time.Duration
	err      error
}

// leechOneTorrent adds torrentBytes to client without seed_mode and polls
// GetTorrentStatus until infoHash is finished or benchmarkLeechTimeout
// elapses, returning the duration since start on success. It runs on its
// own goroutine (one per leecher, see registerSeedingBenchmarkSpec), so it
// reports failure through the returned error rather than a Gomega
// assertion.
func leechOneTorrent(
	name string, client *seeder.Client, torrentBytes []byte, infoHash string, start time.Time,
) benchmarkLeechResult {
	err := client.AddTorrent(context.Background(), torrentBytes, "/leech", false,
		seeder.DefaultMaxUploads, seeder.DefaultMaxConnections)
	if err != nil {
		return benchmarkLeechResult{name: name, err: fmt.Errorf("AddTorrent: %w", err)}
	}

	const pollInterval = 3 * time.Second
	deadline := time.Now().Add(benchmarkLeechTimeout)
	var lastErr error
	for {
		statuses, err := client.GetTorrentStatus(context.Background(), []string{infoHash})
		switch {
		case err != nil:
			lastErr = err
		case statuses[infoHash].IsFinished:
			return benchmarkLeechResult{name: name, duration: time.Since(start)}
		}

		if time.Now().After(deadline) {
			return benchmarkLeechResult{
				name: name,
				err:  fmt.Errorf("did not finish within %s (last GetTorrentStatus error: %v)", benchmarkLeechTimeout, lastErr),
			}
		}
		time.Sleep(pollInterval)
	}
}

// benchmarkLeecherPodName returns the Pod name for leecher index i.
func benchmarkLeecherPodName(i int) string {
	return fmt.Sprintf("%s%d", benchmarkLeecherPodNamePrefix, i)
}

// applyBenchmarkLeecherPod creates one bare leecher Pod (the seeder image
// run as an ezio client, no seed_mode) named name, the same shape the
// image-path stage's own single leecher Pod uses (see
// e2e_image_path_test.go's "leeches the smallest partition's content"
// spec) - each with its own emptyDir, so N leechers never share
// downloaded state.
func applyBenchmarkLeecherPod(name, image string) {
	applyManifest(fmt.Sprintf(`
apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
  labels:
    app.kubernetes.io/name: kezio
    app.kubernetes.io/component: e2e-benchmark-leecher
spec:
  restartPolicy: Never
  securityContext:
    runAsNonRoot: true
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
`, name, namespace, image))
}

// benchmarkRequestedLeecherCount reads E2E_BENCHMARK_LEECHERS (a positive
// integer), defaulting to benchmarkDefaultLeechers when unset. This is the
// count the benchmark was asked to run, before benchmarkAvailableDiskBytes'
// disk headroom caps it down to what the runner can actually hold.
func benchmarkRequestedLeecherCount() int {
	raw := os.Getenv("E2E_BENCHMARK_LEECHERS")
	if raw == "" {
		return benchmarkDefaultLeechers
	}
	n, err := strconv.Atoi(raw)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "E2E_BENCHMARK_LEECHERS must be an integer, got %q", raw)
	ExpectWithOffset(1, n).To(BeNumerically(">", 0), "E2E_BENCHMARK_LEECHERS must be positive, got %d", n)
	return n
}

// benchmarkAvailableDiskBytes returns the free disk space on this test
// process's own root filesystem ("/"), via `df`. On this stage's
// single-node RKE2 cluster (see .github/workflows/benchmark-seeding.yml),
// RKE2 runs directly on the runner (not inside a Kind/Docker node), so this
// is the same disk every leecher Pod's emptyDir volume will actually
// consume - the same assumption the image-path job's own "Resource check"
// step already makes when it runs `df -h` on the runner directly.
func benchmarkAvailableDiskBytes() int64 {
	cmd := exec.Command("df", "--output=avail", "-B1", "/")
	out, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "failed to query free disk space with df")

	lines := strings.Split(strings.TrimSpace(out), "\n")
	ExpectWithOffset(1, len(lines)).To(BeNumerically(">=", 2), "unexpected df output: %q", out)

	avail, err := strconv.ParseInt(strings.TrimSpace(lines[len(lines)-1]), 10, 64)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "failed to parse df output %q", out)
	return avail
}

// seederMetricsSampler periodically samples the seeder Pod's CPU/memory
// via `kubectl top pod` and tracks the peak of each. It degrades
// gracefully when metrics-server is unavailable (or `kubectl top`
// otherwise fails): the first failure is recorded and reported, and
// sampling simply stops producing data rather than failing the benchmark -
// this is a baseline recorder, not a gate, and the seeding throughput
// numbers stand on their own without a resource sample.
type seederMetricsSampler struct {
	mu            sync.Mutex
	peakCPUMilli  int64
	peakMemBytes  int64
	samples       int
	degraded      bool
	degradeReason string
}

func newSeederMetricsSampler() *seederMetricsSampler {
	return &seederMetricsSampler{}
}

// run samples immediately, then every interval, until stop is closed.
func (s *seederMetricsSampler) run(interval time.Duration, stop <-chan struct{}) {
	s.sampleOnce()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			s.sampleOnce()
		}
	}
}

// sampleOnce runs `kubectl top pod` once for the seeder Pod and folds the
// result into the running peak. A failure (metrics-server absent, or a
// malformed line) is recorded once via degraded/degradeReason and does not
// stop future samples from being attempted, in case metrics-server becomes
// available partway through (e.g. it was still starting up).
func (s *seederMetricsSampler) sampleOnce() {
	cmd := exec.Command("kubectl", "top", "pod", "-n", namespace, "-l", benchmarkSeederComponentLabel, "--no-headers")
	out, err := utils.Run(cmd)
	if err != nil {
		s.recordDegraded(err.Error())
		return
	}

	line := strings.TrimSpace(out)
	if line == "" {
		s.recordDegraded("kubectl top pod returned no data for the seeder Pod")
		return
	}
	firstLine := strings.SplitN(line, "\n", 2)[0]

	_, cpuMilli, memBytes, parseErr := utils.ParseKubectlTopLine(firstLine)
	if parseErr != nil {
		s.recordDegraded(parseErr.Error())
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.samples++
	if cpuMilli > s.peakCPUMilli {
		s.peakCPUMilli = cpuMilli
	}
	if memBytes > s.peakMemBytes {
		s.peakMemBytes = memBytes
	}
}

func (s *seederMetricsSampler) recordDegraded(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.degraded {
		s.degraded = true
		s.degradeReason = reason
	}
}

// summary returns the peak CPU (millicores) and memory (bytes) observed,
// how many successful samples contributed to that peak, and whether (and
// why) sampling ever failed.
func (s *seederMetricsSampler) summary() (
	peakCPUMilli, peakMemBytes int64, sampleCount int, degraded bool, degradeReason string,
) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peakCPUMilli, s.peakMemBytes, s.samples, s.degraded, s.degradeReason
}

// benchmarkReport holds every value formatBenchmarkReport renders.
type benchmarkReport struct {
	leecherCount    int
	requestedCount  int
	contentBytes    int64
	contentRole     string
	durations       utils.DurationSummary
	wallTime        time.Duration
	throughputMBps  float64
	peakCPUMilli    int64
	peakMemBytes    int64
	metricsSamples  int
	metricsDegraded bool
	degradeReason   string
}

// formatBenchmarkReport renders r as a Markdown section: a table of the
// headline numbers (leecher count, content size, per-leecher min/median/
// max, aggregate throughput, seeder peak CPU/RSS) plus a closing note that
// this is a baseline recording, not a pass/fail gate, written so it reads
// the same whether it lands in a terminal log or $GITHUB_STEP_SUMMARY.
func formatBenchmarkReport(r benchmarkReport) string {
	var b strings.Builder

	fmt.Fprintf(&b, "\n## Seeding throughput baseline\n\n")
	if r.leecherCount < r.requestedCount {
		fmt.Fprintf(&b, "_Capped to %d leechers (%d requested) by available disk headroom on this runner._\n\n",
			r.leecherCount, r.requestedCount)
	}

	fmt.Fprintf(&b, "| Metric | Value |\n|---|---|\n")
	fmt.Fprintf(&b, "| Leechers | %d |\n", r.leecherCount)
	fmt.Fprintf(&b, "| Content | partition role %q, %s |\n", r.contentRole, formatByteSize(r.contentBytes))
	fmt.Fprintf(&b, "| Per-leecher time (min / median / max) | %s / %s / %s |\n",
		r.durations.Min.Round(time.Millisecond),
		r.durations.Median.Round(time.Millisecond),
		r.durations.Max.Round(time.Millisecond))
	fmt.Fprintf(&b, "| Aggregate wall time | %s |\n", r.wallTime.Round(time.Millisecond))
	fmt.Fprintf(&b, "| Aggregate throughput | %.2f MB/s |\n", r.throughputMBps)
	if r.metricsSamples == 0 {
		fmt.Fprintf(&b, "| Seeder peak CPU / RSS | unavailable (%s) |\n", r.degradeReason)
	} else {
		degradedNote := ""
		if r.metricsDegraded {
			degradedNote = fmt.Sprintf("; sampling degraded partway through (%s)", r.degradeReason)
		}
		fmt.Fprintf(&b, "| Seeder peak CPU / RSS | %dm / %s (%d samples%s) |\n",
			r.peakCPUMilli, formatByteSize(r.peakMemBytes), r.metricsSamples, degradedNote)
	}

	fmt.Fprintf(&b, "\n_Baseline note: this run measures ezio/libtorrent's current mmap-file-mode disk-IO "+
		"backend. That backend may change in a future libtorrent version, so this number exists for "+
		"before/after comparison - it is not a pass/fail threshold._\n")

	return b.String()
}

// formatByteSize renders n bytes as a human-readable binary (KiB/MiB/...)
// size, matching the units `kubectl top`/`df -h` already use elsewhere in
// this stage's output.
func formatByteSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// writeStepSummary appends markdown to $GITHUB_STEP_SUMMARY if set (a
// GitHub Actions runner sets it; a local `go test` run does not), so the
// benchmark's report shows up in the workflow run's summary page in
// addition to the log output registerSeedingBenchmarkSpec already prints.
// A failure to write is logged, not fatal: the report has already reached
// GinkgoWriter and stdout by the time this is called.
func writeStepSummary(markdown string) {
	path := os.Getenv("GITHUB_STEP_SUMMARY")
	if path == "" {
		return
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "WARNING: failed to open GITHUB_STEP_SUMMARY %s: %v\n", path, err)
		return
	}
	defer func() { _ = f.Close() }()

	if _, err := f.WriteString(markdown); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "WARNING: failed to write GITHUB_STEP_SUMMARY: %v\n", err)
	}
}
