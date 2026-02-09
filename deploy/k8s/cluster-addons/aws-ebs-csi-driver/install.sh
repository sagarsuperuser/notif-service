#!/usr/bin/env bash
set -euo pipefail

# Installs AWS EBS CSI driver and a gp3 StorageClass.
# Prereq: your EC2 nodes must have IAM permissions to manage EBS (recommended via instance profile).

NAMESPACE="kube-system"
RELEASE="aws-ebs-csi-driver"
CHART_VERSION="2.35.1" # pin this
TIMEOUT="${TIMEOUT:-15m}"
WAIT="${WAIT:-true}"
ROLLBACK_ON_FAILURE="${ROLLBACK_ON_FAILURE:-true}"

helm repo add aws-ebs-csi-driver https://kubernetes-sigs.github.io/aws-ebs-csi-driver >/dev/null 2>&1 || true
helm repo update >/dev/null

if [[ "$WAIT" == "true" ]]; then
  ROLLBACK_ARGS=()
  if [[ "$ROLLBACK_ON_FAILURE" == "true" ]]; then
    ROLLBACK_ARGS+=(--rollback-on-failure)
  fi

  helm upgrade --install "$RELEASE" aws-ebs-csi-driver/aws-ebs-csi-driver \
    -n "$NAMESPACE" \
    --version "$CHART_VERSION" \
    --wait --timeout "$TIMEOUT" \
    "${ROLLBACK_ARGS[@]}"
else
  helm upgrade --install "$RELEASE" aws-ebs-csi-driver/aws-ebs-csi-driver \
    -n "$NAMESPACE" \
    --version "$CHART_VERSION"
fi

kubectl apply -f "$(dirname "$0")/storageclass-gp3.yaml"

kubectl -n "$NAMESPACE" get pods -o wide | grep -i ebs || true
