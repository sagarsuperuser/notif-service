# notif-service

Event-driven SMS notification service built in Go.

Correctness under injected provider failure: idempotent at-least-once delivery, no duplicate sends, no drops, dead-letter redrive drilled — and throughput reported only alongside invariants checked against the database in SQL (`internal/verify` defines nine), never latency alone.

It accepts message requests, enqueues send jobs to SQS, processes sends asynchronously in workers, ingests provider webhooks, and reconciles final delivery state.

## What Is In This Repo
- `cmd/api`: HTTP API (`POST /v1/sms/messages`, `GET /v1/messages/{id}`)
- `cmd/worker`: SQS consumer that sends messages to provider
- `cmd/webhook`: webhook ingest endpoint
- `internal/`: domain, service, queue, store, provider, observability code
- `deploy/k8s`: Kubernetes manifests and overlays
- `infra`: Terraform infrastructure — deliberately small: public subnets + SG-locked ingress (no NAT), no load balancer (DNS → server EIP → ingress-nginx NodePort), one k3s server + one spot worker ASG, RDS Postgres reached directly by the pgx pools (no RDS Proxy), SQS FIFO + DLQ, SSM for access
- `docs/architecture`: architecture diagrams

## Architecture (High Level)
1. API writes message intent to DB and enqueues send job.
2. Worker consumes queue, applies rate limit/retry/backoff/circuit-breaker, calls provider, updates DB.
3. Provider webhook applies the terminal status update in one statement (ingest-only handler; no intermediate queue).

See diagrams: `docs/architecture/README.md`.

## Local Quick Start

Prerequisites:
- Go 1.25+
- Docker / Docker Compose
- `psql`

1. Start local dependencies and initialize schema/queues:
```bash
make init
```

2. Ensure env file exists:
- `make run-*` targets load `.env.local`
- Update `.env.local` values for your local setup if needed.

3. Run services (separate terminals):
```bash
make run-api
make run-worker
make run-webhook
```

4. Send a test request:
```bash
curl -X POST http://localhost:8080/v1/sms/messages \
  -H 'Content-Type: application/json' \
  -d '{
    "tenantId":"foodapp",
    "idempotencyKey":"demo-1",
    "to":"+14155552671",
    "templateId":"txn_confirm_v1",
    "vars":{"name":"Sam","orderId":"A-123"}
  }'
```

## Useful Commands
```bash
make up                # start postgres + localstack
make queues            # create local SQS queues
make migrate           # run schema
make seed              # seed baseline data
make test              # unit tests
make test-integration  # integration tests
make down              # stop local infra
make reset             # stop + delete volumes
```

## Kubernetes / Infra
- K8s deploy (dev overlay): `make k8s-up`
- Restart workloads: `make k8s-restart`
- Terraform stack: `infra/`
- Dev note: **branch from `origin/main` explicitly** (`git fetch origin && git switch -c my-branch origin/main`). Local `main` checkouts in worktree setups run behind — three stale-base incidents in one week, including the PR that added this line.

## Results

- [100k campaign](docs/campaign-100k/README.md) — 100,000 delivered in 293 s, reconciled
  against AWS CloudWatch (a recording this service does not produce), zero duplicates,
  zero drops, zero dead-lettered.
- [Retry-handling A/B](docs/campaign-100k/retry-handling-ab-2026-08-15.md) — controlled
  experiment on live AWS: 8,728 of 100,000 sends were being silently discarded by a
  classifier that tested the error before the HTTP status.
- [Accept-path benchmark](docs/benchmark-2026-08-14.md) — 2,000 accepts/sec sustained,
  p99 142 ms, and why the send path's separate ~142/s ceiling was our own limiter.
- [Measured improvements](docs/measured-improvements.md) — every figure re-derivable;
  withdrawn claims kept, not deleted.
- [Architecture](docs/architecture/) · [Grafana dashboards](deploy/grafana/dashboards/)
- [500 RPS capacity study (Feb 2026)](docs/500rps-10m-rps/benchmark-scenario-500rps.md)
  — the earlier run that found the processing ceiling at ~241 ops/sec. Superseded;
  kept because it is where the bottleneck work started.

## License

MIT — see [LICENSE](LICENSE).
