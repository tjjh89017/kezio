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
	"context"
	"fmt"
	"os/exec"
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

	cmd := exec.CommandContext(ctx, binary, "--listen", addr)
	if err := cmd.Start(); err != nil {
		return deploy.EzioHandle{}, fmt.Errorf("starting %s: %w", binary, err)
	}
	stop := func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Kill()
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
