# Boot config server

This kustomization exposes the boot config server: the HTTP endpoints a
network-booting firmware and its GRUB loader talk to before an agent
ever runs (`internal/bootserver`). Unlike `config/image-service` and
`config/seeder`, there is no separate Deployment - the server runs
embedded in the `controller-manager` process, gated off by default (see
`cmd/main.go`'s `bootServerConfigFromEnv`). This kustomization adds:

- `service.yaml` - a Service in front of `controller-manager`'s own
  pods, on the boot server's port.
- `manager-port-patch.yaml` - declares that port on the manager
  container (`containerPort: 8090`, named `boot-http`), so the Service's
  `targetPort` resolves to something.

It composes `../default` (rather than being applied alongside it, the
way `config/image-service` and `config/seeder` are) because it needs to
patch the `controller-manager` Deployment `config/default` creates:

```sh
kustomize build config/bootserver | kubectl apply -f -
```

## Before applying

1. **Turn the server on.** It is inert until `BOOT_SERVER_ADDR` is set
   on the `controller-manager` Deployment (see `cmd/main.go`'s
   `bootServerConfigFromEnv` doc comment for the full list):
   - `BOOT_SERVER_ADDR` - the listen address, for example `:8090`
     (matching `manager-port-patch.yaml`'s `containerPort`).
   - `BOOT_ARTIFACTS_DIR` - the local directory `GET
     /boot/artifacts/...` serves the live kernel/initrd/squashfs from.
     This kustomization already sets it to `/boot-artifacts` and mounts
     an emptyDir there, populated at pod startup by a
     `fetch-boot-artifacts` initContainer that `cp`s the files straight
     out of the `kezio-boot-artifacts` OCI image (see
     `.github/workflows/build-live-image.yml`, which builds and pushes
     it, and `boot-artifacts-init-patch.yaml`) - no manual volume setup
     needed. By default it pulls the image's `latest` tag; pin a
     specific published version with `kustomize edit set image
     kezio-boot-artifacts=ghcr.io/tjjh89017/kezio-boot-artifacts:v0.1.0`
     (or a further kustomize patch) instead.
   - `BOOT_SERVER_URL` - this server's own externally reachable base
     URL, for example `http://10.0.0.5:8090`. GRUB and the live
     environment's initrd both fetch the kernel/initrd/squashfs from it
     directly from the boot network, so it must name wherever this
     Service (or whatever the operator fronts it with - see
     `service.yaml`'s comment) is actually reachable from there, not the
     Service's in-cluster ClusterIP. It is *not* where the agent
     registers - see `BOOT_AGENT_SERVER_URL` in `config/agentserver`'s
     README.
   - `BOOT_KERNEL_PATH` / `BOOT_INITRD_PATH` - optional, override the
     artifact file names under `BOOT_ARTIFACTS_DIR` (default `vmlinuz` /
     `initrd.img`).
   - `BOOT_TOKEN_TTL` - optional, overrides how long a minted boot token
     is accepted (a Go duration string, for example `30m`).
   - `BOOT_EFI_DIR` - optional, overrides the directory `GET
     /boot/http/<name>` serves the signed `shimx64.efi`/`grubx64.efi`
     from, for UEFI HTTP Boot (see `config/bootd/README.md`'s UEFI HTTP
     Boot section). Empty means `BOOT_ARTIFACTS_DIR` -
     `boot-artifacts-init-patch.yaml` already fetches both binaries into
     that same directory, so this route works with no further overlay
     either.
2. **Make the server reachable from the boot network - front it with
   kezio-bootd.** `service.yaml` ships as `ClusterIP`, the safe default;
   GRUB running on firmware cannot reach a ClusterIP from outside the
   cluster network. The prescribed way to close that gap is
   `config/bootd`'s reverse proxy: set `BOOTD_BOOT_UPSTREAM_URL` on
   `kezio-bootd`'s Deployment to this Service's in-cluster URL (its
   cluster-DNS name, for example
   `http://kezio-boot-server.kezio-system.svc.cluster.local:8090` - not
   its ClusterIP literal, which changes if the Service is ever
   recreated), and set `BOOT_SERVER_URL` here to **bootd's own** address
   on the boot segment instead of this Service's. bootd is already a
   Multus-attached pod sitting directly on that segment (see
   `config/bootd/README.md`'s "Why Multus, not hostNetwork" reasoning),
   so it needs no further exposure of its own; every `/boot/...` request
   it receives reverse-proxies straight to this Service. A production
   deployment with one provisioning VLAN per site runs one bootd
   instance per site this way, each proxying to this same cluster-wide
   Service - see `config/bootd/README.md`'s "Per-site addressing"
   section for the multisite shape. A site not using bootd's proxy can
   still patch this Service directly instead (NodePort, a LoadBalancer,
   hostNetwork) and set `BOOT_SERVER_URL` to that address; bootd's proxy
   is the default path, not the only one.
