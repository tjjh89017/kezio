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
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"sync"
	"time"

	keziov1alpha1 "github.com/tjjh89017/kezio/api/v1alpha1"
	"github.com/tjjh89017/kezio/internal/agent/deploy"
	"github.com/tjjh89017/kezio/internal/seeder"
)

// ezioBinary is the local ezio daemon binary, present in the live boot
// image alongside kezio-agent (see hack/live-image).
const ezioBinary = "ezio"

// defaultEzioPort is the local ezio daemon's default gRPC listen port;
// kezio-agent only ever runs one local daemon, so it never needs to vary.
const defaultEzioPort = 50051

// ezioStartupTimeout bounds how long Launch waits for the freshly
// spawned daemon to answer GetVersion: long enough for a slow machine's
// startup and libtorrent init, short enough not to silently wedge the
// deploy on a daemon that never comes up.
const ezioStartupTimeout = 15 * time.Second

// ezioLauncher implements deploy.EzioLauncher by spawning a real local
// `ezio` process and dialing it over loopback gRPC (internal/seeder).
// Launch never passes -F, so AddTorrent against this daemon always names
// a raw partition device as save_path (see internal/agent/deploy's
// package doc comment).
type ezioLauncher struct {
	// Logf receives ezio's own stdout/stderr, line by line, plus its
	// exit outcome - see forwardLines and spawnEzio. Nil discards them,
	// the same convention as deploy.Executor.Logf.
	Logf func(format string, args ...any)
	// Binary is the ezio executable to run. Empty means ezioBinary.
	// Overridable only so tests can point it at a fake script; production
	// always leaves this unset.
	Binary string
}

func (l ezioLauncher) log(format string, args ...any) {
	if l.Logf != nil {
		l.Logf(format, args...)
	}
}

func (l ezioLauncher) binary() string {
	if l.Binary != "" {
		return l.Binary
	}
	return ezioBinary
}

func (l ezioLauncher) Launch(ctx context.Context, tuning *keziov1alpha1.MachineEzioTuning) (deploy.EzioHandle, error) {
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

	stop, err := l.spawnEzio(args)
	if err != nil {
		return deploy.EzioHandle{}, err
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

// spawnEzio starts the local ezio daemon with args and wires its stdout
// and stderr into l.Logf line by line, tagged "ezio stdout"/"ezio
// stderr" so it rides kezio-agent's own log path (deploy.Executor.Console)
// attributably instead of landing on inherited file descriptors. This is
// the only place kezio-agent spawns an ezio daemon; on the seeder side,
// docker/seeder/entrypoint.sh execs ezio as the container's own process
// with nothing to wire.
//
// The forwarding goroutines exit only on pipe EOF, which the kernel
// delivers only after the process has exited and closed its end, so no
// line ezio wrote is lost to a race with process exit. A third goroutine
// then reaps the process and logs its exit outcome, distinguishing a
// wedged ezio from a crashed one.
func (l ezioLauncher) spawnEzio(args []string) (stop func() error, err error) {
	//nolint:gosec // args are built entirely from Machine.spec.ezio integer fields, never free-form input
	cmd := exec.Command(l.binary(), args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("opening local ezio daemon stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("opening local ezio daemon stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting local ezio daemon: %w", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); l.forwardLines("ezio stdout", stdout) }()
	go func() { defer wg.Done(); l.forwardLines("ezio stderr", stderr) }()
	go func() {
		wg.Wait()
		if waitErr := cmd.Wait(); waitErr != nil {
			l.log("local ezio daemon exited: %v", waitErr)
		} else {
			l.log("local ezio daemon exited cleanly")
		}
	}()

	return func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Kill()
	}, nil
}

// forwardLines copies each line of r into l.Logf, prefixed with label,
// until r hits EOF or a read error (see spawnEzio's doc comment for why
// that boundary is the process's own exit). A read error other than EOF
// ends forwarding early and is itself logged.
func (l ezioLauncher) forwardLines(label string, r io.Reader) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		l.log("%s: %s", label, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		l.log("reading %s failed; log forwarding stopped early: %v", label, err)
	}
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
