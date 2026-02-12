# In-cluster k6 load tests

This folder contains in-cluster k6 jobs with per-request dynamic `idempotencyKey`.

## Scenarios

### 1) Baseline steady (~56 req/sec for 30 minutes)
```bash
kubectl apply -f deploy/k8s/tools/k6/notif-api-56rps-30m.yaml
kubectl logs -l job-name=k6-notif-api-56rps-30m -f
kubectl delete -f deploy/k8s/tools/k6/notif-api-56rps-30m.yaml
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
