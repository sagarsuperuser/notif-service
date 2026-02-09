#!/usr/bin/env bash
set -euo pipefail

NAMESPACE="monitoring"
RELEASE="kps"
CHART_VERSION="58.6.0"   # pin this
VALUES_FILE="${VALUES_FILE:-values-k3s.yaml}"
TIMEOUT="${TIMEOUT:-10m}"
WAIT="${WAIT:-true}"
ROLLBACK_ON_FAILURE="${ROLLBACK_ON_FAILURE:-true}"

# Add Prometheus Community Helm repo
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts >/dev/null 2>&1 || true
helm repo update >/dev/null

# Namespace for monitoring
kubectl get ns "$NAMESPACE" >/dev/null 2>&1 || kubectl create ns "$NAMESPACE"

# Select values file (k3s/local-path by default; AWS uses gp3 via EBS CSI)
VALUES_PATH="$(dirname "$0")/${VALUES_FILE}"
if [[ ! -f "$VALUES_PATH" ]]; then
  echo "values file not found: $VALUES_PATH" >&2
  echo "hint: set VALUES_FILE=values-aws.yaml for AWS EBS gp3" >&2
  exit 1
fi

# Install (or upgrade) kube-prometheus-stack
if [[ "$WAIT" == "true" ]]; then
  ROLLBACK_ARGS=()
  if [[ "$ROLLBACK_ON_FAILURE" == "true" ]]; then
    ROLLBACK_ARGS+=(--rollback-on-failure)
  fi

  helm upgrade --install "$RELEASE" prometheus-community/kube-prometheus-stack \
    -n "$NAMESPACE" \
    --version "$CHART_VERSION" \
    --wait --timeout "$TIMEOUT" \
    "${ROLLBACK_ARGS[@]}" \
    -f "$VALUES_PATH"
else
  helm upgrade --install "$RELEASE" prometheus-community/kube-prometheus-stack \
    -n "$NAMESPACE" \
    --version "$CHART_VERSION" \
    -f "$VALUES_PATH"
fi

# Watch pods come up
kubectl -n "$NAMESPACE" get pods -o wide
