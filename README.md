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

## License

Apache-2.0. See [LICENSE](LICENSE).
