#!/usr/bin/env bash
set -euo pipefail

NAMESPACE="monitoring"
RELEASE="kps"
CHART_VERSION="58.6.0"   # pin this

# Add Prometheus Community Helm repo
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts >/dev/null 2>&1 || true
helm repo update >/dev/null

# Namespace for monitoring
kubectl get ns "$NAMESPACE" >/dev/null 2>&1 || kubectl create ns "$NAMESPACE"

# Install (or upgrade) kube-prometheus-stack
helm upgrade --install "$RELEASE" prometheus-community/kube-prometheus-stack \
  -n "$NAMESPACE" \
  --version "$CHART_VERSION" \
  --wait --timeout 5m --rollback-on-failure \
  -f "$(dirname "$0")/values-k3s.yaml"

# Watch pods come up
kubectl -n "$NAMESPACE" get pods -o wide
