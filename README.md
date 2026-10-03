# notif-service

Event-driven SMS notification service built in Go.

Correctness under injected provider failure: idempotent at-least-once delivery, no duplicate sends, no drops, dead-letter redrive drilled — and throughput reported only alongside invariants checked against the database in SQL (`internal/verify` defines ten; two of them apply only when a daily cap is set), never latency alone.

It accepts message requests, enqueues send jobs to SQS, processes sends asynchronously in workers, ingests provider webhooks, and reconciles final delivery state.

## What Is In This Repo
- `cmd/api`: HTTP API (`POST /v1/sms/messages`, `GET /v1/messages/{id}`)
- `cmd/worker`: SQS consumer that sends messages to provider
- `cmd/webhook`: webhook ingest endpoint
- `cmd/mock-provider`: simulated SMS provider (Twilio-shaped send endpoint that posts status callbacks to the webhook) for local and load-test runs
- `cmd/verify-run`: checks a load test's results against the database; exits non-zero if an invariant is violated
- `internal/`: domain, service, queue, store, provider, observability code
- `deploy/k8s`: Kubernetes manifests and overlays
- `infra`: Terraform infrastructure — deliberately small: public subnets + SG-locked ingress (no NAT), no load balancer (DNS → server EIP → ingress-nginx NodePort), one k3s server + one on-demand worker ASG (spot optional via `workers_use_spot`), RDS Postgres reached directly by the pgx pools (no RDS Proxy), SQS standard queue + DLQ, SSM for access
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

2. Create the env file:
- `make run-*` targets load `.env.local`. It is gitignored and the repo has no template for it (`notif-secrets.example.env` lists only the secret keys for the k8s `notif-secrets` secret), so create it yourself. The required variables, with values for the `make init` stack (Postgres and LocalStack from `docker-compose.local.yml`):
```bash
cat > .env.local <<'EOF'
DB_DSN=postgres://notif:notif@localhost:5432/notif?sslmode=disable
AWS_REGION=ap-south-1
LOCALSTACK_ENDPOINT=http://localhost:4566
SQS_QUEUE_URL=<the notif-send URL that make init printed>
TWILIO_ACCOUNT_SID=<your-sid>
TWILIO_AUTH_TOKEN=<your-token>
PUBLIC_WEBHOOK_URL=http://localhost:8081/v1/webhooks/twilio/status
EOF
```
- `make init` lists the queues it created; copy the `notif-send` URL from that output (or run `docker exec notif-localstack awslocal sqs get-queue-url --queue-name notif-send`).
- The worker sends to `TWILIO_BASE_URL`, which defaults to the real Twilio API (`https://api.twilio.com`). Set it if you do not want real sends. Other optional variables and their defaults are in `internal/config/config.go`.
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
- [Failure handling under load](docs/campaign-100k/retry-handling-ab-2026-08-15.md) —
  before/after runs on live AWS: 8,728 of 100,000 sends were being silently discarded by
  a classifier that tested the error before the HTTP status; after the fix, a provider
  outage lost zero messages and a full dead-letter queue was recovered with one redrive.
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
