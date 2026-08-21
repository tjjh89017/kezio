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

package bootserver

import "github.com/tjjh89017/kezio/internal/bootd"

// NormalizeMAC validates mac as a colon-separated MAC address and
// returns it lower-cased, so it compares equal to the index key
// IndexMachineBootMAC derives from Machine.spec.bootMACAddress
// regardless of how either side capitalized its hex digits. ok is false
// when mac does not match the expected shape at all; the caller should
// treat that the same as an unknown machine, not as a server error.
//
// Forwards to internal/bootd.NormalizeMAC, the single source of truth -
// this package already imports internal/bootd for its shared
// ShimFilename/GrubFilename constants (see efi.go), and bootd cannot
// import back without a cycle, so the definition lives there.
func NormalizeMAC(mac string) (normalized string, ok bool) {
	return bootd.NormalizeMAC(mac)
}
