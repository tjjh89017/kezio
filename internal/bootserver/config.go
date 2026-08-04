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

import "time"

// DefaultKernelPath and DefaultInitrdPath name the live boot artifacts
// under Config.ArtifactsDir when Config.KernelPath / Config.InitrdPath
// are left empty. Placing artifacts under these names is a convention
// this package sets, not a requirement the artifact build has to follow -
// override them if the build produces different names.
const (
	DefaultKernelPath = "vmlinuz"
	DefaultInitrdPath = "initrd.img"
)

// DefaultTokenTTL bounds how long a minted boot token is accepted. It
// must comfortably cover one PXE cycle - GRUB's config fetch, the
// kernel/initrd HTTP download, and the live OS booting far enough to run
// the agent's registration - without staying valid so long that an
// unused token sits as a live credential.
const DefaultTokenTTL = 30 * time.Minute

// Config configures a Server.
type Config struct {
	// Addr is the address the HTTP server listens on, for example
	// ":8090".
	Addr string
	// ArtifactsDir is the local filesystem directory GET
	// /boot/artifacts/... serves from: the live kernel, initrd, and
	// squashfs. It is expected to be a read-only mounted volume; this
	// package only reads from it.
	ArtifactsDir string
	// ServerURL is this server's own externally reachable base URL, for
	// example "http://10.0.0.5:8090". It is used both to build the
	// kernel/initrd HTTP URLs GRUB fetches and as the kezio.server=
	// cmdline value the agent reads its registration endpoint from.
	ServerURL string
	// KernelPath and InitrdPath name the live boot artifacts under
	// ArtifactsDir. Empty means DefaultKernelPath / DefaultInitrdPath.
	KernelPath string
	InitrdPath string
	// TokenTTL bounds how long a minted boot token is accepted. Zero
	// means DefaultTokenTTL.
	TokenTTL time.Duration
}

// withDefaults returns a copy of c with every zero-valued optional field
// filled in.
func (c Config) withDefaults() Config {
	if c.KernelPath == "" {
		c.KernelPath = DefaultKernelPath
	}
	if c.InitrdPath == "" {
		c.InitrdPath = DefaultInitrdPath
	}
	if c.TokenTTL <= 0 {
		c.TokenTTL = DefaultTokenTTL
	}
	return c
}
