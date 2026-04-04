#!/usr/bin/env bash
#
# Deploy an nginx TLS-terminating proxy in front of an HTTP Prometheus.
# Needed when WVA must talk to a Prometheus that only serves HTTP, since
# WVA enforces HTTPS for PROMETHEUS_BASE_URL.
#
# Creates:
#   - Self-signed TLS secret
#   - nginx configmap
#   - nginx Deployment (1 replica)
#   - Service exposing port 9443
#
# The WVA PROMETHEUS_URL to use after this is:
#   https://<SERVICE_NAME>.<NAMESPACE>.svc.cluster.local:9443
#
# Usage:
#   # Default: proxies http://prometheus-server.wva-monitoring.svc:80 -> https://prometheus-tls.wva-monitoring.svc:9443
#   ./deploy/deploy-prometheus-tls-proxy.sh
#
#   # Custom backend
#   NAMESPACE=monitoring \
#   BACKEND_HOST=prometheus-server.monitoring.svc.cluster.local \
#   BACKEND_PORT=80 \
#     ./deploy/deploy-prometheus-tls-proxy.sh
#

set -euo pipefail

NAMESPACE="${NAMESPACE:-wva-monitoring}"
SERVICE_NAME="${SERVICE_NAME:-prometheus-tls}"
BACKEND_HOST="${BACKEND_HOST:-prometheus-server.wva-monitoring.svc.cluster.local}"
BACKEND_PORT="${BACKEND_PORT:-80}"

echo ">>> Deploying TLS proxy: ${SERVICE_NAME}.${NAMESPACE} -> ${BACKEND_HOST}:${BACKEND_PORT}"

# Generate self-signed cert if secret doesn't exist
if ! kubectl get secret "${SERVICE_NAME}-cert" -n "${NAMESPACE}" >/dev/null 2>&1; then
    TLS_TMP=$(mktemp -d)
    trap 'rm -rf "${TLS_TMP}"' EXIT
    openssl req -x509 -nodes -days 365 -newkey rsa:2048 \
        -keyout "${TLS_TMP}/tls.key" -out "${TLS_TMP}/tls.crt" \
        -subj "/CN=${SERVICE_NAME}.${NAMESPACE}.svc.cluster.local" \
        -addext "subjectAltName=DNS:${SERVICE_NAME}.${NAMESPACE}.svc.cluster.local,DNS:${SERVICE_NAME}.${NAMESPACE}.svc,DNS:${SERVICE_NAME}" \
        2>/dev/null
    kubectl create secret tls "${SERVICE_NAME}-cert" -n "${NAMESPACE}" \
        --cert="${TLS_TMP}/tls.crt" --key="${TLS_TMP}/tls.key"
    echo "  Created TLS secret"
else
    echo "  TLS secret already exists"
fi

cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: ConfigMap
metadata:
  name: ${SERVICE_NAME}-config
  namespace: ${NAMESPACE}
data:
  nginx.conf: |
    events {}
    http {
      server {
        listen 9443 ssl;
        ssl_certificate /etc/tls/tls.crt;
        ssl_certificate_key /etc/tls/tls.key;
        location / {
          proxy_pass http://${BACKEND_HOST}:${BACKEND_PORT};
          proxy_set_header Host \$host;
        }
      }
    }
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${SERVICE_NAME}
  namespace: ${NAMESPACE}
spec:
  replicas: 1
  selector:
    matchLabels: {app: ${SERVICE_NAME}}
  template:
    metadata:
      labels: {app: ${SERVICE_NAME}}
    spec:
      containers:
      - name: nginx
        image: nginx:1.25-alpine
        ports: [{containerPort: 9443}]
        volumeMounts:
        - {name: config, mountPath: /etc/nginx/nginx.conf, subPath: nginx.conf}
        - {name: tls, mountPath: /etc/tls, readOnly: true}
      volumes:
      - {name: config, configMap: {name: ${SERVICE_NAME}-config}}
      - {name: tls, secret: {secretName: ${SERVICE_NAME}-cert}}
---
apiVersion: v1
kind: Service
metadata:
  name: ${SERVICE_NAME}
  namespace: ${NAMESPACE}
spec:
  selector: {app: ${SERVICE_NAME}}
  ports: [{port: 9443, targetPort: 9443}]
EOF

kubectl rollout status deployment "${SERVICE_NAME}" -n "${NAMESPACE}" --timeout=60s

echo ""
echo ">>> TLS proxy ready. Use this URL for PROMETHEUS_URL:"
echo "    https://${SERVICE_NAME}.${NAMESPACE}.svc.cluster.local:9443"
