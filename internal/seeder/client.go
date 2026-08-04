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

// Package seeder is a typed Go client for one ezio daemon's gRPC API
// (proto/ezio.proto, stubs generated into internal/seeder/ezioapi by `make
// proto`). ezio's gRPC service IS the seeder's remote control surface: the
// seeder container ships no other command-receiver, so every state change
// the kezio operator wants (add a torrent, check what is already seeding)
// goes through this client.
package seeder

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/tjjh89017/kezio/internal/seeder/ezioapi"
)

// defaultMaxUploads and defaultMaxConnections mirror ezio's own
// add_torrent default arguments, applied here because AddRequest has no
// "unset" sentinel of its own; passing 0 through would tell ezio to allow
// zero peers.
const (
	defaultMaxUploads     = 3
	defaultMaxConnections = 5
)

// Torrent is one torrent's status, trimmed from ezioapi.Torrent to the
// fields callers need. Hash is lowercase hex, matching both ezio's wire
// format and store.InfoHash.String(). Beyond the fields the seeder
// reconciler diffs against, this also carries the byte counters and
// timers the agent's deploy-side stop policy needs
// (internal/agent/deploy): TotalDone/Total for progress percent,
// TotalPayloadUpload/TotalDone for the "upload > 3x size" pause
// condition, and FinishedTime/LastUpload for the "finished and idle"
// pause condition - see ezio's own reference client
// (tmp/ezio/utils/ezio_cli.py's check_stop) for the exact semantics
// these mirror.
type Torrent struct {
	Hash       string
	IsFinished bool
	IsPaused   bool
	// TotalDone is the bytes downloaded (and verified) so far.
	TotalDone int64
	// Total is the torrent's full byte size; 0 while ezio has not yet
	// resolved metadata for it (never the case here, since AddTorrent
	// always supplies a complete .torrent up front).
	Total int64
	// TotalPayloadUpload is the cumulative bytes this torrent has
	// uploaded to peers.
	TotalPayloadUpload int64
	// FinishedTime is how many seconds this torrent has been finished,
	// counting up from the moment it finished; 0 while not finished.
	FinishedTime int64
	// LastUpload is how many seconds since this torrent last uploaded
	// any bytes, or -1 if it has never uploaded.
	LastUpload int64
}

// Client is a typed wrapper around one ezio daemon's gRPC connection. It
// is safe for concurrent use (grpc.ClientConn is), and holds no
// per-torrent state of its own — every call is a single RPC against the
// daemon's current state.
type Client struct {
	conn *grpc.ClientConn
	rpc  ezioapi.EZIOClient
}

// Dial opens a gRPC connection to the ezio daemon listening at target
// (host:port, the daemon's --listen address). The connection has no
// transport security: ezio's gRPC control plane is reached over the
// cluster network (kube-apiserver-adjacent trust boundary, not the public
// data network the BitTorrent swarm uses), matching how every other
// in-cluster kezio component (image-service, the ingest Job) talks
// unencrypted to trusted peers within the cluster.
func Dial(target string) (*Client, error) {
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("dial ezio at %s: %w", target, err)
	}
	return &Client{conn: conn, rpc: ezioapi.NewEZIOClient(conn)}, nil
}

// Close closes the underlying gRPC connection.
func (c *Client) Close() error {
	return c.conn.Close()
}

// AddTorrent adds torrent (a bencoded .torrent file, see
// internal/store.BuildTorrentFile) to the daemon with save_path as its
// data directory. seedMode marks every piece verified without ezio
// re-hashing the content — the caller's responsibility to guarantee the
// files at save_path truly match torrent, which holds here because
// save_path is a content-addressed, immutable store directory (see
// internal/store's InfoHash contract): if the files did not match, they
// would not be filed under that hash to begin with.
func (c *Client) AddTorrent(ctx context.Context, torrent []byte, savePath string, seedMode bool) error {
	resp, err := c.rpc.AddTorrent(ctx, &ezioapi.AddRequest{
		Torrent:            torrent,
		SavePath:           savePath,
		SeedingMode:        seedMode,
		MaxUploads:         defaultMaxUploads,
		MaxConnections:     defaultMaxConnections,
		SequentialDownload: false,
	})
	if err != nil {
		return fmt.Errorf("AddTorrent: %w", err)
	}
	if !resp.GetResult() {
		return fmt.Errorf("AddTorrent: ezio reported failure for save_path %s", savePath)
	}
	return nil
}

// GetTorrentStatus returns the status of the torrents named by hashes
// (lowercase hex info hashes), or every torrent the daemon knows about
// when hashes is empty (proto/ezio.proto: UpdateRequest's "if empty
// update all"). The seeder reconciler uses the empty form to enumerate
// what is already present, then diffs that against what ought to be.
func (c *Client) GetTorrentStatus(ctx context.Context, hashes []string) (map[string]Torrent, error) {
	resp, err := c.rpc.GetTorrentStatus(ctx, &ezioapi.UpdateRequest{Hashes: hashes})
	if err != nil {
		return nil, fmt.Errorf("GetTorrentStatus: %w", err)
	}
	out := make(map[string]Torrent, len(resp.GetTorrents()))
	for hash, t := range resp.GetTorrents() {
		out[hash] = Torrent{
			Hash:               t.GetHash(),
			IsFinished:         t.GetIsFinished(),
			IsPaused:           t.GetIsPaused(),
			TotalDone:          t.GetTotalDone(),
			Total:              t.GetTotal(),
			TotalPayloadUpload: t.GetTotalPayloadUpload(),
			FinishedTime:       t.GetFinishedTime(),
			LastUpload:         t.GetLastUpload(),
		}
	}
	return out, nil
}

// PauseTorrent pauses the torrent identified by hash (lowercase hex info
// hash). ezio keeps seeding a paused torrent's already-verified pieces to
// nobody - a paused torrent simply stops all network activity - which is
// exactly the deploy-side stop policy's "stop pushing this partition's
// data" primitive (see internal/agent/deploy).
func (c *Client) PauseTorrent(ctx context.Context, hash string) error {
	if _, err := c.rpc.PauseTorrent(ctx, &ezioapi.PauseTorrentRequest{Hash: hash}); err != nil {
		return fmt.Errorf("PauseTorrent: %w", err)
	}
	return nil
}

// ResumeTorrent resumes a previously paused torrent identified by hash.
func (c *Client) ResumeTorrent(ctx context.Context, hash string) error {
	if _, err := c.rpc.ResumeTorrent(ctx, &ezioapi.ResumeTorrentRequest{Hash: hash}); err != nil {
		return fmt.Errorf("ResumeTorrent: %w", err)
	}
	return nil
}

// Shutdown tells the daemon to exit. ezio does not stop itself (see
// proto/ezio.proto's doc comment); a client applies the stop policy and
// calls Shutdown once every torrent it added has been paused, which is
// how internal/agent/deploy retires the local ezio daemon it spawned for
// one deployment once that deployment's content is fully seeded.
func (c *Client) Shutdown(ctx context.Context) error {
	if _, err := c.rpc.Shutdown(ctx, &ezioapi.Empty{}); err != nil {
		return fmt.Errorf("Shutdown: %w", err)
	}
	return nil
}

// Healthy calls GetVersion as a liveness check: any successful response
// (the version string itself is not inspected) means the daemon's gRPC
// service is up and serving.
func (c *Client) Healthy(ctx context.Context) error {
	if _, err := c.rpc.GetVersion(ctx, &ezioapi.Empty{}); err != nil {
		return fmt.Errorf("GetVersion: %w", err)
	}
	return nil
}
