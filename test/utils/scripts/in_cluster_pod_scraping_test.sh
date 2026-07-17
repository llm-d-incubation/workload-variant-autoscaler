# Script to verify PodScrapingSource metrics scraping from inside the cluster.
#
# This script is embedded in the test Job (via //go:embed) and verifies that:
# 1. The EPP pod metrics endpoint is accessible (HTTP 200)
# 2. The response contains valid Prometheus-format metrics
#
# This is used by TestInClusterScraping to verify end-to-end scraping functionality
# when running inside the Kubernetes cluster (where pod IPs are accessible).
#
# This script expects TARGET_URL as a required environment variable.
# BEARER_TOKEN is optional; if set, it is sent as an Authorization header.

set -e

if [ -z "$TARGET_URL" ]; then
  echo "ERROR: Missing required environment variable TARGET_URL"
  exit 1
fi

echo "Testing metrics scraping from inside cluster..."
echo "Target URL: ${TARGET_URL}"
echo ""

# Test 1: Verify endpoint is accessible
echo "Test 1: Checking if metrics endpoint is accessible..."
if [ -n "$BEARER_TOKEN" ]; then
  HTTP_CODE=$(curl -s -o /tmp/metrics.txt -w "%{http_code}" --max-time 10 \
    -H "Authorization: Bearer ${BEARER_TOKEN}" \
    "${TARGET_URL}" || echo "000")
else
  HTTP_CODE=$(curl -s -o /tmp/metrics.txt -w "%{http_code}" --max-time 10 \
    "${TARGET_URL}" || echo "000")
fi

if [ "$HTTP_CODE" != "200" ]; then
  echo "ERROR: Metrics endpoint returned HTTP $HTTP_CODE"
  echo "Response:"
  cat /tmp/metrics.txt || true
  exit 1
fi

echo "Metrics endpoint is accessible (HTTP 200)"

# Test 2: Verify response contains Prometheus metrics
echo ""
echo "Test 2: Verifying response contains Prometheus metrics..."
METRICS_CONTENT=$(cat /tmp/metrics.txt)

if [ -z "$METRICS_CONTENT" ]; then
  echo "ERROR: Metrics response is empty"
  exit 1
fi

# Check for Prometheus metric format (lines starting with # or metric_name)
if ! echo "$METRICS_CONTENT" | grep -qE "^#|^[a-zA-Z_][a-zA-Z0-9_]*"; then
  echo "ERROR: Response does not appear to be in Prometheus format"
  echo "First 500 chars of response:"
  echo "$METRICS_CONTENT" | head -c 500
  exit 1
fi

echo "Response contains Prometheus metrics"
echo ""
echo "Sample metrics (first 10 lines):"
echo "$METRICS_CONTENT" | head -n 10
echo ""
echo "SUCCESS: Metrics scraping works from inside cluster!"
