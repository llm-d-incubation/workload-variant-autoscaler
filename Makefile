# Image URL to use all building/pushing image targets
IMAGE_TAG_BASE ?= ghcr.io/llm-d
IMG_TAG ?= latest
IMG ?= $(IMAGE_TAG_BASE)/llm-d-workload-variant-autoscaler:$(IMG_TAG)
KIND_ARGS ?= -t mix -n 3 -g 2   # Default: 3 nodes, 2 GPUs per node, mixed vendors
CLUSTER_GPU_TYPE ?= nvidia-mix
CLUSTER_NODES ?= 3
CLUSTER_GPUS ?= 4
KUBECONFIG ?= $(HOME)/.kube/config
K8S_VERSION ?= v1.32.0

CONTROLLER_NAMESPACE ?= workload-variant-autoscaler-system
MONITORING_NAMESPACE ?= openshift-user-workload-monitoring
LLMD_NAMESPACE       ?= llm-d-inference-scheduler
GATEWAY_NAME         ?= # discovered automatically in e2es
MODEL_ID             ?= unsloth/Meta-Llama-3.1-8B
DEPLOYMENT           ?= # discovered automatically in e2es
REQUEST_RATE         ?= 20
NUM_PROMPTS          ?= 3000

# E2E test configuration (for test/e2e/ suite)
ENVIRONMENT                 ?= kind-emulator
USE_SIMULATOR               ?= true
SCALE_TO_ZERO_ENABLED       ?= false
SCALER_BACKEND              ?= prometheus-adapter  # prometheus-adapter (HPA), keda (ScaledObject), or none (skip, use pre-installed backend)
E2E_MONITORING_NAMESPACE    ?= workload-variant-autoscaler-monitoring
E2E_EMULATED_LLMD_NAMESPACE ?= llm-d-sim

# Flags for deploy/install.sh installation script
# Full e2e / CI-style cluster infra (WVA + llm-d, no chart VA/HPA): prefer `make deploy-e2e-infra`
# (wraps ./deploy/install.sh with INFRA_ONLY=true; set ENVIRONMENT=kubernetes|openshift|kind-emulator).
CREATE_CLUSTER ?= false
DEPLOY_LLM_D ?= true
DELETE_CLUSTER ?= false
DELETE_NAMESPACES ?= false

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

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases
	cp config/crd/bases/llmd.ai_variantautoscalings.yaml charts/workload-variant-autoscaler/crds/llmd.ai_variantautoscalings.yaml

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet setup-envtest helm ## Run tests.
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" PATH=$(LOCALBIN):$(PATH) go test $$(go list ./... | grep -v /e2e | grep -v /benchmark) -coverprofile cover.out

# Creates a multi-node Kind cluster
# Adds emulated GPU labels and capacities per node
.PHONY: create-kind-cluster
create-kind-cluster:
	export KIND=$(KIND) KUBECTL=$(KUBECTL) && \
		deploy/kind-emulator/setup.sh -t $(CLUSTER_GPU_TYPE) -n $(CLUSTER_NODES) -g $(CLUSTER_GPUS)

# Destroys the Kind cluster created by `create-kind-cluster`
.PHONY: destroy-kind-cluster
destroy-kind-cluster:
	export KIND=$(KIND) KUBECTL=$(KUBECTL) && \
        deploy/kind-emulator/teardown.sh

# Deploys the WVA controller on a pre-existing Kind cluster or creates one if specified.
# Set SCALER_BACKEND=keda if you want to install KEDA instead of Prometheus Adapter.
.PHONY: deploy-wva-emulated-on-kind
deploy-wva-emulated-on-kind: ## Deploy WVA + llm-d on Kind (Prometheus Adapter as scaler backend)
	@echo ">>> Deploying workload-variant-autoscaler (cluster args: $(KIND_ARGS), image: $(IMG))"
	KIND=$(KIND) KUBECTL=$(KUBECTL) IMG=$(IMG) DEPLOY_LLM_D=$(DEPLOY_LLM_D) ENVIRONMENT=kind-emulator CREATE_CLUSTER=$(CREATE_CLUSTER) CLUSTER_GPU_TYPE=$(CLUSTER_GPU_TYPE) CLUSTER_NODES=$(CLUSTER_NODES) CLUSTER_GPUS=$(CLUSTER_GPUS) NAMESPACE_SCOPED=false SCALER_BACKEND=$(SCALER_BACKEND) \
		deploy/install.sh

## Undeploy WVA from the emulated environment on Kind.
## Undeploy WVA from Kind (set SCALER_BACKEND=keda if you deployed with KEDA)
.PHONY: undeploy-wva-emulated-on-kind
undeploy-wva-emulated-on-kind:
	@echo ">>> Undeploying workload-variant-autoscaler from Kind"
	KIND=$(KIND) KUBECTL=$(KUBECTL) ENVIRONMENT=kind-emulator DEPLOY_LLM_D=$(DEPLOY_LLM_D) DELETE_NAMESPACES=$(DELETE_NAMESPACES) DELETE_CLUSTER=$(DELETE_CLUSTER) SCALER_BACKEND=$(SCALER_BACKEND) \
		deploy/install.sh --undeploy

## Deploy WVA to OpenShift cluster with specified image.
.PHONY: deploy-wva-on-openshift
deploy-wva-on-openshift: manifests kustomize ## Deploy WVA to OpenShift cluster with specified image.
	@echo "Deploying WVA to OpenShift with image: $(IMG)"
	@echo "Target namespace: $(or $(NAMESPACE),workload-variant-autoscaler-system)"
	NAMESPACE=$(or $(NAMESPACE),workload-variant-autoscaler-system) IMG=$(IMG) ENVIRONMENT=openshift DEPLOY_LLM_D=$(DEPLOY_LLM_D) ./deploy/install.sh

## Undeploy WVA from OpenShift.
.PHONY: undeploy-wva-on-openshift
undeploy-wva-on-openshift:
	@echo ">>> Undeploying workload-variant-autoscaler from OpenShift"
	export KIND=$(KIND) KUBECTL=$(KUBECTL) ENVIRONMENT=openshift && \
		DEPLOY_LLM_D=$(DEPLOY_LLM_D) deploy/install.sh --undeploy

## Deploy WVA on Kubernetes with the specified image.
.PHONY: deploy-wva-on-k8s
deploy-wva-on-k8s: manifests kustomize ## Deploy WVA on Kubernetes with the specified image.
	@echo "Deploying WVA on Kubernetes with image: $(IMG)"
	@echo "Target namespace: $(or $(NAMESPACE),workload-variant-autoscaler-system)"
	NAMESPACE=$(or $(NAMESPACE),workload-variant-autoscaler-system) IMG=$(IMG) ENVIRONMENT=kubernetes DEPLOY_LLM_D=$(DEPLOY_LLM_D) ./deploy/install.sh

## Undeploy WVA from Kubernetes.
.PHONY: undeploy-wva-on-k8s
undeploy-wva-on-k8s:
	@echo ">>> Undeploying workload-variant-autoscaler from Kubernetes"
	export KIND=$(KIND) KUBECTL=$(KUBECTL) ENVIRONMENT=kubernetes && \
		ENVIRONMENT=kubernetes DEPLOY_LLM_D=$(DEPLOY_LLM_D)  deploy/install.sh --undeploy

# E2E tests on Kind cluster for saturation-based autoscaling
# The default setup assumes Kind is pre-installed and builds/loads the Manager Docker image locally.
# Supports FOCUS and SKIP variables for ginkgo test filtering.
# Setup options:
# - CERT_MANAGER_INSTALL_SKIP=true: Skip certManager installation during test setup.
# - IMAGE_BUILD_SKIP=true: Skip building the WVA docker image during test setup.
# - INFRA_SETUP_SKIP=true: Skip setting up the llm-d and the WVA controller manager during test setup. Reload the docker image if necessary.
# - INFRA_TEARDOWN_SKIP=true: Skip tearing down the Kind cluster during test teardown.

# Consolidated e2e test targets (environment-agnostic)
# These targets use the test/e2e/ suite that works on any Kubernetes cluster
# Supports FOCUS and SKIP variables for ginkgo test filtering.

# Deploys only the infrastructure (WVA controller + llm-d) without VA/HPA resources.
# If IMG is set, builds the image locally first (unless SKIP_BUILD=true).
.PHONY: deploy-e2e-infra
deploy-e2e-infra: ## Deploy e2e test infrastructure (infra-only: WVA + llm-d, no VA/HPA). Uses Prometheus Adapter unless SCALER_BACKEND=keda.
	@echo "Deploying e2e test infrastructure (infra-only mode)..."
	@if [ -n "$(IMG)" ]; then \
		echo "IMG is set to '$(IMG)'"; \
		if [ "$(SKIP_BUILD)" != "true" ]; then \
			echo "Building local image (SKIP_BUILD not set)..."; \
			$(MAKE) docker-build IMG=$(IMG); \
		else \
			echo "Skipping image build (SKIP_BUILD=true) - assuming image already exists"; \
		fi; \
		echo "Extracting image repo and tag from IMG..."; \
		if echo "$(IMG)" | grep -q ":"; then \
			IMAGE_REPO=$$(echo $(IMG) | cut -d: -f1); \
			IMAGE_TAG=$$(echo $(IMG) | cut -d: -f2); \
		else \
			IMAGE_REPO="$(IMG)"; \
			IMAGE_TAG="latest"; \
		fi; \
		echo "Using local image: $$IMAGE_REPO:$$IMAGE_TAG"; \
		ENVIRONMENT=$(ENVIRONMENT) \
		INFRA_ONLY=true \
		USE_SIMULATOR=$(USE_SIMULATOR) \
		SCALE_TO_ZERO_ENABLED=$(SCALE_TO_ZERO_ENABLED) \
		SCALER_BACKEND=$(SCALER_BACKEND) \
		INSTALL_GATEWAY_CTRLPLANE=true \
		NAMESPACE_SCOPED=false \
		DECODE_REPLICAS=$(DECODE_REPLICAS) \
		LLM_D_RELEASE=$(LLM_D_RELEASE) \
		WVA_IMAGE_REPO=$$IMAGE_REPO \
		WVA_IMAGE_TAG=$$IMAGE_TAG \
		WVA_IMAGE_PULL_POLICY=IfNotPresent \
		./deploy/install.sh; \
	else \
		echo "IMG not set - using default image from registry (latest)"; \
		ENVIRONMENT=$(ENVIRONMENT) \
		INFRA_ONLY=true \
		USE_SIMULATOR=$(USE_SIMULATOR) \
		SCALE_TO_ZERO_ENABLED=$(SCALE_TO_ZERO_ENABLED) \
		SCALER_BACKEND=$(SCALER_BACKEND) \
		INSTALL_GATEWAY_CTRLPLANE=true \
		NAMESPACE_SCOPED=false \
		DECODE_REPLICAS=$(DECODE_REPLICAS) \
		LLM_D_RELEASE=$(LLM_D_RELEASE) \
		./deploy/install.sh; \
	fi

# Deploy e2e infrastructure with KEDA as scaler backend (installs KEDA, skips Prometheus Adapter).
# Runs a subset of smoke tests from the e2e suite.
.PHONY: test-e2e-smoke
test-e2e-smoke: ## Run smoke e2e tests
	@echo "Running smoke e2e tests..."
	$(eval FOCUS_ARGS := $(if $(FOCUS),-ginkgo.focus="$(FOCUS)",))
	$(eval SKIP_ARGS := $(if $(SKIP),-ginkgo.skip="$(SKIP)",))
	KUBECONFIG=$(KUBECONFIG) \
	ENVIRONMENT=$(ENVIRONMENT) \
	WVA_NAMESPACE=$(CONTROLLER_NAMESPACE) \
	LLMD_NAMESPACE=$(E2E_EMULATED_LLMD_NAMESPACE) \
	MONITORING_NAMESPACE=$(E2E_MONITORING_NAMESPACE) \
	USE_SIMULATOR=$(USE_SIMULATOR) \
	SCALE_TO_ZERO_ENABLED=$(SCALE_TO_ZERO_ENABLED) \
	SCALER_BACKEND=$(SCALER_BACKEND) \
	MODEL_ID=$(MODEL_ID) \
	go test ./test/e2e/ -timeout 20m -v -ginkgo.v \
		-ginkgo.label-filter="smoke" $(FOCUS_ARGS) $(SKIP_ARGS); \
	TEST_EXIT_CODE=$$?; \
	echo ""; \
	echo "=========================================="; \
	echo "Test execution completed. Exit code: $$TEST_EXIT_CODE"; \
	echo "=========================================="; \
	exit $$TEST_EXIT_CODE

# Runs the complete e2e test suite (excluding flaky tests).
.PHONY: test-e2e-full
test-e2e-full: ## Run full e2e test suite
	@echo "Running full e2e test suite..."
	$(eval FOCUS_ARGS := $(if $(FOCUS),-ginkgo.focus="$(FOCUS)",))
	$(eval SKIP_ARGS := $(if $(SKIP),-ginkgo.skip="$(SKIP)",))
	KUBECONFIG=$(KUBECONFIG) \
	ENVIRONMENT=$(ENVIRONMENT) \
	WVA_NAMESPACE=$(CONTROLLER_NAMESPACE) \
	USE_SIMULATOR=$(USE_SIMULATOR) \
	SCALE_TO_ZERO_ENABLED=$(SCALE_TO_ZERO_ENABLED) \
	SCALER_BACKEND=$(SCALER_BACKEND) \
	MODEL_ID=$(MODEL_ID) \
	go test ./test/e2e/ -timeout 35m -v -ginkgo.v \
		-ginkgo.label-filter="full && !flaky" $(FOCUS_ARGS) $(SKIP_ARGS); \
	TEST_EXIT_CODE=$$?; \
	echo ""; \
	echo "=========================================="; \
	echo "Test execution completed. Exit code: $$TEST_EXIT_CODE"; \
	echo "=========================================="; \
	exit $$TEST_EXIT_CODE

# Convenience targets for local e2e testing

# Convenience target that deploys infra + runs smoke tests.
# Set DELETE_CLUSTER=true to delete Kind cluster after tests (default: keep cluster for debugging).
.PHONY: test-e2e-smoke-with-setup
test-e2e-smoke-with-setup: deploy-e2e-infra test-e2e-smoke

# Convenience target that deploys infra + runs full test suite.
# Set DELETE_CLUSTER=true to delete Kind cluster after tests (default: keep cluster for debugging).
.PHONY: test-e2e-full-with-setup
test-e2e-full-with-setup: deploy-e2e-infra test-e2e-full

# Benchmark targets
.PHONY: test-benchmark
test-benchmark: manifests generate fmt vet ## Run benchmark tests (scale-up-latency scenario)
	@echo "Running benchmark tests..."
	KUBECONFIG=$(KUBECONFIG) \
	ENVIRONMENT=$(ENVIRONMENT) \
	WVA_NAMESPACE=$(CONTROLLER_NAMESPACE) \
	LLMD_NAMESPACE=$(E2E_EMULATED_LLMD_NAMESPACE) \
	MONITORING_NAMESPACE=$(E2E_MONITORING_NAMESPACE) \
	USE_SIMULATOR=$(USE_SIMULATOR) \
	SCALER_BACKEND=$(SCALER_BACKEND) \
	MODEL_ID=$(MODEL_ID) \
	PROMETHEUS_TOKEN=$$(oc whoami -t 2>/dev/null || echo "") \
	go test ./test/benchmark/ -timeout 75m -v -ginkgo.v \
		-ginkgo.label-filter="phase3a"; \
	TEST_EXIT_CODE=$$?; \
	echo ""; \
	echo "=========================================="; \
	echo "Benchmark execution completed. Exit code: $$TEST_EXIT_CODE"; \
	echo "=========================================="; \
	exit $$TEST_EXIT_CODE

# Convenience target that deploys infra + runs benchmark tests.
.PHONY: test-benchmark-with-setup
test-benchmark-with-setup: deploy-e2e-infra test-benchmark

# Stub for llm-d nightly reusable workflows (test_target=nightly-test-llm-d)
# No-op; temporarily satisfies nightly CI make invocation
# TODO: add nightly guide tests here
.PHONY: nightly-test-llm-d
nightly-test-llm-d: ## Nightly CI: noop; use as test_target instead of empty string
	@:

# Shared script: deploy/lib/llm_d_nightly_install.sh
# Canonical target for llm-d-infra nightly reusables: ENVIRONMENT=openshift|kubernetes
.PHONY: nightly-deploy-wva-guide
nightly-deploy-wva-guide: ## Nightly: full WVA+llm-d stack from job env (WVA_NS <- WVA_NAMESPACE or CONTROLLER_NAMESPACE)
	@export WVA_NS="$${WVA_NS:-$${WVA_NAMESPACE:-$${CONTROLLER_NAMESPACE:-}}}"; \
	if [ "$${ENVIRONMENT:-}" = openshift ]; then \
		LLM_D_NIGHTLY_PLATFORM=openshift bash "$(CURDIR)/deploy/lib/llm_d_nightly_install.sh" "$(CURDIR)"; \
	else \
		LLM_D_NIGHTLY_PLATFORM=cks bash "$(CURDIR)/deploy/lib/llm_d_nightly_install.sh" "$(CURDIR)"; \
	fi

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	$(GOLANGCI_LINT) run

.PHONY: lint-deploy-scripts
lint-deploy-scripts: ## Run bash -n for deploy/install.sh, deploy/lib/*.sh, and deploy plugins
	@echo "Syntax-checking deploy shell scripts..."
	@bash -n deploy/install.sh
	@for script in deploy/lib/*.sh; do bash -n "$$script"; done
	@for script in deploy/*/install.sh; do if [ -f "$$script" ]; then bash -n "$$script"; fi; done
	@for script in deploy/kind-emulator/*.sh; do if [ -f "$$script" ]; then bash -n "$$script"; fi; done
	@echo "deploy script syntax OK"

.PHONY: smoke-deploy-scripts
smoke-deploy-scripts: lint-deploy-scripts ## Non-interactive deploy script smoke check (source order + arg parsing)
	@echo "Running deploy script smoke check..."
	@SKIP_CHECKS=true E2E_TESTS_ENABLED=true INSTALL_GATEWAY_CTRLPLANE=true ENVIRONMENT=kubernetes ./deploy/install.sh --help >/dev/null
	@echo "deploy script smoke OK"

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

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image to your registry (i.e. if you do not set a valid value via IMG=<myregistry/image:<tag>> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/arm64,linux/amd64
BUILDER_NAME ?= workload-variant-autoscaler-builder

.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	# copy existing Dockerfile and insert --platform=${BUILDPLATFORM} into Dockerfile.cross, and preserve the original Dockerfile
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' Dockerfile > Dockerfile.cross
	- $(CONTAINER_TOOL) buildx create --name workload-variant-autoscaler-builder
	$(CONTAINER_TOOL) buildx use workload-variant-autoscaler-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm workload-variant-autoscaler-builder
	rm Dockerfile.cross

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
HELM ?= $(LOCALBIN)/helm

## Tool Versions
KUSTOMIZE_VERSION ?= v5.6.0
CONTROLLER_TOOLS_VERSION ?= v0.17.2
#ENVTEST_VERSION is the version of controller-runtime release branch to fetch the envtest setup script (i.e. release-0.20)
ENVTEST_VERSION ?= $(shell go list -m -f "{{ .Version }}" sigs.k8s.io/controller-runtime | awk -F'[v.]' '{printf "release-%d.%d", $$2, $$3}')
#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell go list -m -f "{{ .Version }}" k8s.io/api | awk -F'[v.]' '{printf "1.%d", $$3}')
GOLANGCI_LINT_VERSION ?= v2.8.0
HELM_VERSION ?= v3.17.1

.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))


CRD_REF_DOCS_BIN := $(shell go env GOPATH)/bin/crd-ref-docs
CRD_SOURCE_PATH := ./api/v1alpha1
CRD_CONFIG := ./hack/crd-doc-gen/config.yaml
CRD_RENDERER := markdown
CRD_OUTPUT := ./docs/user-guide/crd-reference.md

.PHONY: crd-docs install-crd-ref-docs

# Install crd-ref-docs if not already present
install-crd-ref-docs:
	@if [ ! -f "$(CRD_REF_DOCS_BIN)" ]; then \
		echo "Installing crd-ref-docs..."; \
		go install github.com/elastic/crd-ref-docs@latest; \
	fi

# Generate CRD documentation
crd-docs: install-crd-ref-docs
	$(CRD_REF_DOCS_BIN) \
		--source-path=$(CRD_SOURCE_PATH) \
		--config=$(CRD_CONFIG) \
		--renderer=$(CRD_RENDERER)
		# Fallback: if the tool produced out.md, rename it
	@if [ -f ./out.md ]; then mv ./out.md $(CRD_OUTPUT); fi
	@if [ -f ./docs/out.md ]; then mv ./docs/out.md $(CRD_OUTPUT); fi
	@test -f $(CRD_OUTPUT) && echo "✅ CRD documentation generated at $(CRD_OUTPUT)" || \
	 (echo "❌ Expected $(CRD_OUTPUT) not found. Check $(CRD_CONFIG) or tool output."; exit 1)

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
	@[ -f "$(LOCALBIN)/golangci-lint-$(GOLANGCI_LINT_VERSION)" ] || { \
	set -e; \
	echo "Downloading golangci-lint $(GOLANGCI_LINT_VERSION)"; \
	curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $(LOCALBIN) $(GOLANGCI_LINT_VERSION); \
	if [ -f "$(LOCALBIN)/golangci-lint" ]; then \
		mv $(LOCALBIN)/golangci-lint $(LOCALBIN)/golangci-lint-$(GOLANGCI_LINT_VERSION); \
	fi; \
	} ;\
	ln -sf golangci-lint-$(GOLANGCI_LINT_VERSION) $(GOLANGCI_LINT)

.PHONY: helm
helm: $(HELM) ## Download helm locally if necessary.
$(HELM): $(LOCALBIN)
	@[ -f "$(LOCALBIN)/helm-$(HELM_VERSION)" ] || { \
	set -e; \
	echo "Downloading helm $(HELM_VERSION)"; \
	curl -sSfL https://get.helm.sh/helm-$(HELM_VERSION)-$(shell go env GOOS)-$(shell go env GOARCH).tar.gz | tar xz --no-same-owner -C $(LOCALBIN) --strip-components=1 $(shell go env GOOS)-$(shell go env GOARCH)/helm; \
	mv $(LOCALBIN)/helm $(LOCALBIN)/helm-$(HELM_VERSION); \
	} ;\
	ln -sf helm-$(HELM_VERSION) $(HELM)

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
