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
	"os"
	"path"
	"path/filepath"
)

// artifactsPrefix is the URL prefix GET /boot/artifacts/... is mounted
// under; renderNetBootConfig builds its kernel/initrd URLs against the
// same constant.
const artifactsPrefix = "/boot/artifacts/"

// artifactsHandler serves the live kernel, initrd, and squashfs out of
// dir. It wraps http.FileServer (which already resolves ".." path
// segments against dir before opening anything, so it cannot be made to
// read outside it) with one extra check: refuse a request that resolves
// to a directory, so nothing under dir is ever browsable. This directory
// is expected to hold only a handful of named boot artifacts; there is
// no legitimate reason to list it, and doing so would give an
// unauthenticated caller a map of exactly what this deployment serves.
func artifactsHandler(dir string) http.Handler {
	fileServer := http.FileServer(http.Dir(dir))
	inner := http.StripPrefix(artifactsPrefix, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requestsDirectory(dir, r.URL.Path) {
			http.NotFound(w, r)
			return
		}
		fileServer.ServeHTTP(w, r)
	}))
	return inner
}

// requestsDirectory reports whether reqPath (already stripped of
// artifactsPrefix) resolves, under dir, to a directory rather than a
// regular file. It mirrors http.Dir's own path handling (clean against a
// leading "/" before joining) so its containment check agrees with what
// http.FileServer will actually open.
func requestsDirectory(dir, reqPath string) bool {
	clean := path.Clean("/" + reqPath)
	full := filepath.Join(dir, filepath.FromSlash(clean))
	info, err := os.Stat(full)
	if err != nil {
		return false
	}
	return info.IsDir()
}
