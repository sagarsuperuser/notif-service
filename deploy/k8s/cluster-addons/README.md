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
  Installs **cert-manager** and provides **Let’s Encrypt ClusterIssuers**.
  - `clusterissuer-letsencrypt-staging.yaml` (use first)
  - `clusterissuer-letsencrypt-prod.yaml` (use after staging works)
  - `install.sh` installs/updates cert-manager in `cert-manager` namespace.

## Install order

```bash
# 1) Ingress controller
bash deploy/k8s/cluster-addons/ingress-nginx/install.sh

# 2) cert-manager
bash deploy/k8s/cluster-addons/cert-manager/install.sh

# 3) Choose issuer (start with staging)
kubectl apply -f deploy/k8s/cluster-addons/cert-manager/clusterissuer-letsencrypt-staging.yaml
# later:
# kubectl apply -f deploy/k8s/cluster-addons/cert-manager/clusterissuer-letsencrypt-prod.yaml