# Cluster Addons

This folder contains **cluster-level dependencies** that you install **once per Kubernetes cluster** (not per app deploy).

## Contents

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
  - `install.sh` installs/updates the Helm release in `monitoring` namespace.

- `keda/`
  Installs **KEDA** for event-driven autoscaling (for example SQS-based worker scaling).
  - `install.sh` installs/updates the Helm release in `keda` namespace.

## Install order

```bash
# 1) Ingress controller
bash deploy/k8s/cluster-addons/ingress-nginx/install.sh

# 2) cert-manager
bash deploy/k8s/cluster-addons/cert-manager/install.sh

# 3) Prometheus stack
bash deploy/k8s/cluster-addons/prometheus/install.sh

# 4) KEDA
bash deploy/k8s/cluster-addons/keda/install.sh

# 5) Choose issuer (start with staging, later prod)
kubectl apply -f deploy/k8s/overlays/dev/clusterissuer-letsencrypt-staging.yaml

# 6) apply ingress
kubectl apply -f deploy/k8s/overlays/prod/ingress-hosts.yaml

# later:
# kubectl apply -f deploy/k8s/overlays/prod/clusterissuer-letsencrypt-prod.yaml
# kubectl apply -f deploy/k8s/overlays/prod/ingress-hosts.yaml
