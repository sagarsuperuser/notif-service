# DB Migrate + Seed Job

Run migrations and seed data from inside the cluster using the existing `notif-secrets` secret (`DB_DSN`).
SQL is loaded from `deploy/k8s/jobs/sql/`.

This folder also includes a CronJob that reconciles messages stuck in `submitted` by applying the latest terminal
delivery events (`delivered`/`failed`/`undelivered`) back onto the `messages` table.

The reconciler is designed to be production-safe:
- Runs every 5 minutes.
- Uses a statement timeout / lock timeout to avoid long-running DB pressure.
- Batches updates (see `LIMIT` inside `sql/reconcile-submitted.sql`) so each run is bounded.

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

## Reconciler

```bash
kubectl get cronjob notif-db-reconcile-submitted
kubectl get jobs --sort-by=.metadata.creationTimestamp | tail
kubectl logs -l job-name=notif-db-reconcile-submitted --tail=200
```
