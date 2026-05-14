#!/usr/bin/env bash
#
# Apply or delete a second WVA controller for dual-controller smoke tests.
# Usage: WVA_PROJECT=... WVA_NS=... (and other vars required by wva_kustomize_apply) $0 apply|delete
#
set -euo pipefail
ACTION="${1:?apply or delete}"
WVA_PROJECT="${WVA_PROJECT:?}"
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'
_lib_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# wva_kustomize.sh uses log_* from common.sh (install.sh normally sources it first).
# shellcheck source=lib/common.sh
source "${_lib_dir}/common.sh"
# shellcheck source=lib/wva_kustomize.sh
source "${_lib_dir}/wva_kustomize.sh"
case "$ACTION" in
apply) wva_kustomize_apply ;;
delete) wva_kustomize_delete ;;
*)
    echo "usage: $0 apply|delete" >&2
    exit 1
    ;;
esac
