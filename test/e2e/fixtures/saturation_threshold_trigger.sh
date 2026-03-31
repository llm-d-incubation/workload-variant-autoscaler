#!/bin/sh
# Bounded HTTP traffic for e2e saturation threshold scenarios (OpenAI-compatible
# /v1/models preflight, then /v1/completions). Parameterized via env; embedded
# from test/e2e via //go:embed (see createSaturationThresholdTriggerJob).
#
# Required env:
#   NUM_REQUESTS, TARGET_SERVICE, TARGET_PORT, MODEL_ID, MAX_TOKENS,
#   MAX_RETRIES, RETRY_DELAY, PREFLIGHT_TIMEOUT, REQUEST_TIMEOUT
#
# EXIT: 0 if at least one completion returned HTTP 200; non-zero otherwise.

set -eu
echo "Saturation threshold trigger job starting..."
echo "Sending ${NUM_REQUESTS} requests to ${TARGET_SERVICE}:${TARGET_PORT} for model ${MODEL_ID} (max_tokens=${MAX_TOKENS})"

READY=false
i=1
while [ "$i" -le "$MAX_RETRIES" ]; do
  HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" --max-time "$PREFLIGHT_TIMEOUT" "http://${TARGET_SERVICE}:${TARGET_PORT}/v1/models" || true)
  if [ "$HTTP_CODE" = "200" ]; then
    echo "Service preflight passed on attempt $i"
    READY=true
    break
  fi
  echo "Service preflight attempt $i failed (HTTP ${HTTP_CODE:-}), retrying in ${RETRY_DELAY}s..."
  sleep "$RETRY_DELAY"
  i=$((i + 1))
done
if [ "$READY" != "true" ]; then
  echo "Service preflight failed after ${MAX_RETRIES} attempts"
  exit 1
fi

SENT=0
SUCCESS=0
FAILED=0
while [ "$SENT" -lt "$NUM_REQUESTS" ]; do
  echo "Sending request $((SENT + 1)) / ${NUM_REQUESTS}..."
  RESPONSE=$(curl -s -w "\n%{http_code}" --max-time "$REQUEST_TIMEOUT" -X POST "http://${TARGET_SERVICE}:${TARGET_PORT}/v1/completions" \
    -H "Content-Type: application/json" \
    -d "{\"model\":\"${MODEL_ID}\",\"prompt\":\"Deterministic saturation threshold crossing prompt\",\"max_tokens\":${MAX_TOKENS}}" 2>&1)
  HTTP_CODE=$(echo "$RESPONSE" | tail -n1)

  if [ "$HTTP_CODE" = "200" ]; then
    SUCCESS=$((SUCCESS + 1))
    echo "Request $((SENT + 1)) succeeded (HTTP $HTTP_CODE)"
  else
    FAILED=$((FAILED + 1))
    echo "Request $((SENT + 1)) failed (HTTP $HTTP_CODE)"
    echo "Response body:"
    echo "$RESPONSE" | sed '$d'
  fi

  SENT=$((SENT + 1))
done

echo "Saturation threshold trigger job completed: sent=$SENT, success=$SUCCESS, failed=$FAILED"
if [ "$SUCCESS" -gt 0 ]; then
  exit 0
fi
exit 1
