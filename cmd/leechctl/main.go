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

// Command leechctl drives one ezio daemon through a single leech and
// byte-compares the result: fetch a content's .torrent over HTTP, add it
// to the ezio daemon at --ezio-target in non-seeding mode, wait for the
// download to finish, reconstruct the original partition from the
// downloaded extent files, and print its sha256 (optionally failing if
// it does not match --want-sha256).
//
// It is a CI/test tool, not a shipped component: it exists so the
// image-path e2e lane's run-leecher action can prove a real BitTorrent
// download reconstructs byte-identical partition content, without
// needing partclone or any cluster-side tool on the leech side. See
// internal/leech for the actual logic.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/tjjh89017/kezio/internal/leech"
	"github.com/tjjh89017/kezio/internal/seeder"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("leechctl", flag.ContinueOnError)
	ezioTarget := fs.String("ezio-target", "127.0.0.1:50051", "ezio daemon gRPC listen address")
	torrentURL := fs.String("torrent-url", "", "HTTP URL to fetch the content's .torrent from (required)")
	infoHash := fs.String("info-hash", "", "expected BitTorrent v1 info hash, lowercase hex (required)")
	savePath := fs.String("save-path", "", "directory for ezio to download into (required)")
	outPath := fs.String("out", "", "path to write the reconstructed partition to (required)")
	partitionSizeBytes := fs.Int64("partition-size-bytes", 0, "size in bytes of the original partition (required)")
	wantSHA256 := fs.String("want-sha256", "", "expected sha256 of the reconstructed partition; empty skips the check")
	timeout := fs.Duration("timeout", 3*time.Minute, "how long to wait for the download to finish")
	pollInterval := fs.Duration("poll-interval", 2*time.Second, "how often to poll ezio for download status")
	maxUploads := fs.Int64("max-uploads", int64(seeder.DefaultMaxUploads), "AddTorrent max_uploads")
	maxConnections := fs.Int64("max-connections", int64(seeder.DefaultMaxConnections), "AddTorrent max_connections")
	if err := fs.Parse(args); err != nil {
		return err
	}

	for name, val := range map[string]string{
		"torrent-url": *torrentURL,
		"info-hash":   *infoHash,
		"save-path":   *savePath,
		"out":         *outPath,
	} {
		if val == "" {
			return fmt.Errorf("--%s is required", name)
		}
	}
	if *partitionSizeBytes <= 0 {
		return fmt.Errorf("--partition-size-bytes must be positive, got %d", *partitionSizeBytes)
	}

	client, err := seeder.Dial(*ezioTarget)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	result, err := leech.Run(ctx, client, http.DefaultClient, leech.Options{
		TorrentURL:         *torrentURL,
		InfoHash:           *infoHash,
		SavePath:           *savePath,
		OutPath:            *outPath,
		PartitionSizeBytes: *partitionSizeBytes,
		MaxUploads:         int32(*maxUploads),
		MaxConnections:     int32(*maxConnections),
		PollInterval:       *pollInterval,
		WantSHA256:         *wantSHA256,
	})
	if err != nil {
		if result.SHA256 != "" {
			fmt.Fprintln(os.Stderr, "reconstructed sha256:", result.SHA256)
		}
		return err
	}

	fmt.Println(result.SHA256)
	return nil
}
