#!/usr/bin/env bash
set -euo pipefail

NAMESPACE="ingress-nginx"
RELEASE="ingress-nginx"
CHART_VERSION="4.11.3"   # pin this

helm repo add ingress-nginx https://kubernetes.github.io/ingress-nginx >/dev/null 2>&1 || true
helm repo update >/dev/null

kubectl get ns "$NAMESPACE" >/dev/null 2>&1 || kubectl create ns "$NAMESPACE"

helm upgrade --install "$RELEASE" ingress-nginx/ingress-nginx \
  -n "$NAMESPACE" \
  --version "$CHART_VERSION" \
  --wait --timeout 5m --rollback-on-failure \
  -f "$(dirname "$0")/values.yaml"
  

kubectl -n "$NAMESPACE" get svc ingress-nginx-controller -o wide