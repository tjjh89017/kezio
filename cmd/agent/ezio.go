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

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/agent/deploy"
	"github.com/tjjh89017/kezio/internal/seeder"
)

// ezioBinary is the local ezio daemon binary, present in the live boot
// image alongside kezio-agent (see hack/live-image).
const ezioBinary = "ezio"

// defaultEzioPort is the local ezio daemon's default gRPC listen port -
// no reason to vary it, since kezio-agent runs exactly one local ezio
// daemon at a time, for one machine's own deployment.
const defaultEzioPort = 50051

// ezioStartupTimeout bounds how long Launch waits for the freshly
// spawned daemon to answer GetVersion before giving up: long enough for
// process startup and libtorrent init on a slow machine, short enough
// that a daemon that will never come up does not wedge the whole deploy
// silently.
const ezioStartupTimeout = 15 * time.Second

// ezioLauncher implements deploy.EzioLauncher by spawning a real local
// `ezio` process and dialing it over loopback gRPC (internal/seeder).
// AddTorrent against this daemon always names a raw partition device as
// save_path (see internal/agent/deploy's package doc comment) - Launch
// itself never passes -F, which is what puts the daemon in that mode.
type ezioLauncher struct{}

func (ezioLauncher) Launch(ctx context.Context, tuning *keziov1alpha1.MachineEzioTuning) (deploy.EzioHandle, error) {
	port := defaultEzioPort
	if tuning != nil && tuning.Port != nil {
		port = int(*tuning.Port)
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	args := []string{"--listen", addr}
	if tuning != nil {
		if tuning.CacheSizeMB != nil {
			args = append(args, "--cache-size", strconv.Itoa(int(*tuning.CacheSizeMB)))
		}
		if tuning.AioThreads != nil {
			args = append(args, "--aio-threads", strconv.Itoa(int(*tuning.AioThreads)))
		}
	}

	//nolint:gosec // args are built entirely from Machine.spec.ezio integer fields, never free-form input
	cmd := exec.Command(ezioBinary, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return deploy.EzioHandle{}, fmt.Errorf("starting local ezio daemon: %w", err)
	}

	stop := func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Kill()
	}

	client, err := dialEzioWhenReady(ctx, addr)
	if err != nil {
		_ = stop()
		return deploy.EzioHandle{}, err
	}

	return deploy.EzioHandle{
		Client: client,
		Stop: func() error {
			_ = client.Close()
			return stop()
		},
	}, nil
}

// dialEzioWhenReady dials addr and polls GetVersion until it succeeds or
// ezioStartupTimeout elapses: the daemon accepts TCP connections before
// its gRPC service is actually ready to answer, so a bare successful
// Dial is not enough to prove the daemon is usable yet.
func dialEzioWhenReady(ctx context.Context, addr string) (*seeder.Client, error) {
	client, err := seeder.Dial(addr)
	if err != nil {
		return nil, fmt.Errorf("dialing local ezio daemon at %s: %w", addr, err)
	}

	deadline := time.Now().Add(ezioStartupTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		healthCtx, cancel := context.WithTimeout(ctx, time.Second)
		lastErr = client.Healthy(healthCtx)
		cancel()
		if lastErr == nil {
			return client, nil
		}

		select {
		case <-ctx.Done():
			_ = client.Close()
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}

	_ = client.Close()
	return nil, fmt.Errorf("local ezio daemon at %s did not become healthy within %s: %w",
		addr, ezioStartupTimeout, lastErr)
}
