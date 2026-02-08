COMPOSE = docker compose -f docker-compose.local.yml
PG_DSN = postgres://notif:notif@localhost:5432/notif?sslmode=disable
LS_CONTAINER = notif-localstack
ENV_FILE=.env.local

.PHONY: up down logs reset queues migrate seed init test test-integration k8s-up k8s-down k8s-restart k8s-secrets
.PHONY: docker-build k3d-import k3d-build-import

up:
	$(COMPOSE) up -d

down:
	$(COMPOSE) down

logs:
	$(COMPOSE) logs -f

reset:
	$(COMPOSE) down -v

queues:
	docker exec -i $(LS_CONTAINER) bash -lc '\
	set -euo pipefail; \
	echo "Creating DLQ..."; \
	awslocal sqs create-queue --queue-name notif-send-dlq.fifo \
	  --attributes "{\"FifoQueue\":\"true\",\"ContentBasedDeduplication\":\"true\"}" >/dev/null || true; \
	DLQ_URL=$$(awslocal sqs get-queue-url --queue-name notif-send-dlq.fifo --query QueueUrl --output text); \
	DLQ_ARN=$$(awslocal sqs get-queue-attributes --queue-url "$$DLQ_URL" --attribute-names QueueArn --query Attributes.QueueArn --output text); \
	REDRIVE=$$(printf "{\"deadLetterTargetArn\":\"%s\",\"maxReceiveCount\":\"5\"}" "$$DLQ_ARN"); \
	REDRIVE_ESC=$${REDRIVE//\"/\\\"}; \
	echo "Creating main FIFO queue with DLQ redrive..."; \
	awslocal sqs create-queue --queue-name notif-send.fifo \
	  --attributes "{\"FifoQueue\":\"true\",\"ContentBasedDeduplication\":\"true\",\"RedrivePolicy\":\"$$REDRIVE_ESC\"}" >/dev/null || true; \
	awslocal sqs list-queues; \
	echo "Done.";'

migrate:
	psql "$(PG_DSN)" -f deploy/k8s/jobs/sql/001_init.sql

seed:
	psql "$(PG_DSN)" -f deploy/k8s/jobs/sql/seed.sql


init: up queues migrate seed
	@echo "Local infra ready."


env:
	@test -f $(ENV_FILE) || (echo "Missing $(ENV_FILE). Create it from .env.example" && exit 1)

run-api: env
	@set -a; . ./$(ENV_FILE); set +a; \
	PORT=$${API_PORT:-8080} METRICS_PORT=$${API_METRICS_PORT:-9090} go run ./cmd/api

run-worker: env
	@set -a; . ./$(ENV_FILE); set +a; \
	PORT=$${WORKER_PORT:-8082} METRICS_PORT=$${WORKER_METRICS_PORT:-9090} go run ./cmd/worker

run-webhook: env
	@set -a; . ./$(ENV_FILE); set +a; \
	PORT=$${WEBHOOK_PORT:-8081} METRICS_PORT=$${WEBHOOK_METRICS_PORT:-9090} go run ./cmd/webhook


test:
	go test ./... -v

test-integration: env
	@set -a; . ./$(ENV_FILE); set +a; \
	go test -tags=integration ./tests/integration -v

docker-build:
	docker build -t notif-api:dev --build-arg CMD=api .
	docker build -t notif-worker:dev --build-arg CMD=worker .
	docker build -t notif-webhook:dev --build-arg CMD=webhook .
	docker build -t notif-mock-provider:dev --build-arg CMD=mock-provider .

k3d-import:
	k3d image import notif-api:dev notif-worker:dev notif-webhook:dev notif-mock-provider:dev -c notif

k3d-build-import: docker-build k3d-import


k8s-restart:
	kubectl rollout restart deploy/notif-api deploy/notif-worker deploy/notif-webhook

k8s-up:
	kubectl apply -k deploy/k8s/overlays/dev
	

k8s-down:
	kubectl delete -k deploy/k8s/overlays/dev


k8s-secrets:
	kubectl create secret generic notif-secrets --from-env-file=.env.secrets \
	  --dry-run=client -o yaml | kubectl apply -f -
