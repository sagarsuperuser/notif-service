# Changelog

All notable changes to this project will be documented in this file.
Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Changed

- **Infrastructure simplified to pragmatic components** (2026-08-20). The
  production-shaped topology served its purpose during the benchmark
  campaigns; the running system now carries only what it uses. Removed: the
  load balancers and the `use_load_balancers` toggle (this account cannot
  create LBs — an account-level hold — so the stack already ran without
  them; DNS now points at the k3s server's EIP and ingress-nginx NodePorts
  are the entry), NAT gateway + private subnets (public subnets with
  SG-locked ingress; SQS via the IGW is free, NAT billed every worker→SQS
  byte), the bastion (SSM is the access path; 6443/SSH SG-locked to
  `admin_cidr`), RDS Proxy with its Secrets Manager secret, SG and IAM role
  (the services' pgx pools hold few long-lived connections), and the
  3-server etcd control plane + five role-pinned node pools + three spot
  ASGs (now one server + one worker ASG; manifests no longer pin
  `workload=` selectors/taints, required pod anti-affinity is preferred
  rather than required, and ingress-nginx uses externalTrafficPolicy
  Cluster). `infra/main.tf` 1,191 → ~490 lines. Benchmarks still fit:
  `worker_on_demand_percentage = 100` + `worker_count = N` reproduces a
  measurement-grade pool on non-burstable types; the scale-up path (HA
  control plane, LB) is documented in `docs/architecture/04` instead of
  pre-built. Historical campaign docs unchanged apart from a one-line
  architecture pointer; diagrams redrawn (including removing the
  webhook-processor/queue that #5 had already removed from the code).
