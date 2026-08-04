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

// Command image-service runs the server-side target of `kezioctl image
// upload`: an HTTP service that streams an uploaded image file onto a
// staging volume and hands back a staged reference for the ingest job to
// resolve. It is its own binary, separate from the controller manager
// (cmd/main.go), because it mounts a different volume (staging, not the
// slice store) and scales independently of the reconcile loop.
//
// Required port: this service listens on one plain HTTP port
// (--addr, default :8080) inside the cluster network. Whether that port
// is reachable only from inside the cluster (ClusterIP) or exposed
// further (LoadBalancer, NodePort, Ingress) is a decision for whoever
// deploys it, not something this binary or its manifests impose; see
// imageservice.Authenticator's doc comment for what that choice implies
// for the bearer token's confidentiality in transit.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tjjh89017/kezio/internal/imageservice"
)

func main() {
	var (
		addr           string
		stagingDir     string
		tokenFile      string
		maxUploadBytes int64
	)
	flag.StringVar(&addr, "addr", ":8080", "address the HTTP server listens on")
	flag.StringVar(&stagingDir, "staging-dir", "/staging",
		"root of the staging volume uploads are written under")
	flag.StringVar(&tokenFile, "token-file", "/etc/kezio/image-service/token",
		"path to a file holding the bearer token (typically a mounted Secret key)")
	flag.Int64Var(&maxUploadBytes, "max-upload-bytes", imageservice.DefaultMaxUploadBytes,
		"maximum accepted size of a single upload, in bytes")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	if err := run(logger, addr, stagingDir, tokenFile, maxUploadBytes); err != nil {
		logger.Error("image-service exited with an error", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger, addr, stagingDir, tokenFile string, maxUploadBytes int64) error {
	token, err := imageservice.LoadToken(tokenFile)
	if err != nil {
		return err
	}
	auth, err := imageservice.NewAuthenticator(token)
	if err != nil {
		return err
	}
	staging, err := imageservice.NewStaging(stagingDir)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           imageservice.NewServer(staging, auth, maxUploadBytes, logger).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("image-service listening", "addr", addr, "stagingDir", stagingDir)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}
