#!/usr/bin/env bash
set -euo pipefail

NAMESPACE="cert-manager"
RELEASE="cert-manager"

helm repo add jetstack https://charts.jetstack.io >/dev/null 2>&1 || true
helm repo update >/dev/null

kubectl get ns "$NAMESPACE" >/dev/null 2>&1 || kubectl create ns "$NAMESPACE"

helm upgrade --install "$RELEASE" jetstack/cert-manager \
  -n "$NAMESPACE" \
  --set crds.enabled=true

kubectl -n "$NAMESPACE" rollout status deploy/cert-manager
kubectl -n "$NAMESPACE" rollout status deploy/cert-manager-webhook
kubectl -n "$NAMESPACE" rollout status deploy/cert-manager-cainjector
kubectl get pods -n "$NAMESPACE" -o wide