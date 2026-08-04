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
2. **Turn the real deployer on.** The agent server has nothing to do
   until a Machine reaches the Inspecting state under a Deployer that
   actually waits on it - set `DEPLOYER=agent` on the same Deployment
   (see `cmd/main.go`'s `deployerFactoryFromEnv`). Leaving `DEPLOYER`
   unset keeps the fake deployer, which never talks to this endpoint.
3. **Make the Service reachable from the boot network.** Same
   requirement as `config/bootserver`'s Service: kezio-agent reaches it
   directly from the target machine's network, so a bare `ClusterIP` is
   not enough outside a single-cluster test. In production this is
   ordinarily the same reachable address as the boot config server
   (`BOOT_SERVER_URL`), since one `controller-manager` process serves
   both.
