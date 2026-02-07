# In-cluster k6 load tests

This folder contains in-cluster k6 jobs with per-request dynamic `idempotencyKey`.

## Apply (2 req/sec for 10 minutes)

```bash
kubectl apply -f deploy/k8s/tools/k6/notif-api-2rps-10m.yaml
```

## Tail logs

```bash
kubectl logs -l job-name=k6-notif-api-2rps-10m -f
```

## Cleanup

```bash
kubectl delete -f deploy/k8s/tools/k6/notif-api-2rps-10m.yaml
```

## Notes

- Update `API_URL` in the Job env if needed.
- Modify `notif-api-2rps-10m.js` for rate/duration.
