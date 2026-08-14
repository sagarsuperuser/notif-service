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

## What "2,000 rps" is and is not

It is **accepts** per second, not sends. An accept is one Postgres write plus a
durable SQS enqueue, answered with 202. It is not a delivered SMS.

Sends are a separate and much smaller number, below.

## Sustained accepts: 2,000 rps

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

## Sends: ~142/s, and that is the real ceiling

The number that matters for anything time-sensitive is how fast messages LEAVE
the queue, and it is far below the accept rate.

Over the ramp, 1,479,081 messages were accepted and 1,374,558 were still
queued at the end, so roughly 104,500 were sent in about 735 seconds:

```
send throughput     ~142/s
```

That is exactly two worker pods times TWILIO_RPS_PER_POD=70 — the configured
per-pod provider rate limit, not a property of the code. The receiving end was
the mock provider, not a real one.

So this run demonstrates an accept path that sustains 2,000/s and a send path
that sustained ~142/s against a synthetic provider. Quoting the first number
without the second describes a system that accepts work far faster than it can
do it.

## Correctness

Throughput means nothing on its own. A number this size is easy to reach by
dropping, double-sending, or double-charging a recipient's daily cap, and none
of those shows up in a latency histogram.

The repository defines eight invariants (internal/verify/invariants.go). The
table below reports six of them, checked with direct SQL over all 1,479,081
accepted messages.

**cmd/verify-run was not run, and would have failed this run.** Its
"no message was left queued" check treats a queued message as a silent drop and
is meant to run after the queue has drained; 1,374,558 were still queued, so it
would report that many violations and exit non-zero. The two daily-cap checks
are also absent here because the cap was configured at 1,000,000 and nothing
could approach it.

Read the table as "these six held", not as "the suite passed":

| invariant | result |
|---|---|
| no recipient sent the same message twice | **0 violations** |
| every sent message carries a provider id | **0 violations** |
| idempotency keys unique per tenant | **0 violations** |
| suppressed messages never sent | **0 violations** |
| nothing stuck mid-flight | **0 violations** |
| messages in DLQ | **0** — see caveat |

The DLQ result is a null result rather than a pass. Redrive fires after five
receives (sqs_send_max_receive_count = 5) and about 93% of the corpus had never
been received even once when this was measured, so an empty DLQ was
arithmetically guaranteed regardless of whether the system is correct.

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
- **Any before/after attribution.** There is no before column. The earlier run
  in docs/500rps-10m-rps/ was taken on a burstable db.t4g.xlarge with spot
  workers, and this one on a non-burstable db.m7g.xlarge, so the difference
  between them cannot be attributed to the code changes.
- **A real provider.** Every send here went to the in-cluster mock. At the time
  of this run the mock did not model Twilio's concurrency limit or its
  per-sender MPS pacing, so it cannot stand in for provider-side behaviour.
- **Control-plane HA.** A single k3s server, because this AWS account cannot
  currently create load balancers and agents join on the server's private IP.
  The control plane schedules pods and does not carry requests.
- **A large fleet.** The account's 22 vCPU limit capped the cluster at 16. The
  per-core figure is the more useful number anyway.
