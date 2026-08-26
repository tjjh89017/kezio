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
	"os/exec"
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
	// Logf receives ezio's own stdout and stderr, one call per line, in
	// fmt.Printf style, so `journalctl -u kezio-agent` shows ezio's
	// output alongside the agent's own. Nil discards it.
	Logf func(format string, args ...any)
}

var _ deploy.EzioLauncher = ExecEzioLauncher{}

// Launch implements deploy.EzioLauncher.
func (l ExecEzioLauncher) Launch(ctx context.Context) (deploy.EzioHandle, error) {
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

	cmd := exec.CommandContext(ctx, binary, "--listen", addr)
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
