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

  entry["server EIP + NodePort (no LB)"]
  api[notif-api]
  webhook[notif-webhook ingest]
  worker[notif-worker]
  qsend[(SQS send)]
  db[(Postgres RDS)]

  entry --> api
  entry --> webhook
  api --> qsend --> worker --> mock
  mock --> entry
  api --> db
  worker --> db
  webhook --> db

  classDef risk fill:#ffe6e6,stroke:#c00,color:#000;
  classDef ctrl fill:#e6f3ff,stroke:#1f78b4,color:#000;

  overload_worker["If send rate > worker throughput, queue lag rises"]:::risk
  overload_db["If DB CPU/connections saturate, timeout/error rates rise"]:::risk
  spot_loss["If spot nodes are evicted, replicas drop and latency spikes"]:::risk
  controls["Controls: KEDA bounds, worker/webhook concurrency caps, retries with exponential backoff, circuit breakers, pgx pool caps, DLQ, idempotency"]:::ctrl

  qsend -.-> overload_worker
  db -.-> overload_db
  worker -.-> spot_loss
  controls -.-> app
  controls -.-> data
```

Backpressure operating rule:
- Tune `notif-worker` concurrency and autoscaling bounds based on queue lag and DB headroom, so backlog is absorbed in SQS while Postgres and downstream dependencies stay within safe CPU, connection, and timeout limits.
- Keep retries bounded with exponential backoff and use circuit breakers to fail fast during sustained downstream failures, preventing retry storms from exhausting DB and compute resources.

How this scales (and what it costs today by not pre-building it):
- Workers scale by one variable (`worker_count`; KEDA scales pods within the
  pool). A measurement-grade pool is `worker_on_demand_percentage = 100`.
- The single k3s server is the availability trade: control plane and ingress
  entry ride one instance (its EIP survives replacement; ~5 min to recreate
  from Terraform). If that ever stops being acceptable, the upgrade path is
  the machinery deliberately removed on 2026-08-20: 3 servers with etcd, a
  join endpoint, and a load balancer in front of ingress — reintroduce it
  when the requirement is real, not before.
