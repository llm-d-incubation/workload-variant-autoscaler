#!/usr/bin/env python3
import argparse
import json
import os
import datetime
import yaml
import subprocess
import urllib.parse
from typing import Dict, Any, Tuple, List
import sys

def check_privileges(token: str = None, server: str = None):
    """Verify the user is logged into OpenShift with sufficient monitoring exec privileges."""
    if token and server:
        print(f"Logging into OpenShift at {server}...")
        try:
            subprocess.run(["oc", "login", f"--token={token}", f"--server={server}", "--insecure-skip-tls-verify=true"], capture_output=True, check=True)
            print("Successfully logged into OpenShift.")
        except subprocess.CalledProcessError as e:
            print("\n❌ ERROR: Failed to log into OpenShift with provided token and server.")
            print(f"Details: {e.stderr if e.stderr else 'Check your token and server URL.'}")
            sys.exit(1)

    print("Checking OpenShift privileges...")
    try:
        subprocess.run(["oc", "whoami"], capture_output=True, check=True)
    except subprocess.CalledProcessError:
        print("\n❌ ERROR: You are not logged into OpenShift.")
        print("Please run: oc login --token=<your_token> --server=<cluster_url>")
        sys.exit(1)
        
    try:
        result = subprocess.run(
            ["oc", "auth", "can-i", "create", "pods/exec", "-n", "openshift-monitoring"],
            capture_output=True, text=True, check=True
        )
        if "yes" not in result.stdout.lower():
            raise subprocess.CalledProcessError(1, "oc auth")
    except subprocess.CalledProcessError:
        print("\n❌ ERROR: Insufficient privileges.")
        print("You need 'cluster-admin' (or similar role able to exec into monitoring pods) to run this script.")
        print("Please log in as an admin or ask a cluster administrator for access.")
        sys.exit(1)

def query_prometheus(query: str, eval_time: float = None, user_workload: bool = False) -> Dict[str, Any]:
    encoded_query = urllib.parse.quote(query)
    namespace = "openshift-user-workload-monitoring" if user_workload else "openshift-monitoring"
    label_selector = "app.kubernetes.io/name=prometheus,prometheus=user-workload" if user_workload else "app.kubernetes.io/name=prometheus,prometheus=k8s"
    
    try:
        cmd_str = f"oc get pods -n {namespace} -l {label_selector} --no-headers | grep -v Terminating | awk '{{print $1}}' | head -n 1"
        pod_res = subprocess.run(cmd_str, shell=True, capture_output=True, text=True, check=True)
        pod = pod_res.stdout.strip()
        if not pod:
            raise Exception("No running prometheus pod found")
    except Exception as e:
        print(f"Error finding running prometheus pod: {e}")
        pod = "prometheus-user-workload-0" if user_workload else "prometheus-k8s-0"
    
    if eval_time:
        url = f"http://localhost:9090/api/v1/query?query={encoded_query}&time={eval_time}"
    else:
        url = f"http://localhost:9090/api/v1/query?query={encoded_query}"
        
    cmd = [
        "oc", "exec", "-n", namespace, pod, "-c", "prometheus", "--",
        "curl", "-s", url
    ]
    try:
        result = subprocess.run(cmd, capture_output=True, text=True, check=True)
        return json.loads(result.stdout)
    except Exception as e:
        print(f"Error querying Prometheus for query: {query}")
        print(e)
        return {}

def query_prometheus_range(query: str, start: float, end: float, step: str = "15s", user_workload: bool = False) -> Dict[str, Any]:
    encoded_query = urllib.parse.quote(query)
    
    namespace = "openshift-user-workload-monitoring" if user_workload else "openshift-monitoring"
    label_selector = "app.kubernetes.io/name=prometheus,prometheus=user-workload" if user_workload else "app.kubernetes.io/name=prometheus,prometheus=k8s"
    
    try:
        cmd_str = f"oc get pods -n {namespace} -l {label_selector} --no-headers | grep -v Terminating | awk '{{print $1}}' | head -n 1"
        pod_res = subprocess.run(cmd_str, shell=True, capture_output=True, text=True, check=True)
        pod = pod_res.stdout.strip()
        if not pod:
            raise Exception("No running prometheus pod found")
    except Exception as e:
        print(f"Error finding running prometheus pod: {e}")
        pod = "prometheus-user-workload-0" if user_workload else "prometheus-k8s-0"

    cmd = [
        "oc", "exec", "-n", namespace, pod, "-c", "prometheus", "--",
        "curl", "-s", f"http://localhost:9090/api/v1/query_range?query={encoded_query}&start={start}&end={end}&step={step}"
    ]
    try:
        result = subprocess.run(cmd, capture_output=True, text=True, check=True)
        return json.loads(result.stdout)
    except Exception as e:
        print(f"Error querying Prometheus range for query: {query}")
        print(e)
        return {}

def get_node_gpus() -> Dict[str, str]:
    cmd = [
        "oc", "get", "nodes",
        "-o", "jsonpath={range .items[*]}{.metadata.name}{'\\t'}{.metadata.labels.nvidia\\.com/gpu\\.product}{'\\n'}{end}"
    ]
    try:
        result = subprocess.run(cmd, capture_output=True, text=True, check=True)
        node_gpu_map = {}
        for line in result.stdout.strip().split('\n'):
            parts = line.split('\t')
            if len(parts) == 2:
                node, gpu = parts[0].strip(), parts[1].strip()
                if gpu:
                    node_gpu_map[node] = gpu
        return node_gpu_map
    except Exception as e:
        print(f"Warning: Could not fetch node GPU labels: {e}")
        return {}

def get_epp_config(namespace: str) -> str:
    cmd = ["oc", "get", "cm", "-n", namespace, "-o", "json"]
    try:
        result = subprocess.run(cmd, capture_output=True, text=True, check=True)
        items = json.loads(result.stdout).get("items", [])
        for item in items:
            data = item.get("data", {})
            if "default-plugins.yaml" in data:
                return data["default-plugins.yaml"]
    except Exception as e:
        print(f"Warning: Could not fetch EPP config: {e}")
    return "EPP Config: Not Found"

def get_wva_config(namespace: str) -> str:
    cmd = ["oc", "get", "cm", "workload-variant-autoscaler-wva-saturation-scaling-config", "-n", namespace, "-o", "json"]
    try:
        result = subprocess.run(cmd, capture_output=True, text=True, check=True)
        data = json.loads(result.stdout).get("data", {})
        if "default" in data:
            return data["default"].strip()
        return str(data)
    except Exception as e:
        print(f"Warning: Could not fetch WVA saturation config: {e}")
    return "WVA Config: Not Found"

def get_benchmark_config(namespace: str) -> str:
    cmd = ["oc", "get", "cm", "guidellm-profiles", "-n", namespace, "-o", "json"]
    try:
        result = subprocess.run(cmd, capture_output=True, text=True, check=True)
        data = json.loads(result.stdout).get("data", {})
        for key, value in data.items():
            if key.endswith(".yaml"):
                return f"Profile: {key}\n{value}"
        return str(data)
    except Exception as e:
        print(f"Warning: Could not fetch guidellm-profiles config: {e}")
    return "Benchmark Config: Not Found"

def get_autoscaling_config(namespace: str) -> Tuple[str, float]:
    report = ""
    va_cost = 1.0 
    cmd_va = ["oc", "get", "va", "-n", namespace, "-o", "json"]
    try:
        result = subprocess.run(cmd_va, capture_output=True, text=True, check=True)
        items = json.loads(result.stdout).get("items", [])
        if items:
            for item in items:
                name = item.get("metadata", {}).get("name", "Unknown")
                spec = item.get("spec", {})
                min_rep = spec.get("minReplicas", "N/A")
                max_rep = spec.get("maxReplicas", "N/A")
                va_cost = float(spec.get("variantCost", 1.0))
                report += f"Variant (VA) [{name}]: Min Replicas = {min_rep}, Max Replicas = {max_rep}, Cost Factor = {va_cost}\n"
        else:
            report += "Variant (VA): Not Found\n"
    except Exception as e:
        report += f"Warning: Could not fetch VA config: {e}\n"

    cmd_hpa = ["oc", "get", "hpa", "-n", namespace, "-o", "json"]
    try:
        result = subprocess.run(cmd_hpa, capture_output=True, text=True, check=True)
        items = json.loads(result.stdout).get("items", [])
        if items:
            for item in items:
                name = item.get("metadata", {}).get("name", "Unknown")
                spec = item.get("spec", {})
                min_rep = spec.get("minReplicas", "N/A")
                max_rep = spec.get("maxReplicas", "N/A")
                report += f"HorizontalPodAutoscaler (HPA) [{name}]: Min Replicas = {min_rep}, Max Replicas = {max_rep}\n"
                
                behavior = spec.get("behavior")
                if behavior:
                    scale_up = behavior.get("scaleUp", {})
                    scale_down = behavior.get("scaleDown", {})
                    s_up_win = scale_up.get("stabilizationWindowSeconds", "N/A")
                    s_up_pol = json.dumps(scale_up.get("policies", []))
                    s_dn_win = scale_down.get("stabilizationWindowSeconds", "N/A")
                    s_dn_pol = json.dumps(scale_down.get("policies", []))
                    report += f"  > ScaleUp:   Stabilization Window = {s_up_win}s | Policies = {s_up_pol}\n"
                    report += f"  > ScaleDown: Stabilization Window = {s_dn_win}s | Policies = {s_dn_pol}\n"
                else:
                    annotations = item.get("metadata", {}).get("annotations", {})
                    beh_str = annotations.get("autoscaling.alpha.kubernetes.io/behavior")
                    if beh_str:
                        try:
                            beh = json.loads(beh_str)
                            scale_up = beh.get("ScaleUp", {})
                            scale_down = beh.get("ScaleDown", {})
                            s_up_win = scale_up.get("StabilizationWindowSeconds", "N/A")
                            s_up_pol = json.dumps(scale_up.get("Policies", []))
                            s_dn_win = scale_down.get("StabilizationWindowSeconds", "N/A")
                            s_dn_pol = json.dumps(scale_down.get("Policies", []))
                            report += f"  > ScaleUp:   Stabilization Window = {s_up_win}s | Policies = {s_up_pol}\n"
                            report += f"  > ScaleDown: Stabilization Window = {s_dn_win}s | Policies = {s_dn_pol}\n"
                        except Exception as parse_e:
                            report += f"  > Behavior annotation found but could not be cleanly parsed.\n"
        else:
            report += "HorizontalPodAutoscaler (HPA): Not Found\n"
    except Exception as e:
        report += f"Warning: Could not fetch HPA config: {e}\n"

    return report, va_cost

def main():
    parser = argparse.ArgumentParser(description="Dump ALL cluster metrics and configurations for offline report generation.")
    parser.add_argument("-n", "--namespace", default="default", help="The namespace to query")
    parser.add_argument("-r", "--results-dir", required=True, help="Path to the GuideLLM exp-docs folder")
    parser.add_argument("-t", "--token", default=None, help="OpenShift login token")
    parser.add_argument("-s", "--server", default=None, help="OpenShift server URL")
    parser.add_argument("--direct-hpa", action="store_true", help="Bypass WVA config scraping for Native HPA tests")
    
    args = parser.parse_args()
    
    check_privileges(args.token, args.server)
    
    results_dir = args.results_dir
    if not os.path.exists(results_dir):
        print(f"Error: Directory {results_dir} does not exist.")
        exit(1)

    # Determine window by looking at GuideLLM yamls
    yaml_first = os.path.join(results_dir, "benchmark_report,_results.json_0.yaml")
    yaml_first_alt = os.path.join(results_dir, "benchmark_report_v0.2,_results.json_0.yaml")
    start_yaml = yaml_first if os.path.exists(yaml_first) else (yaml_first_alt if os.path.exists(yaml_first_alt) else None)
    
    yaml_last = os.path.join(results_dir, "benchmark_report,_results.json_3.yaml")
    yaml_last_alt = os.path.join(results_dir, "benchmark_report_v0.2,_results.json_3.yaml")
    end_yaml = yaml_last if os.path.exists(yaml_last) else (yaml_last_alt if os.path.exists(yaml_last_alt) else start_yaml)
    
    start_time = None
    end_time = None
    if start_yaml and end_yaml:
        try:
            with open(start_yaml, 'r') as f:
                d_start = yaml.safe_load(f)
                start_time = float(d_start['metrics']['time']['start']) - 60
            with open(end_yaml, 'r') as f:
                d_end = yaml.safe_load(f)
                end_time = float(d_end['metrics']['time']['stop']) + 60
            print(f"Time window automatically derived: {datetime.datetime.fromtimestamp(start_time)} to {datetime.datetime.fromtimestamp(end_time)}")
        except Exception as e:
            print(f"Warning: Failed to parse exact benchmark bounds from YAMLs: {e}")
            
    if not start_time or not end_time:
        print("Warning: Could not determine start and end times from YAML files. Using 1 hour lookback.")
        end_time = datetime.datetime.now().timestamp()
        start_time = end_time - 3600

    namespace = args.namespace
    
    dump_data = {
        "start_time": start_time,
        "end_time": end_time,
        "configs": {},
        "instant_metrics": {},
        "range_metrics": {}
    }

    # 1. Fetch Static Configurations
    print("Fetching static configurations...")
    dump_data["configs"]["node_gpus"] = get_node_gpus()
    dump_data["configs"]["epp_config"] = get_epp_config(namespace)
    dump_data["configs"]["benchmark_config"] = get_benchmark_config(namespace)
    
    if not args.direct_hpa:
        dump_data["configs"]["wva_config"] = get_wva_config(namespace)
    
    autoscaling_report, va_cost = get_autoscaling_config(namespace)
    dump_data["configs"]["autoscaling_report"] = autoscaling_report
    dump_data["configs"]["va_cost"] = va_cost

    # 2. Fetch Range Metrics (for plots and aggregations)
    range_queries = {
        # EPP Metrics (User Workload)
        "queue_duration": (f'inference_extension_flow_control_request_queue_duration_seconds{{namespace="{namespace}"}}', True),
        "queue_size": (f'sum(inference_extension_flow_control_queue_size{{namespace="{namespace}"}})', True),
        "queue_bytes": (f'sum(inference_extension_flow_control_queue_bytes{{namespace="{namespace}"}})', True),
        "dispatch_cycle": (f'inference_extension_flow_control_dispatch_cycle_duration_seconds{{namespace="{namespace}"}}', True),
        "enqueue_duration": (f'inference_extension_flow_control_request_enqueue_duration_seconds{{namespace="{namespace}"}}', True),
        "pool_saturation": (f'inference_extension_flow_control_pool_saturation{{namespace="{namespace}"}}', True),
        "request_total": (f'inference_objective_request_total{{namespace="{namespace}"}}', True),
        "request_error_total": (f'inference_objective_request_error_total{{namespace="{namespace}"}}', True),
        "running_requests": (f'inference_objective_running_requests{{namespace="{namespace}"}}', True),
        "request_duration_seconds": (f'inference_objective_request_duration_seconds_sum{{namespace="{namespace}"}}', True),
        "request_duration_count": (f'inference_objective_request_duration_seconds_count{{namespace="{namespace}"}}', True),
        "request_sizes_bytes": (f'inference_objective_request_sizes_sum{{namespace="{namespace}"}}', True),
        "response_sizes_bytes": (f'inference_objective_response_sizes_sum{{namespace="{namespace}"}}', True),
        "input_tokens": (f'inference_objective_input_tokens_sum{{namespace="{namespace}"}}', True),
        "output_tokens": (f'inference_objective_output_tokens_sum{{namespace="{namespace}"}}', True),
        "prompt_cached_tokens": (f'inference_objective_prompt_cached_tokens_sum{{namespace="{namespace}"}}', True),
        "normalized_ttft": (f'inference_objective_normalized_time_per_output_token_seconds_sum{{namespace="{namespace}"}}', True),
        "pool_per_pod_queue_size": (f'inference_pool_per_pod_queue_size{{namespace="{namespace}"}}', True),
        "pool_average_queue_size": (f'inference_pool_average_queue_size{{namespace="{namespace}"}}', True),
        "pool_average_kv_cache_utilization": (f'inference_pool_average_kv_cache_utilization{{namespace="{namespace}"}}', True),
        "pool_ready_pods": (f'inference_pool_ready_pods{{namespace="{namespace}"}}', True),

        # Legacy Report Metrics (Platform Monitoring or User Monitoring depending on setup)
        "reqs_waiting": (f'vllm:num_requests_waiting{{namespace="{namespace}"}}', False),
        "kv_cache_usage_perc": (f'vllm:kv_cache_usage_perc{{namespace="{namespace}"}}', False),
        "kv_cache_usage_perc_v2": (f'vllm:gpu_cache_usage_perc{{namespace="{namespace}"}}', False),
        "rep_count": (f'count(kube_pod_info{{namespace="{namespace}", pod=~".*decode.*"}}) by (namespace)', False),
        "hpa_desired": (f'kube_horizontalpodautoscaler_status_desired_replicas{{namespace="{namespace}"}}', False),
        "wva_desired": (f'wva_desired_replicas{{namespace="{namespace}"}}', False)
    }

    print("Fetching timeline metrics range data...")
    for name, (query, uw) in range_queries.items():
        data = query_prometheus_range(query, start_time, end_time, step="15s", user_workload=uw)
        dump_data["range_metrics"][name] = data

    # 3. Fetch Instant Metrics (for single value extraction)
    # The original script computes active/pending pods over a custom window up to end_time.
    # window = "60m" or whatever, we can just use start_time to end_time.
    # duration in minutes:
    diff_minutes = max(1, int((end_time - start_time) / 60))
    window_str = f"{diff_minutes}m"
    
    instant_queries = {
        "active_pods": (f'max_over_time(kube_pod_status_phase{{namespace="{namespace}",pod=~".*decode.*",phase="Running"}}[{window_str}:15s])', False),
        "pending_pods": (f'max_over_time(kube_pod_start_time{{namespace="{namespace}",pod=~".*decode.*"}}[{window_str}])', False),
        "ready_pods": (f'min_over_time((timestamp(kube_pod_container_status_ready{{namespace="{namespace}",pod=~".*decode.*",container="vllm"}}==1))[{window_str}:1m])', False),
        "node_info": (f'max_over_time(kube_pod_info{{namespace="{namespace}",pod=~".*decode.*"}}[{window_str}:1m])', False)
    }

    print("Fetching instant calculation metrics...")
    for name, (query, uw) in instant_queries.items():
        data = query_prometheus(query, eval_time=end_time, user_workload=uw)
        dump_data["instant_metrics"][name] = data

    output_path = os.path.join(results_dir, "all_metrics_dump.json")
    with open(output_path, "w") as f:
        json.dump(dump_data, f, indent=2)

    print(f"✅ Successfully dumped all cluster config & prometheus metrics to {output_path}")

if __name__ == "__main__":
    main()
