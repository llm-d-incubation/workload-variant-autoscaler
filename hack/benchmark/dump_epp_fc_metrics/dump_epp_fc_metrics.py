#!/usr/bin/env python3
import argparse
import json
import os
import datetime
import yaml

try:
    from dump_all_metrics import check_privileges, query_prometheus_range
except ImportError as e:
    print(f"Error importing from dump_all_metrics: {e}")
    print("Ensure this script is running in the same directory as dump_all_metrics.py")
    exit(1)

def main():
    parser = argparse.ArgumentParser(description="Dump Flow Control metrics for a specific run.")
    parser.add_argument(
        "-n", "--namespace",
        default="default",
        help="The namespace to query (default: default)"
    )
    parser.add_argument(
        "-r", "--results-dir",
        required=True,
        help="Path to the GuideLLM exp-docs folder to parse results and save the dump."
    )
    parser.add_argument(
        "-t", "--token",
        default=None,
        help="OpenShift login token."
    )
    parser.add_argument(
        "-s", "--server",
        default=None,
        help="OpenShift server URL."
    )
    args = parser.parse_args()
    
    check_privileges(args.token, args.server)
    
    results_dir = args.results_dir
    if not os.path.exists(results_dir):
        print(f"Error: Directory {results_dir} does not exist.")
        exit(1)

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
        print("Error: Could not determine start and end times from YAML files in the results directory.")
        exit(1)

    namespace = args.namespace
    
    # We use sum() for size and bytes to aggregate across instances/queues.
    # Other metrics like duration are histograms or summaries in Prometheus, so we generally look at the raw metric.
    metrics = {
        # Flow Control Metrics
        "queue_duration": f'inference_extension_flow_control_request_queue_duration_seconds{{namespace="{namespace}"}}',
        "queue_size": f'sum(inference_extension_flow_control_queue_size{{namespace="{namespace}"}})',
        "queue_bytes": f'sum(inference_extension_flow_control_queue_bytes{{namespace="{namespace}"}})',
        "dispatch_cycle": f'inference_extension_flow_control_dispatch_cycle_duration_seconds{{namespace="{namespace}"}}',
        "enqueue_duration": f'inference_extension_flow_control_request_enqueue_duration_seconds{{namespace="{namespace}"}}',
        "pool_saturation": f'inference_extension_flow_control_pool_saturation{{namespace="{namespace}"}}',
        
        # General EPP Objective Metrics
        "request_total": f'inference_objective_request_total{{namespace="{namespace}"}}',
        "request_error_total": f'inference_objective_request_error_total{{namespace="{namespace}"}}',
        "running_requests": f'inference_objective_running_requests{{namespace="{namespace}"}}',
        "request_duration_seconds": f'inference_objective_request_duration_seconds_sum{{namespace="{namespace}"}}',
        "request_duration_count": f'inference_objective_request_duration_seconds_count{{namespace="{namespace}"}}',
        "request_sizes_bytes": f'inference_objective_request_sizes_sum{{namespace="{namespace}"}}',
        "response_sizes_bytes": f'inference_objective_response_sizes_sum{{namespace="{namespace}"}}',
        "input_tokens": f'inference_objective_input_tokens_sum{{namespace="{namespace}"}}',
        "output_tokens": f'inference_objective_output_tokens_sum{{namespace="{namespace}"}}',
        "prompt_cached_tokens": f'inference_objective_prompt_cached_tokens_sum{{namespace="{namespace}"}}',
        "normalized_ttft": f'inference_objective_normalized_time_per_output_token_seconds_sum{{namespace="{namespace}"}}',

        # EPP Pool Metrics
        "pool_per_pod_queue_size": f'inference_pool_per_pod_queue_size{{namespace="{namespace}"}}',
        "pool_average_queue_size": f'inference_pool_average_queue_size{{namespace="{namespace}"}}',
        "pool_average_kv_cache_utilization": f'inference_pool_average_kv_cache_utilization{{namespace="{namespace}"}}',
        "pool_ready_pods": f'inference_pool_ready_pods{{namespace="{namespace}"}}'
    }

    dump_data = {}

    for name, query in metrics.items():
        print(f"Fetching metric: {name} ...")
        # EPP metrics are in user workload monitoring
        data = query_prometheus_range(query, start_time, end_time, step="15s", user_workload=True)
        dump_data[name] = data

    output_path = os.path.join(results_dir, "epp_metrics_dump.json")
    with open(output_path, "w") as f:
        json.dump(dump_data, f, indent=2)

    print(f"✅ Successfully dumped all EPP metrics to {output_path}")

if __name__ == "__main__":
    main()
