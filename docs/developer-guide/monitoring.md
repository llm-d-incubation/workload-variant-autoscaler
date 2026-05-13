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

- Browse to "Connections/Data sources", you should see a Prometheus data source `https://kube-prometheus-stack-prometheus.workload-variant-autoscaler-monitoring.svc.cluster.local:9090`. Click on `Test` button to test the data source.

- Browse to "Dashboards", you should see a dashboard called `WVA Operational Dashboard`.


## Import Operational Dashboard
The pre-installed `WVA Operational Dashboard` is read-only. You can import `WVA Operational Dashboard` to a new dashboard so you can update the dashboard as follows:
- Browse to "Dashboards", you should see a dashboard called `WVA Operational Dashboard/New/Import`.
- Copy and paste the content of `deploy/grafana/operational-dashboard.json`.
- Name the new dashboard such as `My WVA Operational Dashboard`.
- Your new dashboard now is the same as `WVA Operational Dashboard` except that you can edit and save.


## Troubleshooting
### No Data
  - Check the datasource by browse to "Connections/Data sources", you should see a Prometheus data source `https://kube-prometheus-stack-prometheus.workload-variant-autoscaler-monitoring.svc.cluster.local:9090`. Click on `Test` button to test the data source.