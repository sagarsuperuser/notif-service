# Changelog

All notable changes to this project will be documented in this file.
Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Fixed

- **Permanent provider rejections no longer bounce through the queue**
  (2026-10-02). Before, the worker wrote `state='failed'` for a non-retryable
  send (e.g. HTTP 400) and then returned the provider error. The SQS consumer
  reads that error as "not finished". So the job was redelivered after the
  visibility timeout, refused by `ClaimAndLoad`, and deleted as a duplicate.
  - No message was lost. Each permanent failure cost one wasted receive, one
    claim query and one misleading `sqs handler error` log.
  - The same pairing of a terminal write with an error caused the 15 August
    silent-loss bug.
  - `Process` now returns nil for outcomes the database records as final, and
    logs the rejection itself. The return contract is documented on `Process`
    and in `docs/architecture/03`.
  - Regression test: `TestProcessor_PermanentFailureIsAcknowledged`.
- Docs: the 15 August failure-handling write-up gains a plain-language summary
  and drops the "A/B" framing for "before/after runs".
- **Worker fixes:**
  - a duplicate of a held message is no longer deleted;
  - every claimed write is guarded by a claim token;
  - a sent-but-unrecorded write is retried;
  - 400s no longer trip the breaker;
  - 401/403/404 are treated as configuration errors;
  - StatusCallback is sent with every message.
- **API fixes:**
  - the API drains before exit;
  - an enqueue failure keeps the key retryable (503, and a retry re-enqueues);
  - a reused key with a different payload is 409;
  - 64 KiB body and field limits;
  - fixed error messages.
- **Infra: standard SQS queues restored.** PR #75 had reverted them to FIFO.
  Also: DLQ retention 14 days; visibility timeout 180 s; worker liveness
  check is process-only; IMDSv2.
- Invariants: lost delivery updates and enqueue-failed rows are now caught;
  cap check compares UTC days.

- **Concurrent duplicate accepts no longer double-spend the daily cap**
  (2026-10-03). A per-(tenant, key) advisory lock now runs in the same
  pipelined batch as the accept statement, so it is still one round-trip. The
  query tracer and the round-trip test count a pipelined batch as one call.
- **Tag-bump PRs can merge again.** GitHub holds the CI run of a PR opened by
  `github-actions[bot]` for approval, and those runs had been expiring. The
  publish workflow now approves the held run and closes superseded bump PRs.
  This replaces a `workflow_dispatch` step from PR #79, which ran but did not
  count toward branch protection.

### Security

- Patched 29 reachable vulnerabilities (toolchain go1.25.13, pgx v5.11.0,
  x/text v0.41.0). CI gains gofmt, staticcheck and govulncheck gates.

Review notes and the deliberately deferred gaps: `docs/engineering-notes.md`,
"Correctness review, 2 October 2026".

### Changed

- **Infrastructure simplified to pragmatic components** (2026-08-20). The
  production-shaped topology served its purpose during the benchmark
  campaigns. The running system now carries only what it uses. Removed:
  - The load balancers and the `use_load_balancers` toggle. This account
    cannot create LBs (an account-level hold), so the stack already ran
    without them. DNS now points at the k3s server's EIP, and ingress-nginx
    NodePorts are the entry.
  - NAT gateway + private subnets. Public subnets now use SG-locked ingress.
    SQS via the IGW is free, while NAT billed every worker→SQS byte.
  - The bastion. SSM is the access path; 6443/SSH are SG-locked to
    `admin_cidr`.
  - RDS Proxy with its Secrets Manager secret, SG and IAM role. The services'
    pgx pools hold few long-lived connections.
  - The 3-server etcd control plane + five role-pinned node pools + three spot
    ASGs. There is now one server + one worker ASG. Manifests no longer pin
    `workload=` selectors/taints. Required pod anti-affinity is preferred
    rather than required. Ingress-nginx uses externalTrafficPolicy Cluster.

  Result: `infra/main.tf` 1,191 → ~490 lines. Benchmarks still fit:
  `worker_on_demand_percentage = 100` + `worker_count = N` reproduces a
  measurement-grade pool on non-burstable types. The scale-up path (HA
  control plane, LB) is documented in `docs/architecture/04` instead of
  pre-built. Historical campaign docs are unchanged apart from a one-line
  architecture pointer. Diagrams are redrawn, including removing the
  webhook-processor/queue that PR #5 had already removed from the code.
