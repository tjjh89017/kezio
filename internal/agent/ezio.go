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

package agent

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/tjjh89017/kezio/internal/agent/deploy"
	"github.com/tjjh89017/kezio/internal/seeder"
)

// DefaultEzioBinary is the local ezio daemon binary name ExecEzioLauncher
// runs, resolved through PATH - the live image ships it there.
const DefaultEzioBinary = "ezio"

// DefaultEzioListenAddress is the local address ExecEzioLauncher tells
// ezio to listen its gRPC control plane on, and dials right back:
// loopback-only, since nothing outside this machine ever needs to reach
// it.
const DefaultEzioListenAddress = "127.0.0.1:9001"

// DefaultEzioReadyTimeout bounds how long ExecEzioLauncher waits for the
// freshly spawned daemon to answer its gRPC health check before giving
// up.
const DefaultEzioReadyTimeout = 10 * time.Second

// DefaultMeminfoPath is where Launch reads MemTotal from to compute the
// automatic cache size, when the launch config's CacheSizeMB is unset.
const DefaultMeminfoPath = "/proc/meminfo"

// ExecEzioLauncher is the production deploy.EzioLauncher: it spawns the
// local ezio binary in raw-device mode and dials its gRPC control plane.
type ExecEzioLauncher struct {
	// Binary is the ezio executable to run. Empty means DefaultEzioBinary.
	Binary string
	// ListenAddress is passed to ezio's --listen flag, and dialed back.
	// Empty means DefaultEzioListenAddress.
	ListenAddress string
	// ReadyTimeout bounds the wait for the daemon to answer its health
	// check. Zero means DefaultEzioReadyTimeout.
	ReadyTimeout time.Duration
	// MeminfoPath is read to compute the automatic --cache-size value
	// when Launch's EzioLaunchConfig.CacheSizeMB is nil. Empty means
	// DefaultMeminfoPath; tests point it at a fixture file.
	MeminfoPath string
	// Logf receives ezio's own stdout and stderr, one call per line, in
	// fmt.Printf style, so `journalctl -u kezio-agent` shows ezio's
	// output alongside the agent's own. Nil discards it.
	Logf func(format string, args ...any)
}

var _ deploy.EzioLauncher = ExecEzioLauncher{}

// Launch implements deploy.EzioLauncher.
func (l ExecEzioLauncher) Launch(ctx context.Context, cfg deploy.EzioLaunchConfig) (deploy.EzioHandle, error) {
	binary := l.Binary
	if binary == "" {
		binary = DefaultEzioBinary
	}
	addr := l.ListenAddress
	if addr == "" {
		addr = DefaultEzioListenAddress
	}
	readyTimeout := l.ReadyTimeout
	if readyTimeout <= 0 {
		readyTimeout = DefaultEzioReadyTimeout
	}

	logf := l.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	args := []string{"--listen", addr}
	if cacheSizeMB := l.resolveCacheSizeMB(cfg, logf); cacheSizeMB != nil {
		args = append(args, "--cache-size", strconv.Itoa(int(*cacheSizeMB)))
	}
	if cfg.AioThreads != nil {
		args = append(args, "--aio-threads", strconv.Itoa(int(*cfg.AioThreads)))
	}
	if cfg.Port != nil {
		args = append(args, "--port", strconv.Itoa(int(*cfg.Port)))
	}

	cmd := exec.CommandContext(ctx, binary, args...)
	stop, err := startWithLoggedOutput(cmd, logf)
	if err != nil {
		return deploy.EzioHandle{}, fmt.Errorf("starting %s: %w", binary, err)
	}

	client, err := seeder.Dial(addr)
	if err != nil {
		_ = stop()
		return deploy.EzioHandle{}, fmt.Errorf("dialing ezio at %s: %w", addr, err)
	}
	if err := waitHealthy(ctx, client, readyTimeout); err != nil {
		_ = client.Close()
		_ = stop()
		return deploy.EzioHandle{}, err
	}

	return deploy.EzioHandle{Client: client, Stop: stop}, nil
}

// resolveCacheSizeMB returns the --cache-size value Launch should pass,
// logging whether it came from cfg (an explicit machine.spec.ezio
// override) or was computed automatically from this machine's own
// memory. A nil return means no flag at all: neither an override was set
// nor could MemTotal be read, so ezio's own fixed built-in default cache
// size applies.
func (l ExecEzioLauncher) resolveCacheSizeMB(cfg deploy.EzioLaunchConfig, logf func(format string, args ...any)) *int32 {
	if cfg.CacheSizeMB != nil {
		logf("ezio: cache size %d MB (override)", *cfg.CacheSizeMB)
		return cfg.CacheSizeMB
	}

	path := l.MeminfoPath
	if path == "" {
		path = DefaultMeminfoPath
	}
	memTotal, err := readMemTotalBytes(path)
	if err != nil {
		logf("ezio: reading %s to compute an automatic cache size: %v; using ezio's own default", path, err)
		return nil
	}
	auto := AutoCacheSizeMB(memTotal)
	logf("ezio: cache size %d MB (auto, MemTotal %d bytes)", auto, memTotal)
	return &auto
}

// readMemTotalBytes reads path (normally /proc/meminfo) and returns its
// MemTotal value in bytes.
func readMemTotalBytes(path string) (uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	m := meminfoTotalPattern.FindSubmatch(data)
	if m == nil {
		return 0, fmt.Errorf("MemTotal not found in %s", path)
	}
	kb, err := strconv.ParseUint(string(m[1]), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing MemTotal in %s: %w", path, err)
	}
	return kb * 1024, nil
}

// autoCacheReserveBytes is held back from the automatic cache-size
// calculation: 1 GiB for the OS, the agent, and the live image, plus 1
// GiB for ezio's own fixed buffer pools (outside the unified cache this
// flag sizes).
const autoCacheReserveBytes = 2 << 30

// autoCacheMinMB and autoCacheMaxMB bound AutoCacheSizeMB's result: a
// small DUT still gets a usable cache, and a huge one does not hand ezio
// more than it needs.
const (
	autoCacheMinMB = 64
	autoCacheMaxMB = 8192
)

// AutoCacheSizeMB computes the automatic --cache-size value for a DUT
// reporting memTotalBytes total memory: half of whatever remains after
// autoCacheReserveBytes is held back, in megabytes, clamped to
// [autoCacheMinMB, autoCacheMaxMB]. The other half of the remainder stays
// available for the kernel page cache. memTotalBytes at or below the
// reserve clamps to autoCacheMinMB rather than going negative.
func AutoCacheSizeMB(memTotalBytes uint64) int32 {
	if memTotalBytes <= autoCacheReserveBytes {
		return autoCacheMinMB
	}
	cacheMB := (memTotalBytes - autoCacheReserveBytes) / 2 / (1 << 20)
	return int32(min(max(cacheMB, autoCacheMinMB), autoCacheMaxMB))
}

// startWithLoggedOutput starts cmd with its stdout and stderr streamed
// line by line into logf, prefixed with "ezio: " so they are
// attributable in the agent's own log (see ExecEzioLauncher.Logf). It
// returns a stop function that kills the process and blocks until it has
// actually exited and both pipes are fully drained - cmd.Wait must not
// run before every read from its pipes is done, and the caller must not
// treat the process as gone (nor risk leaving a zombie) until Wait has
// run, so stop only returns once that has happened and the exit result
// is logged.
func startWithLoggedOutput(cmd *exec.Cmd, logf func(format string, args ...any)) (stop func() error, err error) {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("piping stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("piping stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	var copiers sync.WaitGroup
	copiers.Add(2)
	go copyLines(stdout, logf, &copiers)
	go copyLines(stderr, logf, &copiers)

	reaped := make(chan struct{})
	go func() {
		copiers.Wait()
		logf("ezio: exited: %v", cmd.Wait())
		close(reaped)
	}()

	return func() error {
		if cmd.Process == nil {
			return nil
		}
		err := cmd.Process.Kill()
		<-reaped
		return err
	}, nil
}

// copyLines reads r line by line and logs each one prefixed with
// "ezio: ". It returns once r hits EOF (the process exited and closed
// the pipe) and signals wg, letting the caller wait out both the stdout
// and stderr copiers before reaping the process with cmd.Wait.
func copyLines(r io.Reader, logf func(format string, args ...any), wg *sync.WaitGroup) {
	defer wg.Done()
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		logf("ezio: %s", scanner.Text())
	}
}

// waitHealthy polls client.Healthy until it succeeds or timeout elapses.
func waitHealthy(ctx context.Context, client *seeder.Client, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := client.Healthy(ctx); err == nil {
			return nil
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("ezio did not become healthy within %s: %w", timeout, lastErr)
}
