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

// DefaultKernelPath, DefaultInitrdPath, and DefaultSquashfsPath name the
// live boot artifacts under Config.ArtifactsDir when Config.KernelPath /
// Config.InitrdPath / Config.SquashfsPath are left empty. Placing
// artifacts under these names is a convention this package sets, not a
// requirement the artifact build has to follow - override them if the
// build produces different names. They match the filenames the live
// image build writes to dist/live/ (hack/live-image/build.sh).
const (
	DefaultKernelPath   = "vmlinuz"
	DefaultInitrdPath   = "initrd.img"
	DefaultSquashfsPath = "filesystem.squashfs"
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
	// example "http://10.0.0.5:8090". It is used to build the
	// kernel/initrd/squashfs HTTP URLs GRUB and live-boot's initrd fetch.
	ServerURL string
	// AgentServerURL is internal/agentserver's own externally reachable
	// base URL, for example "http://10.0.0.5:8091" - a different address
	// from ServerURL whenever the boot config server and the agent
	// registration server are reachable on different ports or hosts (the
	// ordinary case: they are two different Services in front of the
	// same controller-manager Pod, on two different container ports).
	// It is used as the kezio.server= cmdline value the agent reads its
	// registration endpoint from. Empty means ServerURL, which is only
	// correct when both servers genuinely share the exact same address
	// (for example, a single reverse proxy in front of both) - using it
	// as a silent default otherwise sends every kezio-agent registration
	// to a server that never mounts internal/agentserver's routes, which
	// then fails without ever surfacing an error server-side (see
	// renderNetBootConfig's doc comment).
	AgentServerURL string
	// KernelPath, InitrdPath, and SquashfsPath name the live boot
	// artifacts under ArtifactsDir. Empty means DefaultKernelPath /
	// DefaultInitrdPath / DefaultSquashfsPath. SquashfsPath is not served
	// through the linux/initrd GRUB directives; it is only referenced in
	// the kernel cmdline's fetch= value, which live-boot's initrd reads
	// to download the root file system over HTTP.
	KernelPath   string
	InitrdPath   string
	SquashfsPath string
	// EFIDir is the local filesystem directory GET /boot/http/... serves
	// the signed shim/grub EFI binaries from (see efi.go) - the HTTP
	// counterpart to internal/bootd's TFTP allowlist, for UEFI HTTP
	// Boot. Empty means ArtifactsDir: the same read-only mounted volume
	// already used for the kernel/initrd/squashfs is the natural place
	// to also stage shimx64.efi/grubx64.efi, so a deployment that adds
	// those two files to that volume needs no separate mount or env var
	// to serve them. Set it only if the EFI binaries live somewhere
	// else.
	EFIDir string
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
	if c.SquashfsPath == "" {
		c.SquashfsPath = DefaultSquashfsPath
	}
	if c.TokenTTL <= 0 {
		c.TokenTTL = DefaultTokenTTL
	}
	if c.EFIDir == "" {
		c.EFIDir = c.ArtifactsDir
	}
	if c.AgentServerURL == "" {
		c.AgentServerURL = c.ServerURL
	}
	return c
}
