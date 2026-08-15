# 100,000-message campaign — evidence

Run on AWS, 15 August 2026. Commit `sha-3d08e8d`. Provider profile: short code
at 500 MPS (Twilio's own Account Based Throughput worked example).

There are no Grafana screenshots here. Prometheus was never installed on this
cluster — the addon was skipped to keep the bootstrap tractable, and rather
than stand it up afterwards and re-run, the evidence below is taken from
sources that were already recording: AWS CloudWatch, which observes SQS
independently of anything this service reports about itself, and the
application's own Prometheus endpoint scraped directly.

CloudWatch is arguably the better witness. It is not instrumentation this
project wrote, so it cannot be wrong in the same direction as the code.

## Result

```
COMPLETE: 100,000 delivered in 293s

message states          delivered | 100000     (no other state)
main queue              0
dead-letter queue       0
```

## CloudWatch — AWS/SQS, one-minute periods

Independent of the application. `notif-prod-test-send`.

| minute (IST) | sent to queue | deleted from queue | depth | age of oldest |
|---|---|---|---|---|
| 00:43 | 17,700 | 15,876 | 0 | 0s |
| 00:44 | 22,057 | 22,105 | 2,530 | **11s** |
| 00:45 | 22,006 | 23,375 | 2,099 | 7s |
| 00:46 | 21,915 | 22,181 | 0 | 0s |
| 00:47 | 16,322 | 16,463 | 2 | 1s |
| total | **100,000** | **100,000** | — | — |

Two things this shows that a throughput figure cannot.

Deletions track sends minute for minute, so the workers consumed at the rate
the API produced rather than falling behind and catching up later. And the
queue peaked at 2,530 — 2.5% of the campaign — with the oldest message never
older than eleven seconds. No backlog ever formed.

That age figure is the one that matters for anything time-sensitive. A message
entering this queue during the campaign waited seconds, not the 78 seconds
measured earlier when the pipeline was rate-limited below the provider, and not
the thirty minutes measured when it was badly misconfigured.

## Application metrics, one worker pod of two

Scraped from the pod's /metrics after the run.

```
notif_worker_processed_total{result="success"}   49942   [metric since DELETED]
twilio_send_total{http_status="201",result="ok"} 49942
notif_db_roundtrips_total{outcome="ok"}          99884   [renamed notif_db_query_calls_total]
```

49,942 messages against 99,884 database round-trips is **exactly 2.00 per
message**, which is the worker path this project cut from four — confirmed here
against production traffic rather than in a test. One HTTP 201 per message and
no other status means every message succeeded on its first attempt, with no
retries inflating the throughput.

Timings, same pod:

```
twilio_send_latency_seconds       408ms mean   [metric since DELETED — see note]
notif_worker_processing_seconds   418ms mean   (20895.97s / 49942)
notif_end_to_end_latency_seconds  4.30s mean   (214809.43s / 49942)
```

The provider call is 408ms of the 418ms a message spends being processed, so
97% of worker time is spent waiting on the provider and 3% on everything this
service does. That is the correct shape: the remaining engineering value is in
not adding to it.

## The instruments behind these figures were later found faulty

An audit of every metric against its increment sites, run after this campaign,
returned six verdicts: five misleading and one wrong. Three affect the numbers
above and are marked at the point they appear.

notif_worker_processed_total defaulted its outcome label to "success" and had
five error returns that never reassigned it, so failures counted as successes.
Deleted, replaced by notif_message_outcome_total, which starts empty.

twilio_send_latency_seconds started its clock before a token-bucket rate limiter
and a three-attempt retry loop. The 408ms is limiter queueing plus retries plus
the call, not provider latency. Deleted, replaced by notif_provider_call_seconds
wrapping only the call, with the limiter measured separately.

notif_db_roundtrips_total is neither every query nor one per round-trip: pgx
routes pool.Ping below the tracer, out-of-process SQL never reaches it, acquire
failures produce no increment, and a statement-cache miss is one increment
covering two wire exchanges. Renamed notif_db_query_calls_total. The 2.00
per-message ratio also used a denominator that excludes duplicate deliveries
counted in the numerator.

What survives unaffected: the message counts and states, which come from
Postgres; the SQS series, which come from CloudWatch; and the round-trip
reduction itself, which was measured by a dedicated counted pool in
tests/integration/roundtrips_test.go rather than by the production counter.

## One thing worth fixing

```
notif_db_pool_empty_acquires_total{service="worker"}  8603
notif_db_pool_connections{service="worker",state="max"}  20
```

8,603 acquisitions found the connection pool empty and waited. The pool is 20
and worker concurrency is 100, so five handlers contend for each connection.
It did not stop the run reaching the provider's ceiling — the provider call
dominates so heavily that handlers are rarely all inside the database at once —
but it is real contention and the pool is the wrong size for the concurrency.

## What this run does not show

- **A real provider.** Sends went to the in-cluster simulator, which models
  documented limits (concurrency 429s, per-sender MPS pacing, the ten-hour
  queue and its 30001 overflow) but is not Twilio.
- **Failure handling.** `MOCK_SUCCESS_RATE` was 1.0. Real traffic carries a few
  percent of failures with retries and dead-lettering, which this clean run
  deliberately excludes.
- **Multi-segment messages.** MPS is counted in segments, so a campaign of
  two-segment messages halves the effective rate.
- **Grafana.** As above.
