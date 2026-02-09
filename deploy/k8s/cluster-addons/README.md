# Cluster Addons

This folder contains **cluster-level dependencies** that you install **once per Kubernetes cluster** (not per app deploy).

## Contents

- `aws-ebs-csi-driver/`
  Installs the **AWS EBS CSI driver** and a `gp3` StorageClass for dynamic PVC provisioning on AWS.
  - `install.sh` installs/updates the Helm release in `kube-system` namespace and applies `storageclass-gp3.yaml`.

- `ingress-nginx/`  
  Installs **NGINX Ingress Controller** using Helm.
  - `values.yaml` configures it as **NodePort**:
    - HTTP: `30080`
    - HTTPS: `30443`
  - `install.sh` installs/updates the Helm release in `ingress-nginx` namespace.

- `cert-manager/`  
  Installs **cert-manager** for TLS certificate automation.
  - `install.sh` installs/updates cert-manager in `cert-manager` namespace.

- `prometheus/`  
  Installs **kube-prometheus-stack** (Prometheus + Alertmanager + Grafana).
  - `values-k3s.yaml` config for k3s with local-path storage.
  - `values-aws.yaml` config for AWS using `gp3` StorageClass (requires EBS CSI driver).
  - `install.sh` installs/updates the Helm release in `monitoring` namespace.

- `keda/`
  Installs **KEDA** for event-driven autoscaling (for example SQS-based worker scaling).
  - `install.sh` installs/updates the Helm release in `keda` namespace.

## Install order

```bash
# 0) (AWS only) EBS CSI + gp3 StorageClass
# Required if you want Prometheus/Grafana/Alertmanager PVCs on EBS (gp3).
bash deploy/k8s/cluster-addons/aws-ebs-csi-driver/install.sh

# 1) Ingress controller
bash deploy/k8s/cluster-addons/ingress-nginx/install.sh

# 2) cert-manager
bash deploy/k8s/cluster-addons/cert-manager/install.sh

# 3) Prometheus stack
# k3s/local default:
bash deploy/k8s/cluster-addons/prometheus/install.sh

# AWS/gp3:
# VALUES_FILE=values-aws.yaml bash deploy/k8s/cluster-addons/prometheus/install.sh

# 4) KEDA
bash deploy/k8s/cluster-addons/keda/install.sh

# 5) Choose issuer (start with staging, later prod)
kubectl apply -f deploy/k8s/overlays/dev/clusterissuer-letsencrypt-staging.yaml

# 6) apply ingress
kubectl apply -f deploy/k8s/overlays/prod/ingress-hosts.yaml

# later:
# kubectl apply -f deploy/k8s/overlays/prod/clusterissuer-letsencrypt-prod.yaml
# kubectl apply -f deploy/k8s/overlays/prod/ingress-hosts.yaml
