#!/bin/bash
# ==============================================================================
# Automates the end-to-end benchmarking of the Workload Variant Autoscaler (WVA)
# Intended for execution natively within a CI/CD environment.
# ==============================================================================

# Fail immediately if a command fails, and echo all commands for CI logs
set -e
set -x

# --- Configuration Variables ---
NAMESPACE="default"
MODEL="Qwen/Qwen3-0.6B"
SCENARIO="inference-scheduling"
WORKLOAD_PROFILE="chatbot_synthetic"
DIRECT_HPA=0
WVA_THRESHOLD_CONFIG=""

while getopts ":n:m:s:w:dt:" opt; do
  case $opt in
    n) NAMESPACE="$OPTARG" ;;
    m) MODEL="$OPTARG" ;;
    s) SCENARIO="$OPTARG" ;;
    w) WORKLOAD_PROFILE="$OPTARG" ;;
    d) DIRECT_HPA=1 ;;
    t) WVA_THRESHOLD_CONFIG="$OPTARG" ;;
    \?) echo "Invalid option -$OPTARG" >&2; exit 1 ;;
  esac
done

# Absolute paths based on typical repository structure relative to hack/benchmark/run
BASE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
REPO_ROOT="$BASE_DIR/llm-d-benchmark"
export LLMDBENCH_MAIN_DIR="${REPO_ROOT}"
export LLMDBENCH_CONTROL_CALLER="run_ci_benchmark.sh"
SCENARIO_PATH="${REPO_ROOT}/scenarios/guides/inference-scheduling.sh"

if [ -f "${REPO_ROOT}/setup/env.sh" ]; then
    echo "Sourcing environment variables from ${REPO_ROOT}/setup/env.sh..."
    source "${REPO_ROOT}/setup/env.sh"
    # Force admin designation since env.sh's 'oc auth whoami' check is broken on OpenShift
    # Without this, env.sh silently disables EPP Prometheus ServiceMonitors
    export LLMDBENCH_USER_IS_ADMIN=1
    
    # Override WVA Image/Chart version since OCI repo tag lookup fails inside env.sh
    export LLMDBENCH_WVA_CHART_VERSION="0.6.0-rc2"
    export LLMDBENCH_WVA_IMAGE_TAG="v0.6.0-rc2"
else
    echo "⚠️ Warning: setup/env.sh not found. Ensure required variables (e.g., LLMDBENCH_HF_TOKEN) are exported."
fi

echo "============================================================================="
echo "▶️ STEP 1: Teardown Existing Environment"
echo "============================================================================="
"${REPO_ROOT}/setup/teardown.sh" -c "${SCENARIO_PATH}" -p "${NAMESPACE}" --deep

echo "Waiting for namespace ${NAMESPACE} to fully clear..."
# Polling loop to ensure all decode pods are gone
MAX_WAIT_CLEAN=60 # seconds
for (( i=1; i<=MAX_WAIT_CLEAN; i++ )); do
    POD_COUNT=$(oc get pods -n "${NAMESPACE}" -l "model=${MODEL##*/}" --no-headers 2>/dev/null | wc -l || true)
    if [ "${POD_COUNT}" -eq 0 ]; then
        echo "✅ Namespace cleanly purged."
        break
    fi
    echo "Wait ${i}/${MAX_WAIT_CLEAN}: Waiting for decode pods to terminate..."
    sleep 5
done


echo "============================================================================="
echo "▶️ STEP 2: Standup Stack with WVA"
echo "============================================================================="

# Temporarily inject exactly 0 prefill and 1 decode replica into the scenario file on-the-fly
echo "Injecting 0 prefill and 1 decode replica override into ${SCENARIO_PATH}..."
cp "${SCENARIO_PATH}" "${SCENARIO_PATH}.bak"

# Trap guarantees restoration of the file even if standup script crashes violently
trap 'mv "${SCENARIO_PATH}.bak" "${SCENARIO_PATH}" 2>/dev/null || true' EXIT INT TERM

sed -e 's/LLMDBENCH_VLLM_MODELSERVICE_PREFILL_REPLICAS=.*/LLMDBENCH_VLLM_MODELSERVICE_PREFILL_REPLICAS=0/' \
    -e 's/LLMDBENCH_VLLM_MODELSERVICE_DECODE_REPLICAS=.*/LLMDBENCH_VLLM_MODELSERVICE_DECODE_REPLICAS=1/' "${SCENARIO_PATH}" > "${SCENARIO_PATH}.tmp"
mv "${SCENARIO_PATH}.tmp" "${SCENARIO_PATH}"

"${REPO_ROOT}/setup/standup.sh" -p "${NAMESPACE}" -m "${MODEL}" -c "${SCENARIO}" --wva

# Revert the scenario file back to its pristine state immediately upon success
echo "Reverting ${SCENARIO_PATH} to original state..."
mv "${SCENARIO_PATH}.bak" "${SCENARIO_PATH}"
trap - EXIT INT TERM

if [ "$DIRECT_HPA" -eq 1 ]; then
    echo "============================================================================="
    echo "▶️ OPTIONAL: Applying Direct HPA Override"
    echo "============================================================================="
    
    # Find the matching decode deployment name dynamically by structural suffix
    DECODE_DEPLOY=$(oc get deploy -n "${NAMESPACE}" -o custom-columns=":metadata.name" --no-headers | grep "\-decode" | head -n 1)
    
    if [ -z "$DECODE_DEPLOY" ]; then
        echo "❌ ERROR: Could not dynamically find decode deployment for model ${MODEL} in namespace ${NAMESPACE}"
        exit 1
    fi
    
    echo "Dynamically targeting deployment: ${DECODE_DEPLOY} for Direct HPA..."
    sed "s/TARGET_DEPLOYMENT_NAME/$DECODE_DEPLOY/g" "${BASE_DIR}/hack/benchmark/run/bypass_wva_direct_hpa.yaml" | oc apply -n "${NAMESPACE}" -f -
    
    echo "✅ Direct HPA deployed and WVA Controller scaled to 0."
elif [ -n "$WVA_THRESHOLD_CONFIG" ]; then
    echo "============================================================================="
    echo "▶️ OPTIONAL: Applying Custom WVA Threshold ConfigMap"
    echo "============================================================================="
    if [ -f "$WVA_THRESHOLD_CONFIG" ]; then
        oc apply -f "$WVA_THRESHOLD_CONFIG" -n "${NAMESPACE}"
        echo "✅ Custom ConfigMap applied successfully. Bouncing WVA controller..."
        oc delete pod -l app.kubernetes.io/name=workload-variant-autoscaler -n "${NAMESPACE}" || true
    else
        echo "❌ ERROR: Custom configmap file not found at: $WVA_THRESHOLD_CONFIG"
        exit 1
    fi
fi

echo "============================================================================="
echo "▶️ STEP 3: Verify HPA Scale-to-One/Zero Logic"
echo "============================================================================="
echo "Waiting for HPA to become available in namespace ${NAMESPACE}..."
HPA_TARGET_NAME="workload-variant-autoscaler-hpa"

HPA_NAME=""
MAX_WAIT_HPA=30 # seconds
for (( i=1; i<=MAX_WAIT_HPA; i++ )); do
    # Suppress errors on stderr but grab the output if successful
    temp_hpa=$(oc get hpa "${HPA_TARGET_NAME}" -n "${NAMESPACE}" -o jsonpath="{.metadata.name}" 2>/dev/null || true)
    if [ -n "$temp_hpa" ]; then
        HPA_NAME="$temp_hpa"
        echo "✅ HPA found: ${HPA_NAME}"
        break
    fi
    echo "Wait ${i}/${MAX_WAIT_HPA}: HPA ${HPA_TARGET_NAME} not found yet. Retrying..."
    sleep 10
done

if [ -z "$HPA_NAME" ]; then
    echo "❌ ERROR: HPA was not created within timeout."
    exit 1
fi

echo "Patching HPA ${HPA_NAME} scaleUp window to 0s and scaleDown window to 240s..."
oc patch hpa "${HPA_NAME}" -n "${NAMESPACE}" --type=merge -p '{"spec":{"behavior":{"scaleUp":{"stabilizationWindowSeconds":0},"scaleDown":{"stabilizationWindowSeconds":240}}}}'

if [ "$DIRECT_HPA" -eq 1 ]; then
    echo "⏩ Bypassing ScalingActive metrics check for Direct HPA (cold-start metrics may not exist until workload initiates)."
else
    echo "Waiting for HPA to successfully connect to external metrics (ScalingActive=True)..."
    MAX_WAIT_METRICS=60 # 60 * 5s = 300 seconds
    METRICS_READY=false
    for (( i=1; i<=MAX_WAIT_METRICS; i++ )); do
        # Suppress errors if condition array bounds evaluate strangely during initialization
        HPA_STATUS=$(oc get hpa "${HPA_NAME}" -n "${NAMESPACE}" -o jsonpath='{.status.conditions[?(@.type=="ScalingActive")].status}' 2>/dev/null || true)
        
        if [ "${HPA_STATUS}" == "True" ]; then
            echo "✅ HPA metrics connection is healthy!"
            METRICS_READY=true
            break
        fi
        echo "Wait ${i}/${MAX_WAIT_METRICS}: HPA metrics not ready yet (status=${HPA_STATUS:-unknown}). Waiting..."
        sleep 5
    done

    if [ "$METRICS_READY" = false ]; then
        echo "❌ ERROR: HPA failed to fetch metrics within timeout. Check Prometheus Adapter."
        exit 1
    fi
fi

MAX_WAIT_SCALE=120
SCALED_DOWN=false
for (( i=1; i<=MAX_WAIT_SCALE; i++ )); do
    CURRENT_REPLICAS=$(oc get hpa "${HPA_NAME}" -n "${NAMESPACE}" -o jsonpath='{.status.currentReplicas}')
    if [ "${CURRENT_REPLICAS}" -eq 1 ] || [ "${CURRENT_REPLICAS}" -eq 0 ]; then
        echo "✅ HPA successfully scaled down to ${CURRENT_REPLICAS} replica(s)."
        SCALED_DOWN=true
        break
    fi
    echo "Wait ${i}/${MAX_WAIT_SCALE}: Currently at ${CURRENT_REPLICAS} replicas. Waiting for scale down..."
    sleep 5
done

if [ "$SCALED_DOWN" = false ]; then
    echo "❌ ERROR: HPA failed to scale down to 1 or 0 within timeout. Scale-to-Zero logic might be broken."
    exit 1
fi


echo "============================================================================="
echo "▶️ STEP 4: Reconcile HPA and Variant (VA) Objectives"
echo "============================================================================="
if [ "$DIRECT_HPA" -eq 1 ]; then
    echo "⏩ Bypassing Variant alignment since WVA is completely disabled (Direct HPA Mode)."
else
    HPA_MAX=$(oc get hpa "${HPA_NAME}" -n "${NAMESPACE}" -o jsonpath='{.spec.maxReplicas}')
    VA_NAME=$(oc get va -n "${NAMESPACE}" -o jsonpath="{.items[0].metadata.name}")
    VA_MAX=$(oc get va "${VA_NAME}" -n "${NAMESPACE}" -o jsonpath='{.spec.maxReplicas}')

    echo "Detected HPA maxReplicas: ${HPA_MAX}"
    echo "Detected VA maxReplicas: ${VA_MAX}"

    if [ "${HPA_MAX}" != "${VA_MAX}" ]; then
        echo "⚠️ Discrepancy detected between HPA and VA! Patching Variant to align with HPA..."
        oc patch va "${VA_NAME}" -n "${NAMESPACE}" --type=merge -p "{\"spec\": {\"maxReplicas\": ${HPA_MAX}}}"
        echo "✅ Variant patched successfully."
    else
        echo "✅ HPA and Variant maxReplicas are perfectly aligned."
    fi
fi


echo "============================================================================="
echo "▶️ STEP 5: Execute Benchmark"
echo "============================================================================="

if [ "$WORKLOAD_PROFILE" == "sharegpt" ]; then
    echo "Copying local sharegpt_data.jsonl to cluster PVC (/requests)..."
    DATA_ACCESS_POD=$(oc get pod -l role=llm-d-benchmark-data-access -n "$NAMESPACE" --no-headers -o 'jsonpath={.items[0].metadata.name}')
    if [ -n "$DATA_ACCESS_POD" ]; then
        oc cp "$REPO_ROOT/workload/profiles/guidellm/sharegpt_data.jsonl" "$NAMESPACE/$DATA_ACCESS_POD:/requests/sharegpt_data.jsonl"
        echo "✅ Dataset file successfully copied to the cluster."
    else
        echo "⚠️  Warning: Could not find data-access pod to upload dataset!"
    fi
fi

echo "Triggering GuideLLM load generator in background..."
cd "$REPO_ROOT" || exit 1

SCENARIO_INJECT="$WORKLOAD_PROFILE"

# --- Custom Scenario Injection ---
CUSTOM_SCENARIO_DIR="$BASE_DIR/hack/benchmark/scenarios/$WORKLOAD_PROFILE"
if [ -d "$CUSTOM_SCENARIO_DIR" ]; then
    echo "Detected custom scenario for profiles: $WORKLOAD_PROFILE, copying to upstream workload directory..."
    mkdir -p "${REPO_ROOT}/workload/profiles/guidellm"
    cp "$CUSTOM_SCENARIO_DIR"/*.yaml.in "${REPO_ROOT}/workload/profiles/guidellm/"
    
    # Extract the base name of the first yaml file in that directory for run.sh injection
    FIRST_YAML=$(ls "$CUSTOM_SCENARIO_DIR"/*.yaml.in | head -n 1)
    BASE_YAML=$(basename "$FIRST_YAML" .yaml.in)
    SCENARIO_INJECT="$BASE_YAML"
    echo "Resolved custom scenario file injection target: $SCENARIO_INJECT"
fi

export LLMDBENCH_RUN_EXPERIMENT_ID=$(date +%s)
./run.sh -l guidellm -w "$SCENARIO_INJECT" -p "$NAMESPACE" -m "$MODEL" -c "$SCENARIO" -f &
RUN_PID=$!

echo "Waiting for benchmark pod to initialize..."
LATEST_DIR=""
for i in {1..30}; do
    POD_NAME=$(oc get pods -n "${NAMESPACE}" -l function=load_generator --no-headers -o custom-columns=":metadata.name" --sort-by=.metadata.creationTimestamp 2>/dev/null | tail -1)
    if [ -n "$POD_NAME" ]; then
        DIR_NAME=$(oc get pod "$POD_NAME" -n "${NAMESPACE}" -o jsonpath='{.spec.containers[0].env[?(@.name=="LLMDBENCH_RUN_EXPERIMENT_RESULTS_DIR")].value}' 2>/dev/null || true)
        if [ -n "$DIR_NAME" ]; then
            LATEST_DIR="$DIR_NAME"
            echo "✅ Target PVC Directory Captured: $LATEST_DIR"
            break
        fi
    fi
    sleep 5
done

if [ -z "$LATEST_DIR" ]; then
    echo "⚠️  Warning: Directory capture failed during initialization. Will fallback."
fi

echo "Waiting for benchmark to complete execution..."
wait $RUN_PID
if [ $? -ne 0 ]; then
    echo "❌ ERROR: GuideLLM Benchmark execution failed!"
    exit 1
fi

echo "============================================================================="
echo "▶️ STEP 6: Automating Data Extraction & Visualization"
echo "============================================================================="

export PYTHONDONTWRITEBYTECODE=1

# Establish deterministic Python virtual environment
VENV_DIR="$BASE_DIR/hack/benchmark/.venv"
if [ ! -d "$VENV_DIR" ]; then
    echo "Creating fully isolated Python virtual environment..."
    python3 -m venv "$VENV_DIR"
    source "$VENV_DIR/bin/activate"
    pip install --upgrade pip
    pip install -r "$BASE_DIR/hack/benchmark/requirements.txt"
else
    source "$VENV_DIR/bin/activate"
fi

# Extract latest raw json logs
if [ -z "$LATEST_DIR" ]; then
    echo "Locating latest benchmark results in PVC fallback..."
    LATEST_DIR=$(oc exec access-to-harness-data-workload-pvc -n "${NAMESPACE}" -- sh -c "ls -td /requests/guidellm_* | head -n 1")
    if [ -z "$LATEST_DIR" ]; then
        echo "❌ Error: Could not find result directory in PVC."
        exit 1
    fi
fi

EXP_DATA_DIR="$BASE_DIR/exp_data/$(basename $LATEST_DIR)"
echo "Copying out $LATEST_DIR to $EXP_DATA_DIR ..."
mkdir -p "$EXP_DATA_DIR"
oc rsync -n "${NAMESPACE}" "access-to-harness-data-workload-pvc:${LATEST_DIR}/" "${EXP_DATA_DIR}/" --include='*.yaml' --include='*.json' --exclude='*' || echo "⚠️ Warning: oc rsync threw non-zero exit, proceeding..."

# Jump to Python extract scripts
EXTRACT_DIR="$BASE_DIR/hack/benchmark/extract"
cd "$EXTRACT_DIR" || exit 1

DUMP_ARGS="-r $EXP_DATA_DIR -n $NAMESPACE"
REPORT_ARGS="-r $EXP_DATA_DIR -n $NAMESPACE -w 60m --scenario $SCENARIO_INJECT"

if [ "$DIRECT_HPA" -eq 1 ]; then
    DUMP_ARGS="$DUMP_ARGS --direct-hpa"
    REPORT_ARGS="$REPORT_ARGS --direct-hpa"
elif [ -n "$WVA_THRESHOLD_CONFIG" ]; then
    # Inject the specific overriding configmap payload directly into the final PDF 
    REPORT_ARGS="$REPORT_ARGS --wva-config-file $WVA_THRESHOLD_CONFIG"
fi

echo "Dumping raw offline PromQL metrics to unified JSON dump..."
PYTHONPATH=$(pwd) python3 ../dump_epp_fc_metrics/dump_all_metrics.py $DUMP_ARGS

echo "Generating static plot and scoring visualization outputs purely offline..."
cd "$EXP_DATA_DIR" || exit 1

python3 "$EXTRACT_DIR/get_benchmark_report.py" $REPORT_ARGS


echo "============================================================================="
echo "✅ CI Benchmark Run Fully Complete!"
echo "📂 All results securely stored in: $EXP_DATA_DIR"
echo "============================================================================="
