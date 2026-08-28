# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project status

kezio is a demo/proof of concept for ezio, not a shipped product yet — it may become one.
The API is `v1alpha3` and deliberately unstable: a breaking CRD change costs nothing today
(no conversion webhook is owed), so fix API shape now rather than working around it. Favour
cheap, low-risk changes over deep rewrites.

## Repository rule: do not grep Go files

A `PreToolUse` hook (`.claude/hooks/block-go-grep.sh`) blocks `grep`/`rg`/`sed`/`awk`
over Go files, including one-file "quick checks". Use the gopls MCP tools instead —
`go_search` (find a symbol), `go_symbol_references` (exact reference set),
`go_file_context`, `go_package_api`, `go_diagnostics` — and the Read tool for a file
whose path you already know. `find`, `ls`, and `wc` are fine, as is grep over non-Go
files (`config/**/*.yaml`, docs, workflows).

## Commands

```sh
make build              # manager binary -> bin/ (runs manifests, generate, fmt, vet first)
make build-kezioctl     # the CLI
make build-ingest       # kezio-ingest (runs inside ImageImport/PartitionContent Jobs)
make build-bootd        # kezio-bootd (per-Subnet proxyDHCP/TFTP daemon)
make test               # unit + envtest, everything except ./test/e2e
make lint               # golangci-lint
make manifests generate # regenerate CRDs/RBAC/webhook config and deepcopy
```

`make test` downloads envtest binaries into `bin/` and exports `KUBEBUILDER_ASSETS`.
To run a single test you must set that yourself, or the envtest suites fail to start:

```sh
export KUBEBUILDER_ASSETS="$(bin/setup-envtest use -p path --bin-dir bin)"
go test ./internal/controller/ -run TestMachineReconcile -v
```

`make test-e2e` needs Kind and is not the fast feedback loop; CI (`.github/workflows/main.yaml`)
runs the real KubeVirt-based lanes.

**After editing anything in `api/v1alpha3/`, run `make manifests generate`** — the CRD
YAML under `config/crd/bases/` and `zz_generated.deepcopy.go` are both generated, and
`controller-gen` scans exactly the roots in the Makefile's `GEN_PATHS` (not `./...`,
because the tree can hold other Go modules).

Lint gotchas that bite: `lll` (line length), `gocyclo`, `dupl`, and `goconst` are all
on; `dupl` and `lll` are excluded under `internal/`, `lll` under `api/`.

## Architecture

kezio is an operator-sdk/kubebuilder operator that deploys an OS image onto bare-metal
machines. The payload moves over BitTorrent using [ezio](https://github.com/tjjh89017/ezio):
partclone slices of each partition are seeded once per Site and leeched by every machine
being deployed. `README.md` has the user-facing walkthrough; `docs/crd-reference.md`
describes every field.

### Binaries

| Binary | Source | Where it runs |
| --- | --- | --- |
| manager | `cmd/main.go` | the operator Deployment; also hosts the boot and agent HTTP servers as manager `Runnable`s |
| `kezioctl` | `cmd/kezioctl` | operator CLI (image upload, machine enroll, deploy, status) |
| `kezio-agent` | `cmd/agent` | inside the live boot environment on the target machine |
| `kezio-ingest` | `cmd/ingest` | the ImageImport ingest Job and the PartitionContent publish Job |
| `kezio-bootd` | `cmd/bootd` | one Deployment per Subnet: dnsmasq proxyDHCP + in-process TFTP |
| image-service | `cmd/image-service` | upload staging endpoint for `kezioctl image upload` |
| seeder / leechctl | `cmd/seeder`, `cmd/leechctl` | ezio seeder pod / leecher control |

### The deploy path, end to end

1. **Ingest** — `kezioctl image upload` stages a disk image, an `ImageImport` runs the
   ingest Job (`internal/ingest`), which slices every partition with partclone and yields
   one `PartitionContent` per partition plus an immutable `Image`. The `.torrent` is built
   inside the content PVC by the publish Job, never passed through job output.
2. **Net boot** — the machine's BMC (`internal/bmc`, `redfish://` or `ipmi://`) powers it
   on; `kezio-bootd` answers proxyDHCP/TFTP; GRUB fetches its config from
   `internal/bootserver` (`GET /boot/grub.cfg-<mac>`), which mints a single-use boot token.
3. **Register** — `kezio-agent` exchanges that token at `internal/agentserver`
   (`POST /agent/register`) for a session credential and reports inventory as a
   same-name `MachineHardware`.
4. **Deploy** — the agent polls `/agent/next`, gets a plan built by `internal/planbuild`,
   writes the partitions (`internal/agent/deploy`), replays the partition table, runs
   `PostHook`s, sets the UEFI boot entry, and reports steps to `/agent/progress`.
5. **Finish** — the machine is power-cycled into the deployed disk; the seeder stops after
   a grace period once the last deploy of that Image finishes.

### Facts worth knowing before changing a controller

`internal/controller/doc.go` is the authoritative version of this; read it. The short form:

- **Ownership decides watches.** Site owns the tracker Deployment, Subnet owns the bootd
  Deployment, Image owns the seeder Deployment (one per Image×Site), PartitionContent owns
  its PVC and publish Job, ImageImport owns the ingest Job, Machine owns the DeployRun.
  Because a seeder Deployment is owned by the *Image* and not the Site it runs at, any
  other reconciler reacting to it needs an explicit `Watches` with a mapping — `Owns` will
  not see it.
- **A Machine never names its Site.** Always derive it through `internal/sitederive`:
  `Machine.spec.subnetRef` → Subnet → `spec.siteRef` → Site → `spec.seederSubnetRef`. A
  machine's broadcast domain and its seeder's may legitimately differ within one Site, so
  never shortcut by treating the Machine's own Subnet as the seeding one.
- **Foreign objects are never adopted.** A Deployment occupying a managed name without an
  owner reference back (`metav1.IsControlledBy`) is left completely alone — never patched,
  never deleted — and the collision is surfaced in status instead. Since no `Owns` watch
  can see such an object, those reconcilers poll with a bounded `RequeueAfter`.
- **Both HTTP servers are unauthenticated by design** and trust only a presented token's
  hash matching live Machine status, never caller identity or network position. Keep that
  property when touching `internal/bootserver` or `internal/agentserver`.
- **`DEPLOYER` selects hardware access.** Unset or `fake` gives `deployer.FakeDeployer`
  (envtest and the fast e2e lane); `agent` gives `AgentDeployer`, which drives real BMCs.
  Any other value fails startup rather than silently falling back.

### Configuration convention

Every optional subsystem is configured by environment variables parsed in `cmd/main.go`
into a `Config` struct, and each is **inert by default**: an unset image or address does
not error at startup — the reconciler still computes status and explains in a condition
why nothing is happening (`BOOT_SERVER_ADDR`, `AGENT_SERVER_ADDR`, `BOOTD_DEPLOYMENT_IMAGE`,
`TRACKER_DEPLOYMENT_IMAGE`, `IMAGE_INGEST_IMAGE`, `PARTITIONCONTENT_PUBLISH_IMAGE`,
`PARTITIONCONTENT_SEEDER_IMAGE`). Preserve that shape when adding a setting.

### Testing shape

Unit tests sit beside their source; envtest suites are named `*_envtest_test.go` and share
`internal/controller/suite_test.go`. Packages that shell out to external tools hide them
behind small interfaces (`QemuImg`, `Sfdisk`, `Blkid`, `Partclone`, `Attacher` in
`internal/ingest`; `ExecRunner` in `internal/agent`) with the exec-backed implementations
in `cmd/`, so orchestration is testable without the tools installed. Anything reading the
live filesystem takes an explicit root directory instead of hard-coding `/`.
