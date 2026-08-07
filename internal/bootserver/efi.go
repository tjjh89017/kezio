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
// loads. This package's HTTP Boot route and internal/bootd's TFTP server
// both need the exact same two names; internal/bootd.ShimFilename /
// internal/bootd.GrubFilename alias these rather than redefining them, so
// the two allowlists cannot drift apart.
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
// counterpart to internal/bootd's TFTP shim/grub service - where
// internal/bootd's Config.HTTPBootURL is expected to point, e.g.
// "http://10.0.0.5:8090/boot/http/shimx64.efi".
//
// name is a single ServeMux path segment (cannot contain "/"), checked
// against allowedEFIFiles by exact equality before ever being joined onto
// Config.EFIDir - filepath.Join only ever sees one of the two literal
// allowlisted strings, never attacker-controlled input.
//
// http.ServeFile (not a hand-rolled io.Copy) handles the response:
// correct Content-Length, conditional-GET caching, and Range support,
// since some UEFI HTTP Boot firmware issues ranged fetches.
func (s *Server) handleEFI(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !allowedEFIFiles[name] {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", efiContentType)
	http.ServeFile(w, r, filepath.Join(s.Config.EFIDir, name))
}
