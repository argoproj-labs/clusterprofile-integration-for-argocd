# Container image settings
IMAGE_REPOSITORY?=ghcr.io/argoproj-labs
IMAGE_NAME=clusterprofile-integration-for-argocd
IMAGE_PLATFORM?=linux/amd64
IMAGE_MULTIARCH_PLATFORMS?=linux/amd64,linux/arm64
IMAGE_TAG?=latest
IMG ?= $(IMAGE_REPOSITORY)/$(IMAGE_NAME):$(IMAGE_TAG)

# E2E settings
E2E_INSTALL_METHOD?=helm

# Generated install manifest settings
KUSTOMIZE ?= kubectl kustomize
KUSTOMIZE_ROOT := artifacts/manifests
INSTALL_MANIFEST := $(KUSTOMIZE_ROOT)/install.yaml

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
manifests: ## Generate the consolidated install manifest from the Kustomize sources.
	@set -e; \
	tmp=$$(mktemp "$(KUSTOMIZE_ROOT)/.install.yaml.tmp.XXXXXX"); \
	trap 'rm -f "$$tmp"' EXIT; \
	$(KUSTOMIZE) $(KUSTOMIZE_ROOT) >"$$tmp"; \
	chmod 0644 "$$tmp"; \
	mv "$$tmp" $(INSTALL_MANIFEST)

.PHONY: validate-manifests
validate-manifests: ## Verify the consolidated install manifest is up to date.
	@set -e; \
	tmp=$$(mktemp); \
	trap 'rm -f "$$tmp"' EXIT; \
	$(KUSTOMIZE) $(KUSTOMIZE_ROOT) >"$$tmp"; \
	diff -u $(INSTALL_MANIFEST) "$$tmp"

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
e2e: ## Run full live and multi-node HA kind-based e2e tests.
	$(MAKE) docker-build
	E2E_IMG=$(IMG) E2E_INSTALL_METHOD=$(E2E_INSTALL_METHOD) ./hack/e2e-kind.sh

##@ Build

.PHONY: build
build: fmt vet ## Build manager binary.
	go build -o bin/manager .

.PHONY: run
run: fmt vet ## Run a controller from your host.
	go run .

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

.PHONY: validate-helm-rendering
validate-helm-rendering: ## Verify default and VPA-enabled Helm rendering.
	@set -e; \
	tmp=$$(mktemp -d); \
	trap 'rm -rf "$$tmp"' EXIT; \
	helm template test $(HELM_VALUES_SCHEMA_CHART) >"$$tmp/default.yaml"; \
	if grep -q '^kind: VerticalPodAutoscaler$$' "$$tmp/default.yaml"; then \
		echo "default Helm rendering unexpectedly contains a VerticalPodAutoscaler" >&2; \
		exit 1; \
	fi; \
	grep -q 'memory: 256Mi$$' "$$tmp/default.yaml"; \
	grep -q 'cpu: 10m$$' "$$tmp/default.yaml"; \
	grep -q 'memory: 128Mi$$' "$$tmp/default.yaml"; \
	if grep -q 'cpu: 500m$$' "$$tmp/default.yaml"; then \
		echo "default Helm rendering unexpectedly contains a CPU limit" >&2; \
		exit 1; \
	fi; \
	helm template test $(HELM_VALUES_SCHEMA_CHART) \
		--set vpa.enabled=true \
		>"$$tmp/vpa-default.yaml"; \
	grep -q '^kind: VerticalPodAutoscaler$$' "$$tmp/vpa-default.yaml"; \
	grep -q 'updateMode: "Recreate"$$' "$$tmp/vpa-default.yaml"; \
	helm template test $(HELM_VALUES_SCHEMA_CHART) \
		--set vpa.enabled=true \
		--set-string vpa.updateMode=Off \
		--set-string vpa.labels.test-label=custom \
		--set-string vpa.annotations.test-annotation=custom \
		--set-string vpa.containerPolicy.controlledValues=RequestsOnly \
		>"$$tmp/vpa.yaml"; \
	grep -q '^kind: VerticalPodAutoscaler$$' "$$tmp/vpa.yaml"; \
	grep -q 'updateMode: "Off"$$' "$$tmp/vpa.yaml"; \
	grep -q 'containerName: clusterprofile-controller$$' "$$tmp/vpa.yaml"; \
	grep -q 'controlledValues: RequestsOnly$$' "$$tmp/vpa.yaml"; \
	grep -q 'test-label: custom$$' "$$tmp/vpa.yaml"; \
	grep -q 'test-annotation: custom$$' "$$tmp/vpa.yaml"; \
	if helm template test $(HELM_VALUES_SCHEMA_CHART) \
		--set vpa.enabled=true \
		--set-string vpa.containerPolicy.containerName=other \
		>/dev/null 2>&1; then \
		echo "vpa.containerPolicy.containerName override unexpectedly rendered" >&2; \
		exit 1; \
	fi; \
	for mode in Off Initial Recreate InPlaceOrRecreate; do \
		helm template test $(HELM_VALUES_SCHEMA_CHART) \
			--set vpa.enabled=true \
			--set-string vpa.updateMode="$$mode" \
			>/dev/null; \
	done; \
	for mode in Auto InPlace Unknown; do \
		if helm template test $(HELM_VALUES_SCHEMA_CHART) \
			--set vpa.enabled=true \
			--set-string vpa.updateMode="$$mode" \
			>/dev/null 2>&1; then \
			echo "unsupported VPA update mode $$mode unexpectedly passed validation" >&2; \
			exit 1; \
		fi; \
	done

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
