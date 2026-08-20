# 01) System Context

```mermaid
flowchart LR
    subgraph ext[External]
      users[Clients / Campaign Systems]
      k6[k6 Load Job]
      provider["Provider Twilio Mock"]
    end

    subgraph aws[AWS]
      entry["server EIP : ingress-nginx NodePort"]
      api[notif-api]
      worker[notif-worker]
      webhook[notif-webhook]
      keda[KEDA]
      mon[Prometheus + Grafana]

      qsend[(SQS Send Queue)]
      db[(Postgres RDS)]
    end

    users --> entry --> api
    k6 --> entry

    api --> qsend
    qsend --> worker
    worker --> provider

    provider --> entry --> webhook

    api --> db
    worker --> db
    webhook --> db

    keda -. scales .-> worker

    api -. metrics .-> mon
    worker -. metrics .-> mon
    webhook -. metrics .-> mon
```

No load balancer (the account cannot create them; DNS points at the server
EIP), no RDS Proxy (each service's pgx pool talks to Postgres directly), no
webhook queue (the webhook handler applies the status update in one
statement). The pre-2026-08-20 topology these replaced is recorded in the
campaign docs.
