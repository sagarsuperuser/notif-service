# In-cluster k6 load tests

This folder contains in-cluster k6 jobs with per-request dynamic `idempotencyKey`.

## Apply (~56 req/sec for 30 minutes)

```bash
kubectl apply -f deploy/k8s/tools/k6/notif-api-56rps-30m.yaml
```

## Tail logs

```bash
kubectl logs -l job-name=k6-notif-api-56rps-30m -f
```

## Cleanup

```bash
kubectl delete -f deploy/k8s/tools/k6/notif-api-56rps-30m.yaml
```

## Notes

- Update `API_URL` in the Job env if needed.
- Modify `notif-api-56rps-30m.js` for rate/duration.
