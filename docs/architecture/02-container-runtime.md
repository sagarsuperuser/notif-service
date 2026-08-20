# 02) Container / Runtime View

```mermaid
flowchart TB
  subgraph ext[External]
    client[Clients / k6]
    provider["Provider Twilio Mock"]
  end

  subgraph aws[AWS VPC — public subnets, SG-locked]
    entry["server EIP :30080/:30443 (ingress-nginx NodePort)"]

    subgraph k8s["k3s: 1 server (m7i.large) + worker ASG (c7i.large, spot)"]
      api[notif-api]
      worker[notif-worker]
      webhook["notif-webhook ingest"]
      keda[KEDA]
      prom[Prometheus]
      grafana[Grafana]
    end

    qsend[(SQS send.fifo + DLQ)]
    db[(Postgres RDS — direct, pgx pools)]
  end

  %% ingress and request path
  client --> entry
  entry --> api
  provider --> entry
  entry --> webhook

  %% message processing
  api --> qsend
  qsend --> worker
  worker --> provider

  %% data path
  api --> db
  worker --> db
  webhook --> db

  %% control and observability
  keda -. scales by queue depth .-> worker

  api -. metrics .-> prom
  worker -. metrics .-> prom
  webhook -. metrics .-> prom
  prom --> grafana
```

Access for operators is SSM (no bastion, no SSH required); kubectl reaches
the server EIP on 6443, SG-locked to the admin CIDR. SQS is reached via the
internet gateway (public subnets — no NAT, so no per-GB toll on the
worker→SQS path).
