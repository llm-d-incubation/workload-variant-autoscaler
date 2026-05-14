#!/usr/bin/env bash
#
# Apply or delete OpenShift CI "Model B" client-only manifests (VA, HPA, vLLM Service/ServiceMonitor)
# via kubectl apply -k from config/deploy/ci/openshift-model-b-wva/, mirroring the former
# charts/workload-variant-autoscaler Helm client-only install.
#
# Required env:
#   MODEL_B_RELEASE, LLMD_NAMESPACE_B, MODEL_ID, ACCELERATOR_TYPE, CONTROLLER_INSTANCE
# Optional:
#   HPA_STABILIZATION_SECONDS (default 240)
#   WVA_PROJECT (repo root; default: parent of deploy/lib)
#
set -euo pipefail

ci_model_b_wva_fullname() {
  local release="${1:?MODEL_B_RELEASE required}"
  local cn="workload-variant-autoscaler"
  local out
  case "$release" in
    *"$cn"*) out="$release" ;;
    *) out="${release}-${cn}" ;;
  esac
  echo -n "$out" | cut -c1-63 | sed 's/-$//'
}

ci_model_b_wva_substitute_and_apply() {
  local src="${1:?}"
  local tmp="${2:?}"
  export WVA_FULLNAME
  WVA_FULLNAME="$(ci_model_b_wva_fullname "${MODEL_B_RELEASE}")"
  export HPA_STABILIZATION_SECONDS="${HPA_STABILIZATION_SECONDS:-240}"
  export MODEL_B_RELEASE LLMD_NAMESPACE_B MODEL_ID ACCELERATOR_TYPE CONTROLLER_INSTANCE

  python3 - "$src" "$tmp" <<'PY'
import pathlib
import os
import shutil
import sys

src = pathlib.Path(sys.argv[1])
dst = pathlib.Path(sys.argv[2])
shutil.rmtree(dst, ignore_errors=True)
shutil.copytree(src, dst)

subs = {
    "__LLMD_NAMESPACE_B__": os.environ["LLMD_NAMESPACE_B"],
    "__MODEL_B_RELEASE__": os.environ["MODEL_B_RELEASE"],
    "__WVA_FULLNAME__": os.environ["WVA_FULLNAME"],
    "__MODEL_ID__": os.environ["MODEL_ID"],
    "__ACCELERATOR_TYPE__": os.environ["ACCELERATOR_TYPE"],
    "__CONTROLLER_INSTANCE__": os.environ["CONTROLLER_INSTANCE"],
    "__HPA_STABILIZATION_SECONDS__": os.environ.get("HPA_STABILIZATION_SECONDS", "240"),
}
for path in dst.rglob("*.yaml"):
    text = path.read_text()
    for k, v in subs.items():
        text = text.replace(k, v)
    path.write_text(text)
PY
  kubectl apply -k "$tmp"
}

ci_model_b_wva_apply() {
  : "${MODEL_B_RELEASE:?MODEL_B_RELEASE is required}"
  : "${LLMD_NAMESPACE_B:?LLMD_NAMESPACE_B is required}"
  : "${MODEL_ID:?MODEL_ID is required}"
  : "${ACCELERATOR_TYPE:?ACCELERATOR_TYPE is required}"
  : "${CONTROLLER_INSTANCE:?CONTROLLER_INSTANCE is required}"

  local root="${WVA_PROJECT:-}"
  if [ -z "$root" ]; then
    root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
  fi
  local base="${root}/config/deploy/ci/openshift-model-b-wva"
  if [ ! -f "${base}/kustomization.yaml" ]; then
    echo "error: missing kustomize base at ${base}" >&2
    return 1
  fi

  local tmp
  tmp="$(mktemp -d "${TMPDIR:-/tmp}/ci-model-b-wva.XXXXXX")"
  # Expand $tmp when registering the trap (set -u: avoid referencing unset tmp on EXIT).
  trap "rm -rf '${tmp}'" EXIT

  ci_model_b_wva_substitute_and_apply "$base" "$tmp"
}

ci_model_b_wva_delete() {
  : "${MODEL_B_RELEASE:?MODEL_B_RELEASE is required}"
  : "${LLMD_NAMESPACE_B:?LLMD_NAMESPACE_B is required}"

  kubectl delete servicemonitor,svc,va,hpa \
    -l "app.kubernetes.io/instance=${MODEL_B_RELEASE},app.kubernetes.io/name=workload-variant-autoscaler,app.kubernetes.io/managed-by=kustomize" \
    -n "${LLMD_NAMESPACE_B}" --ignore-not-found
}

main() {
  case "${1:-}" in
    apply) ci_model_b_wva_apply ;;
    delete) ci_model_b_wva_delete ;;
    *)
      echo "usage: $0 apply|delete" >&2
      return 1
      ;;
  esac
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  main "$@"
fi
