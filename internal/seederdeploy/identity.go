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

// Package seederdeploy is the identity contract for a per-(Image, Site)
// seeder Deployment: the deterministic name internal/controller derives it
// under (see Name), and the fixed port its pods serve .torrent files on
// (see TorrentHTTPPort).
package seederdeploy

import (
	"crypto/sha256"
	"encoding/hex"
)

// TorrentHTTPPort is the fixed port cmd/seeder's HTTP server listens on
// inside the seeder-register container, serving every mounted content's
// .torrent file by info hash - see cmd/seeder/main.go.
const TorrentHTTPPort int32 = 8080

// EzioGRPCPort is the fixed port the ezio container's gRPC control plane
// listens on. Both containers share one pod network namespace, so
// seeder-register always reaches it over loopback at this port.
const EzioGRPCPort int32 = 50051

// EzioBTPort is the seeder's fixed BitTorrent listen port, passed to ezio
// as --port. Every seeder pod has its own network namespace, so nothing
// here needs it to avoid colliding with another pod - it is pinned so the
// data network can be firewalled to exactly the ports that must be open;
// an ephemeral port would work per pod but make that firewall rule
// impossible to write.
const EzioBTPort int32 = 16881

// TorrentHealthzPath is the path on TorrentHTTPPort that only proves the
// HTTP server itself is bound and answering, independent of whether any
// content has been registered yet. cmd/seeder's torrentMux serves it,
// and the seeder Deployment's readiness probe (internal/controller)
// hits it.
const TorrentHealthzPath = "/healthz"

// namePrefix identifies a Deployment as a per-(Image, Site) seeder this
// operator manages, at a glance in `kubectl get deployments`.
const namePrefix = "kezio-seeder-"

// maxNameLength is the Kubernetes Deployment name limit, kept well inside
// the 63-character DNS-1035 limit the ReplicaSet/Pod names generated from
// it must also satisfy.
const maxNameLength = 63

// Name returns the deterministic Deployment name for imageName's seeder at
// the Site identified by siteIdentity (the "namespace/name" string
// sitederive.SiteIdentity returns): namePrefix, then imageName (truncated
// as needed to fit), then an 8-hex-character suffix derived from
// siteIdentity.
//
// Deterministic so a reconciler stays idempotent across reconciles. The
// suffix is always present, not just on overflow: imageName alone is
// shared by every Site that Image seeds, so it cannot disambiguate them on
// its own, and the suffix is what keeps two Sites of the same Image from
// ever colliding on one Deployment name.
func Name(imageName, siteIdentity string) string {
	sum := sha256.Sum256([]byte(siteIdentity))
	suffix := "-" + hex.EncodeToString(sum[:])[:8]

	maxBaseLen := maxNameLength - len(namePrefix) - len(suffix)
	base := imageName
	if len(base) > maxBaseLen {
		base = base[:maxBaseLen]
	}
	return namePrefix + base + suffix
}
