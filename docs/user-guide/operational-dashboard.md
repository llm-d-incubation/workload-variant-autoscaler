# Operational Dashboard

## Overview
For observability, WVA records a number of metrics which are scraped by Prometheus. This document shows how to enable the operational dashboard in your Kubernetes cluster. Once enabled, you can view these metrics through the provided dashboard with Grafana.

## Enable Operational Dashboard
The operational dashboard is not installed by default. To enable, set the environment variable `DEPLOY_OPERATIONAL_DASHBOARD` to `true` and run or re-run the installation. Following is an example for installation using `Make` method:
```
export DEPLOY_OPERATIONAL_DASHBOARD=true
make deploy-wva-on-k8s
```

## Access Operational Dashboard
Once the operational dashboard is enabled, Grafana is installed, configured, and ready to display the dashboard. Here are the next steps:

- Forward Grafana port so the dashboard can be accessed locally:
  ```
  $ kubectl port-forward -n workload-variant-autoscaler-monitoring svc/kube-prometheus-stack-grafana 3000:80 &
  ```
- Get Grafana `admin` password:
  ```
  $ kubectl get secret -n workload-variant-autoscaler-monitoring   kube-prometheus-stack-grafana   -o jsonpath="{.data.admin-password}" | base64 -d;echo
  Z9FEW12xG2k2tTZJVML75Kd80qi2oI0nJBsCjv7q
  ```
- Point browser to `http://localhost:3000/`, login with username `admin` and the password obtained in previous step.