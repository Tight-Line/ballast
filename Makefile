.PHONY: build clean test test-coverage test-coverage-check lint lint-fix lint-config fmt vet tidy tools \
        setup-hooks docker check \
        manifests generate install uninstall deploy undeploy \
        setup-envtest setup-test-e2e test-e2e cleanup-test-e2e \
        build-installer help \
        helm-build helm-lint helm-template helm-package

# Build variables
VERSION ?= 0.1.0
IMG ?= ghcr.io/tight-line/ballast:$(VERSION)

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

CONTAINER_TOOL ?= docker

SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

# YEAR for kubebuilder boilerplate header
YEAR ?= $(shell date +%Y)

all: build

##@ General

help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

manifests: controller-gen ## Generate CRDs, RBAC, and webhook manifests from markers.
	"$(CONTROLLER_GEN)" rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

generate: controller-gen ## Generate DeepCopy, DeepCopyInto, and DeepCopyObject implementations.
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt",year=$(YEAR) paths="./..."

fmt: ## Run go fmt and goimports (goimports skipped if not installed).
	go fmt ./...
	@command -v goimports >/dev/null 2>&1 && \
		goimports -w -local github.com/tight-line/ballast . || true

vet: ## Run go vet.
	go vet ./...

tidy: ## Tidy go modules.
	go mod tidy

tools: ## Install development tools (goimports).
	go install golang.org/x/tools/cmd/goimports@latest

setup-hooks: ## Install git pre-commit hook.
	@echo "Installing pre-commit hook..."
	@cp scripts/pre-commit .git/hooks/pre-commit
	@chmod +x .git/hooks/pre-commit
	@echo "Pre-commit hook installed."

##@ Build

build: manifests generate fmt vet ## Build the ballastd binary.
	go build -o bin/ballastd ./cmd/ballastd

clean: ## Remove build artifacts and coverage files; clear the Go test cache.
	go clean -testcache
	rm -f bin/ballastd coverage.out coverage.filtered.out coverage.html

run: manifests generate fmt vet ## Run ballastd from your host (uses current kubeconfig).
	go run ./cmd/ballastd

docker: ## Build the Docker image.
	$(CONTAINER_TOOL) build -t $(IMG) --build-arg VERSION=$(VERSION) .

##@ Testing

test: manifests generate fmt vet setup-envtest ## Run all tests (unit + envtest).
	KUBEBUILDER_ASSETS="$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" \
		go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

test-coverage: manifests generate fmt vet setup-envtest ## Run tests and generate coverage.html.
	KUBEBUILDER_ASSETS="$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" \
		go test -v -coverprofile=coverage.out -tags=ci $$(go list ./... | grep -v /e2e)
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

test-coverage-check: manifests generate fmt vet setup-envtest ## Enforce 100% coverage; also generates coverage.filtered.out for Codecov.
	KUBEBUILDER_ASSETS="$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" \
		./scripts/check-coverage.sh --codecov

##@ Linting

lint: golangci-lint ## Run golangci-lint.
	"$(GOLANGCI_LINT)" run

lint-fix: golangci-lint ## Run golangci-lint with auto-fix.
	"$(GOLANGCI_LINT)" run --fix

lint-config: golangci-lint ## Verify golangci-lint configuration.
	"$(GOLANGCI_LINT)" config verify

##@ Release Gate

check: lint test-coverage-check build ## Full pre-release gate: lint + coverage + build.
	@echo "All checks passed. Ready for release."

##@ Helm Chart

HELM ?= helm
CHART_DIR ?= charts/ballast

helm-build: manifests ## Sync CRDs from config/crd/bases/ and download chart dependencies.
	cp config/crd/bases/*.yaml $(CHART_DIR)/crds/
	$(HELM) dependency update $(CHART_DIR)

helm-lint: helm-build ## Lint the Helm chart.
	$(HELM) lint $(CHART_DIR)

helm-template: helm-build ## Render Helm templates to stdout for inspection.
	$(HELM) template ballast $(CHART_DIR) --namespace ballast-system

helm-package: helm-build ## Package the chart into a self-contained .tgz release artifact under dist/.
	mkdir -p dist
	$(HELM) package $(CHART_DIR) --destination dist/

##@ Cluster Deployment

install: manifests kustomize ## Install CRDs into the cluster in ~/.kube/config.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" apply -f -; else echo "No CRDs to install; skipping."; fi

uninstall: manifests kustomize ## Uninstall CRDs from the cluster.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -; else echo "No CRDs to delete; skipping."; fi

deploy: manifests kustomize ## Deploy controller to the cluster.
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=$(IMG)
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" apply -f -

undeploy: kustomize ## Undeploy controller from the cluster.
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -

build-installer: manifests generate kustomize ## Generate dist/install.yaml (CRDs + Deployment).
	mkdir -p dist
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=$(IMG)
	"$(KUSTOMIZE)" build config/default > dist/install.yaml

ifndef ignore-not-found
  ignore-not-found = false
endif

KIND_CLUSTER ?= ballast-test-e2e

setup-test-e2e: ## Set up a Kind cluster for e2e tests.
	@command -v $(KIND) >/dev/null 2>&1 || { echo "Kind is not installed."; exit 1; }
	@case "$$($(KIND) get clusters)" in \
		*"$(KIND_CLUSTER)"*) echo "Kind cluster '$(KIND_CLUSTER)' already exists." ;; \
		*) echo "Creating Kind cluster '$(KIND_CLUSTER)'..."; $(KIND) create cluster --name $(KIND_CLUSTER) ;; \
	esac

test-e2e: setup-test-e2e manifests generate fmt vet ## Run e2e tests against a Kind cluster.
	KIND=$(KIND) KIND_CLUSTER=$(KIND_CLUSTER) go test -tags=e2e ./test/e2e/ -v -ginkgo.v
	$(MAKE) cleanup-test-e2e

cleanup-test-e2e: ## Tear down the Kind cluster used for e2e tests.
	@$(KIND) delete cluster --name $(KIND_CLUSTER)

##@ Dependencies

LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

KUBECTL    ?= kubectl
KIND       ?= kind
KUSTOMIZE  ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST    ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint

KUSTOMIZE_VERSION       ?= v5.8.1
CONTROLLER_TOOLS_VERSION ?= v0.21.0
GOLANGCI_LINT_VERSION   ?= v2.12.2

ENVTEST_VERSION ?= $(shell v='$(call gomodver,sigs.k8s.io/controller-runtime)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_VERSION manually" >&2; exit 1; }; \
  printf '%s\n' "$$v")

ENVTEST_K8S_VERSION ?= $(shell v='$(call gomodver,k8s.io/api)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_K8S_VERSION manually" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?[0-9]+\.([0-9]+).*/1.\1/')

.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: setup-envtest
setup-envtest: envtest ## Download envtest binaries for the current K8s version.
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@"$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path || { \
		echo "Error: Failed to set up envtest binaries."; exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

# go-install-tool: install $2@$3 to $1 (versioned symlink pattern)
define go-install-tool
@[ -f "$(1)-$(3)" ] && [ "$$(readlink -- "$(1)" 2>/dev/null)" = "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f "$(1)" ;\
GOBIN="$(LOCALBIN)" go install $${package} ;\
mv "$(LOCALBIN)/$$(basename "$(1)")" "$(1)-$(3)" ;\
} ;\
ln -sf "$$(realpath "$(1)-$(3)")" "$(1)"
endef

define gomodver
$(shell go list -m -f '{{if .Replace}}{{.Replace.Version}}{{else}}{{.Version}}{{end}}' $(1) 2>/dev/null)
endef
