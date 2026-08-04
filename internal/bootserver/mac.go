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
	"regexp"
	"strings"
)

// macPattern matches a colon-separated MAC address, case-insensitively.
// It intentionally mirrors keziov1alpha1.MACAddressPattern rather than
// importing it: the two must accept the same strings, but this package
// only ever needs the pattern to normalize an incoming path segment, not
// the CRD validation marker that string is also used as.
var macPattern = regexp.MustCompile(`^([0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}$`)

// NormalizeMAC validates mac as a colon-separated MAC address and returns
// it lower-cased, so it compares equal to the index key IndexMachineBootMAC
// derives from Machine.spec.bootMACAddress regardless of how either side
// capitalized its hex digits. ok is false when mac does not match the
// expected shape at all; the caller should treat that the same as an
// unknown machine, not as a server error.
func NormalizeMAC(mac string) (normalized string, ok bool) {
	if !macPattern.MatchString(mac) {
		return "", false
	}
	return strings.ToLower(mac), true
}
