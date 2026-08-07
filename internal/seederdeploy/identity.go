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

// Package seederdeploy is the identity contract for a per-Image,
// per-site seeder Deployment: the deterministic name
// internal/controller derives it under (see Name), and the fixed port
// its pods serve .torrent files on (see TorrentHTTPPort).
//
// It exists as its own package, rather than living directly in
// internal/controller (which creates these Deployments) or being
// re-derived in internal/agentserver (which discovers them to resolve a
// content partition's fetch URL - see agentserver's
// resolveSeederTorrentURL), because internal/controller's own test
// suite already imports internal/agentserver (to drive envtest through
// the real agent HTTP server): internal/agentserver importing
// internal/controller directly would close that into an import cycle.
// This package depends on neither, so both can depend on it.
package seederdeploy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// TorrentHTTPPort is the fixed port cmd/seeder's HTTP server listens on
// inside the seeder-register container, serving each mounted partition's
// .torrent by info hash (GET /torrents/<hash>) - see cmd/seeder/main.go.
const TorrentHTTPPort int32 = 8080

// namePrefix identifies a Deployment as a per-Image seeder this operator
// manages, at a glance in `kubectl get deployments`.
const namePrefix = "kezio-seeder-"

// maxNameLength is the Kubernetes Deployment name limit: a ReplicaSet
// name adds a hash suffix and a Pod name adds a further one on top of
// that, so the Deployment name itself is kept well inside the 63-
// character DNS-1035 label limit those generated names must also
// satisfy.
const maxNameLength = 63

// Name returns the deterministic Deployment name for imageName's seeder
// at site. Deterministic so internal/controller's reconciler stays
// idempotent, and always hash-suffixed (unlike a plain
// prefix+imageName) because two different sites of the same Image must
// never collide on one name, and internal/agentserver needs to compute
// this exact same name (rather than list-and-filter) to Get the
// Deployment directly when resolving a content partition's seeder URL.
func Name(imageName, site string) string {
	sum := sha256.Sum256([]byte(imageName + "\x00" + site))
	suffix := "-" + hex.EncodeToString(sum[:])[:8]

	name := namePrefix + imageName + suffix
	if len(name) <= maxNameLength {
		return name
	}

	maxBaseLen := maxNameLength - len(namePrefix) - len(suffix)
	return fmt.Sprintf("%s%s%s", namePrefix, imageName[:maxBaseLen], suffix)
}
