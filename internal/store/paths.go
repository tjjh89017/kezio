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

package store

import "path/filepath"

// This file defines the deterministic filesystem layout under one store
// root. The store root is a volume the ingest Job and the seeder mount in
// common (an RWX PVC or equivalent); everything below is a plain path
// contract, with no Kubernetes API involved.
//
//	<root>/
//	  contents/
//	    <info-hash-hex>/
//	      torrent.info
//	      content/
//	        <extent files, named by ExtentFileName>
//	      content.torrent
//	  images/
//	    <image-name>/
//	      layout.json
//
// contents/ holds immutable, content-addressed partition payloads, keyed by
// InfoHash so identical content always lands at the same path no matter
// which Image referenced it. images/ holds the per-Image slot layout (which
// content goes to which position on a target disk); that mapping is an
// Image-level concern, so this package only reserves the path for it and
// does not define layout.json's schema.
//
// The extent files sit inside a content/ subdirectory - not directly
// inside <info-hash-hex>/ - because BuildInfoDict's info dict is a
// BitTorrent v1 multi-file torrent with "name": contentName ("content").
// BEP3 multi-file semantics resolve every file entry as
// save_path/<name>/<path...>, and every compliant client (libtorrent
// included) enforces that unconditionally - there is no way to add a
// multi-file torrent whose files resolve directly under save_path with
// no extra directory level. So for AddTorrent(save_path=ContentDir(...))
// to find real bytes, the extent files have to actually live at
// ContentDir(...)/content/<hex>, matching what the torrent's own file
// entries already imply (see ContentDataDir, NestExtentFiles).

const (
	// contentsDirName is the contents/ subdirectory under a store root.
	contentsDirName = "contents"
	// imagesDirName is the images/ subdirectory under a store root.
	imagesDirName = "images"
	// ingestDirName is the .ingest/ subdirectory under a store root: a
	// scratch tree ingest uses to land a partition content directory on
	// the store's own filesystem before the final rename into
	// contents/, so that rename is a same-filesystem, atomic operation
	// (see IngestScratchDir).
	ingestDirName = ".ingest"
	// ContentTorrentFileName is the generated .torrent file name inside a
	// content directory.
	ContentTorrentFileName = "content.torrent"
	// ImageLayoutFileName is the per-Image layout metadata file name inside
	// an image directory. Its schema belongs to the Image-level package
	// that consumes it, not to this one.
	ImageLayoutFileName = "layout.json"
)

// ContentDir returns the directory holding a content's extent files and
// torrent.info, keyed by its info hash.
func ContentDir(root string, hash InfoHash) string {
	return filepath.Join(root, contentsDirName, hash.String())
}

// ContentTorrentInfoPath returns the torrent.info path inside a content's
// directory.
func ContentTorrentInfoPath(root string, hash InfoHash) string {
	return ContentDirTorrentInfoPath(ContentDir(root, hash))
}

// ContentExtentPath returns the extent file path for one offset inside a
// content's directory (under its content/ data subdirectory - see
// ContentDataDir).
func ContentExtentPath(root string, hash InfoHash, offset uint64) string {
	return filepath.Join(ContentDataDir(ContentDir(root, hash)), ExtentFileName(offset))
}

// ContentTorrentPath returns the generated .torrent file path inside a
// content's directory.
func ContentTorrentPath(root string, hash InfoHash) string {
	return filepath.Join(ContentDir(root, hash), ContentTorrentFileName)
}

// IngestScratchDir returns the per-Image scratch directory ingest may
// use to stage a partition content directory on the store's own
// filesystem before publishing it into contents/. It is keyed by
// imageName (a validated Kubernetes object name, safe as a path
// component) rather than by a random or per-attempt token: the Image
// controller runs at most one ingest Job per Image, with a deterministic
// Job name (see internal/controller's ingestJobName), so a retried
// attempt after a crash reuses this same path and can safely clean up
// whatever a previous attempt left behind before starting - no leftover
// scratch survives a crashed job, and no coordination between attempts
// is needed since they never run concurrently for the same Image.
func IngestScratchDir(root, imageName string) string {
	return filepath.Join(root, ingestDirName, imageName)
}

// ImageDir returns the directory holding an Image's layout metadata. The
// caller is expected to pass a name already validated as a Kubernetes
// object name (DNS-1123 subdomain), so it is safe to use as a path
// component.
func ImageDir(root, imageName string) string {
	return filepath.Join(root, imagesDirName, imageName)
}

// ImageLayoutPath returns the layout metadata file path for an Image.
func ImageLayoutPath(root, imageName string) string {
	return filepath.Join(ImageDir(root, imageName), ImageLayoutFileName)
}
