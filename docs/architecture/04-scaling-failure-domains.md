# 04) Scaling and Failure Domains

```mermaid
flowchart LR
  subgraph ext[External]
    mock[provider mock]
  end

  subgraph domains[Runtime Domains]
    ingress[Ingress domain]
    app[App processing domain]
    data[Data domain]
  end

  ingress --> app --> data

  nlb[NLB + NodePort targets]
  api[notif-api]
  webhook[notif-webhook ingest]
  worker[notif-worker]
  whproc[webhook-processor]
  qsend[(SQS send)]
  qwebhook[(SQS webhook-events)]
  proxy[(RDS Proxy)]
  db[(Postgres RDS)]

  nlb --> api
  nlb --> webhook
  api --> qsend --> worker --> mock
  mock --> nlb
  webhook --> qwebhook --> whproc
  api --> proxy
  worker --> proxy
  whproc --> proxy
  proxy --> db

  classDef risk fill:#ffe6e6,stroke:#c00,color:#000;
  classDef ctrl fill:#e6f3ff,stroke:#1f78b4,color:#000;

  overload_worker["If send rate > worker throughput, queue lag rises"]:::risk
  overload_db["If DB CPU/connections saturate, timeout/error rates rise"]:::risk
  spot_loss["If spot nodes are evicted, replicas drop and latency spikes"]:::risk
  controls["Controls: KEDA bounds, worker/webhook concurrency caps, retries with exponential backoff, circuit breakers, proxy tuning, DLQ, idempotency"]:::ctrl

  qsend -.-> overload_worker
  proxy -.-> overload_db
  worker -.-> spot_loss
  whproc -.-> spot_loss
  controls -.-> app
  controls -.-> data
```

Backpressure operating rule:
- Tune `notif-worker` and `webhook-processor` concurrency and autoscaling bounds based on queue lag and DB headroom, so backlog is absorbed in SQS while `Postgres/RDS Proxy` and downstream dependencies stay within safe CPU, connection, and timeout limits.
- Keep retries bounded with exponential backoff and use circuit breakers to fail fast during sustained downstream failures, preventing retry storms from exhausting DB and compute resources.
