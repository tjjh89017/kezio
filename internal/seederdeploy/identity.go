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

// Package seederdeploy is the identity contract for a per-content seeder
// Deployment: the deterministic name internal/controller derives it
// under (see Name), and the fixed port its pods serve .torrent files on
// (see TorrentHTTPPort).
package seederdeploy

import "github.com/tjjh89017/kezio/internal/store"

// TorrentHTTPPort is the fixed port cmd/seeder's HTTP server listens on
// inside the seeder-register container, serving the mounted content's
// .torrent file - see cmd/seeder/main.go.
const TorrentHTTPPort int32 = 8080

// namePrefix identifies a Deployment as a per-content seeder this
// operator manages, at a glance in `kubectl get deployments`.
const namePrefix = "kezio-seeder-"

// Name returns the deterministic Deployment name for the content
// identified by hash: namePrefix plus hash's lowercase hex string.
// Deterministic so a reconciler stays idempotent, and one Deployment
// per content (no site or network qualifier) since seeding is not yet
// site-aware. hash.String() is a fixed 40 hex characters, so the result
// always fits well within the 63-character DNS-1035 label limit a
// generated ReplicaSet/Pod name must also satisfy - no truncation logic
// is needed here.
func Name(hash store.InfoHash) string {
	return namePrefix + hash.String()
}
