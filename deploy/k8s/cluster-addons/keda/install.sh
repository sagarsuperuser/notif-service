#!/usr/bin/env bash
set -euo pipefail

NAMESPACE="keda"
RELEASE="keda"
CHART_VERSION="2.14.2"   # pin this

helm repo add kedacore https://kedacore.github.io/charts >/dev/null 2>&1 || true
helm repo update >/dev/null

kubectl get ns "$NAMESPACE" >/dev/null 2>&1 || kubectl create ns "$NAMESPACE"

helm upgrade --install "$RELEASE" kedacore/keda \
  -n "$NAMESPACE" \
  --version "$CHART_VERSION" \
  --wait --timeout 5m --rollback-on-failure

kubectl -n "$NAMESPACE" get pods -o wide
