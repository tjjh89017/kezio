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

package bootserver

import (
	"net/http"
	"path/filepath"
)

// ShimFilename and GrubFilename name the two signed EFI binaries both
// boot fronts serve: shim (chainloading into grub) and the grub image it
// loads. This package's HTTP Boot route (handleEFI, GET
// /boot/http/<name>, below) and internal/bootd's TFTP server
// (internal/bootd/tftp.go) both need the exact same two names, and
// internal/bootd already imports this package (for NormalizeMAC), so
// this is the single definition: internal/bootd.ShimFilename /
// internal/bootd.GrubFilename alias these rather than redefining them,
// which is what keeps the two fronts from drifting apart - a name added
// to one allowlist and not the other is the failure mode a single source
// of truth closes off.
const (
	ShimFilename = "shimx64.efi"
	GrubFilename = "grubx64.efi"
)

// allowedEFIFiles is the exact set of names GET /boot/http/<name> (see
// Server.Handler) will ever serve.
var allowedEFIFiles = map[string]bool{
	ShimFilename: true,
	GrubFilename: true,
}

// efiContentType is served for every EFI binary response. UEFI HTTP Boot
// firmware does not generally inspect Content-Type, and "application/efi"
// is not a registered IANA media type, so the generic, always-safe binary
// type is used instead of guessing at one that is not standardized.
const efiContentType = "application/octet-stream"

// handleEFI implements GET /boot/http/{name}, the UEFI HTTP Boot
// counterpart to internal/bootd's TFTP shim/grub service: it is where
// Config.HTTPBootURL (internal/bootd's Config field bootd hands firmware
// as the DHCP boot filename) is expected to point, for example
// "http://10.0.0.5:8090/boot/http/shimx64.efi" (see
// config/bootd/README.md's UEFI HTTP Boot section).
//
// name comes from a single net/http ServeMux path segment ({name} cannot
// itself contain a "/", encoded or not: PathValue decodes only the bytes
// within that one segment), and is then checked against allowedEFIFiles
// by exact equality before it is ever joined onto Config.EFIDir. Neither
// an unrecognized name nor a path-traversal attempt can reach the
// filesystem: filepath.Join is only ever called with one of the two
// literal strings in allowedEFIFiles, never with attacker-controlled
// input. This mirrors TFTPServer.readHandler's allowlist-by-basename
// posture (internal/bootd/tftp.go) applied to a request shape where the
// filesystem-facing check is even simpler, since the route pattern
// already bounds name to one segment.
//
// http.ServeFile (not a hand-rolled io.Copy) handles the actual
// response: correct Content-Length, conditional-GET caching headers, and
// Range support, since some UEFI HTTP Boot firmware issues ranged
// fetches.
func (s *Server) handleEFI(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !allowedEFIFiles[name] {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", efiContentType)
	http.ServeFile(w, r, filepath.Join(s.Config.EFIDir, name))
}
