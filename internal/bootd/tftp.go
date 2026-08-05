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

package bootd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/go-logr/logr"
	tftp "github.com/pin/tftp/v3"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/tjjh89017/kezio/internal/bootserver"
)

// ShimFilename and GrubFilename are the only two names the TFTP server
// (TFTPServer) ever serves: shim (chainloading into grub, see
// DefaultBootFilename's doc comment) and the grub image it loads,
// which then fetches its real config over HTTP from the boot config
// server (internal/bootserver) - TFTP's job in this boot flow ends
// there. These alias bootserver.ShimFilename / bootserver.GrubFilename
// (rather than redefining the two names) so that package's UEFI HTTP
// Boot route serves exactly the same allowlist TFTP does - see
// bootserver.ShimFilename's doc comment for why the definition lives
// there.
const (
	ShimFilename = bootserver.ShimFilename
	GrubFilename = bootserver.GrubFilename
)

// allowedTFTPFiles is queried by basename only (see readHandler): a
// fixed allowlist, not a directory listing, is what makes this server
// safe to expose to any device on the boot L2 segment - there is no
// filename that reaches anything outside cfg.TFTPDir, because there is
// no filename this server will open that was not already hard-coded
// here.
var allowedTFTPFiles = map[string]bool{
	ShimFilename: true,
	GrubFilename: true,
}

// TFTPServer wraps a read-only, two-file github.com/pin/tftp/v3 server:
// see the package doc comment's TFTP paragraph for the read-only,
// path-restricted contract every request goes through.
type TFTPServer struct {
	// Dir is the local filesystem directory ShimFilename and
	// GrubFilename are read from.
	Dir string
	// Addr is the UDP address to listen on, for example ":69". Empty
	// means ":69" (the standard TFTP port).
	Addr string
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

// readHandler returns the RRQ handler passed to tftp.NewServer. filename
// arrives from the wire exactly as the client requested it; it is
// validated against allowedTFTPFiles by basename before ever touching
// the filesystem, which rejects both an unrecognized name and any
// path-traversal attempt (a request for "../../etc/passwd" has
// filepath.Base "passwd", which is not in the allowlist, so it is
// rejected the same as any other unknown name - it is never joined onto
// t.Dir at all).
func (t *TFTPServer) readHandler(log logr.Logger) func(filename string, rf io.ReaderFrom) error {
	return func(filename string, rf io.ReaderFrom) error {
		base := filepath.Base(filename)
		if !allowedTFTPFiles[base] || base != filename {
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
		// Logged at the default level, matching serveUDP's "answering
		// PXE request" log (server.go): a completed TFTP transfer is the
		// next low-volume, high-value checkpoint in the same boot
		// sequence, and its absence is what tells an operator a client
		// got a DHCP offer but never came back for the file it named.
		log.Info("served TFTP file", "requested", filename)
		return nil
	}
}
