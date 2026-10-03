# Benchmark — 14 August 2026

> Historical record: this ran on the pre-2026-08-20 architecture (NAT + private subnets, internal API NLB path, bastion, RDS Postgres behind RDS Proxy, role-pinned node pools). The infrastructure has since been simplified (see `docs/architecture/`). The numbers here describe the runs as they ran and are not restated.

Measured on AWS, against real RDS Postgres and real SQS. Every number here
came from a run on the stack described below; nothing is extrapolated.

## In short

- **The question.** How many SMS requests per second can the API accept and
  durably queue, and does it lose or duplicate any of them?
- **The result.** The accept path sustained 2,000 rps for three minutes with
  0 failures. A step ramp up to 5,000 rps dropped nothing; only latency grew.
- **The evidence.** Every request the API acknowledged has a row in the
  database. Five of the nine correctness invariants were checked and showed
  0 violations. Read that as "these five held", not as "the suite passed":
  cmd/verify-run was not run and would have failed while messages were still
  queued (see [Correctness](#correctness)).
- **The limits.** Sends ran at ~142/s, a ceiling set by a limiter inside this
  service (since removed). Sends went to a mock provider, not to Twilio. See
  [What this does not show](#what-this-does-not-show).

Two statements in this record were corrected on 15 August 2026. The
[Corrections](#corrections) section at the end keeps the original wording.

## What was measured

The **accept path**: `POST /v1/sms/messages` through to a durably queued send.
The accept path is the part of a request the caller waits on. Its cost per
request was the subject of the work leading up to this run.

The send path is deliberately not the headline. It is bounded by
`TWILIO_RPS_PER_POD=70`, so measuring it would measure the limiter.

*Corrected 15 August 2026: this section originally blamed that bound on the
provider. See [Corrections](#corrections).*

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

Every instance carrying load is non-burstable. Burstable t-family instances
throttle once their CPU credits are spent. A sustained run on them slows down
partway through and stops being reproducible.

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

**Nothing was dropped or rejected at any rate up to 5,000 rps.** Above the
sustained point, latency degrades but correctness does not. The aggregate p99
across the whole ladder is 1.52s, dominated by the top two steps.

At 1,000 rps the run needed ~16 concurrent connections to hold the rate. That
puts per-request latency around 16ms, with the API tier at 12% CPU.

## Sends: ~142/s, which was a self-imposed ceiling

*Heading corrected 15 August 2026; see [Corrections](#corrections).*

For anything time-sensitive, the number that matters is how fast messages
LEAVE the queue. That rate is far below the accept rate.

Over the ramp, 1,479,081 messages were accepted and 1,374,558 were still
queued at the end. So roughly 104,500 were sent in about 735 seconds:

```
send throughput     ~142/s
```

That is exactly two worker pods times TWILIO_RPS_PER_POD=70. The ceiling was
the configured per-pod limiter, which was a property of this service's code.
The receiving end was the mock provider (`cmd/mock-provider`), not a real
provider.

The limiter has since been deleted, for two reasons:

- It bounded requests per second, while the provider bounds requests in
  flight.
- It was per-pod, and the autoscaler adds pods on queue depth. So its
  account-wide ceiling rose with the backlog.

142/s was this service throttling itself, not the provider pushing back.

So this run demonstrates two numbers: an accept path that sustains 2,000/s,
and a send path that sustained ~142/s against a synthetic provider. Quoting
the first number without the second describes a system that accepts work far
faster than it can do it.

## Correctness

Throughput means nothing on its own. A system can reach a number this size by
dropping messages, sending one twice, or charging a recipient's daily cap
twice. None of those shows up in a latency histogram.

The repository defines nine invariants (internal/verify/invariants.go). The
table below reports five of them, checked with direct SQL over all 1,479,081
accepted messages.

**cmd/verify-run was not run, and would have failed this run.** Two of its
checks split the queued messages by whether a message has ever been attempted:

- **"no message was left queued without being attempted"** treats a queued
  message that was never attempted as a silent drop.
- **"no message was parked after repeated provider failures"** covers the
  rest. A parked message is one that was attempted, failed with a transient
  error, and was handed back to the queue.

Both checks are meant to run after the queue has drained. Mid-run, a message
sitting in the queue is work still in flight, not work abandoned. Here
1,374,558 were still queued, so together the two checks would report that many
violations and exit non-zero. That is why both are absent from the table.

The two daily-cap checks are also absent here, because the cap was configured
at 1,000,000 and nothing could approach it.

Read the table as "these five held", not as "the suite passed":

| invariant | result |
|---|---|
| no recipient sent the same message twice | **0 violations** |
| every sent message carries a provider id | **0 violations** |
| idempotency keys unique per tenant | **0 violations** |
| suppressed messages never sent | **0 violations** |
| nothing stuck mid-flight | **0 violations** |

The DLQ (dead-letter queue) held **0** messages. That is a queue observation,
not one of the nine invariants, and it is a null result rather than a pass.
Redrive fires after five receives (sqs_send_max_receive_count = 5). When this
was measured, about 93% of the corpus had never been received even once. So an
empty DLQ was arithmetically guaranteed, whether or not the system is correct.

The count deserves its own line. k6 reported 1,479,077 requests accepted, and
the database holds 1,479,081 rows: those requests plus four from the earlier
smoke test. **Every request the API acknowledged has a row.** Nothing was
acknowledged and lost.

## The queue absorbed the difference

At the end of the ramp, 1,374,558 messages were still queued and 0 were in the
DLQ. The accept path outran the send path by more than an order of magnitude.
That is what the queue is for: the drain rate sets how fast messages leave,
and the queue lets a burst be accepted long before it can be delivered. On
this run, the drain rate was set by the limiter described above, not by
anything the provider imposed.

## What this does not show

- **The ingress path.** k6 ran in-cluster against the service address. The
  nginx ingress carries `limit-rps: 20`, keyed on client address, so a run
  through the front door would have measured that limiter. Raising it is a
  per-deployment decision and was out of scope here.
- **A tuned ceiling.** 5,000 rps is where the ladder stopped, not where the
  service stopped. Nothing was dropped there.
- **Any before/after attribution.** There is no before column. The earlier run
  in docs/500rps-10m-rps/ was taken on a burstable db.t4g.xlarge with spot
  workers. This one ran on a non-burstable db.m7g.xlarge. So the difference
  between them cannot be attributed to the code changes.
- **A real provider.** Every send here went to the in-cluster mock provider.
  At the time of this run, the mock did not model Twilio's concurrency limit
  or its per-sender MPS (messages per second) pacing. So it cannot stand in
  for provider-side behaviour.
- **Control-plane HA.** The cluster had a single k3s server, because this AWS
  account cannot currently create load balancers and agents join on the
  server's private IP. The control plane schedules pods and does not carry
  requests.
- **A large fleet.** The account's 22 vCPU limit capped the cluster at 16. The
  per-core figure is the more useful number anyway.

## Corrections

**Correction, 15 August 2026** (to [What was measured](#what-was-measured)).
That section originally called the send-path bound "the provider's rate limit
... not [something] in this service". That was backwards.
`TWILIO_RPS_PER_POD` was a token bucket inside this service, and the ceiling it
produced was self-imposed rather than external. The measurement is unaffected,
because 142/s is what the run did. But the attribution was wrong, and that
particular error is what let the limiter survive as long as it did. When a
ceiling is blamed on the provider, nobody treats it as a bug in this service.
The limiter has since been removed; see
[Sends](#sends-142s-which-was-a-self-imposed-ceiling) above.

**Corrected heading, 15 August 2026** (to
[Sends](#sends-142s-which-was-a-self-imposed-ceiling)). That section was titled
"and that is the real ceiling". The rate was real and remains as measured;
calling it *the* ceiling was not. It was the per-pod limiter, described in that
section.
