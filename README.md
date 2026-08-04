# kezio

Kubernetes operator that deploys bare-metal machines with EZIO (BitTorrent) and partclone.

## Prerequisites

- [Go](https://go.dev/) 1.24+
- [operator-sdk](https://sdk.operatorframework.io/) v1.42+
- Access to a Kubernetes cluster (e.g. via `kubectl`) for `deploy`/`run` targets

## Build

```sh
make build          # compile the manager binary into bin/
make manifests       # generate CRD/RBAC/webhook manifests
make generate         # generate DeepCopy/runtime.Object code
make test            # run unit tests
make lint             # run golangci-lint
make docker-build     # build the manager container image
```

See `make help` for the full list of targets.

## Documentation

- [`docs/secure-boot.md`](docs/secure-boot.md): the UEFI Secure Boot
  signature chain the network boot path relies on, the kernel-signing
  story, and what CI does and does not verify.
- [`docs/e2e-kubevirt-reusable.md`](docs/e2e-kubevirt-reusable.md): how
  to reuse kezio's KubeVirt BT-transfer e2e test as a `workflow_call`
  from another repository (the ezio repository in particular), including
  the input list and defaults.

## License

Apache-2.0. See [LICENSE](LICENSE).
