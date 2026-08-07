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
}

func (f *fakeQemuImg) Info(_ context.Context, _ string) (QemuImgInfo, error) {
	if f.infoErr != nil {
		return QemuImgInfo{}, f.infoErr
	}
	return QemuImgInfo{Format: f.format, VirtualSizeBytes: f.rawSize}, nil
}

func (f *fakeQemuImg) ConvertToRaw(_ context.Context, _, _, dst string) error {
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
// real one for exercising the store/move/dedup logic this package owns.
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

// fakeLayoutWriter records every sfdiskJSON value it was asked to
// persist, standing in for the real Kubernetes-API-backed LayoutWriter.
type fakeLayoutWriter struct {
	written []string
	err     error
}

func (f *fakeLayoutWriter) WriteLayout(_ context.Context, sfdiskJSON string) error {
	if f.err != nil {
		return f.err
	}
	f.written = append(f.written, sfdiskJSON)
	return nil
}
