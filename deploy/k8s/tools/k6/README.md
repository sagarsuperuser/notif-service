# In-cluster k6 load tests

This folder contains in-cluster k6 jobs with per-request dynamic `idempotencyKey`.

## Scenarios

### 1) Baseline steady (~56 req/sec for 30 minutes)
```bash
kubectl apply -f deploy/k8s/tools/k6/notif-api-500rps-30m.yaml
kubectl logs -l job-name=k6-notif-api-500rps-30m -f
kubectl delete -f deploy/k8s/tools/k6/notif-api-500rps-30m.yaml
```

### 2) Target steady (100 req/sec for 15 minutes)
```bash
kubectl apply -f deploy/k8s/tools/k6/notif-api-100rps-15m.yaml
kubectl logs -l job-name=k6-notif-api-100rps-15m -f
kubectl delete -f deploy/k8s/tools/k6/notif-api-100rps-15m.yaml
```

### 3) Burst + recovery (0 -> 150 req/sec -> 0)
```bash
kubectl apply -f deploy/k8s/tools/k6/notif-api-burst-150rps-recovery.yaml
kubectl logs -l job-name=k6-notif-api-burst-150rps-recovery -f
kubectl delete -f deploy/k8s/tools/k6/notif-api-burst-150rps-recovery.yaml
```

## Notes

- Update `API_URL` in each Job env if needed.
- Each script iterates across a deterministic 100k phone space (`+19990000000`..`+19990099999`).

## Step ramp (finding the limit)

```bash
kubectl apply -f deploy/k8s/tools/k6/notif-api-stepramp.yaml
kubectl logs -l job-name=k6-notif-api-stepramp -f
```

Holds 200, 500, 1000, 2000, 3000, 4000 and 5000 rps for three minutes each.
A fixed-rate run can only tell you whether that one rate works; it cannot tell
you where the limit is, and a run well under the limit looks the same as a run
at half of it. The knee is the step where p99 departs from flat.

Thresholds fail the run if acceptance drops below 99.9% or accept p99 passes
500ms, so the knee is identified by the run rather than by eye.

The job is pinned to the monitoring pool. k6 has no scheduling constraints of
its own, so it otherwise lands on the general pool next to the API pods and
competes for CPU with the thing being measured.

## After any run

```bash
go run ./cmd/verify-run -dsn "$DB_DSN" -since 1h -max-per-day 1000000
```

Throughput only means something alongside the invariants. See internal/verify.
