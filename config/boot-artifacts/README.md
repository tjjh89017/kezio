# Boot artifacts fetch script (shared Component)

This is a Kustomize [Component](https://kubectl.docs.kubernetes.io/guides/config_management/components/),
not a deployable kustomization on its own. It contributes exactly one
thing - a `boot-artifacts-fetch-script` ConfigMap holding `fetch.sh` -
for `config/bootserver` and `config/bootd` to each mount into their own
`fetch-boot-artifacts` initContainer:

- `config/bootserver` fetches `vmlinuz`, `initrd.img`,
  `filesystem.squashfs`, and `manifest.json` into the volume
  `BOOT_ARTIFACTS_DIR` points at.
- `config/bootd` fetches `shimx64.efi` and `grubx64.efi` into its TFTP
  volume.

Both sets come from the same place: a GitHub Release published by
`.github/workflows/build-live-image.yml` (see that workflow's own
comment for why a Release asset, not a ghcr OCI artifact, is this
project's publishing target). `fetch.sh` downloads the release's
`sha256sums` asset alongside whatever files it was told to fetch
(`BOOT_ARTIFACTS_FILES`) and verifies each one before the initContainer
exits successfully - a checksum mismatch fails the fetch, keeping a
corrupted or truncated download from ever reaching the main container.

## Configuration

Every knob is an environment variable, set by whichever initContainer
patch mounts this script (`config/bootserver/boot-artifacts-init-
patch.yaml`, `config/bootd/deployment.yaml`) and overridable per-
Deployment with `kubectl set env <deployment> -c fetch-boot-artifacts
<VAR>=<value>` or a further kustomize patch:

| Variable                | Meaning                                                      |
| ------------------------ | ------------------------------------------------------------ |
| `BOOT_ARTIFACTS_REPO`    | `owner/repo` to fetch releases from. Defaults to `tjjh89017/kezio` in both consumers. |
| `BOOT_ARTIFACTS_VERSION` | A release tag (`v0.1.0`), or the literal `latest`. Both consumers default to `latest`. |
| `BOOT_ARTIFACTS_FILES`   | Space-separated release asset names to fetch (excluding `sha256sums`, which is always fetched). |
| `DEST_DIR`               | Directory to fetch into - the mount path of the volume the main container also mounts. |

Publishing a new release is a maintainer action (push a `v*` tag, or
dispatch `build-live-image.yml` with `publish: true`); this Component
only ever consumes what has already been published, never triggers a
build itself.
