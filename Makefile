# Image URL to use all building/pushing image targets.
# NOTE: :latest is a dev/manual default for `make deploy`. Production must deploy the
# digest-pinned dist/install.yaml from a GitHub Release, or override IMG with a digest.
IMG ?= ghcr.io/chan-mai/cloudnative-misskey:latest

# CONTAINER_TOOL defines the container tool to be used for building images.
CONTAINER_TOOL ?= docker

# Get the currently used golang install path
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General
.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development
.PHONY: manifests
manifests: controller-gen ## Generate CRDs and RBAC manifests.
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate DeepCopy methods.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: fmt
fmt: ## Run go fmt.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet ## Run tests.
	go test ./... -coverprofile cover.out

.PHONY: test-e2e
test-e2e: kind kustomize ## Run kind e2e tests (requires docker).
	hack/e2e.sh

##@ Build
.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -o bin/manager cmd/main.go

.PHONY: run
run: manifests generate fmt vet ## Run the controller against the configured cluster.
	go run ./cmd/main.go

.PHONY: docker-build
docker-build: ## Build the manager container image.
	$(CONTAINER_TOOL) build -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push the manager container image.
	$(CONTAINER_TOOL) push ${IMG}

##@ Deployment
.PHONY: install
install: manifests kustomize ## Install CRDs into the cluster.
	$(KUSTOMIZE) build config/crd | kubectl apply -f -

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the cluster.
	$(KUSTOMIZE) build config/crd | kubectl delete --ignore-not-found -f -

.PHONY: deploy
deploy: manifests kustomize ## Deploy the controller to the cluster.
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/default | kubectl apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy the controller from the cluster.
	$(KUSTOMIZE) build config/default | kubectl delete --ignore-not-found -f -

##@ Tools
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
KUSTOMIZE ?= $(LOCALBIN)/kustomize
KIND ?= $(LOCALBIN)/kind

CONTROLLER_TOOLS_VERSION ?= v0.21.0
KUSTOMIZE_VERSION ?= v5.8.1
KIND_VERSION ?= v0.32.0

.PHONY: controller-gen
controller-gen: $(LOCALBIN) ## Download controller-gen locally if necessary.
	@test -s $(CONTROLLER_GEN) || GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)

.PHONY: kustomize
kustomize: $(LOCALBIN) ## Download kustomize locally if necessary.
	@test -s $(KUSTOMIZE) || GOBIN=$(LOCALBIN) go install sigs.k8s.io/kustomize/kustomize/v5@$(KUSTOMIZE_VERSION)

.PHONY: kind
kind: $(LOCALBIN) ## Download kind locally if necessary.
	@test -s $(KIND) || GOBIN=$(LOCALBIN) go install sigs.k8s.io/kind@$(KIND_VERSION)
