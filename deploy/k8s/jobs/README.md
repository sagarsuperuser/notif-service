# DB Migrate + Seed Job

Run migrations and seed data from inside the cluster using the existing `notif-secrets` secret (`DB_DSN`).
SQL is loaded from `deploy/k8s/jobs/sql/`.

## Run

```bash
kubectl delete job notif-db-migrate-seed --ignore-not-found
kubectl apply -k deploy/k8s/jobs
kubectl logs -f job/notif-db-migrate-seed
```

## Verify

```bash
kubectl get jobs
kubectl get pods -l job-name=notif-db-migrate-seed
```
