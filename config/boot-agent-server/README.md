# Boot config and agent registration servers

This kustomization exposes the two HTTP servers a machine talks to while
it deploys:

- the **boot config server** (`internal/bootserver`) - what a
  network-booting firmware and its GRUB loader fetch from before an
  agent ever runs;
- the **agent registration server** (`internal/agentserver`) - what
  kezio-agent registers with once it boots in the live environment.

Unlike `config/image-service` and `config/seeder`, neither has a
Deployment of its own: both run embedded in the `controller-manager`
process, each gated off by default (`cmd/main.go`'s
`bootServerConfigFromEnv` and `agentServerConfigFromEnv`). This
kustomization composes `../default` and adds, through the components in
`config/components`:

- a Service in front of `controller-manager`'s own pods for each server,
  and the matching `containerPort` on the manager container
  (`boot-http` 8090, `agent-http` 8091), so each Service's `targetPort`
  resolves to something;
- a `fetch-boot-artifacts` initContainer that populates the directory
  the boot config server serves the live kernel, initrd and squashfs
  from.

```sh
kustomize build config/boot-agent-server | kubectl apply -f -
```

Apply this instead of, not beside, `config/default`.

**Both servers or neither.** There is no supported shape that turns on
one alone: a machine that never net boots never registers, and a machine
that registers with nothing listening stays in Inspecting forever. That
is why these are one kustomization and one README rather than two of
each.

## Before applying

1. **Turn both servers on.** They are inert until their listen
   addresses are set on the `controller-manager` Deployment:
   - `BOOT_SERVER_ADDR` - for example `:8090`, matching the
     `boot-http` containerPort.
   - `AGENT_SERVER_ADDR` - for example `:8091`, matching `agent-http`.
2. **Point the artifacts directory at the initContainer's volume.**
   `BOOT_ARTIFACTS_DIR` is the local directory `GET
   /boot/artifacts/...` serves from. This kustomization already sets it
   to `/boot-artifacts` and mounts an emptyDir there, populated at pod
   startup by `fetch-boot-artifacts`, which `cp`s the files straight out
   of the `kezio-boot-artifacts` OCI image (`.github/workflows/release.yaml`'s
   `boot-artifacts` job builds and pushes it). No manual volume setup is
   needed. By default it pulls the image's `latest` tag; pin a published
   version from this directory with

   ```sh
   kustomize edit set image \
     ghcr.io/tjjh89017/kezio-boot-artifacts=ghcr.io/tjjh89017/kezio-boot-artifacts:0.1.8
   ```

   Name the rewritten image, not `kezio-boot-artifacts`: the component
   in `config/components/boot-artifacts` has already renamed it by the
   time this kustomization's own image transformer runs. The image tag
   has no "v", although the git tag it comes from does.
3. **Set both externally reachable URLs, and set them separately.**
   - `BOOT_SERVER_URL` - the boot config server's own reachable base
     URL. GRUB and the live environment's initrd fetch the kernel,
     initrd and squashfs from it directly from the boot network.
   - `BOOT_AGENT_SERVER_URL` - the agent registration server's own
     reachable base URL, which `internal/bootserver` embeds as the
     `kezio.server=` cmdline value kezio-agent registers against.

   Neither may be a ClusterIP; see step 5. Leaving
   `BOOT_AGENT_SERVER_URL` unset falls back to `BOOT_SERVER_URL`, which
   is only correct if both truly share one address - not the case for
   the shipped manifests, since they are two Services on two different
   container ports fronting the same Pod. Getting this wrong sends every
   registration to a server that never mounts the agent routes: the
   agent's POST 404s and it retries forever, with nothing on the
   controller-manager side to explain why the Machine never leaves
   Inspecting.
4. **Turn the real deployer on.** The agent server has nothing to do
   until a Machine reaches Inspecting under a Deployer that waits on it
   - set `DEPLOYER=agent` (`cmd/main.go`'s `deployerFactoryFromEnv`).
   Leaving `DEPLOYER` unset keeps the fake deployer, which never talks
   to this endpoint.
5. **Make both servers reachable from the boot network - front them
   with kezio-bootd.** Both Services ship as `ClusterIP`, the safe
   default, and firmware cannot reach a ClusterIP from outside the
   cluster network. The prescribed way to close that gap is
   `config/bootd`'s reverse proxy. On `kezio-bootd`'s Deployment set:
   - `BOOTD_BOOT_UPSTREAM_URL` to the boot server Service's cluster-DNS
     name, for example
     `http://kezio-boot-server.kezio-system.svc.cluster.local:8090`;
   - `BOOTD_AGENT_UPSTREAM_URL` to the agent server Service's, for
     example
     `http://kezio-agent-server.kezio-system.svc.cluster.local:8091`.

   Use the DNS names, not the ClusterIP literals, which change if a
   Service is ever recreated. Then set both `BOOT_SERVER_URL` and
   `BOOT_AGENT_SERVER_URL` to **bootd's own** address on the boot
   segment - the same address for both, since one bootd instance
   proxies both by route prefix. bootd is already a Multus-attached pod
   sitting directly on that segment (`config/bootd/README.md`'s "Why
   Multus, not hostNetwork"), so it needs no further exposure of its
   own. A production deployment with one provisioning VLAN per site runs
   one bootd per site, each proxying to these same cluster-wide
   Services - see `config/bootd/README.md`'s "Per-site addressing".

   A site not using bootd's proxy can patch the Services directly
   instead (NodePort, LoadBalancer, hostNetwork, or its own reverse
   proxy) and point the two URLs at those addresses; bootd's proxy is
   the default path, not the only one.

## Optional boot config server settings

- `BOOT_KERNEL_PATH` / `BOOT_INITRD_PATH` - override the artifact file
  names under `BOOT_ARTIFACTS_DIR` (default `vmlinuz` / `initrd.img`).
- `BOOT_TOKEN_TTL` - how long a minted boot token stays accepted, as a
  Go duration string, for example `30m`.
- `BOOT_EFI_DIR` - the directory `GET /boot/http/<name>` serves the
  signed `shimx64.efi`/`grubx64.efi` from, for UEFI HTTP Boot (see
  `config/bootd/README.md`). Empty means `BOOT_ARTIFACTS_DIR`, and the
  initContainer already fetches both binaries into that same directory,
  so this route works with no further overlay.
