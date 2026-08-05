# VERSION defines the project version for the bundle.
# Update this value when you upgrade the version of your project.
# To re-generate a bundle for another specific version without changing the standard setup, you can:
# - use the VERSION as arg of the bundle target (e.g make bundle VERSION=0.0.2)
# - use environment variables to overwrite this value (e.g export VERSION=0.0.2)
VERSION ?= 0.0.1

# CHANNELS define the bundle channels used in the bundle.
# Add a new line here if you would like to change its default config. (E.g CHANNELS = "candidate,fast,stable")
# To re-generate a bundle for other specific channels without changing the standard setup, you can:
# - use the CHANNELS as arg of the bundle target (e.g make bundle CHANNELS=candidate,fast,stable)
# - use environment variables to overwrite this value (e.g export CHANNELS="candidate,fast,stable")
ifneq ($(origin CHANNELS), undefined)
BUNDLE_CHANNELS := --channels=$(CHANNELS)
endif

# DEFAULT_CHANNEL defines the default channel used in the bundle.
# Add a new line here if you would like to change its default config. (E.g DEFAULT_CHANNEL = "stable")
# To re-generate a bundle for any other default channel without changing the default setup, you can:
# - use the DEFAULT_CHANNEL as arg of the bundle target (e.g make bundle DEFAULT_CHANNEL=stable)
# - use environment variables to overwrite this value (e.g export DEFAULT_CHANNEL="stable")
ifneq ($(origin DEFAULT_CHANNEL), undefined)
BUNDLE_DEFAULT_CHANNEL := --default-channel=$(DEFAULT_CHANNEL)
endif
BUNDLE_METADATA_OPTS ?= $(BUNDLE_CHANNELS) $(BUNDLE_DEFAULT_CHANNEL)

# IMAGE_TAG_BASE defines the docker.io namespace and part of the image name for remote images.
# This variable is used to construct full image tags for bundle and catalog images.
#
# For example, running 'make bundle-build bundle-push catalog-build catalog-push' will build and push both
# kojuro.date/kezio-bundle:$VERSION and kojuro.date/kezio-catalog:$VERSION.
IMAGE_TAG_BASE ?= ghcr.io/tjjh89017/kezio

# BUNDLE_IMG defines the image:tag used for the bundle.
# You can use it as an arg. (E.g make bundle-build BUNDLE_IMG=<some-registry>/<project-name-bundle>:<tag>)
BUNDLE_IMG ?= $(IMAGE_TAG_BASE)-bundle:v$(VERSION)

# BUNDLE_GEN_FLAGS are the flags passed to the operator-sdk generate bundle command
BUNDLE_GEN_FLAGS ?= -q --overwrite --version $(VERSION) $(BUNDLE_METADATA_OPTS)

# USE_IMAGE_DIGESTS defines if images are resolved via tags or digests
# You can enable this value if you would like to use SHA Based Digests
# To enable set flag to true
USE_IMAGE_DIGESTS ?= false
ifeq ($(USE_IMAGE_DIGESTS), true)
	BUNDLE_GEN_FLAGS += --use-image-digests
endif

# Set the Operator SDK version to use. By default, what is installed on the system is used.
# This is useful for CI or a project to utilize a specific version of the operator-sdk toolkit.
OPERATOR_SDK_VERSION ?= v1.42.3
# Image URL to use all building/pushing image targets
IMG ?= controller:latest

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

# GEN_PATHS lists the Go package roots that controller-gen scans. It is kept
# explicit (not "./...") because the repo tree can contain other Go modules
# (each with its own go.mod) that controller-gen's directory walk does not
# skip the way `go build ./...` does. Extend this list as new package roots
# are added.
GEN_PATHS ?= ./cmd/... ./api/... ./internal/...

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd webhook $(addprefix paths=,$(GEN_PATHS)) output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" $(addprefix paths=,$(GEN_PATHS))

.PHONY: proto
proto: buf protoc-gen-go protoc-gen-go-grpc ## Regenerate the seeder gRPC client stubs from proto/ezio.proto into internal/seeder/ezioapi.
	PATH="$(LOCALBIN):$$PATH" $(BUF) generate

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet setup-envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

KIND_CLUSTER ?= kezio-test-e2e

# E2E_CLUSTER selects the cluster flavor the e2e suite targets. The default
# "kind" creates and deletes a throwaway Kind cluster locally. Set
# E2E_CLUSTER=k3s to run against a pre-provisioned single-node k3s cluster
# (the k3s CI workflow stands one up and sets KUBECONFIG itself): setup and
# cleanup become no-ops that leave the external cluster intact, and the
# manager image is imported into the node's containerd instead of `kind
# load`.
E2E_CLUSTER ?= kind

.PHONY: setup-test-e2e
setup-test-e2e: ## Set up a Kind cluster for e2e tests if it does not exist (skipped when E2E_CLUSTER!=kind)
ifeq ($(E2E_CLUSTER),kind)
	@command -v $(KIND) >/dev/null 2>&1 || { \
		echo "Kind is not installed. Please install Kind manually."; \
		exit 1; \
	}
	@case "$$($(KIND) get clusters)" in \
		*"$(KIND_CLUSTER)"*) \
			echo "Kind cluster '$(KIND_CLUSTER)' already exists. Skipping creation." ;; \
		*) \
			echo "Creating Kind cluster '$(KIND_CLUSTER)'..."; \
			$(KIND) create cluster --name $(KIND_CLUSTER) ;; \
	esac
else
	@echo "E2E_CLUSTER=$(E2E_CLUSTER): using pre-provisioned cluster, skipping Kind creation."
	@$(KUBECTL) cluster-info >/dev/null 2>&1 || { \
		echo "No reachable cluster: set KUBECONFIG to the pre-provisioned $(E2E_CLUSTER) cluster."; \
		exit 1; \
	}
endif

.PHONY: test-e2e
test-e2e: setup-test-e2e manifests generate fmt vet ## Run the e2e tests. Expected an isolated Kind environment, or a pre-provisioned cluster via E2E_CLUSTER.
	KIND_CLUSTER=$(KIND_CLUSTER) E2E_CLUSTER=$(E2E_CLUSTER) go test ./test/e2e/ -v -ginkgo.v -timeout 15m
	$(MAKE) cleanup-test-e2e E2E_CLUSTER=$(E2E_CLUSTER)

.PHONY: cleanup-test-e2e
cleanup-test-e2e: ## Tear down the Kind cluster used for e2e tests (skipped when E2E_CLUSTER!=kind)
ifeq ($(E2E_CLUSTER),kind)
	@$(KIND) delete cluster --name $(KIND_CLUSTER)
else
	@echo "E2E_CLUSTER=$(E2E_CLUSTER): pre-provisioned cluster left intact."
endif

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	$(GOLANGCI_LINT) run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	$(GOLANGCI_LINT) run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	$(GOLANGCI_LINT) config verify

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -o bin/manager cmd/main.go

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go

.PHONY: build-image-service
build-image-service: fmt vet ## Build the image-service binary (server side of `kezioctl image upload`).
	go build -o bin/image-service ./cmd/image-service

.PHONY: run-image-service
run-image-service: fmt vet ## Run the image-service from your host.
	go run ./cmd/image-service

# IMAGE_SERVICE_IMG is the image tag for the image-service binary. It is a
# separate image from IMG (the controller manager) because the two ship
# and deploy independently; see Dockerfile.image-service and
# config/image-service.
IMAGE_SERVICE_IMG ?= $(IMAGE_TAG_BASE)-image-service:latest

.PHONY: docker-build-image-service
docker-build-image-service: ## Build docker image for image-service.
	$(CONTAINER_TOOL) build -t ${IMAGE_SERVICE_IMG} -f Dockerfile.image-service .

.PHONY: docker-push-image-service
docker-push-image-service: ## Push docker image for image-service.
	$(CONTAINER_TOOL) push ${IMAGE_SERVICE_IMG}

.PHONY: build-ingest
build-ingest: fmt vet ## Build the kezio-ingest binary (runs inside the Image reconciler's ingest Job).
	go build -o bin/kezio-ingest ./cmd/ingest

# INGEST_IMG is the image tag for kezio-ingest. It is a separate image
# from IMG (the controller manager) and IMAGE_SERVICE_IMG because it runs
# as a Job pod (not a Deployment) and needs partclone/qemu-img/sfdisk/
# blkid installed; see Dockerfile.ingest.
INGEST_IMG ?= $(IMAGE_TAG_BASE)-ingest:latest

.PHONY: docker-build-ingest
docker-build-ingest: ## Build docker image for kezio-ingest.
	$(CONTAINER_TOOL) build -t ${INGEST_IMG} -f Dockerfile.ingest .

.PHONY: docker-push-ingest
docker-push-ingest: ## Push docker image for kezio-ingest.
	$(CONTAINER_TOOL) push ${INGEST_IMG}

# SEEDER_IMG is the image tag for the ezio seeder container (Dockerfile.seeder).
# It ships no kezio Go binary (see Dockerfile.seeder's header comment), so
# there is no matching "build-seeder" Go-build target, only the docker one.
SEEDER_IMG ?= $(IMAGE_TAG_BASE)-seeder:latest

.PHONY: docker-build-seeder
docker-build-seeder: ## Build docker image for the ezio seeder.
	$(CONTAINER_TOOL) build -t ${SEEDER_IMG} -f Dockerfile.seeder .

.PHONY: docker-push-seeder
docker-push-seeder: ## Push docker image for the ezio seeder.
	$(CONTAINER_TOOL) push ${SEEDER_IMG}

.PHONY: build-bootd
build-bootd: fmt vet ## Build the kezio-bootd binary (proxyDHCP/PXE/TFTP, see internal/bootd).
	go build -o bin/bootd ./cmd/bootd

# BOOTD_IMG is the image tag for kezio-bootd. It is a separate image
# from IMG (the controller manager) because bootd is not part of the
# manager process - it is its own per-site binary needing privileged UDP
# ports; see Dockerfile.bootd and config/bootd.
BOOTD_IMG ?= $(IMAGE_TAG_BASE)-bootd:latest

.PHONY: docker-build-bootd
docker-build-bootd: ## Build docker image for kezio-bootd.
	$(CONTAINER_TOOL) build -t ${BOOTD_IMG} -f Dockerfile.bootd .

.PHONY: docker-push-bootd
docker-push-bootd: ## Push docker image for kezio-bootd.
	$(CONTAINER_TOOL) push ${BOOTD_IMG}

.PHONY: build-kezioctl
build-kezioctl: fmt vet ## Build the kezioctl binary (the operator-side CLI client; no container image, it runs on an operator's workstation).
	go build -o bin/kezioctl ./cmd/kezioctl

# build-agent is for local development only (a native-arch build to run
# internal/agent's own tests or poke at the binary by hand). The live
# image itself never uses this target - hack/live-image/build.sh cross-
# compiles cmd/agent to linux/amd64 in its own containerized step
# (build_agent), the same way it resolves the ezio binary, so the live
# image build has no dependency on the host's Go toolchain or arch.
.PHONY: build-agent
build-agent: fmt vet ## Build the kezio-agent binary for the host's own OS/arch (see hack/live-image/build.sh for the live-image cross-compile).
	go build -o bin/agent ./cmd/agent

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

# IMG_IPMI is the image tag for the opt-in, ipmitool-enabled manager
# build (Dockerfile.manager-ipmi). It is a separate tag from IMG because
# most deployments should stay on the smaller, dependency-free default
# manager image; only use this build if you have ipmi:// BMCs (see
# internal/bmc/ipmi's package doc comment and the README).
IMG_IPMI ?= $(IMAGE_TAG_BASE)-ipmi:latest

.PHONY: docker-build-manager-ipmi
docker-build-manager-ipmi: ## Build the opt-in ipmitool-enabled manager image (for ipmi:// BMCs).
	$(CONTAINER_TOOL) build -t ${IMG_IPMI} -f Dockerfile.manager-ipmi .

.PHONY: docker-push-manager-ipmi
docker-push-manager-ipmi: ## Push the opt-in ipmitool-enabled manager image.
	$(CONTAINER_TOOL) push ${IMG_IPMI}

# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image to your registry (i.e. if you do not set a valid value via IMG=<myregistry/image:<tag>> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	# Dockerfile already declares --platform=$$BUILDPLATFORM on its builder stage, so build it directly
	- $(CONTAINER_TOOL) buildx create --name kezio-builder
	$(CONTAINER_TOOL) buildx use kezio-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --tag ${IMG} -f Dockerfile .
	- $(CONTAINER_TOOL) buildx rm kezio-builder

.PHONY: build-installer
build-installer: manifests generate kustomize ## Generate a consolidated YAML with CRDs and deployment.
	mkdir -p dist
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/default > dist/install.yaml

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) apply -f -

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/default | $(KUBECTL) apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/default | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

# deploy-image-path applies config/e2e-image-path (config/default plus
# config/image-service and config/seeder, plus a store PVC and a store
# mount on the manager - see that overlay's README-style comment) and
# then wires INGEST_IMAGE/SEEDER_* onto the already-deployed
# controller-manager with `kubectl set env`, since those values are
# per-run image tags kustomize's static YAML has no way to express (see
# config/e2e-image-path/kustomization.yaml). Used only by the e2e
# image-path stage (test/e2e's "Image ingest and seeding" Context); run
# `deploy` first so config/default's CRDs/RBAC/webhook/manager exist to
# add these resources on top of.
.PHONY: deploy-image-path
deploy-image-path: manifests kustomize ## Deploy image-service+seeder+store and wire real ingest/seeding onto the controller-manager (e2e image-path stage only).
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	cd config/image-service && $(KUSTOMIZE) edit set image image-service=${IMAGE_SERVICE_IMG}
	cd config/seeder && $(KUSTOMIZE) edit set image seeder=${SEEDER_IMG}
	$(KUSTOMIZE) build config/e2e-image-path | $(KUBECTL) apply -f -
	$(KUBECTL) -n kezio-system set env deployment/kezio-controller-manager \
		INGEST_IMAGE=${INGEST_IMG} \
		INGEST_STORE_PVC=kezio-store \
		INGEST_STAGING_PVC=kezio-image-service-staging \
		SEEDER_TRACKER_URL=http://kezio-opentracker.kezio-system.svc.cluster.local:6969/announce \
		SEEDER_STORE_ROOT=/store \
		SEEDER_SERVICE_NAMESPACE=kezio-system \
		SEEDER_SERVICE_NAME=kezio-ezio-seeder

# deploy-boot-path applies config/e2e-boot-path (config/default plus the
# boot config server's and agent registration server's Services - see
# that overlay's README-style comment) and then wires BOOT_SERVER_ADDR/
# BOOT_ARTIFACTS_DIR/BOOT_SERVER_URL/AGENT_SERVER_ADDR/DEPLOYER onto the
# already-deployed controller-manager with `kubectl set env`, mirroring
# deploy-image-path's shape. Used only by the e2e boot-path stage
# (test/e2e's "Boot path (control-plane wiring)" Context); run `deploy`
# first so config/default's CRDs/RBAC/webhook/manager exist to add these
# resources on top of.
#
# BOOT_ARTIFACTS_DIR points at a directory this stage never populates:
# the control-plane-wiring test only exercises GET /boot/grub.cfg-<mac>
# and POST /agent/register, never GET /boot/artifacts/..., so no real
# kernel/initrd/squashfs need to exist on the manager container's file
# system for this stage to pass (see internal/bootserver.artifactsHandler,
# which only stats a file when a request for it actually arrives).
.PHONY: deploy-boot-path
deploy-boot-path: manifests kustomize ## Deploy the boot config + agent registration Services and wire DEPLOYER=agent onto the controller-manager (e2e boot-path stage only).
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/e2e-boot-path | $(KUBECTL) apply -f -
	$(KUBECTL) -n kezio-system set env deployment/kezio-controller-manager \
		BOOT_SERVER_ADDR=:8090 \
		BOOT_ARTIFACTS_DIR=/tmp \
		BOOT_SERVER_URL=http://kezio-boot-server.kezio-system.svc.cluster.local:8090 \
		AGENT_SERVER_ADDR=:8091 \
		DEPLOYER=agent

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
KUBECTL ?= kubectl
KIND ?= kind
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint
BUF ?= $(LOCALBIN)/buf
PROTOC_GEN_GO ?= $(LOCALBIN)/protoc-gen-go
PROTOC_GEN_GO_GRPC ?= $(LOCALBIN)/protoc-gen-go-grpc

## Tool Versions
KUSTOMIZE_VERSION ?= v5.6.0
CONTROLLER_TOOLS_VERSION ?= v0.18.0
# BUF_VERSION and the two protoc-gen-go* versions are only used by `make
# proto` (proto/ezio.proto -> internal/seeder/ezioapi). buf bundles its own
# protobuf compiler (no system protoc needed); protoc-gen-go and
# protoc-gen-go-grpc are pinned close to the google.golang.org/protobuf and
# google.golang.org/grpc versions already in go.mod so generated code
# matches what the module actually vendors.
BUF_VERSION ?= v1.47.2
PROTOC_GEN_GO_VERSION ?= v1.36.5
PROTOC_GEN_GO_GRPC_VERSION ?= v1.5.1
#ENVTEST_VERSION is the version of controller-runtime release branch to fetch the envtest setup script (i.e. release-0.20)
ENVTEST_VERSION ?= $(shell go list -m -f "{{ .Version }}" sigs.k8s.io/controller-runtime | awk -F'[v.]' '{printf "release-%d.%d", $$2, $$3}')
#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell go list -m -f "{{ .Version }}" k8s.io/api | awk -F'[v.]' '{printf "1.%d", $$3}')
GOLANGCI_LINT_VERSION ?= v2.12.2

.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: setup-envtest
setup-envtest: envtest ## Download the binaries required for ENVTEST in the local bin directory.
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@$(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

.PHONY: buf
buf: $(BUF) ## Download buf locally if necessary.
$(BUF): $(LOCALBIN)
	$(call go-install-tool,$(BUF),github.com/bufbuild/buf/cmd/buf,$(BUF_VERSION))

.PHONY: protoc-gen-go
protoc-gen-go: $(PROTOC_GEN_GO) ## Download protoc-gen-go locally if necessary.
$(PROTOC_GEN_GO): $(LOCALBIN)
	$(call go-install-tool,$(PROTOC_GEN_GO),google.golang.org/protobuf/cmd/protoc-gen-go,$(PROTOC_GEN_GO_VERSION))

.PHONY: protoc-gen-go-grpc
protoc-gen-go-grpc: $(PROTOC_GEN_GO_GRPC) ## Download protoc-gen-go-grpc locally if necessary.
$(PROTOC_GEN_GO_GRPC): $(LOCALBIN)
	$(call go-install-tool,$(PROTOC_GEN_GO_GRPC),google.golang.org/grpc/cmd/protoc-gen-go-grpc,$(PROTOC_GEN_GO_GRPC_VERSION))

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f $(1) || true ;\
GOBIN=$(LOCALBIN) go install $${package} ;\
mv $(1) $(1)-$(3) ;\
} ;\
ln -sf $(1)-$(3) $(1)
endef

.PHONY: operator-sdk
OPERATOR_SDK ?= $(LOCALBIN)/operator-sdk
operator-sdk: ## Download operator-sdk locally if necessary.
ifeq (,$(wildcard $(OPERATOR_SDK)))
ifeq (, $(shell which operator-sdk 2>/dev/null))
	@{ \
	set -e ;\
	mkdir -p $(dir $(OPERATOR_SDK)) ;\
	OS=$(shell go env GOOS) && ARCH=$(shell go env GOARCH) && \
	curl -sSLo $(OPERATOR_SDK) https://github.com/operator-framework/operator-sdk/releases/download/$(OPERATOR_SDK_VERSION)/operator-sdk_$${OS}_$${ARCH} ;\
	chmod +x $(OPERATOR_SDK) ;\
	}
else
OPERATOR_SDK = $(shell which operator-sdk)
endif
endif

.PHONY: bundle
bundle: manifests kustomize operator-sdk ## Generate bundle manifests and metadata, then validate generated files.
	$(OPERATOR_SDK) generate kustomize manifests -q
	cd config/manager && $(KUSTOMIZE) edit set image controller=$(IMG)
	$(KUSTOMIZE) build config/manifests | $(OPERATOR_SDK) generate bundle $(BUNDLE_GEN_FLAGS)
	$(OPERATOR_SDK) bundle validate ./bundle

.PHONY: bundle-build
bundle-build: ## Build the bundle image.
	$(CONTAINER_TOOL) build -f bundle.Dockerfile -t $(BUNDLE_IMG) .

.PHONY: bundle-push
bundle-push: ## Push the bundle image.
	$(MAKE) docker-push IMG=$(BUNDLE_IMG)

.PHONY: opm
OPM = $(LOCALBIN)/opm
opm: ## Download opm locally if necessary.
ifeq (,$(wildcard $(OPM)))
ifeq (,$(shell which opm 2>/dev/null))
	@{ \
	set -e ;\
	mkdir -p $(dir $(OPM)) ;\
	OS=$(shell go env GOOS) && ARCH=$(shell go env GOARCH) && \
	curl -sSLo $(OPM) https://github.com/operator-framework/operator-registry/releases/download/v1.55.0/$${OS}-$${ARCH}-opm ;\
	chmod +x $(OPM) ;\
	}
else
OPM = $(shell which opm)
endif
endif

# A comma-separated list of bundle images (e.g. make catalog-build BUNDLE_IMGS=example.com/operator-bundle:v0.1.0,example.com/operator-bundle:v0.2.0).
# These images MUST exist in a registry and be pull-able.
BUNDLE_IMGS ?= $(BUNDLE_IMG)

# The image tag given to the resulting catalog image (e.g. make catalog-build CATALOG_IMG=example.com/operator-catalog:v0.2.0).
CATALOG_IMG ?= $(IMAGE_TAG_BASE)-catalog:v$(VERSION)

# Set CATALOG_BASE_IMG to an existing catalog image tag to add $BUNDLE_IMGS to that image.
ifneq ($(origin CATALOG_BASE_IMG), undefined)
FROM_INDEX_OPT := --from-index $(CATALOG_BASE_IMG)
endif

# Build a catalog image by adding bundle images to an empty catalog using the operator package manager tool, 'opm'.
# This recipe invokes 'opm' in 'semver' bundle add mode. For more information on add modes, see:
# https://github.com/operator-framework/community-operators/blob/7f1438c/docs/packaging-operator.md#updating-your-existing-operator
.PHONY: catalog-build
catalog-build: opm ## Build a catalog image.
	$(OPM) index add --container-tool $(CONTAINER_TOOL) --mode semver --tag $(CATALOG_IMG) --bundles $(BUNDLE_IMGS) $(FROM_INDEX_OPT)

# Push the catalog image.
.PHONY: catalog-push
catalog-push: ## Push a catalog image.
	$(MAKE) docker-push IMG=$(CATALOG_IMG)
