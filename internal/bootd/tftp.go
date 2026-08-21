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

package bootd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-logr/logr"
	tftp "github.com/pin/tftp/v3"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// GrubConfigPath is the one non-basename path TFTPServer serves: Debian's
// netboot-signed GRUB image (grubx64.efi, embedded prefix "/grub") sources
// (tftp,<next-server>)/grub/grub.cfg after shim chainloads it - the image
// is monolithic (no module tree fetched separately). TFTPServer answers
// this from TFTPServer.GrubConfig, rendered in-memory content rather than
// a file under Dir, because it embeds the site-specific boot config
// server URL (RenderGrubConfig) a release-published artifact can't carry.
const GrubConfigPath = "grub/grub.cfg"

// allowedTFTPFiles is queried by basename only (see readHandler): a fixed
// allowlist, not a directory listing, is what makes this server safe to
// expose on the boot L2 segment - no filename reaches outside cfg.TFTPDir
// because none but these are ever opened.
var allowedTFTPFiles = map[string]bool{
	ShimFilename: true,
	GrubFilename: true,
}

// TFTPServer wraps a read-only, allowlisted github.com/pin/tftp/v3
// server: see the package doc comment's TFTP paragraph for the
// read-only, path-restricted contract every request goes through.
type TFTPServer struct {
	// Dir is the local filesystem directory ShimFilename and
	// GrubFilename are read from.
	Dir string
	// Addr is the UDP address to listen on, for example ":69". Empty
	// means ":69" (the standard TFTP port).
	Addr string
	// GrubConfig is the rendered GRUB bootstrap config served at
	// GrubConfigPath (see that constant's doc comment and
	// RenderGrubConfig). Empty means the path is not served - GRUB
	// then falls to its rescue prompt, the same behavior as before
	// this config existed - and bootd logs the gap at startup (see
	// cmd/bootd).
	GrubConfig string
}

var _ manager.Runnable = (*TFTPServer)(nil)

// DefaultTFTPAddr is the standard TFTP port; see TFTPServer.Addr.
const DefaultTFTPAddr = ":69"

// Start implements manager.Runnable: it serves until ctx is cancelled.
func (t *TFTPServer) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("bootd-tftp")
	addr := t.Addr
	if addr == "" {
		addr = DefaultTFTPAddr
	}

	srv := tftp.NewServer(t.readHandler(log), nil) // nil writeHandler: every write request is rejected.

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe(addr) }()

	select {
	case <-ctx.Done():
		srv.Shutdown()
		return nil
	case err := <-errCh:
		return err
	}
}

// readHandler returns the RRQ handler passed to tftp.NewServer. After
// stripping one optional leading "/" (GRUB requests prefix-relative
// files, firmware requests the DHCP-advertised name bare), GrubConfigPath
// is answered from memory and everything else is validated against
// allowedTFTPFiles by basename before touching the filesystem - a
// path-traversal request like "../../etc/passwd" has basename "passwd",
// not in the allowlist, so it's rejected before ever being joined onto
// t.Dir.
func (t *TFTPServer) readHandler(log logr.Logger) func(filename string, rf io.ReaderFrom) error {
	return func(filename string, rf io.ReaderFrom) error {
		name := strings.TrimPrefix(filename, "/")

		if name == GrubConfigPath && t.GrubConfig != "" {
			if _, err := rf.ReadFrom(strings.NewReader(t.GrubConfig)); err != nil {
				return fmt.Errorf("serving %s: %w", GrubConfigPath, err)
			}
			log.Info("served TFTP file", "requested", filename)
			return nil
		}

		base := filepath.Base(name)
		if !allowedTFTPFiles[base] || base != name {
			log.Info("rejecting TFTP read for disallowed filename", "requested", filename)
			return fmt.Errorf("file %q not available", filename)
		}

		f, err := os.Open(filepath.Join(t.Dir, base))
		if err != nil {
			return fmt.Errorf("opening %s: %w", base, err)
		}
		defer func() { _ = f.Close() }()

		if _, err := rf.ReadFrom(f); err != nil {
			return fmt.Errorf("serving %s: %w", base, err)
		}
		// A completed TFTP transfer is the next checkpoint after
		// dnsmasq's forwarded per-request lines; its absence tells an
		// operator a client got a DHCP offer but never fetched the file.
		log.Info("served TFTP file", "requested", filename)
		return nil
	}
}
