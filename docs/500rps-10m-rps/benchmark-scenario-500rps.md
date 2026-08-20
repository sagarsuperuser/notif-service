# Benchmark Scenario: 500 RPS for 10 Minutes

> **Superseded.** This is the February 2026 run that found the processing ceiling at ~241 ops/sec; the current measurements are in [benchmark-2026-08-14.md](../benchmark-2026-08-14.md).

## 1) Test Goal
Validate steady-state behavior at **500 RPS** for **10 minutes**.

## 2) Environment
Fill these with exact run values for traceability.

- Commit SHA: `sha-e409a91`
- Region: `ap-south-1`
- Test date/time window: `2026-02-18 19:00:00+05:30` to `2026-02-18 19:10:00+05:30`
- Cluster topology: `servers - 3 (spot=0, on-demand=3) , workers - 24 (spot=16, on-demand=8)`
- DB class and storage: `db-instance-class="db.t4g.xlarge", storage="gp3"`
- KEDA settings:
  - Worker min, max replicas: `2, 4`(Dedicated Nodes)
  - Webhook processor min, max replicas: `2, 2` (Dedicated Nodes)
- Provider mode: `mock`
- Relevant manifests/values:
  - `deploy/k8s/overlays/prod/keda-sqs-worker.yaml`
  - `deploy/k8s/overlays/prod/keda-sqs-webhook-processor.yaml`
  - `deploy/k8s/base/notif-config.yaml`

## 3) Workload
- Load generator: `k6`
- Target: `500 RPS`
- Duration: `10m`
- Endpoint: `POST /v1/sms/messages`
- Payload profile: `message size/template/tenant mix: ~176B fixed payload, single template (txn_confirm_v1), single tenant (foodapp)`
- Unique numbers in pool: `100k`
- Number selection strategy: `sequential`

## 4) Key Results (Observed)
### Throughput
- API ingress RPS (peak): **500 RPS**
- Enqueue rate (peak): **500 RPS**
- Worker processed rate (peak): **242 ops/sec**
- Webhook RPS (peak): **733 RPS**
- Backlog drain result: **~293,000 messages drained in ~28 minutes** (during + post-load)

### Latency
- End-to-end latency (message created -> first provider hit):
  - p50: **12.6 min**
  - p95: **15.4 min**
  - p99: **19.1 min**
- Provider latency:
  - p50: **309 ms**
  - p95: **485 ms**
  - p99: **550 ms**

### Provider response mix (peak rates)
- Timeout: **0.533 ops/sec**
- HTTP 201: **242 ops/sec**
- HTTP 400: **0.700 ops/sec**
- HTTP 429: **2.23 ops/sec**
- HTTP 500: **0.733 ops/sec**

### Webhook event mix (peak rates)
- queued: **244 ops/sec**
- sent: **244 ops/sec**
- delivered: **244 ops/sec**
- failed: **1.43 ops/sec**
- undelivered: **1.07 ops/sec**

### Worker processing duration
- p50: **330 ms**
- p95: **670 ms**
- p99: **943 ms**

## 5) Interpretation
- System is functional and stable end-to-end, but slow at this load profile.
- System is **over capacity** at 500 RPS for this topology.
- Backlog grows because **ingress (500 RPS) > processing (~241 ops/sec)**.
- Elevated E2E latency is dominated by **queueing delay**, not provider call latency (provider p95 is sub-500ms).

## 6) Pass/Fail vs SLO
### Example SLO
- E2E p95 < 2 minutes
- API success rate >= 99.9%
- No sustained queue growth over steady window

### Result for this run
- **Fail** for 500 RPS under the above SLO.

## 7) Bottleneck Summary
Primary bottleneck appears in the processing path:
- Worker + webhook-processor + DB update/enqueue/dequeue path cannot sustain 500 RPS input.
- Provider latency is not the limiting factor in this scenario.

## 8) Next Actions
1. Increase effective processing throughput in worker/webhook-processor and DB path.
2. Re-run the same 500 RPS / 10 min scenario without changing workload profile.
3. Compare before/after using identical Grafana queries and SQL windows.

## 9) Evidence Checklist
Attach the following artifacts for review:
- Grafana screenshots:
  - API RPS, Queue Enqueue RPS
  - queue depth / queue age
  - worker processed RPS, worker process duration
  - E2E latency p95/p99
  - DB CPU + connections
  - provider latency + status mix
  - webhook RPS + event mix
- k6 run output/logs
- SQL snapshots:
  - message state distribution
  - delivery event distribution
- Exact deployment/config used:
  - image tags
  - kustomize overlay refs
  - Terraform vars relevant to capacity

## 10) Repro Commands (Template)
```bash
# Apply infra and k8s config
terraform -chdir=infra plan
terraform -chdir=infra apply
kubectl apply -k deploy/k8s/overlays/prod

# Run load scenario
kubectl apply -f deploy/k8s/tools/k6/<scenario-file>.yaml
kubectl logs -f job/<k6-job-name>

# Capture quick state
kubectl top pods
kubectl top nodes
```

## 11) Change Log
- v1: Initial report for 500 RPS steady 10-minute scenario.
- v2: Added backlog drain observation (~294k in ~30 min).
