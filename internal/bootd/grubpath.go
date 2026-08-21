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
	"fmt"
	"net/url"
	"strings"
)

// GrubNetPath converts an HTTP base URL plus an absolute path into GRUB's
// network file syntax: "http://192.0.2.1:8090" + "/boot/x" becomes
// "(http,192.0.2.1:8090)/boot/x". GRUB does not understand bare URLs -
// grub_file_open treats only a leading "(" as naming a device, otherwise
// resolving the path relative to $root (the TFTP server on a net boot)
// and failing; "(<protocol>,<server[:port]>)/<path>" is the form GRUB's
// network stack actually resolves. Only http is accepted: GRUB's netboot
// images carry an http module but no TLS stack.
//
// It lives in bootd rather than internal/bootserver (both packages need
// it - RenderGrubConfig here, renderNetBootConfig there) for the same
// import-direction reason as NormalizeMAC (see mac.go):
// internal/bootserver.GrubNetPath forwards here.
func GrubNetPath(serverURL, filePath string) (string, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return "", fmt.Errorf("parsing server URL %q: %w", serverURL, err)
	}
	if u.Scheme != httpScheme {
		return "", fmt.Errorf("server URL %q: GRUB's network stack supports only http, not %q", serverURL, u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("server URL %q carries no host", serverURL)
	}
	if !strings.HasPrefix(filePath, "/") {
		filePath = "/" + filePath
	}
	return fmt.Sprintf("(%s,%s)%s%s", u.Scheme, u.Host, strings.TrimRight(u.Path, "/"), filePath), nil
}
