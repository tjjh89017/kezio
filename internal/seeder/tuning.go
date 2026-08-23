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

package seeder

// ResolveMaxUploads applies the three-layer AddTorrent max_uploads
// precedence: DefaultMaxUploads, overridden by clusterDefault when
// positive, overridden again by override when non-nil.
//
// clusterDefault is deliberately just a plain int32, not a shared type:
// the seeder side and the leecher plan side keep separate cluster-wide
// operator defaults on purpose (a seeder serves every leecher at its Site
// at once, a leecher serves only itself), so each caller passes its own
// value rather than sharing one setting.
func ResolveMaxUploads(clusterDefault int32, override *int32) int32 {
	v := int32(DefaultMaxUploads)
	if clusterDefault > 0 {
		v = clusterDefault
	}
	if override != nil {
		v = *override
	}
	return v
}

// ResolveMaxConnections applies the same three-layer precedence as
// ResolveMaxUploads, for max_connections.
func ResolveMaxConnections(clusterDefault int32, override *int32) int32 {
	v := int32(DefaultMaxConnections)
	if clusterDefault > 0 {
		v = clusterDefault
	}
	if override != nil {
		v = *override
	}
	return v
}
