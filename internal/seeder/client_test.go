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

package seeder

import (
	"context"
	"net"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"k8s.io/utils/ptr"

	keziov1alpha2 "github.com/tjjh89017/kezio/api/v1alpha2"
	"github.com/tjjh89017/kezio/internal/seeder/ezioapi"
)

// fakeEZIO is an in-memory ezioapi.EZIOServer standing in for a real ezio
// daemon, with enough behavior to exercise Client's request/response
// mapping.
type fakeEZIO struct {
	ezioapi.UnimplementedEZIOServer

	mu       sync.Mutex
	added    []*ezioapi.AddRequest
	torrents map[string]*ezioapi.Torrent
	// addErr, when set, is returned by every AddTorrent call instead of
	// the normal response.
	addErr error
	// version is returned by GetVersion; a non-nil healthErr instead
	// simulates the daemon being unreachable.
	healthErr error
}

func newFakeEZIO() *fakeEZIO {
	return &fakeEZIO{torrents: make(map[string]*ezioapi.Torrent)}
}

func (f *fakeEZIO) AddTorrent(_ context.Context, req *ezioapi.AddRequest) (*ezioapi.AddResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.addErr != nil {
		return nil, f.addErr
	}
	f.added = append(f.added, req)
	return &ezioapi.AddResponse{Result: true}, nil
}

func (f *fakeEZIO) GetTorrentStatus(_ context.Context, req *ezioapi.UpdateRequest) (*ezioapi.UpdateStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := &ezioapi.UpdateStatus{Torrents: make(map[string]*ezioapi.Torrent)}
	if len(req.GetHashes()) == 0 {
		for h, t := range f.torrents {
			out.Torrents[h] = t
		}
		return out, nil
	}
	for _, h := range req.GetHashes() {
		if t, ok := f.torrents[h]; ok {
			out.Torrents[h] = t
		}
	}
	return out, nil
}

func (f *fakeEZIO) GetVersion(_ context.Context, _ *ezioapi.Empty) (*ezioapi.VersionResponse, error) {
	if f.healthErr != nil {
		return nil, f.healthErr
	}
	return &ezioapi.VersionResponse{Version: "test"}, nil
}

// putTorrent seeds f's status table directly, simulating a torrent that
// was already added in an earlier call (or by a previous daemon
// lifetime).
func (f *fakeEZIO) putTorrent(hash string, finished bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.torrents[hash] = &ezioapi.Torrent{Hash: hash, IsFinished: finished}
}

// startFakeEZIO starts fake behind an in-memory listener and returns a
// Client dialed against it. The server and client both stop when the
// test's Cleanup runs.
func startFakeEZIO(t *testing.T, fake *fakeEZIO) *Client {
	t.Helper()

	lis := bufconn.Listen(1024 * 1024)
	srv := grpc.NewServer()
	ezioapi.RegisterEZIOServer(srv, fake)
	go func() {
		_ = srv.Serve(lis)
	}()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial bufconn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return &Client{conn: conn, rpc: ezioapi.NewEZIOClient(conn)}
}

func TestClient_AddTorrent(t *testing.T) {
	fake := newFakeEZIO()
	c := startFakeEZIO(t, fake)

	torrentBytes := []byte("fake-torrent-bytes")
	if err := c.AddTorrent(context.Background(), torrentBytes, "/content/pc-deadbeef-content", true, 4, 12); err != nil {
		t.Fatalf("AddTorrent: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.added) != 1 {
		t.Fatalf("got %d AddTorrent calls, want 1", len(fake.added))
	}
	got := fake.added[0]
	if string(got.GetTorrent()) != string(torrentBytes) {
		t.Errorf("torrent bytes = %q, want %q", got.GetTorrent(), torrentBytes)
	}
	if got.GetSavePath() != "/content/pc-deadbeef-content" {
		t.Errorf("save_path = %q, want /content/pc-deadbeef-content", got.GetSavePath())
	}
	if !got.GetSeedingMode() {
		t.Error("seeding_mode = false, want true")
	}
	if got.GetMaxUploads() != 4 {
		t.Errorf("max_uploads = %d, want 4", got.GetMaxUploads())
	}
	if got.GetMaxConnections() != 12 {
		t.Errorf("max_connections = %d, want 12", got.GetMaxConnections())
	}
}

func TestClient_AddTorrent_DaemonFailure(t *testing.T) {
	fake := newFakeEZIO()
	c := startFakeEZIO(t, fake)

	fake.addErr = context.DeadlineExceeded
	if err := c.AddTorrent(context.Background(), []byte("x"), "/content/pc-x-content", true, DefaultMaxUploads, DefaultMaxConnections); err == nil {
		t.Fatal("AddTorrent: got nil error, want one from the daemon's failure")
	}
}

func TestClient_GetTorrentStatus_Empty(t *testing.T) {
	fake := newFakeEZIO()
	c := startFakeEZIO(t, fake)

	status, err := c.GetTorrentStatus(context.Background(), nil)
	if err != nil {
		t.Fatalf("GetTorrentStatus: %v", err)
	}
	if len(status) != 0 {
		t.Errorf("status = %v, want empty", status)
	}
}

func TestClient_GetTorrentStatus_AllWhenHashesEmpty(t *testing.T) {
	fake := newFakeEZIO()
	fake.putTorrent("aaaa", true)
	fake.putTorrent("bbbb", false)
	c := startFakeEZIO(t, fake)

	status, err := c.GetTorrentStatus(context.Background(), nil)
	if err != nil {
		t.Fatalf("GetTorrentStatus: %v", err)
	}
	if len(status) != 2 {
		t.Fatalf("status = %v, want 2 entries", status)
	}
	if !status["aaaa"].IsFinished {
		t.Error("aaaa.IsFinished = false, want true")
	}
	if status["bbbb"].IsFinished {
		t.Error("bbbb.IsFinished = true, want false")
	}
}

func TestClient_GetTorrentStatus_FiltersByHash(t *testing.T) {
	fake := newFakeEZIO()
	fake.putTorrent("aaaa", true)
	fake.putTorrent("bbbb", false)
	c := startFakeEZIO(t, fake)

	status, err := c.GetTorrentStatus(context.Background(), []string{"aaaa"})
	if err != nil {
		t.Fatalf("GetTorrentStatus: %v", err)
	}
	if len(status) != 1 {
		t.Fatalf("status = %v, want 1 entry", status)
	}
	if _, ok := status["aaaa"]; !ok {
		t.Error("status missing aaaa")
	}
}

func TestClient_Healthy(t *testing.T) {
	fake := newFakeEZIO()
	c := startFakeEZIO(t, fake)

	if err := c.Healthy(context.Background()); err != nil {
		t.Fatalf("Healthy: %v", err)
	}
}

func TestClient_Healthy_Unreachable(t *testing.T) {
	fake := newFakeEZIO()
	fake.healthErr = context.DeadlineExceeded
	c := startFakeEZIO(t, fake)

	if err := c.Healthy(context.Background()); err == nil {
		t.Fatal("Healthy: got nil error, want one reflecting daemon failure")
	}
}

func TestResolveMaxUploads(t *testing.T) {
	if got := ResolveMaxUploads(nil); got != DefaultMaxUploads {
		t.Errorf("ResolveMaxUploads(nil) = %d, want %d", got, DefaultMaxUploads)
	}
	if got := ResolveMaxUploads(&keziov1alpha2.MachineEzioTuning{}); got != DefaultMaxUploads {
		t.Errorf("ResolveMaxUploads(no override) = %d, want %d", got, DefaultMaxUploads)
	}
	tuning := &keziov1alpha2.MachineEzioTuning{MaxUploads: ptr.To(int32(7))}
	if got := ResolveMaxUploads(tuning); got != 7 {
		t.Errorf("ResolveMaxUploads(override 7) = %d, want 7", got)
	}
}

func TestResolveMaxConnections(t *testing.T) {
	if got := ResolveMaxConnections(nil); got != DefaultMaxConnections {
		t.Errorf("ResolveMaxConnections(nil) = %d, want %d", got, DefaultMaxConnections)
	}
	if got := ResolveMaxConnections(&keziov1alpha2.MachineEzioTuning{}); got != DefaultMaxConnections {
		t.Errorf("ResolveMaxConnections(no override) = %d, want %d", got, DefaultMaxConnections)
	}
	tuning := &keziov1alpha2.MachineEzioTuning{MaxConnections: ptr.To(int32(8))}
	if got := ResolveMaxConnections(tuning); got != 8 {
		t.Errorf("ResolveMaxConnections(override 8) = %d, want 8", got)
	}
}
