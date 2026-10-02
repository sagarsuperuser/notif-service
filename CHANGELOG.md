# Changelog

All notable changes to this project will be documented in this file.
Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Fixed

- **Permanent provider rejections no longer bounce through the queue**
  (2026-10-02). After writing `state='failed'` for a non-retryable send (e.g.
  HTTP 400), the worker returned the provider error, which the SQS consumer
  reads as "not finished": the job was redelivered after the visibility
  timeout, refused by `ClaimAndLoad`, and deleted as a duplicate. No loss, but
  one wasted receive, claim query and misleading `sqs handler error` log per
  permanent failure — and the same terminal-write-plus-error pairing behind
  the 15 August silent-loss bug. `Process` now returns nil for outcomes the
  database records as final and logs the rejection itself. The return
  contract is documented on `Process` and in `docs/architecture/03`.
  Regression test: `TestProcessor_PermanentFailureIsAcknowledged`.
- Docs: the 15 August failure-handling write-up gains a plain-language summary
  and drops the "A/B" framing for "before/after runs".
- **Worker: duplicate of a held message no longer deleted; every claimed write
  guarded by a claim token; sent-but-unrecorded write retried; 400s no longer
  trip the breaker; 401/403/404 treated as configuration errors; StatusCallback
  sent with every message.**
- **API: drains before exit; enqueue failure keeps the key retryable (503 +
  retry re-enqueues); reused key with a different payload is 409; 64 KiB body
  and field limits; fixed error messages.**
- **Infra: standard SQS queues restored** (#75 had reverted them to FIFO); DLQ
  retention 14 days; visibility timeout 180 s; worker liveness process-only;
  IMDSv2.
- Invariants: lost delivery updates and enqueue-failed rows are now caught;
  cap check compares UTC days.

### Security

- Patched 29 reachable vulnerabilities (toolchain go1.25.13, pgx v5.11.0,
  x/text v0.41.0). CI gains gofmt, staticcheck and govulncheck gates.

Review notes and the deliberately deferred gaps: `docs/engineering-notes.md`,
"Correctness review, 2 October 2026".

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
