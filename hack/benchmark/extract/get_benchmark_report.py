import argparse
import json
import subprocess
import sys
import os
import urllib.parse
from typing import Dict, Any, Tuple, List
import datetime

try:
    import matplotlib.pyplot as plt
    import matplotlib.dates as mdates
    from matplotlib.backends.backend_pdf import PdfPages
    import yaml
except ImportError:
    print("❌ ERROR: Required package 'matplotlib' or 'pyyaml' is not installed.")
    print("Please run: pip install matplotlib pyyaml")
    sys.exit(1)

# Replaced check_privileges and promql hooks with pure offline evaluator

def process_guidellm_results(filepath: str, step_seconds: int = 15) -> Tuple[List[datetime.datetime], List[int], List[int], List[int]]:
    with open(filepath, 'r') as f:
        results = json.load(f)
        
    successful_times = []
    failed_times = []
    incomplete_times = []
    
    for benchmark in results.get('benchmarks', []):
        for status in ['successful', 'incomplete', 'errored']:
            for req in benchmark.get('requests', {}).get(status, []):
                req_start = req.get('request_start_time', 0)
                if status == 'successful':
                    successful_times.append(req_start)
                elif status == 'errored':
                    failed_times.append(req_start)
                else: # incomplete
                    incomplete_times.append(req_start)
                    
    if not successful_times and not failed_times and not incomplete_times:
        return [], [], [], []
        
    min_time = min(successful_times + failed_times + incomplete_times)
    max_time = max(successful_times + failed_times + incomplete_times)
    
    # Create bins
    bins = {}
    current = min_time
    while current <= max_time + step_seconds:
        bins[current] = {'success': 0, 'fail': 0, 'incomplete': 0}
        current += step_seconds
        
    for t in successful_times:
        bin_time = min_time + ((t - min_time) // step_seconds) * step_seconds
        if bin_time in bins:
            bins[bin_time]['success'] += 1
            
    for t in failed_times:
        bin_time = min_time + ((t - min_time) // step_seconds) * step_seconds
        if bin_time in bins:
            bins[bin_time]['fail'] += 1

    for t in incomplete_times:
        bin_time = min_time + ((t - min_time) // step_seconds) * step_seconds
        if bin_time in bins:
            bins[bin_time]['incomplete'] += 1
            
    sorted_bins = sorted(bins.keys())
    x_times = [datetime.datetime.fromtimestamp(b) for b in sorted_bins]
    # converting count per 'step_seconds' into RPS (Rate Per Second)
    y_success = [bins[b]['success'] / step_seconds for b in sorted_bins]
    y_fail = [bins[b]['fail'] / step_seconds for b in sorted_bins]
    y_incomplete = [bins[b]['incomplete'] / step_seconds for b in sorted_bins]
    
    return x_times, y_success, y_fail, y_incomplete

def get_true_serving_capacity(filepath: str) -> str:
    """Parses results.json to find the highest achieved RPS before errors occur."""
    try:
        with open(filepath, 'r') as f:
            results = json.load(f)
            
        max_achieved_rps = 0.0
        details = []
        for b in results.get('benchmarks', []):
            rate = b.get('config', {}).get('strategy', {}).get('rate', 'N/A')
            metrics = b.get('metrics', {})
            
            achieved_rps = metrics.get('requests_per_second', {}).get('successful', {}).get('mean', 0)
            err_rps = metrics.get('requests_per_second', {}).get('errored', {}).get('mean', 0)
            tokens_sec = metrics.get('tokens_per_second', {}).get('successful', {}).get('mean', 0)
            
            details.append(f"Rate: {rate} RPS | Achieved: {achieved_rps:.2f} RPS | Errors: {err_rps:.2f} RPS | Tokens/s: {tokens_sec:.2f}")
            
            if err_rps == 0 and achieved_rps > max_achieved_rps:
                max_achieved_rps = achieved_rps
                
        # If there are no benchmarks with 0 errors, max_achieved_rps remains 0.0
        
        report = "True Serving Capacity Analysis (GuideLLM results.json)\n"
        report += "-" * 70 + "\n"
        report += "\n".join(details) + "\n"
        report += "-" * 70 + "\n"
        if max_achieved_rps > 0:
            report += f"=> Estimated True Serving Capacity: ~{max_achieved_rps:.2f} RPS (Highest successful rate with 0 errors)\n"
        else:
            report += "=> Estimated True Serving Capacity: Could not be determined (All runs had errors or 0 RPS)\n"
            
        report += "\n" + "-" * 70 + "\n"
        report += "Understanding GuideLLM Metrics:\n"
        report += "• Target Rate (RPS): The configured constant request rate the load generator attempts to send.\n"
        report += "• Achieved RPS: The actual rate of requests that completed successfully with a full response.\n"
        report += "• Error RPS: The rate of requests that failed (e.g., HTTP 5xx) or were strictly dropped/incomplete.\n"
        report += "• True Serving Capacity: Evaluated as the highest Achieved RPS recorded just before the system\n"
        report += "  reaches hardware saturation and begins generating Error RPS (dropped requests).\n"
        return report
        
    except Exception as e:
        return f"Could not determine True Serving Capacity: {e}\n"

def parse_guidellm_latencies(filepath: str) -> Tuple[List[str], List[float], List[float], List[float], List[float], List[float], List[float], List[float]]:
    """Returns (labels, ttft_mean, ttft_p99, itl_mean, itl_p99, tps_mean, conc_mean, req_lat_mean) for all benchmarks."""
    labels = []
    ttft_mean, ttft_p99 = [], []
    itl_mean, itl_p99 = [], []
    tps_mean = []
    conc_mean = []
    req_lat_mean = []
    
    try:
        with open(filepath, 'r') as f:
            results = json.load(f)
        for i, benchmark in enumerate(results.get('benchmarks', [])):
            rate = benchmark.get('config', {}).get('strategy', {}).get('rate')
            if rate is not None:
                labels.append(f"{rate} RPS")
            else:
                labels.append(f"Run {i+1}")
            metrics = benchmark.get('metrics', {})
            
            # TTFT
            ttft = metrics.get('time_to_first_token_ms', {}).get('successful', {})
            ttft_mean.append(ttft.get('mean', 0))
            ttft_p99.append(ttft.get('percentiles', {}).get('p99', 0))
            
            # ITL
            itl = metrics.get('inter_token_latency_ms', {}).get('successful', {})
            itl_mean.append(itl.get('mean', 0))
            itl_p99.append(itl.get('percentiles', {}).get('p99', 0))
            
            # Token Throughput
            tps = metrics.get('tokens_per_second', {}).get('successful', {})
            tps_mean.append(tps.get('mean', 0))
            
            # Concurrency & Request Latency (End-to-End)
            conc = metrics.get('request_concurrency', {}).get('total', {})
            conc_mean.append(conc.get('mean', 0))
            
            reqlat = metrics.get('request_latency', {}).get('successful', {})
            # It's reported in seconds in GuideLLM, multiply by 1000 for ms
            req_lat_mean.append(reqlat.get('mean', 0) * 1000)
            
    except Exception as e:
        print(f"Warning: Could not parse latencies from {filepath}: {e}")
        
    return labels, ttft_mean, ttft_p99, itl_mean, itl_p99, tps_mean, conc_mean, req_lat_mean

def parse_window_to_seconds(window: str) -> int:
    unit = window[-1]
    value = int(window[:-1])
    if unit == 'h':
        return value * 3600
    elif unit == 'm':
        return value * 60
    elif unit == 'd':
        return value * 86400
    elif unit == 's':
        return value
    return value * 3600 # Default to hours if parse fails

def main():
    parser = argparse.ArgumentParser(description="Fetch startup metrics and plot usage metrics from Prometheus.")
    parser.add_argument(
        "-n", "--namespace",
        default="default",
        help="The namespace to query (default: default)"
    )
    parser.add_argument(
        "-w", "--window",
        default="1h",
        help="The time window to look back for metrics, e.g., '30m', '1h', '2h' (default: 1h)"
    )
    parser.add_argument(
        "-o", "--output",
        default="metrics_usage.png",
        help="The output filename for the plots (default: metrics_usage.png)"
    )
    parser.add_argument(
        "-r", "--results-dir",
        default=None,
        help="Path to a GuideLLM exp-docs folder to parse results.json for succeed/failed requests."
    )
    # Excluded tokens inside isolated offline architecture array
    parser.add_argument(
        "--direct-hpa",
        action="store_true",
        help="Indicates if this run was purely an HPA test, bypassing WVA ConfigMap parsing."
    )
    parser.add_argument(
        "--scenario",
        default="Unknown Scenario",
        help="The name of the load generation scenario being executed."
    )
    parser.add_argument(
        "--wva-config-file",
        default=None,
        help="Path to an explicit WVA ConfigMap file to inject directly into the report."
    )
    args = parser.parse_args()
    
    dump_file = os.path.join(args.results_dir, "all_metrics_dump.json")
    if not os.path.exists(dump_file):
        print(f"Error: {dump_file} not found. Please run dump_all_metrics.py first.")
        sys.exit(1)
        
    with open(dump_file, 'r') as f:
        all_metrics = json.load(f)
    
    namespace = args.namespace
    window = args.window
    output_png = args.output
    results_dir = args.results_dir

    if results_dir and output_png == "metrics_usage.png":
        basename = os.path.basename(os.path.normpath(results_dir))
        output_png = os.path.join(results_dir, f"metrics_usage_{basename}.png")

    window_seconds = parse_window_to_seconds(window)
    end_time = datetime.datetime.now().timestamp()
    start_time = end_time - window_seconds
    
    if results_dir:
        import yaml
        yaml_first = os.path.join(results_dir, "benchmark_report,_results.json_0.yaml")
        yaml_first_alt = os.path.join(results_dir, "benchmark_report_v0.2,_results.json_0.yaml")
        start_yaml = yaml_first if os.path.exists(yaml_first) else (yaml_first_alt if os.path.exists(yaml_first_alt) else None)
        
        yaml_last = os.path.join(results_dir, "benchmark_report,_results.json_3.yaml")
        yaml_last_alt = os.path.join(results_dir, "benchmark_report_v0.2,_results.json_3.yaml")
        end_yaml = yaml_last if os.path.exists(yaml_last) else (yaml_last_alt if os.path.exists(yaml_last_alt) else None)
        
        if start_yaml:
            try:
                with open(start_yaml, 'r') as f:
                    data = yaml.safe_load(f)
                    if 'metadata' in data and 'timestamp' in data['metadata']:
                        raw_ts = data['metadata']['timestamp']
                        start_time = datetime.datetime.strptime(raw_ts, "%Y-%m-%dT%H:%M:%S.%f%z").timestamp()
                    else:
                        start_time = float(data['metrics']['time']['start'])
                    start_time -= 60 # Add buffer
                    
                if end_yaml:
                    with open(end_yaml, 'r') as f:
                        d_end = yaml.safe_load(f)
                        end_time = float(d_end['metrics']['time']['stop']) + 60
                else:
                    end_time = float(data['metrics']['time']['stop']) + 60
                    
                window_seconds = int(end_time - start_time)
                window = f"{window_seconds}s"
                print(f"\nClamping Prometheus queries exactly to benchmark duration: {datetime.datetime.fromtimestamp(start_time)} -> {datetime.datetime.fromtimestamp(end_time)}\n")
            except Exception as e:
                print(f"Warning: Failed to parse exact benchmark bounds from YAMLs, falling back to window: {e}")

    print(f"Using namespace: {namespace}")
    print(f"Using time window: {window}")
    print("-" * 50)

    print("Discovering Active Pods from Metrics Dump...")
    active_data = all_metrics.get("instant_metrics", {}).get("active_pods", {})
    pending_data = all_metrics.get("instant_metrics", {}).get("pending_pods", {})
    ready_data = all_metrics.get("instant_metrics", {}).get("ready_pods", {})
    node_data = all_metrics.get("instant_metrics", {}).get("node_info", {})
    
    valid_pods = set()
    if active_data.get('status') == 'success' and 'result' in active_data['data']:
        for result in active_data['data']['result']:
            pod = result['metric'].get('pod')
            if pod:
                valid_pods.add(pod)

    pod_stats = {}
    if node_data.get('status') == 'success' and 'result' in node_data['data']:
        for result in node_data['data']['result']:
            pod = result['metric'].get('pod')
            if pod and pod in valid_pods:
                if pod not in pod_stats:
                    pod_stats[pod] = {}
                node = result['metric'].get('node', 'Unknown')
                if node != 'Unknown':
                    pod_stats[pod]['node'] = node

    if pending_data.get('status') == 'success' and 'result' in pending_data['data']:
        for result in pending_data['data']['result']:
            pod = result['metric'].get('pod')
            if pod and pod in valid_pods:
                if pod not in pod_stats:
                    pod_stats[pod] = {}
                val = float(result['value'][1])
                if 'pending_time' not in pod_stats[pod] or val < pod_stats[pod]['pending_time']:
                    pod_stats[pod]['pending_time'] = val

    if ready_data.get('status') == 'success' and 'result' in ready_data['data']:
        for result in ready_data['data']['result']:
            pod = result['metric'].get('pod')
            if pod and pod in valid_pods:
                if pod not in pod_stats:
                    pod_stats[pod] = {}
                val = float(result['value'][1])
                if 'ready_time' not in pod_stats[pod] or val < pod_stats[pod]['ready_time']:
                    pod_stats[pod]['ready_time'] = val

    node_gpus = all_metrics.get("configs", {}).get("node_gpus", {})
    autoscaler_type = "Direct HPA (Legacy) Baseline" if args.direct_hpa else "Workload Variant Autoscaler (WVA)"
    
    scenario_name = args.scenario
    if scenario_name == "Unknown Scenario":
        bench_yaml = all_metrics.get("configs", {}).get("benchmark_config", "")
        if bench_yaml and bench_yaml != "Not Found":
            for line in bench_yaml.split('\n'):
                if line.startswith('Profile:'):
                    scenario_name = line.split(':', 1)[1].strip()
                    break

    report_text = "\n" + "="*115 + "\n"
    report_text += f"{'AUTOSCALER TYPE':<25}: {autoscaler_type}\n"
    report_text += f"{'PAYLOAD SCENARIO':<25}: {scenario_name}\n"
    report_text += "="*115 + "\n\n"
    
    report_text += "="*115 + "\n"
    report_text += f"{'Pod Name':<53} | {'Node':<20} | {'GPU':<20} | {'Startup (sec)':<15}\n"
    report_text += "="*115 + "\n"
    found_pods = False
    for pod, data in pod_stats.items():
        found_pods = True
        node = data.get('node', 'Unknown')
        gpu = node_gpus.get(node, "Unknown/None")
        
        pending_time = data.get('pending_time')
        ready_time = data.get('ready_time')
        
        if pending_time and ready_time:
            startup_time = round(ready_time - pending_time, 2)
            report_text += f"{pod:<53} | {node:<20} | {gpu:<20} | {startup_time}s\n"
        elif ready_time:
            report_text += f"{pod:<53} | {node:<20} | {gpu:<20} | N/A (Missing Pending Data)\n"
        elif pending_time:
            report_text += f"{pod:<53} | {node:<20} | {gpu:<20} | N/A (Missing Ready Data)\n"
        else:
            report_text += f"{pod:<53} | {node:<20} | {gpu:<20} | N/A\n"
            
    if not found_pods:
        report_text += f"No decode pods found in the {window} history for namespace: {namespace}\n"

    epp_yaml = all_metrics.get("configs", {}).get("epp_config", "Not Found")
    report_text += "\n" + "="*115 + "\n"
    report_text += "EPP Configuration (Feature Gates & Scorer Weights)\n"
    report_text += "="*115 + "\n"
    report_text += str(epp_yaml).strip() + "\n"

    bench_yaml = all_metrics.get("configs", {}).get("benchmark_config", "Not Found")
    report_text += "\n" + "="*115 + "\n"
    report_text += "Benchmark Load Generator Configuration\n"
    report_text += "="*115 + "\n"
    report_text += str(bench_yaml).strip() + "\n"

    if not args.direct_hpa:
        if args.wva_config_file and os.path.exists(args.wva_config_file):
            with open(args.wva_config_file, 'r') as f:
                wva_yaml = f.read()
        else:
            wva_yaml = all_metrics.get("configs", {}).get("wva_config", "Not Found")
            
        report_text += "\n" + "="*115 + "\n"
        report_text += "WVA Saturation Scaling Configuration\n"
        report_text += "="*115 + "\n"
        report_text += str(wva_yaml).strip() + "\n"

    autoscaling_config = all_metrics.get("configs", {}).get("autoscaling_report", "Not Found")
    extracted_va_cost = all_metrics.get("configs", {}).get("va_cost", 1.0)
    report_text += "\n" + "="*115 + "\n"
    report_text += "Autoscaling Configuration (HPA & VA)\n"
    report_text += "="*115 + "\n"
    report_text += str(autoscaling_config).strip() + "\n"

    req_x, req_succ, req_fail, req_incomp = [], [], [], []
    latency_labels, ttft_mean, ttft_p99, itl_mean, itl_p99, tps_mean, conc_mean, req_lat_mean = [], [], [], [], [], [], [], []
    has_results = False
    if results_dir:
        results_file = os.path.join(results_dir, "results.json")
        if os.path.exists(results_file):
            print(f"\nProcessing GuideLLM results from {results_file}...")
            
            # 1. Parse capacity and append to text report
            capacity_report = get_true_serving_capacity(results_file)
            report_text += "\n" + "="*115 + "\n"
            report_text += capacity_report
            
            req_x, req_succ, req_fail, req_incomp = process_guidellm_results(results_file)
            latency_labels, ttft_mean, ttft_p99, itl_mean, itl_p99, tps_mean, conc_mean, req_lat_mean = parse_guidellm_latencies(results_file)
            if req_x or latency_labels:
                has_results = True
            if not req_x:
                print("No requests found in results.json.")
                
            # --- AUTOSCALING SCORING CALCULATION ---
            if has_results and len(ttft_p99) > 0 and len(itl_p99) > 0:
                # Find maximum worst-case P99 latencies across all benchmarks
                max_ttft_p99 = max(ttft_p99)
                max_itl_p99 = max(itl_p99)
                
                avg_repl = 1.0
                try:
                    rep_data_temp = all_metrics.get("range_metrics", {}).get("rep_count", {})
                    if rep_data_temp.get('status') == 'success' and 'result' in rep_data_temp['data'] and len(rep_data_temp['data']['result']) > 0:
                        values = rep_data_temp['data']['result'][0].get('values', [])
                        if values:
                            y_vals = [float(v[1]) for v in values]
                            if len(y_vals) > 0:
                                avg_repl = sum(y_vals) / len(y_vals)
                except Exception as e:
                    print(f"Warning: Could not fetch replica count for scoring: {e}")

                # Calculate Average EPP Queue Size from local dict
                avg_epp = 0.0
                epp_found = False
                try:
                    d = all_metrics.get("range_metrics", {}).get("queue_size", {})
                    if d.get('status') == 'success' and 'result' in d['data']:
                        res = d['data']['result']
                        vals = [float(v[1]) for r in res for v in r.get('values', [])]
                        if vals:
                            avg_epp = sum(vals) / len(vals)
                            epp_found = True
                except Exception as e:
                    print(f"Warning: Could not parse local EPP metric dump for scoring: {e}")

                # Target SLAs
                target_ttft = 50.0 # Strict 50 ms SLA as requested
                target_itl = 50.0 # 50 ms SLA
                
                # Calculate penalties
                ttft_penalty = max_ttft_p99 / target_ttft
                itl_penalty = max_itl_p99 / target_itl
                
                # Apply the Extracted VA Cost Factor and Avg Replicas
                final_score = avg_repl * extracted_va_cost * (ttft_penalty + itl_penalty)
                
                report_text += "\n" + "="*115 + "\n"
                report_text += "Autoscaling Run Score (Lower is Better)\n"
                report_text += "="*115 + "\n"
                report_text += f"Worst-Case P99 TTFT: {max_ttft_p99:.2f} ms\n"
                report_text += f"Worst-Case P99 ITL:  {max_itl_p99:.2f} ms\n"
                report_text += f"Average Replicas:    {avg_repl:.2f}\n"
                if epp_found:
                    report_text += f"Average EPP Queue:   {avg_epp:.2f}\n\n"
                else:
                    report_text += f"Average EPP Queue:   N/A\n\n"
                report_text += f"Target SLAs: TTFT = {target_ttft}ms | ITL = {target_itl}ms\n\n"
                report_text += "Formula Engine:\n"
                report_text += "1. TTFT Penalty = (Actual P99 TTFT) / (SLA TTFT)\n"
                report_text += "2. ITL Penalty = (Actual P99 ITL) / (SLA ITL)\n"
                report_text += f"3. Cost Factor = VA Custom Resource declared Cost ({extracted_va_cost})\n"
                report_text += f"4. Avg Replicas Factor = Average running decode pods ({avg_repl:.2f})\n"
                report_text += "Autoscaling Score = Avg_Replicas × Cost_Factor × (TTFT_Penalty + ITL_Penalty)\n\n"
                report_text += f"Latency Penalty Subtotal = ({max_ttft_p99:.2f}/{target_ttft}) + ({max_itl_p99:.2f}/{target_itl}) = {(ttft_penalty + itl_penalty):.2f}\n"
                report_text += f"Resource Multiplier = {avg_repl:.2f} × {extracted_va_cost} = {(avg_repl * extracted_va_cost):.2f}\n"
                report_text += f"=> Final Score = {(avg_repl * extracted_va_cost):.2f} × {(ttft_penalty + itl_penalty):.2f} = {final_score:.2f}\n"
                
                # --- CALCULATE INTER-PHASE DELAYS ---
                report_text += "\n" + "="*115 + "\n"
                report_text += "GuideLLM Benchmark Inter-Phase Delay Analysis\n"
                report_text += "="*115 + "\n"
                import glob
                import yaml
                yaml_files = sorted(glob.glob(os.path.join(results_dir, 'benchmark_report,_results.json_*.yaml')))
                if not yaml_files:
                    yaml_files = sorted(glob.glob(os.path.join(results_dir, 'benchmark_report_v0.2,_results.json_*.yaml')))
                if yaml_files:
                    previous_stop = None
                    for idx, yaml_file in enumerate(yaml_files):
                        try:
                            with open(yaml_file, 'r') as f:
                                data = yaml.safe_load(f)
                                current_start = float(data.get('metrics', {}).get('time', {}).get('start', 0))
                                current_stop = float(data.get('metrics', {}).get('time', {}).get('stop', 0))
                                
                                file_name = os.path.basename(yaml_file)
                                
                                if previous_stop is not None and current_start > 0:
                                    delay = current_start - previous_stop
                                    report_text += f"Gap between Phase {idx-1} and {idx}: {delay:.2f} seconds\n"
                                
                                duration = current_stop - current_start
                                report_text += f"Phase {idx} ({file_name}): Start = {current_start:.2f}, Stop = {current_stop:.2f} (Duration: {duration:.1f}s)\n"
                                
                                previous_stop = current_stop
                        except Exception as e:
                            report_text += f"Warning: Could not parse {os.path.basename(yaml_file)} for delay analysis: {e}\n"
                else:
                    report_text += "Warning: No *_results.json_*.yaml files found to analyze delays.\n"
                
        else:
            print(f"\n❌ ERROR: results.json not found in {results_dir}")

    print(report_text)

    # ---- PLOTTING METRICS ----
    
    kv_data = all_metrics.get("range_metrics", {}).get("kv_cache_usage_perc", {})
    # Fallback to newer vLLM gpu cache metric if legacy vLLM metric is empty
    if not (kv_data.get('status') == 'success' and 'result' in kv_data.get('data', {}) and len(kv_data['data']['result']) > 0):
        kv_data = all_metrics.get("range_metrics", {}).get("kv_cache_usage_perc_v2", {})
    # Fallback to modern GAIE EPP metric if all vLLM metrics are empty
    if not (kv_data.get('status') == 'success' and 'result' in kv_data.get('data', {}) and len(kv_data['data']['result']) > 0):
        kv_data = all_metrics.get("range_metrics", {}).get("pool_average_kv_cache_utilization", {})

    req_data = all_metrics.get("range_metrics", {}).get("reqs_waiting", {})
    # Fallback to modern GAIE EPP per-pod queue size if legacy vllm metric is empty
    if not (req_data.get('status') == 'success' and 'result' in req_data.get('data', {}) and len(req_data['data']['result']) > 0):
        req_data = all_metrics.get("range_metrics", {}).get("pool_per_pod_queue_size", {})
    # Fallback to modern GAIE EPP average queue size if per-pod is also empty
    if not (req_data.get('status') == 'success' and 'result' in req_data.get('data', {}) and len(req_data['data']['result']) > 0):
        req_data = all_metrics.get("range_metrics", {}).get("pool_average_queue_size", {})

    rep_data = all_metrics.get("range_metrics", {}).get("rep_count", {})
    hpa_data = all_metrics.get("range_metrics", {}).get("hpa_desired", {})
    wva_data = all_metrics.get("range_metrics", {}).get("wva_desired", {})
    epp_data = all_metrics.get("range_metrics", {}).get("queue_size", {})
    
    has_kv = kv_data.get('status') == 'success' and 'result' in kv_data['data'] and len(kv_data['data']['result']) > 0
    has_req = req_data.get('status') == 'success' and 'result' in req_data['data'] and len(req_data['data']['result']) > 0
    has_rep = rep_data.get('status') == 'success' and 'result' in rep_data['data'] and len(rep_data['data']['result']) > 0
    has_epp = epp_data.get('status') == 'success' and 'result' in epp_data['data'] and len(epp_data['data']['result']) > 0

    if has_kv or has_req or has_rep or has_epp or has_results:
        num_subplots = 3 + (5 if has_results else 0)
        fig, axes = plt.subplots(num_subplots, 1, figsize=(12, 4 * num_subplots))
        
        # Determine the dynamic Autoscaler Title (Removed as per user request to declutter visual graphs)
        autoscaler_type = "Direct HPA (Legacy) Baseline" if args.direct_hpa else "Workload Variant Autoscaler (WVA)"
        scenario_name = args.scenario
        # fig.suptitle(f"Autoscaler Performance Report\nAutoscaler Type: {autoscaler_type}  |  Payload Scenario: {scenario_name}", fontsize=16, fontweight='bold', y=1.02)
        
        if num_subplots == 1:
            axes = [axes]
            
        ax_kv, ax_req, ax_rep = axes[0], axes[1], axes[2]
        
        if has_kv:
            plotted_series = 0
            for result in kv_data['data']['result']:
                pod = result['metric'].get('model_server_pod') or result['metric'].get('pod', 'Unknown Pod')
                values = result.get('values', [])
                
                if not values:
                    continue
                    
                x_times = [datetime.datetime.fromtimestamp(float(v[0])) for v in values]
                y_vals = [float(v[1]) * 100 for v in values] # Convert metric to percentage
                
                ax_kv.plot(x_times, y_vals, label=pod, linewidth=1.5)
                plotted_series += 1
                
            if plotted_series > 0:
                kv_source_name = kv_data['data']['result'][0]['metric'].get('__name__', '')
                if "pool_average_kv_cache" in kv_source_name:
                    ax_kv.set_title(f'Inference Pool Average KV Cache Usage Over Time (Namespace: {namespace})')
                else:
                    ax_kv.set_title(f'KV Cache Usage Percentage Over Time (Namespace: {namespace})')
                ax_kv.set_ylabel('KV Cache Usage (%)')
                ax_kv.set_ylim(0, 100) # Optional: bound Y-axis from 0 to 100%
                ax_kv.xaxis.set_major_formatter(mdates.DateFormatter('%H:%M:%S'))
                ax_kv.grid(True, linestyle='--', alpha=0.7)
                ax_kv.legend(loc='upper right', fontsize='small', bbox_to_anchor=(1.25, 1.0))
        else:
            ax_kv.set_title(f'KV Cache Usage Percentage Over Time (Namespace: {namespace}) - NO DATA')
            
        if has_req:
            plotted_series = 0
            for result in req_data['data']['result']:
                pod = result['metric'].get('model_server_pod') or result['metric'].get('pod', 'Unknown Pod')
                values = result.get('values', [])
                
                if not values:
                    continue
                    
                x_times = [datetime.datetime.fromtimestamp(float(v[0])) for v in values]
                y_vals = [float(v[1]) for v in values]
                
                ax_req.plot(x_times, y_vals, label=pod, linewidth=1.5)
                plotted_series += 1
                
            if plotted_series > 0:
                ax_req.set_title(f'Number of Requests Waiting Over Time (Namespace: {namespace})')
                ax_req.set_ylabel('Requests Waiting')
                ax_req.xaxis.set_major_formatter(mdates.DateFormatter('%H:%M:%S'))
                ax_req.grid(True, linestyle='--', alpha=0.7)
                ax_req.legend(loc='upper right', fontsize='small', bbox_to_anchor=(1.25, 1.0))
        else:
            ax_req.set_title(f'Number of Requests Waiting Over Time (Namespace: {namespace}) - NO DATA')

        if has_rep:
            plotted_series = 0
            for result in rep_data['data']['result']:
                values = result.get('values', [])
                
                if not values:
                    continue
                    
                x_times = [datetime.datetime.fromtimestamp(float(v[0])) for v in values]
                y_vals = [float(v[1]) for v in values]
                
                line_rep = ax_rep.step(x_times, y_vals, label="Actual Replicas", linewidth=2.0, color='blue', where='post')
                plotted_series += 1

            # Plot HPA Desired Replicas
            if hpa_data.get('status') == 'success' and 'result' in hpa_data['data']:
                for result in hpa_data['data']['result']:
                    values = result.get('values', [])
                    if values:
                        x_times = [datetime.datetime.fromtimestamp(float(v[0])) for v in values]
                        y_vals = [float(v[1]) for v in values]
                        ax_rep.step(x_times, y_vals, label="HPA Desired Replicas", linewidth=2.0, color='purple', linestyle='--', where='post')

            # Plot WVA Desired Replicas
            if wva_data.get('status') == 'success' and 'result' in wva_data['data']:
                for result in wva_data['data']['result']:
                    values = result.get('values', [])
                    if values:
                        x_times = [datetime.datetime.fromtimestamp(float(v[0])) for v in values]
                        y_vals = [float(v[1]) for v in values]
                        ax_rep.step(x_times, y_vals, label="WVA Desired Replicas", linewidth=2.0, color='green', linestyle=':', where='post')

            if has_epp and plotted_series > 0:
                ax_epp = ax_rep.twinx()
                for result in epp_data['data']['result']:
                    epp_values = result.get('values', [])
                    if not epp_values:
                        continue
                    
                    e_x_times = [datetime.datetime.fromtimestamp(float(v[0])) for v in epp_values]
                    e_y_vals = [float(v[1]) for v in epp_values]
                    
                    line_epp = ax_epp.fill_between(e_x_times, e_y_vals, color='orange', alpha=0.3, label="EPP Queue Size")
                    line_epp_border = ax_epp.plot(e_x_times, e_y_vals, color='darkorange', linewidth=1.5)
                    
                ax_epp.set_ylabel('EPP Flow Control Queue Size', color='darkorange')
                ax_epp.tick_params(axis='y', labelcolor='darkorange')
                
            if plotted_series > 0:
                ax_rep.set_title(f'Decode Replica Count & EPP Queue Over Time (Namespace: {namespace})')
                ax_rep.set_xlabel('Time')
                ax_rep.set_ylabel('Replica Count', color='blue')
                ax_rep.tick_params(axis='y', labelcolor='blue')
                ax_rep.xaxis.set_major_formatter(mdates.DateFormatter('%H:%M:%S'))
                ax_rep.grid(True, linestyle='--', alpha=0.7)
                
                # Combine legends if EPP exists
                if has_epp:
                    lines, labels = ax_rep.get_legend_handles_labels()
                    lines2, labels2 = ax_epp.get_legend_handles_labels()
                    ax_rep.legend(lines + lines2, labels + labels2, loc='upper right', fontsize='small', bbox_to_anchor=(1.25, 1.0))
                else:
                    ax_rep.legend(loc='upper right', fontsize='small', bbox_to_anchor=(1.25, 1.0))
                
                # Ensure Y axis uses integers since replica counts are whole numbers
                ax_rep.yaxis.set_major_locator(plt.MaxNLocator(integer=True))
        else:
            ax_rep.set_title(f'Decode Replica Count Over Time (Namespace: {namespace}) - NO DATA')
            
        if has_results:
            ax_res = axes[3]
            ax_ttft = axes[4]
            ax_itl = axes[5]
            ax_tps = axes[6]
            ax_conc = axes[7]
            
            # Since req_succ, req_fail, req_incomp are lists of (count/step_seconds), we need to reconstruct the total sum
            # The easiest way is to sum the raw values and multiply by step_seconds, because y = counts / step
            # Actually, the parsing function used `bins[b]['success'] / step_seconds`. To get the exact total,
            # we should just pass the totals back from parse_guidellm_results, or we can approximate by multiplying back.
            # However, for 100% accuracy, let's reverse the math: total_count = sum( RPS * step_seconds ).
            # Assuming step_seconds is 15.
            total_succ = int(sum(req_succ) * 15)
            total_fail = int(sum(req_fail) * 15)
            total_incomp = int(sum(req_incomp) * 15)
            
            ax_res.plot(req_x, req_succ, label=f"Successful RPS (Total: {total_succ})", linewidth=2.0, color='green')
            
            if total_fail > 0:
                ax_res.plot(req_x, req_fail, label=f"Failed RPS (Total: {total_fail})", linewidth=2.0, color='red', linestyle='--')
            if total_incomp > 0:
                ax_res.plot(req_x, req_incomp, label=f"Incomplete RPS (Total: {total_incomp})", linewidth=2.0, color='orange', linestyle='-.')
            
            ax_res.set_title(f'GuideLLM Requests (Succeeded vs Failed vs Incomplete) Over Time')
            ax_res.set_xlabel('Time')
            ax_res.set_ylabel('Requests / Second (RPS)')
            ax_res.grid(True, linestyle='--', alpha=0.7)
            ax_res.legend(loc='upper right', fontsize='small', bbox_to_anchor=(1.25, 1.0))
            
            # TTFT and ITL plots
            import numpy as np
            x_pos = np.arange(len(latency_labels))
            width = 0.35
            
            # TTFT Bar Chart
            ax_ttft.bar(x_pos - width/2, ttft_mean, width, label='Mean TTFT', color='skyblue')
            ax_ttft.bar(x_pos + width/2, ttft_p99, width, label='P99 TTFT', color='salmon')
            ax_ttft.set_title('Time To First Token (TTFT) per Run')
            ax_ttft.set_ylabel('TTFT (ms, log scale)')
            ax_ttft.set_yscale('log')
            ax_ttft.set_xticks(x_pos)
            ax_ttft.set_xticklabels(latency_labels)
            ax_ttft.legend(loc='upper right', fontsize='small', bbox_to_anchor=(1.25, 1.0))
            ax_ttft.grid(True, axis='y', linestyle='--', alpha=0.7)

            # ITL Bar Chart
            ax_itl.bar(x_pos - width/2, itl_mean, width, label='Mean ITL', color='lightgreen')
            ax_itl.bar(x_pos + width/2, itl_p99, width, label='P99 ITL', color='orchid')
            ax_itl.set_title('Inter-Token Latency (ITL) per Run')
            ax_itl.set_ylabel('ITL (ms, log scale)')
            ax_itl.set_yscale('log')
            ax_itl.set_xticks(x_pos)
            ax_itl.set_xticklabels(latency_labels)
            ax_itl.legend(loc='upper right', fontsize='small', bbox_to_anchor=(1.25, 1.0))
            ax_itl.grid(True, axis='y', linestyle='--', alpha=0.7)
            
            # Token Throughput Bar Chart
            ax_tps.bar(x_pos, tps_mean, width*1.5, label='Mean Tokens/s', color='gold')
            ax_tps.set_title('Overall Token Throughput per Run')
            ax_tps.set_ylabel('Tokens / Second')
            ax_tps.set_xticks(x_pos)
            ax_tps.set_xticklabels(latency_labels)
            
            # Annotate bars with standard value formatting
            for i, v in enumerate(tps_mean):
                ax_tps.text(i, v + (max(tps_mean)*0.01), f"{int(v)}", ha='center', va='bottom', fontsize=9)
                
            ax_tps.legend(loc='upper right', fontsize='small', bbox_to_anchor=(1.25, 1.0))
            ax_tps.grid(True, axis='y', linestyle='--', alpha=0.7)
            
            # Concurrency vs Request Latency Dual Axis Plot
            # Bar chart for Total Concurrency (Left Axis)
            bar1 = ax_conc.bar(x_pos, conc_mean, width*1.5, label='Mean Concurrency', color='green', alpha=0.7)
            ax_conc.set_title('Request Concurrency vs End-to-End Latency Profile')
            ax_conc.set_ylabel('Total In-flight Requests', color='darkgreen')
            ax_conc.tick_params(axis='y', labelcolor='darkgreen')
            ax_conc.set_xticks(x_pos)
            ax_conc.set_xticklabels(latency_labels)
            
            # Line chart for Request Latency (Right Axis)
            ax_conc_tw = ax_conc.twinx()
            line1 = ax_conc_tw.plot(x_pos, req_lat_mean, color='red', marker='o', linewidth=2.5, label='Mean Request Latency')
            ax_conc_tw.set_ylabel('Total Request Latency (ms)', color='red')
            ax_conc_tw.tick_params(axis='y', labelcolor='red')
            
            # Combine legends
            bars_lines = [bar1, line1[0]]
            labels = [l.get_label() for l in bars_lines]
            ax_conc.legend(bars_lines, labels, loc='upper right', fontsize='small', bbox_to_anchor=(1.25, 1.0))
            ax_conc.grid(True, axis='y', linestyle='--', alpha=0.4)
        # Rotate dates manually instead of autofmt_xdate which hides top axis labels
        for ax in axes:
            plt.setp(ax.get_xticklabels(), rotation=30, ha='right')

        plt.tight_layout()
        plt.savefig(output_png, bbox_inches="tight")
        
        pdf_filename = output_png.replace(".png", ".pdf")
        if not output_png.endswith(".png"):
            pdf_filename = output_png + ".pdf"
            
        with PdfPages(pdf_filename) as pdf:
            # --- Text Pagination Logic ---
            lines = report_text.split('\n')
            lines_per_page = 60 # Adjust based on font size and figure size
            
            for i in range(0, len(lines), lines_per_page):
                page_lines = lines[i:i+lines_per_page]
                page_text = '\n'.join(page_lines)
                
                fig_text = plt.figure(figsize=(10, 11))
                fig_text.clf()
                fig_text.text(0.05, 0.95, page_text, family='monospace', size=8, va='top', ha='left')
                pdf.savefig(fig_text)
                plt.close(fig_text)
            
            # --- Append Metrics Plots ---
            pdf.savefig(fig, bbox_inches="tight")
            
        print(f"Metrics plot successfully saved to {output_png}")
        print(f"Full PDF report successfully saved to {pdf_filename}")
    else:
        print(f"No metric data found for namespace: {namespace} in the last {window}.")


if __name__ == "__main__":
    main()
