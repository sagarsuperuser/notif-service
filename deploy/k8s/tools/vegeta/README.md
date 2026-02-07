# In-cluster Vegeta load tests

This folder contains simple in-cluster Vegeta jobs for repeatable load tests.

## Apply (2 req/sec for 10 minutes)

```bash
kubectl apply -f deploy/k8s/tools/vegeta/notif-api-2rps-10m.yaml
```

## Tail logs

```bash
kubectl logs -l job-name=vegeta-notif-api-2rps-10m -f
```

## Cleanup

```bash
kubectl delete -f deploy/k8s/tools/vegeta/notif-api-2rps-10m.yaml
```

## Notes

- The job runs inside the cluster and hits `http://notif-api-svc/v1/sms/messages`.
- Update the payload in the ConfigMap if your API schema changes.
- Set `duration` and `rate` in the job args as needed.
