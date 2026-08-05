# Agent registration server

This kustomization exposes the agent registration server: the HTTP
endpoint kezio-agent talks to once it boots in the live environment
(`internal/agentserver`). Like `config/bootserver`, there is no separate
Deployment - the server runs embedded in the `controller-manager`
process, gated off by default (see `cmd/main.go`'s
`agentServerConfigFromEnv`). This kustomization adds:

- `service.yaml` - a Service in front of `controller-manager`'s own
  pods, on the agent server's port.
- `manager-port-patch.yaml` - declares that port on the manager
  container (`containerPort: 8091`, named `agent-http`), so the
  Service's `targetPort` resolves to something.

It composes `../default` the same way `config/bootserver` does, because
it needs to patch the `controller-manager` Deployment `config/default`
creates:

```sh
kustomize build config/agentserver | kubectl apply -f -
```

## Before applying

1. **Turn the server on.** It is inert until `AGENT_SERVER_ADDR` is set
   on the `controller-manager` Deployment, for example `:8091` (matching
   `manager-port-patch.yaml`'s `containerPort`).
2. **Tell the agent where to find it.** Set `BOOT_AGENT_SERVER_URL` on
   the same Deployment, alongside `config/bootserver`'s `BOOT_SERVER_URL`
   - this server's own externally reachable base URL, for example
   `http://10.0.0.5:8091`, which internal/bootserver embeds as the
   `kezio.server=` cmdline value kezio-agent registers against.
   Leaving it unset falls back to `BOOT_SERVER_URL`, which is only
   correct when this Service and `config/bootserver`'s truly share one
   address - not the case for the shipped manifests, since they are two
   Services on two different container ports (`8090` vs `8091`)
   fronting the same Pod. Getting this wrong sends every registration to
   a server that never mounts this package's routes: the agent's POST
   404s and it retries forever, with nothing surfacing on the
   controller-manager side to explain why the Machine never leaves
   Inspecting.
4. **Turn the real deployer on.** The agent server has nothing to do
   until a Machine reaches the Inspecting state under a Deployer that
   actually waits on it - set `DEPLOYER=agent` on the same Deployment
   (see `cmd/main.go`'s `deployerFactoryFromEnv`). Leaving `DEPLOYER`
   unset keeps the fake deployer, which never talks to this endpoint.
5. **Make the Service reachable from the boot network.** Same
   requirement as `config/bootserver`'s Service: kezio-agent reaches it
   directly from the target machine's network, so a bare `ClusterIP` is
   not enough outside a single-cluster test. This Service and the boot
   config server's are two different Services on two different
   container ports fronting the same Pod, so each needs its own
   reachability path patched in (a NodePort, a LoadBalancer, hostNetwork,
   or a reverse proxy on the boot network) - whatever address this one
   ends up reachable at is what `BOOT_AGENT_SERVER_URL` (step 2) must
   name.
