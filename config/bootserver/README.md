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
     Mounting a volume there is left to the cluster operator (the
     artifact build and its placement are a separate concern from this
     server).
   - `BOOT_SERVER_URL` - this server's own externally reachable base
     URL, for example `http://10.0.0.5:8090`. GRUB and the live agent
     both fetch from it directly from the boot network, so it must name
     wherever this Service (or whatever the operator fronts it with -
     see `service.yaml`'s comment) is actually reachable from there, not
     the Service's in-cluster ClusterIP.
   - `BOOT_KERNEL_PATH` / `BOOT_INITRD_PATH` - optional, override the
     artifact file names under `BOOT_ARTIFACTS_DIR` (default `vmlinuz` /
     `initrd.img`).
   - `BOOT_TOKEN_TTL` - optional, overrides how long a minted boot token
     is accepted (a Go duration string, for example `30m`).
2. **Make the Service reachable from the boot network.** `service.yaml`
   ships as `ClusterIP`, the safe default; GRUB running on firmware
   cannot reach a ClusterIP from outside the cluster network. Patch it
   in a further overlay (NodePort, a LoadBalancer, hostNetwork, or a
   reverse proxy on the boot network) to match however `kezio-bootd`'s
   segment actually reaches the cluster, and set `BOOT_SERVER_URL`
   accordingly.
