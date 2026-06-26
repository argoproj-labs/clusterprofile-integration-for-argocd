# Container image settings
IMAGE_REPOSITORY?=ghcr.io/argoproj-labs
IMAGE_NAME=clusterprofile-integration-for-argocd
IMAGE_PLATFORM?=linux/amd64
IMAGE_MULTIARCH_PLATFORMS?=linux/amd64,linux/arm64
IMAGE_TAG?=latest
IMG ?= $(IMAGE_REPOSITORY)/$(IMAGE_NAME):$(IMAGE_TAG)

# Helm tooling settings
HELM_CHART_DIRS := $(shell find charts -mindepth 1 -maxdepth 1 -type d -exec test -f '{}/Chart.yaml' ';' -print | sort)
HELM_VALUES_SCHEMA_CHART := charts/argocd-clusterprofile-controller
HELM_SCHEMA_VERSION ?= 2.5.0
HELM_SCHEMA := go run github.com/losisin/helm-values-schema-json/v2@v$(HELM_SCHEMA_VERSION)
HELM_DOCS_VERSION ?= 1.14.2
HELM_DOCS := go run github.com/norwoodj/helm-docs/cmd/helm-docs@v$(HELM_DOCS_VERSION)

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

.PHONY: all
all: build

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.

.PHONY: generate
generate: ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: fmt vet ## Run tests.
	go test ./... -coverprofile cover.out

.PHONY: e2e
e2e: ## Run full kind-based e2e tests.
	$(MAKE) docker-build
	E2E_IMG=$(IMG) ./hack/e2e-kind.sh

##@ Build

.PHONY: build
build: fmt vet ## Build manager binary.
	go build -o bin/manager main.go controller.go

.PHONY: run
run: fmt vet ## Run a controller from your host.
	go run main.go controller.go

.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	docker build -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	docker push ${IMG}

##@ Container image (CI)

.PHONY: image
image: ## Build single-arch container image.
	docker build --platform $(IMAGE_PLATFORM) -t $(IMG) .

.PHONY: image-multiarch
image-multiarch: ## Build and push multi-arch container image.
	docker buildx build --platform $(IMAGE_MULTIARCH_PLATFORMS) --push -t $(IMG) .

.PHONY: push-image
push-image: ## Push single-arch container image.
	docker push $(IMG)

##@ Helm

.PHONY: helm-lint
helm-lint: ## Lint Helm charts.
	helm lint $(HELM_CHART_DIRS)

.PHONY: validate-values-schema
validate-values-schema: ## Verify generated Helm values schema is up to date.
	@set -e; \
	tmp=$$(mktemp -d); \
	trap 'rm -rf "$$tmp"' EXIT; \
	cd $(HELM_VALUES_SCHEMA_CHART); \
	$(HELM_SCHEMA) lint --strict; \
	$(HELM_SCHEMA) --output "$$tmp/values.schema.json"; \
	diff -u values.schema.json "$$tmp/values.schema.json"

.PHONY: generate-values-schema
generate-values-schema: ## Generate Helm values schema.
	cd $(HELM_VALUES_SCHEMA_CHART) && $(HELM_SCHEMA)

.PHONY: generate-helm-docs
generate-helm-docs: ## Generate Helm chart README files.
	$(HELM_DOCS) --chart-search-root=charts
