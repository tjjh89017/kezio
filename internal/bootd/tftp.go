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
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-logr/logr"
	tftp "github.com/pin/tftp/v3"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/tjjh89017/kezio/internal/bootserver"
)

// ShimFilename and GrubFilename are the two on-disk names the TFTP
// server (TFTPServer) serves: shim (chainloading into grub, see
// DefaultBootFilename's doc comment) and the grub image it loads.
// These alias bootserver.ShimFilename / bootserver.GrubFilename
// (rather than redefining the two names) so that package's UEFI HTTP
// Boot route serves exactly the same allowlist TFTP does - see
// bootserver.ShimFilename's doc comment for why the definition lives
// there.
const (
	ShimFilename = bootserver.ShimFilename
	GrubFilename = bootserver.GrubFilename
)

// GrubConfigPath is the one non-basename path TFTPServer serves:
// Debian's netboot-signed GRUB image (grub-efi-amd64-signed's
// grubnetx64.efi.signed, published as grubx64.efi - see
// hack/live-image/build.sh) is built with the embedded prefix "/grub"
// resolved against the device it was loaded from, so after shim
// chainloads it over TFTP it sources (tftp,<next-server>)/grub/grub.cfg
// (via its embedded memdisk bootstrap config; the image is monolithic,
// with every module it needs built in, so no x86_64-efi module tree is
// ever fetched). TFTPServer answers this path from TFTPServer.GrubConfig
// - rendered in-memory content, not a file under Dir, because it embeds
// the site-specific boot config server URL (see RenderGrubConfig) that
// a release-published artifact could never carry.
const GrubConfigPath = "grub/grub.cfg"

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
	// VRFDevice, when set, binds the listening UDP socket to this
	// device (SO_BINDTODEVICE, see bindToDeviceControl) before it
	// binds to Addr, so the socket's routing follows that device's
	// VRF table instead of the pod's default one. Set this to
	// ProvisioningVRFName exactly when Config.ProvisioningGateway is
	// configured (see cmd/bootd). Empty (the default) binds the
	// socket exactly as before this field existed.
	VRFDevice string
}

var _ manager.Runnable = (*TFTPServer)(nil)

// DefaultTFTPAddr is the standard TFTP port; see TFTPServer.Addr.
const DefaultTFTPAddr = ":69"

// Start implements manager.Runnable: it serves until ctx is cancelled.
//
// The UDP socket is built explicitly (rather than via
// tftp.Server.ListenAndServe) so VRFDevice can be applied to it: empty,
// net.ListenConfig.Control is nil and the resulting socket is
// identical to what ListenAndServe would have produced. pin/tftp's
// Serve type-asserts the conn to *net.UDPConn for per-packet source
// address handling; net.ListenConfig.ListenPacket("udp", ...) returns
// exactly that concrete type, so the assertion still succeeds.
func (t *TFTPServer) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("bootd-tftp")
	addr := t.Addr
	if addr == "" {
		addr = DefaultTFTPAddr
	}

	lc := net.ListenConfig{}
	if t.VRFDevice != "" {
		lc.Control = bindToDeviceControl(t.VRFDevice)
	}
	conn, err := lc.ListenPacket(ctx, "udp", addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", addr, err)
	}

	srv := tftp.NewServer(t.readHandler(log), nil) // nil writeHandler: every write request is rejected.

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(conn) }()

	select {
	case <-ctx.Done():
		srv.Shutdown()
		return nil
	case err := <-errCh:
		return err
	}
}

// readHandler returns the RRQ handler passed to tftp.NewServer. filename
// arrives from the wire exactly as the client requested it; after
// stripping one optional leading "/" (GRUB requests prefix-relative
// files as "/grub/grub.cfg", firmware requests the DHCP-advertised name
// without a slash), it is matched exactly: GrubConfigPath is answered
// from memory (never the filesystem), and everything else is validated
// against allowedTFTPFiles by basename before ever touching the
// filesystem, which rejects both an unrecognized name and any
// path-traversal attempt (a request for "../../etc/passwd" has
// filepath.Base "passwd", which is not in the allowlist, so it is
// rejected the same as any other unknown name - it is never joined onto
// t.Dir at all).
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
		// Logged at the default level, alongside dnsmasq's own
		// forwarded per-request lines (see dnsmasq.go): a completed
		// TFTP transfer is the next low-volume, high-value checkpoint
		// in the same boot sequence, and its absence is what tells an
		// operator a client got a DHCP offer but never came back for
		// the file it named.
		log.Info("served TFTP file", "requested", filename)
		return nil
	}
}
