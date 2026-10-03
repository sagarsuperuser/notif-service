# notif-service

Event-driven SMS notification service built in Go.

It accepts message requests, enqueues send jobs to SQS, processes sends asynchronously in workers, ingests provider webhooks, and reconciles final delivery state.

**Start here:** [engineering notes](docs/engineering-notes.md) (a two-minute read on what this project demonstrates), then [Results](#results).

It is correct under injected provider failure:

- Delivery is idempotent and at-least-once, with no duplicate sends and no drops.
- Dead-letter redrive has been drilled.
- Throughput is reported only together with invariants checked against the database in SQL, never as latency alone. `internal/verify` defines ten invariants; two of them apply only when a daily cap is set.

## What Is In This Repo
- `cmd/api`: HTTP API (`POST /v1/sms/messages`, `GET /v1/messages/{id}`)
- `cmd/worker`: SQS consumer that sends messages to provider
- `cmd/webhook`: webhook ingest endpoint
- `cmd/mock-provider`: simulated SMS provider (Twilio-shaped send endpoint that posts status callbacks to the webhook) for local and load-test runs
- `cmd/verify-run`: checks a load test's results against the database; exits non-zero if an invariant is violated
- `internal/`: domain, service, queue, store, provider, observability code
- `deploy/k8s`: Kubernetes manifests and overlays
- `infra`: Terraform infrastructure, kept deliberately small:
  - public subnets and SG-locked ingress (no NAT)
  - no load balancer: DNS → server EIP (Elastic IP) → ingress-nginx NodePort
  - one k3s server and one on-demand worker ASG (spot optional via `workers_use_spot`)
  - RDS Postgres reached directly by the pgx pools (no RDS Proxy)
  - SQS standard queue + DLQ
  - SSM for access
- `docs/architecture`: architecture diagrams

## Architecture (High Level)
1. API writes message intent to DB and enqueues send job.
2. Worker consumes queue, applies rate limit/retry/backoff/circuit-breaker, calls provider, updates DB.
3. Provider webhook applies the terminal status update in one statement (ingest-only handler; no intermediate queue).

See diagrams: `docs/architecture/README.md`.

## Results

- [100k campaign](docs/campaign-100k/README.md): 100,000 delivered in 293 s, with zero
  duplicates, zero drops and zero dead-lettered. The run was reconciled against AWS
  CloudWatch, a recording this service does not produce. Sends went to the mock
  provider, and nothing was set to fail.
- [Failure handling under load](docs/campaign-100k/retry-handling-ab-2026-08-15.md):
  before/after runs on live AWS. Before the fix, 8,838 of 100,000 sends (8.8%) were
  being silently discarded by a classifier that tested the error before the HTTP status.
  With the fix, the same run delivered 8,728 more messages. A provider outage lost zero
  messages, and one redrive recovered a full dead-letter queue.
- [Accept-path benchmark](docs/benchmark-2026-08-14.md): the accept path sustained
  2,000 accepts/sec at p99 142 ms. The doc also explains why the send path had a
  separate ceiling of ~142/s: our own limiter.
- [Measured improvements](docs/measured-improvements.md): every figure is re-derivable,
  and withdrawn claims are kept, not deleted.
- [Architecture](docs/architecture/) · [Grafana dashboards](deploy/grafana/dashboards/)
- [500 RPS capacity study (Feb 2026)](docs/500rps-10m-rps/benchmark-scenario-500rps.md):
  the earlier run that found the processing ceiling at ~241 ops/sec. It is superseded,
  and kept because it is where the bottleneck work started.

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
- `make run-*` targets load `.env.local`. It is gitignored and the repo has no template for it, so create it yourself.
- `notif-secrets.example.env` is not that template: it lists only the secret keys for the k8s `notif-secrets` secret.
- The required variables are below, with values for the `make init` stack (Postgres and LocalStack from `docker-compose.local.yml`):
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
- Dev note: branch from `origin/main` explicitly (`git fetch origin && git switch -c my-branch origin/main`). Local `main` checkouts in worktree setups run behind. This caused three stale-base incidents in one week, including the PR that added this line.

## License

MIT. See [LICENSE](LICENSE).
