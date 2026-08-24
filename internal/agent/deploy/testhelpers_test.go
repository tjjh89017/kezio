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

package deploy

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/tjjh89017/kezio/internal/seeder"
)

// fakeCall records one Runner.Run/RunEnv invocation.
type fakeCall struct {
	stdin string
	name  string
	args  []string
	env   []string
}

func (c fakeCall) String() string {
	return fmt.Sprintf("%s %s", c.name, strings.Join(c.args, " "))
}

// fakeRunner is a Runner recording every call in order. blockdevSizes
// answers "blockdev --getsize64 <disk>"; every other command succeeds
// with empty output unless errs or outputs names it.
type fakeRunner struct {
	mu            sync.Mutex
	calls         []fakeCall
	blockdevSizes map[string]int64
	// errs, keyed by fakeCall.String(), makes that exact call fail.
	errs map[string]error
	// errPrefixes makes any call whose fakeCall.String() starts with the
	// key fail - for a call carrying an argument (a generated temp path)
	// the test cannot spell out exactly.
	errPrefixes map[string]error
	// outputs, keyed by fakeCall.String(), answers a call with canned
	// stdout instead of the empty-success default.
	outputs map[string][]byte
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{
		blockdevSizes: map[string]int64{},
		errs:          map[string]error{},
		errPrefixes:   map[string]error{},
		outputs:       map[string][]byte{},
	}
}

func (f *fakeRunner) Run(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	return f.RunEnv(ctx, nil, stdin, name, args...)
}

func (f *fakeRunner) RunEnv(_ context.Context, env []string, stdin []byte, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := fakeCall{stdin: string(stdin), name: name, args: args, env: env}
	f.calls = append(f.calls, call)

	key := call.String()
	if err, ok := f.errs[key]; ok {
		return nil, err
	}
	if out, ok := f.outputs[key]; ok {
		return out, nil
	}
	// errPrefixes matches a call whose args end in a value the test
	// cannot predict up front (a temp mountpoint path, for example) -
	// exact-match errs above always wins when both could match.
	for prefix, err := range f.errPrefixes {
		if strings.HasPrefix(key, prefix) {
			return nil, err
		}
	}

	if name == "blockdev" && len(args) == 2 && args[0] == "--getsize64" {
		disk := args[1]
		size, ok := f.blockdevSizes[disk]
		if !ok {
			size = 100 << 30 // 100 GiB, comfortably large by default
		}
		return fmt.Appendf(nil, "%d\n", size), nil
	}
	return nil, nil
}

func (f *fakeRunner) commandNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	names := make([]string, len(f.calls))
	for i, c := range f.calls {
		names[i] = c.String()
	}
	return names
}

// fakeEzioClient is an in-memory EzioClient recording AddTorrent calls and
// serving scripted GetTorrentStatus responses, one slice entry consumed
// per poll (the last entry repeats once exhausted).
type fakeEzioClient struct {
	mu             sync.Mutex
	added          map[string]string // hash (as torrent bytes) -> save_path
	statusSequence []map[string]seeder.Torrent
	statusCalls    int
	paused         []string
	shutdownCalled bool
	shutdownErr    error
}

func newFakeEzioClient(statusSequence []map[string]seeder.Torrent) *fakeEzioClient {
	return &fakeEzioClient{added: map[string]string{}, statusSequence: statusSequence}
}

func (f *fakeEzioClient) AddTorrent(_ context.Context, torrent []byte, savePath string, _ bool, _, _ int32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.added[string(torrent)] = savePath
	return nil
}

func (f *fakeEzioClient) GetTorrentStatus(_ context.Context, _ []string) (map[string]seeder.Torrent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	idx := f.statusCalls
	if idx >= len(f.statusSequence) {
		idx = len(f.statusSequence) - 1
	}
	f.statusCalls++
	return f.statusSequence[idx], nil
}

func (f *fakeEzioClient) PauseTorrent(_ context.Context, hash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paused = append(f.paused, hash)
	return nil
}

func (f *fakeEzioClient) Shutdown(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shutdownCalled = true
	return f.shutdownErr
}

// fakeEzioLauncher returns a fixed EzioHandle (or a fixed error) from
// Launch, recording whether Stop was called.
type fakeEzioLauncher struct {
	client     EzioClient
	launchErr  error
	stopCalled bool
	launched   bool
}

func (f *fakeEzioLauncher) Launch(context.Context) (EzioHandle, error) {
	f.launched = true
	if f.launchErr != nil {
		return EzioHandle{}, f.launchErr
	}
	return EzioHandle{Client: f.client, Stop: func() error {
		f.stopCalled = true
		return nil
	}}, nil
}

// fakeTorrentFetcher returns torrent bytes keyed by URL.
type fakeTorrentFetcher struct {
	bytes map[string][]byte
	err   error
}

func (f *fakeTorrentFetcher) FetchTorrent(_ context.Context, url string) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.bytes[url], nil
}
