# Benchmark — 14 August 2026

Measured on AWS, against real RDS Postgres and real SQS. Every number here
came from a run on the stack described below; nothing is extrapolated.

## What was measured

The **accept path**: `POST /v1/sms/messages` through to a durably queued send.
That is the path a caller waits on, and the one whose cost per request was the
subject of the work leading up to this.

The send path is deliberately not the headline. It is bounded by the provider's
rate limit (`TWILIO_RPS_PER_POD=70`), not by anything in this service, so
measuring it would measure the limiter.

## Stack

| | |
|---|---|
| API tier | 4 pods on 2 × `c7i.large` — **4 vCPU total** |
| Workers | 2 pods on 2 × `c7i.large` |
| Database | RDS Postgres `db.m7g.xlarge` (4 vCPU, 16 GiB) via RDS Proxy |
| Queue | SQS **standard** |
| Orchestration | self-managed k3s v1.34.3 on EC2, 8 nodes |
| Load generator | k6 on a dedicated `m7i.large`, pinned off the app nodes |
| Region | ap-south-1 |

Every instance carrying load is non-burstable. t-family instances throttle once
their CPU credits are spent, which makes a sustained run decay partway through
and stop being reproducible.

## Sustained: 2,000 rps

Three minutes at a constant arrival rate.

```
requests          359,598
accepted          359,598   (100%)
failed                  0   (0.00%)
achieved rate     1,997.6/s

latency   median   17.6 ms
          p95      52.1 ms
          p99     141.5 ms
          max     612.5 ms
```

## Step ramp: 200 → 5,000 rps

Seven held rates, 90 seconds each.

```
requests        1,479,077
accepted        1,479,077   (100%)
failed                  0
dropped                 0
DLQ                     0
```

**Nothing was dropped or rejected at any rate up to 5,000 rps.** What degrades
above the sustained point is latency, not correctness: the aggregate p99 across
the whole ladder is 1.52s, dominated by the top two steps.

At 1,000 rps the run needed ~16 concurrent connections to hold the rate, which
puts per-request latency around 16ms — the API tier was at 12% CPU.

## Correctness

Throughput means nothing on its own. A number this size is easy to reach by
dropping, double-sending, or double-charging a recipient's daily cap, and none
of those shows up in a latency histogram.

Checked against the database over all 1,479,081 accepted messages:

| invariant | result |
|---|---|
| no recipient sent the same message twice | **0 violations** |
| every sent message carries a provider id | **0 violations** |
| idempotency keys unique per tenant | **0 violations** |
| suppressed messages never sent | **0 violations** |
| nothing stuck mid-flight | **0 violations** |
| messages in DLQ | **0** |

The count is worth its own line: k6 reported 1,479,077 requests accepted, and
the database holds 1,479,081 rows — those requests plus four from the earlier
smoke test. **Every request the API acknowledged has a row.** Nothing was
acknowledged and lost.

## The queue absorbed the difference

At the end of the ramp, 1,374,558 messages were still queued and 0 were in the
DLQ. The accept path outran the send path by more than an order of magnitude,
which is what the queue is there for: the provider rate limit sets how fast
messages leave, and the queue lets a burst be accepted long before it can be
delivered.

## What this does not show

- **The ingress path.** k6 ran in-cluster against the service address. The
  nginx ingress carries `limit-rps: 20`, keyed on client address, so a run
  through the front door would have measured that limiter. Raising it is a
  per-deployment decision and was out of scope here.
- **A tuned ceiling.** 5,000 rps is where the ladder stopped, not where the
  service stopped. Nothing was dropped there.
- **Control-plane HA.** A single k3s server, because this AWS account cannot
  currently create load balancers and agents join on the server's private IP.
  The control plane schedules pods and does not carry requests.
- **A large fleet.** The account's 22 vCPU limit capped the cluster at 16. The
  per-core figure is the more useful number anyway.
