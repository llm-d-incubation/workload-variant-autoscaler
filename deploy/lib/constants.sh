#!/usr/bin/env bash
#
# Shared deploy script constants.
# Sourced by deploy/install.sh before runtime helper libs.
# No function dependencies.
# Exposes shared names for waits, selectors, and common Kubernetes resources.
#

# Poll/wait defaults
WAIT_INTERVAL_10S=10
DEFAULT_VERIFY_STARTUP_SLEEP_SECONDS=10

# Shared resource names
EXTERNAL_METRICS_APISERVICE_NAME='v1beta1.external.metrics.k8s.io'
PROMETHEUS_ADAPTER_SERVICE_NAME='prometheus-adapter'
PROMETHEUS_CA_CONFIGMAP_NAME='prometheus-ca'
KEDA_RELEASE_NAME='keda'
PROMETHEUS_ADAPTER_RELEASE_NAME='prometheus-adapter'

# Common Kubernetes label selectors
WVA_CONTROLLER_LABEL_SELECTOR='app.kubernetes.io/name=workload-variant-autoscaler'
PROMETHEUS_ADAPTER_LABEL_SELECTOR='app.kubernetes.io/name=prometheus-adapter'
KEDA_OPERATOR_LABEL_SELECTOR='app.kubernetes.io/name=keda-operator'
