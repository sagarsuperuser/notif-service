# notif-service

Event-driven SMS notification service built in Go.

It accepts message requests, enqueues send jobs to SQS, processes sends asynchronously in workers, ingests provider webhooks, and reconciles final delivery state.

## What Is In This Repo
- `cmd/api`: HTTP API (`POST /v1/sms/messages`, `GET /v1/messages/{id}`)
- `cmd/worker`: SQS consumer that sends messages to provider
- `cmd/webhook`: webhook ingest endpoint
- `cmd/webhook-processor`: async webhook event processor
- `internal/`: domain, service, queue, store, provider, observability code
- `deploy/k8s`: Kubernetes manifests and overlays
- `infra`: Terraform infrastructure (AWS network, SQS, RDS, etc.)
- `docs/architecture`: architecture diagrams

## Architecture (High Level)
1. API writes message intent to DB and enqueues send job.
2. Worker consumes queue, applies rate limit/retry/backoff/circuit-breaker, calls provider, updates DB.
3. Provider webhook is ingested and enqueued.
4. Webhook processor consumes webhook queue and applies terminal status updates.

See diagrams: `docs/architecture/README.md`.

## Local Quick Start

Prerequisites:
- Go 1.22+
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
go run ./cmd/webhook-processor
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

## Benchmarks and Docs
- Architecture docs: `docs/architecture/`
- 500 RPS benchmark report: `docs/500rps-10m-rps/benchmark-scenario-500rps.md`
- Grafana dashboards: `deploy/grafana/dashboards/`
