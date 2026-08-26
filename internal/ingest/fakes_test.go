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

package ingest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tjjh89017/kezio/internal/store"
)

// eventAttach and eventDetach are the fakeAttacher.events/order values
// recorded by Attach and its returned detach func.
const (
	eventAttach = "attach"
	eventDetach = "detach"
)

// fakeDownloader records what it was asked to download and writes fixed
// content to destPath, simulating a successful fetch.
type fakeDownloader struct {
	content []byte
	err     error
	calls   []string
}

func (f *fakeDownloader) Download(_ context.Context, url, destPath string) error {
	f.calls = append(f.calls, url)
	if f.err != nil {
		return f.err
	}
	return os.WriteFile(destPath, f.content, 0o600)
}

// fakeStaging resolves every name to a fixed path (a file the test sets
// up itself) and records removals.
type fakeStaging struct {
	paths      map[string]string
	removed    []string
	resolveErr error
	// order, when set, additionally records "remove:<name>" into a
	// timeline shared with another fake (e.g. fakeAttacher's "detach"),
	// so a test can assert cleanup ordering across both.
	order *[]string
}

func (f *fakeStaging) ResolveUpload(name string) (string, error) {
	if f.resolveErr != nil {
		return "", f.resolveErr
	}
	p, ok := f.paths[name]
	if !ok {
		return "", fmt.Errorf("no staged upload named %q", name)
	}
	return p, nil
}

func (f *fakeStaging) RemoveUpload(name string) error {
	f.removed = append(f.removed, name)
	if f.order != nil {
		*f.order = append(*f.order, "remove:"+name)
	}
	return nil
}

// fakeQemuImg reports a fixed format on Info and, on ConvertToRaw, writes
// a zero-filled raw file of rawSize bytes - large enough for every
// partition slice a test extracts from it.
type fakeQemuImg struct {
	format  string
	rawSize int64
	infoErr error
	convErr error
	// convertCalls counts ConvertToRaw invocations, so a test asserting
	// that a raw source skips conversion entirely has something to check.
	convertCalls int
}

func (f *fakeQemuImg) Info(_ context.Context, _ string) (QemuImgInfo, error) {
	if f.infoErr != nil {
		return QemuImgInfo{}, f.infoErr
	}
	return QemuImgInfo{Format: f.format, VirtualSizeBytes: f.rawSize}, nil
}

func (f *fakeQemuImg) ConvertToRaw(_ context.Context, _, _, dst string) error {
	f.convertCalls++
	if f.convErr != nil {
		return f.convErr
	}
	return os.WriteFile(dst, make([]byte, f.rawSize), 0o600)
}

// fakeSfdisk returns a fixed JSON dump regardless of the disk path.
type fakeSfdisk struct {
	json []byte
	err  error
}

func (f *fakeSfdisk) Dump(_ context.Context, _ string) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.json, nil
}

// fakeAttacher simulates the nbd-attach path with no real device: Attach
// returns a fixed dev path and records attach/detach ordering (and
// whether detach ran at all) so tests can assert Run always detaches,
// even on a later failure. PartitionDevice fabricates "<dev>p<num>"
// unless partitionErr is set, simulating a partition node that never
// appeared.
type fakeAttacher struct {
	dev          string
	attachErr    error
	partitionErr error

	attached bool
	detached bool
	// events records, in order, "attach" and "detach" so a test can
	// assert detach only ever happens after attach, exactly once.
	events []string
	// order, when set, additionally records "detach" into a timeline
	// shared with another fake (e.g. fakeStaging's "remove:<name>"), so a
	// test can assert cleanup ordering across both.
	order *[]string
}

func (f *fakeAttacher) Attach(_ context.Context, _, _ string) (string, func(), error) {
	if f.attachErr != nil {
		return "", nil, f.attachErr
	}
	f.attached = true
	f.events = append(f.events, eventAttach)
	detach := func() {
		f.detached = true
		f.events = append(f.events, eventDetach)
		if f.order != nil {
			*f.order = append(*f.order, eventDetach)
		}
	}
	dev := f.dev
	if dev == "" {
		dev = "/dev/nbd0"
	}
	return dev, detach, nil
}

func (f *fakeAttacher) PartitionDevice(_ context.Context, dev string, num int) (string, error) {
	if f.partitionErr != nil {
		return "", f.partitionErr
	}
	return fmt.Sprintf("%sp%d", dev, num), nil
}

// fakeBlkid maps a partition slice path (matched by substring) to a
// fixed FSInfo.
type fakeBlkid struct {
	byPathSubstring map[string]FSInfo
	err             error
}

func (f *fakeBlkid) Detect(_ context.Context, path string) (FSInfo, error) {
	if f.err != nil {
		return FSInfo{}, f.err
	}
	for substr, info := range f.byPathSubstring {
		if strings.Contains(path, substr) {
			return info, nil
		}
	}
	return FSInfo{}, nil
}

// fakePartclone writes a minimal, valid content directory (one extent
// file plus a matching torrent.info) into targetDir, regardless of
// fsType or source content. The piece hash is arbitrary: internal/store
// takes torrent.info's hashes on trust and never re-hashes extent data
// (see internal/store/torrent.go), so a fixture hash is as good as a
// real one for exercising the hash/validate logic this package owns.
type fakePartclone struct {
	// extentContent, when set, is used as the single extent's bytes for
	// every call; otherwise a fixed default is used. Every call
	// producing identical extentContent yields an identical info hash,
	// which the dedup test relies on.
	extentContent []byte
	err           error
	calls         []string
}

func (f *fakePartclone) Clone(_ context.Context, fsType, source, targetDir string) error {
	f.calls = append(f.calls, fmt.Sprintf("%s:%s", fsType, source))
	if f.err != nil {
		return f.err
	}

	content := f.extentContent
	if content == nil {
		content = []byte("fixture-extent-data")
	}

	info := &store.TorrentInfo{
		BlockSize:   4096,
		BlocksTotal: 1,
		Extents:     []store.Extent{{Offset: 0, Length: uint64(len(content))}}, //nolint:gosec // fixture length
		PieceHashes: []store.PieceHash{{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}},
	}

	extentPath := filepath.Join(targetDir, store.ExtentFileName(0))
	if err := os.WriteFile(extentPath, content, 0o600); err != nil {
		return err
	}

	f2, err := os.Create(store.ContentDirTorrentInfoPath(targetDir))
	if err != nil {
		return err
	}
	defer func() { _ = f2.Close() }()
	return store.WriteTorrentInfo(f2, info)
}
