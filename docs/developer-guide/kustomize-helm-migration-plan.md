# Kustomize-first controller install — plan and status

> **Note:** This document is temporary. It will be removed once Helm is fully deprecated.

This document is the **migration plan and drift checklist** for moving the WVA **controller** off Helm and onto Kustomize, while keeping Helm where it still owns other stack pieces. Operational learnings from local Kind e2e and CI alignment are captured here so `deploy/README.md` can stay procedural and this file stays the **source of truth for “what we intended vs what shipped.”**

## Goals (original intent)

1. **Install the WVA controller with Kustomize** (`kubectl apply -k` / `deploy/lib/wva_kustomize.sh`) using bundles under `config/deploy/`, not the Helm chart’s controller templates.
2. **Deprecate Helm for controller install**; keep the chart for OCI distribution, migration, and **chart-only** workloads (e.g. VA/HPA templates in some flows).
3. **Preserve behavior** for monitoring, llm-d (helmfile), scaler backends, e2e, and OpenShift CI.
4. **Document** the supported path and deprecation; avoid silent behavior changes for operators.

## In scope vs out of scope

| In scope | Out of scope (unchanged) |
|----------|---------------------------|
| WVA Deployment, RBAC, metrics Service/ServiceMonitor, saturation + variant ConfigMaps, EPP metrics token wiring | llm-d releases via **helmfile** (`deploy/install-llmd-infra.sh`) |
| `deploy/install.sh` + `deploy/lib/infra_wva.sh` + `deploy/lib/wva_kustomize.sh` + `deploy/lib/cleanup.sh` | kube-prometheus-stack, LWS, Prometheus Adapter installs (Helm) |
| `config/deploy/kubernetes`, `config/deploy/openshift`, `config/deploy/wva-controller*` | Gateway control plane unless we explicitly change that story |
| CI: Kind smoke/full, OpenShift e2e deploy steps | Removing the chart from the repo or stopping chart publish |

## Implementation checklist — aligned with the tree (drift audit)

Use this as a **merge / review gate**: if the codebase diverges, update either the implementation or this section.

| Item | Status | Where |
|------|--------|--------|
| Controller bundles (`config/deploy/wva-controller`, `wva-controller-openshift`) include `../../default` (RBAC + manager + metrics); CRDs applied separately in `wva_kustomize_apply` | Done | `config/deploy/wva-controller/kustomization.yaml`, `deploy/lib/wva_kustomize.sh` |
| Platform entrypoints `config/deploy/kubernetes`, `config/deploy/openshift` compose CRD + controller as documented | Done | `config/deploy/kubernetes/kustomization.yaml`, `config/deploy/openshift/kustomization.yaml` |
| `deploy_wva_controller` uses Kustomize; legacy `helm uninstall` no-op then apply | Done | `deploy/lib/infra_wva.sh` |
| **`IMG` / `WVA_IMAGE_*` reliably set the manager image** after inner `config/manager` resolves `controller` → full registry path (outer `images: name: controller` alone is insufficient) | Done | `deploy/lib/wva_kustomize.sh` — strategic-merge patch sets `spec.template.spec.containers[0].image` |
| Multi-instance / e2e secondary: `nameSuffix` + `CONTROLLER_INSTANCE` + ConfigMap **literal** env names (`CONFIG_MAP_NAME`, `SATURATION_CONFIG_MAP_NAME`) | Done | `deploy/lib/wva_kustomize.sh` |
| EPP metrics projected token Secret: SA annotation matches **suffixed** ServiceAccount | Done | JSON 6902 patch in `wva_kustomize.sh` when CI suffix present |
| Metrics auth `ClusterRoleBinding` subject rewrites with `namePrefix` / `nameSuffix` | Done | `config/rbac/metrics_auth_role_binding.yaml` |
| `install-llmd-infra` skips WVA in helmfile when `DEPLOY_WVA=true`; poolGroup / infra alignment unchanged in intent | Done | `deploy/lib/infra_llmd.sh` |
| Post-deploy Service/ServiceMonitor label alignment uses **jq 1.6-safe** patches (no `$label`; dynamic key for `llm-d.ai/model`) | Done | `deploy/lib/infra_llmd.sh` |
| OpenShift CI passes **full `IMG`** matching `ghcr.io/${{ github.repository }}:$tag` into `install.sh` | Done | `.github/workflows/ci-e2e-openshift.yaml` |
| Kind smoke + full e2e set **`WVA_E2E_REPO_ROOT`** (dual-controller / `e2e_secondary_wva.sh`) | Done | `.github/workflows/ci-pr-checks.yaml` |
| Saturation tuning via **`config/deploy/overlays/{e2e-simulator,benchmark}`** + **`WVA_SATURATION_OVERLAY`** (no env-merge in `wva_kustomize.sh`) | Done | `deploy/lib/wva_kustomize.sh`, workflows, `Makefile` |
| Kind smoke + full unset poisoned **`PROM_CA_CERT_PATH`** before `make` | Done | `.github/workflows/ci-pr-checks.yaml` |
| `deploy/install.sh` documents controller = Kustomize only; no dead `VALUES_FILE` / chart path for the manager | Done | `deploy/install.sh`, `deploy/openshift/install.sh`, `deploy/lib/infra_wva.sh` |
| Cleanup deletes Kustomize controller + chart no-op | Done | `deploy/lib/cleanup.sh`, OpenShift workflow `wva_kustomize_delete` |

**Drift:** If any row regresses (e.g. image reverts to chart-only tagging without the merge patch), treat it as a **release blocker** for Kustomize-first claims in `deploy/README.md`.

## Operational learnings (local + CI)

1. **`PROM_CA_CERT_PATH`**: If set to a non-file (e.g. `/dev/null`), `deploy/install.sh` can fail extracting the Prometheus CA and **never run** `install-llmd-infra.sh` — e2e then fails in `BeforeSuite` (missing llm-d CRDs). **Mitigation:** unset before install; CI does `unset PROM_CA_CERT_PATH` in Kind/OpenShift deploy steps.
2. **`IMG`**: E2e and operators must ensure the **running** Deployment uses the built image (`kubectl get deploy … -o jsonpath='{.spec…image}'`). The merge patch in `wva_kustomize.sh` exists specifically so `IMG` is not ignored.
3. **`WVA_E2E_REPO_ROOT`**: Dual-controller paths need the repository root for `deploy/lib/e2e_secondary_wva.sh`. `make test-e2e-*` exports **`WVA_E2E_REPO_ROOT=$(CURDIR)`**; CI sets it to the Actions checkout path. Deprecated fallbacks in `e2eRepoRoot()`: **`WVA_E2E_CHART_PATH`**, then **`os.Getwd()`**.
4. **`jq` 1.6** (Ubuntu runners): avoid `--arg label` / `$label`; use e.g. `--arg model_label` and dynamic object keys for dotted label names.
5. **`WVA_SATURATION_OVERLAY`**: Saturation tuning for CI/simulator vs benchmark lives in **`config/deploy/overlays/`**; `wva_kustomize_controller_bundle_dir` selects **`overlays/<name>/{kubernetes,openshift}`** when set (otherwise the base **`wva-controller*`** bundle).

## Documentation map (avoid duplicate “plans”)

| Audience | Doc |
|-----------|-----|
| Operators / install flow | [`deploy/README.md`](../../deploy/README.md) |
| E2e env, CI triggers, local simulation | [Testing guide](testing.md) |
| **This file** | Plan, status, drift checklist, learnings |

## Optional follow-ups (not required for “migration done”)

- Narrow or split jobs if you want CI that only validates Kustomize WVA without full llm-d.
- Consider `env -u PROM_CA_CERT_PATH` in `Makefile` e2e targets for local parity with CI (today CI unsets in workflow; Makefile leaves env to the user).
