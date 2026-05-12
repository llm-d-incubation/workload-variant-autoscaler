#!/usr/bin/env bash
# Shared deploy path for llm-d-infra nightly reusables (CKS + OpenShift).
# Invoked via: make nightly-deploy-wva-guide (sets LLM_D_NIGHTLY_PLATFORM=cks|openshift).
set -euo pipefail

ROOT="${1:-.}"
cd "$ROOT"

PLATFORM="${LLM_D_NIGHTLY_PLATFORM:?LLM_D_NIGHTLY_PLATFORM must be cks or openshift}"
if [[ "$PLATFORM" != cks && "$PLATFORM" != openshift ]]; then
	echo "LLM_D_NIGHTLY_PLATFORM must be cks or openshift (got: $PLATFORM)" >&2
	exit 1
fi

if [[ -n "${GITHUB_WORKSPACE:-}" && ! -d llm-d && -d "$GITHUB_WORKSPACE/guides" ]]; then
	ln -sfn "$GITHUB_WORKSPACE" llm-d
	echo "Symlinked $ROOT/llm-d -> $GITHUB_WORKSPACE"
fi

if [[ "$PLATFORM" == cks ]]; then
	for f in deploy/lib/deploy_prometheus_kube_stack.sh deploy/kubernetes/install.sh; do
		if [[ -f "$f" ]] && grep -q 'helm upgrade --install kube-prometheus-stack' "$f" && ! grep -q 'nodeExporter.enabled=false' "$f"; then
			perl -pi -e 's/helm upgrade --install kube-prometheus-stack/helm upgrade --install kube-prometheus-stack --set nodeExporter.enabled=false/g' "$f"
			echo "Patched $f: nodeExporter.enabled=false (CKS nightly)"
		fi
	done
fi

export INSTALL_GATEWAY_CTRLPLANE="${INSTALL_GATEWAY_CTRLPLANE:-false}"
export BENCHMARK_MODE="${BENCHMARK_MODE:-false}"
export NAMESPACE_SCOPED="${NAMESPACE_SCOPED:-false}"
export DEPLOY_WVA="${DEPLOY_WVA:-true}"
export DEPLOY_PROMETHEUS="${DEPLOY_PROMETHEUS:-true}"
export DEPLOY_PROMETHEUS_ADAPTER="${DEPLOY_PROMETHEUS_ADAPTER:-true}"
export SCALER_BACKEND="${SCALER_BACKEND:-keda}"
export ENABLE_SCALE_TO_ZERO="${ENABLE_SCALE_TO_ZERO:-true}"
export POOL_GROUP="${POOL_GROUP:-inference.networking.k8s.io}"
# Nightly path defaults to llm-d main unless the workflow pins LLM_D_RELEASE.
export LLM_D_RELEASE="${LLM_D_RELEASE:-main}"
export LLM_D_PROJECT="${LLM_D_PROJECT:-llm-d}"
export WVA_PROJECT="${WVA_PROJECT:-$ROOT}"
export CLIENT_PREREQ_DIR="${CLIENT_PREREQ_DIR:-$WVA_PROJECT/$LLM_D_PROJECT/helpers/client-setup}"
export GATEWAY_PREREQ_DIR="${GATEWAY_PREREQ_DIR:-$WVA_PROJECT/$LLM_D_PROJECT/helpers/gateway-provider}"
export LLMD_PATCH_EPP_FLOW_CONTROL="${LLMD_PATCH_EPP_FLOW_CONTROL:-true}"
export LLMD_SKIP_INFERENCE_OBJECTIVE="${LLMD_SKIP_INFERENCE_OBJECTIVE:-true}"

if [[ "$PLATFORM" == openshift ]]; then
	export MONITORING_NAMESPACE="${MONITORING_NAMESPACE:-openshift-user-workload-monitoring}"
	export WVA_METRICS_SECURE="${WVA_METRICS_SECURE:-false}"
	export ENVIRONMENT=openshift
	./deploy/install.sh \
		--release-name "${WVA_RELEASE_NAME:-workload-variant-autoscaler}" \
		--environment openshift
	./deploy/install-llmd-infra.sh -e openshift
else
	export ENVIRONMENT=kubernetes
	./deploy/install.sh \
		--release-name "${WVA_RELEASE_NAME:-workload-variant-autoscaler}"
	./deploy/install-llmd-infra.sh -e kubernetes
fi
