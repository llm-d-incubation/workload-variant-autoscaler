# Image URL to use all building/pushing image targets
IMAGE_TAG_BASE ?= ghcr.io/llm-d
IMG_TAG ?= latest
IMG ?= $(IMAGE_TAG_BASE)/workload-variant-autoscaler:$(IMG_TAG)
KIND_ARGS ?= -t mix -n 3 -g 2   # Default: 3 nodes, 2 GPUs per node, mixed vendors
CLUSTER_GPU_TYPE ?= mix
CLUSTER_NODES ?= 3
CLUSTER_GPUS ?= 4
KUBECONFIG ?= $(HOME)/.kube/config
K8S_VERSION ?= v1.32.0

CONTROLLER_NAMESPACE ?= workload-variant-autoscaler-system
MONITORING_NAMESPACE ?= openshift-user-workload-monitoring
LLMD_NAMESPACE       ?= llm-d-inference-scheduler
GATEWAY_NAME         ?= infra-inference-scheduling-inference-gateway-istio
MODEL_ID             ?= unsloth/Meta-Llama-3.1-8B
DEPLOYMENT           ?= ms-inference-scheduling-llm-d-modelservice-decode
REQUEST_RATE         ?= 20
NUM_PROMPTS          ?= 3000

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
test: manifests generate fmt vet setup-envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

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

# Creates Kind cluster with emulated GPU support (if needed)
# Deploys the WVA controller on a Kind cluster
# Deploys the llm-d components in the same Kind cluster
.PHONY: deploy-llm-d-wva-emulated-on-kind-create-cluster
deploy-llm-d-wva-emulated-on-kind-create-cluster:
	@echo ">>> Deploying integrated llm-d and workload-variant-autoscaler (cluster args: $(KIND_ARGS), image: $(IMG))"
	KIND=$(KIND) KUBECTL=$(KUBECTL) IMG=$(IMG) DEPLOY_LLM_D=true ENVIRONMENT=kind-emulator CREATE_CLUSTER=true CLUSTER_GPU_TYPE=$(CLUSTER_GPU_TYPE) CLUSTER_NODES=$(CLUSTER_NODES) CLUSTER_GPUS=$(CLUSTER_GPUS) \
		deploy/install.sh 

# Deploys Kind cluster with emulated GPU support (if needed)
# Deploys the WVA controller on a pre-existing Kind cluster
# Deploys the llm-d components in the same Kind cluster
.PHONY: deploy-llm-d-wva-emulated-on-kind
deploy-llm-d-wva-emulated-on-kind:
	@echo ">>> Deploying integrated llm-d and workload-variant-autoscaler (cluster args: $(KIND_ARGS), image: $(IMG))"
	KIND=$(KIND) KUBECTL=$(KUBECTL) IMG=$(IMG) DEPLOY_LLM_D=true ENVIRONMENT=kind-emulator CREATE_CLUSTER=false CLUSTER_GPU_TYPE=$(CLUSTER_GPU_TYPE) CLUSTER_NODES=$(CLUSTER_NODES) CLUSTER_GPUS=$(CLUSTER_GPUS) \
		deploy/install.sh 

# Deploys the WVA controller on a Kind cluster (creates cluster)
.PHONY: deploy-wva-emulated-on-kind-create-cluster
deploy-wva-emulated-on-kind-create-cluster:
	@echo ">>> Deploying workload-variant-autoscaler (cluster args: $(KIND_ARGS), image: $(IMG))"
	KIND=$(KIND) KUBECTL=$(KUBECTL) IMG=$(IMG) DEPLOY_LLM_D=false ENVIRONMENT=kind-emulator CREATE_CLUSTER=true CLUSTER_GPU_TYPE=$(CLUSTER_GPU_TYPE) CLUSTER_NODES=$(CLUSTER_NODES) CLUSTER_GPUS=$(CLUSTER_GPUS) \
		deploy/install.sh $(KIND_ARGS)

# Deploys the WVA controller on a pre-existing Kind cluster
.PHONY: deploy-wva-emulated-on-kind
deploy-wva-emulated-on-kind:
	@echo ">>> Deploying workload-variant-autoscaler (cluster args: $(KIND_ARGS), image: $(IMG))"
	KIND=$(KIND) KUBECTL=$(KUBECTL) IMG=$(IMG) DEPLOY_LLM_D=false ENVIRONMENT=kind-emulator CREATE_CLUSTER=false CLUSTER_GPU_TYPE=$(CLUSTER_GPU_TYPE) CLUSTER_NODES=$(CLUSTER_NODES) CLUSTER_GPUS=$(CLUSTER_GPUS) \
		deploy/install.sh $(KIND_ARGS)

## Undeploy WVA and llm-d from the emulated environment on Kind.
.PHONY: undeploy-llm-d-wva-emulated-on-kind
undeploy-llm-d-wva-emulated-on-kind:
	@echo ">>> Undeploying llm-d and workload-variant-autoscaler from Kind cluster"
	export KIND=$(KIND) KUBECTL=$(KUBECTL) DEPLOY_LLM_D=true DELETE_NAMESPACES=true ENVIRONMENT=kind-emulator && \
		deploy/install.sh --undeploy

.PHONY: undeploy-wva-on-kind
undeploy-wva-on-kind:
	@echo ">>> Undeploying workload-variant-autoscaler (cluster args: $(KIND_ARGS), image: $(IMG))"
	KIND=$(KIND) KUBECTL=$(KUBECTL) DEPLOY_LLM_D=false DELETE_NAMESPACES=true ENVIRONMENT=kind-emulator \
		deploy/install.sh $(KIND_ARGS) --undeploy

## Undeploy WVA from the emulated environment on Kind and delete the cluster.
.PHONY: undeploy-llm-d-wva-emulated-on-kind-delete-cluster
undeploy-llm-d-wva-emulated-on-kind-delete-cluster:
	@echo ">>> Undeploying llm-d and workload-variant-autoscaler and deleting Kind cluster"
	export KIND=$(KIND) KUBECTL=$(KUBECTL) ENVIRONMENT=kind-emulator DELETE_NAMESPACES=true DELETE_CLUSTER=true && \
		deploy/install.sh --undeploy

.PHONY: undeploy-wva-on-kind-delete-cluster
undeploy-wva-on-kind-delete-cluster:
	@echo ">>> Undeploying workload-variant-autoscaler and deleting Kind cluster"
	KIND=$(KIND) KUBECTL=$(KUBECTL) ENVIRONMENT=kind-emulator DEPLOY_LLM_D=false DELETE_NAMESPACES=true DELETE_CLUSTER=true \
		deploy/install.sh $(KIND_ARGS) --undeploy

## Deploy llm-d and WVA to OpenShift cluster with specified image.
.PHONY: deploy-llm-d-wva-on-openshift
deploy-llm-d-wva-on-openshift: manifests kustomize ## Deploy WVA to OpenShift cluster with specified image.
	@echo "Deploying WVA to OpenShift with image: $(IMG)"
	@echo "Target namespace: $(or $(NAMESPACE),workload-variant-autoscaler-system)"
	NAMESPACE=$(or $(NAMESPACE),workload-variant-autoscaler-system) IMG=$(IMG) ENVIRONMENT=openshift DEPLOY_LLM_D=true ./deploy/install.sh

## Deploy WVA to OpenShift cluster with specified image.
.PHONY: deploy-wva-on-openshift
deploy-wva-on-openshift: manifests kustomize ## Deploy WVA to OpenShift cluster with specified image.
	@echo "Deploying WVA to OpenShift with image: $(IMG)"
	@echo "Target namespace: $(or $(NAMESPACE),workload-variant-autoscaler-system)"
	NAMESPACE=$(or $(NAMESPACE),workload-variant-autoscaler-system) IMG=$(IMG) ENVIRONMENT=openshift DEPLOY_LLM_D=false ./deploy/install.sh

## Undeploy llm-d and WVA from OpenShift.
.PHONY: undeploy-llm-d-wva-on-openshift
undeploy-llm-d-wva-on-openshift:
	@echo ">>> Undeploying llm-d and workload-variant-autoscaler from OpenShift"
	export KIND=$(KIND) KUBECTL=$(KUBECTL) ENVIRONMENT=openshift && \
		ENVIRONMENT=openshift DEPLOY_LLM_D=true deploy/install.sh --undeploy

## Undeploy WVA from OpenShift.
.PHONY: undeploy-wva-on-openshift
undeploy-wva-on-openshift:
	@echo ">>> Undeploying workload-variant-autoscaler from OpenShift"
	export KIND=$(KIND) KUBECTL=$(KUBECTL) ENVIRONMENT=openshift && \
		ENVIRONMENT=openshift DEPLOY_LLM_D=false deploy/install.sh --undeploy

## Deploy llm-d and WVA on Kubernetes with the specified image.
.PHONY: deploy-llm-d-wva-on-k8s
deploy-llm-d-wva-on-k8s: manifests kustomize ## Deploy llm-d and WVA on Kubernetes with the specified image.
	@echo "Deploying llm-d and WVA on Kubernetes with image: $(IMG)"
	@echo "Target namespace: $(or $(NAMESPACE),workload-variant-autoscaler-system)"
	NAMESPACE=$(or $(NAMESPACE),workload-variant-autoscaler-system) IMG=$(IMG) ENVIRONMENT=kubernetes DEPLOY_LLM_D=true ./deploy/install.sh

## Deploy WVA on Kubernetes with the specified image.
.PHONY: deploy-wva-on-k8s
deploy-wva-on-k8s: manifests kustomize ## Deploy WVA on Kubernetes with the specified image.
	@echo "Deploying WVA on Kubernetes with image: $(IMG)"
	@echo "Target namespace: $(or $(NAMESPACE),workload-variant-autoscaler-system)"
	NAMESPACE=$(or $(NAMESPACE),workload-variant-autoscaler-system) IMG=$(IMG) ENVIRONMENT=kubernetes DEPLOY_LLM_D=false ./deploy/install.sh

## Undeploy llm-d and WVA from Kubernetes.
.PHONY: undeploy-llm-d-wva-on-k8s
undeploy-llm-d-wva-on-k8s:
	@echo ">>> Undeploying llm-d and workload-variant-autoscaler from Kubernetes"
	export KIND=$(KIND) KUBECTL=$(KUBECTL) ENVIRONMENT=kubernetes && \
		ENVIRONMENT=kubernetes DEPLOY_LLM_D=true deploy/install.sh --undeploy

## Undeploy WVA from Kubernetes.
.PHONY: undeploy-wva-on-k8s
undeploy-wva-on-k8s:
	@echo ">>> Undeploying workload-variant-autoscaler from Kubernetes"
	export KIND=$(KIND) KUBECTL=$(KUBECTL) ENVIRONMENT=kubernetes && \
		ENVIRONMENT=kubernetes DEPLOY_LLM_D=false deploy/install.sh --undeploy

# TODO(user): To use a different vendor for e2e tests, modify the setup under 'tests/e2e'.
# The default setup assumes Kind is pre-installed and builds/loads the Manager Docker image locally.
# CertManager is installed by default; skip with:
# - CERT_MANAGER_INSTALL_SKIP=true
.PHONY: test-e2e
test-e2e: manifests generate fmt vet ## Run the e2e tests. Expected an isolated environment using Kind.
	@command -v $(KIND) >/dev/null 2>&1 || { \
		echo "Kind is not installed. Please install Kind manually."; \
		exit 1; \
	}
	$(eval FOCUS_ARGS := $(if $(FOCUS),-ginkgo.focus="$(FOCUS)",))
	$(eval SKIP_ARGS := $(if $(SKIP),-ginkgo.skip="$(SKIP)",))
	export KUBECONFIG=$(KUBECONFIG) K8S_EXPECTED_VERSION=$(K8S_VERSION) && go test ./test/e2e/ -timeout 30m -v -ginkgo.v $(FOCUS_ARGS) $(SKIP_ARGS)

# E2E tests on OpenShift cluster
# Requires KUBECONFIG and pre-deployed infrastructure.
.PHONY: test-e2e-openshift
test-e2e-openshift: ## Run the e2e tests on OpenShift. Requires KUBECONFIG and pre-deployed infrastructure.
	@echo "Running e2e tests on OpenShift cluster..."
	@if [ -z "$(KUBECONFIG)" ]; then \
		echo "Error: KUBECONFIG is not set"; \
		exit 1; \
	fi
	$(eval FOCUS_ARGS := $(if $(FOCUS),-ginkgo.focus="$(FOCUS)",))
	$(eval SKIP_ARGS := $(if $(SKIP),-ginkgo.skip="$(SKIP)",))

	CONTROLLER_NAMESPACE=$(CONTROLLER_NAMESPACE) \
	MONITORING_NAMESPACE=$(MONITORING_NAMESPACE) \
	LLMD_NAMESPACE=$(LLMD_NAMESPACE) \
	GATEWAY_NAME=$(GATEWAY_NAME) \
	MODEL_ID=$(MODEL_ID) \
	DEPLOYMENT=$(DEPLOYMENT) \
	REQUEST_RATE=$(REQUEST_RATE) \
	NUM_PROMPTS=$(NUM_PROMPTS) \
	KUBECONFIG=$(KUBECONFIG) \
	go test ./test/e2e-openshift/ -timeout 30m -v -ginkgo.v $(FOCUS_ARGS) $(SKIP_ARGS)

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

## Tool Versions
KUSTOMIZE_VERSION ?= v5.6.0
CONTROLLER_TOOLS_VERSION ?= v0.17.2
#ENVTEST_VERSION is the version of controller-runtime release branch to fetch the envtest setup script (i.e. release-0.20)
ENVTEST_VERSION ?= $(shell go list -m -f "{{ .Version }}" sigs.k8s.io/controller-runtime | awk -F'[v.]' '{printf "release-%d.%d", $$2, $$3}')
#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell go list -m -f "{{ .Version }}" k8s.io/api | awk -F'[v.]' '{printf "1.%d", $$3}')
GOLANGCI_LINT_VERSION ?= v1.63.4

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
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

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
