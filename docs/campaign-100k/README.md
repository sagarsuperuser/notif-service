# 100,000-message campaign — evidence

> Historical record: this ran on the pre-2026-08-20 architecture. That stack
> had NAT + private subnets, an internal API NLB path, a bastion, RDS Postgres
> behind RDS Proxy, and role-pinned node pools. The infrastructure has since
> been simplified (see `docs/architecture/`). The numbers here describe the
> runs as they ran and are not restated.

## In short

- **Question:** can the pipeline deliver a 100,000-message campaign at the
  provider's ceiling without a backlog forming?
- **Result:** 100,000 delivered in 293s, all in the delivered state, with an
  empty main queue and an empty dead-letter queue.
- **Proof:** AWS CloudWatch, which records SQS independently of this service,
  shows deletions tracking sends minute for minute. The oldest message was
  never older than eleven seconds.
- **Limits:** sends went to a mock provider, not Twilio, and nothing was set to
  fail. See [What this does not show](#what-this-does-not-show).

**Conditions.** Run on AWS, 15 August 2026. Commit `sha-3d08e8d`. Provider
profile: short code at 500 MPS (messages per second). This profile is Twilio's
own Account Based Throughput worked example.

## Result

```
COMPLETE: 100,000 delivered in 293s

message states          delivered | 100000     (no other state)
main queue              0
dead-letter queue       0
```

> The last line is weaker evidence than it looks. Nothing failed on this run, so
> nothing could have been dead-lettered. See "What this does not show".

**Where the evidence comes from.** There are no Grafana screenshots here,
because Prometheus was never installed on this cluster. The addon was skipped
to keep the bootstrap tractable. Rather than stand it up afterwards and re-run,
the evidence below comes from two sources that were already recording:

- AWS CloudWatch, which observes SQS independently of anything this service
  reports about itself.
- The application's own Prometheus endpoint, scraped directly.

CloudWatch is arguably the better witness. This project did not write it, so
it cannot be wrong in the same direction as the code.

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

This table shows two things that a throughput figure cannot:

- **The workers kept pace.** Deletions track sends minute for minute. The
  workers consumed at the rate the API produced, and did not fall behind and
  catch up later.
- **No backlog ever formed.** The queue peaked at 2,530 messages, which is 2.5%
  of the campaign. The oldest message was never older than eleven seconds.

The age figure is the one that matters for anything time-sensitive. A message
that entered this queue during the campaign waited seconds. Earlier runs
measured longer waits:

- 78 seconds, when the pipeline was rate-limited below the provider.
- Thirty minutes, when it was badly misconfigured.

## Application metrics, one worker pod of two

> Three of the metrics below were later found faulty, and are tagged where they
> appear. See [The instruments behind these figures were later found
> faulty](#the-instruments-behind-these-figures-were-later-found-faulty). The
> message counts and states in the Result block (from Postgres) and the SQS
> series (from CloudWatch) are unaffected; the per-pod counters below are not
> those counts.

Scraped from the pod's /metrics after the run.

```
notif_worker_processed_total{result="success"}   49942   [metric since DELETED]
twilio_send_total{http_status="201",result="ok"} 49942
notif_db_roundtrips_total{outcome="ok"}          99884   [renamed notif_db_query_calls_total]
```

49,942 messages against 99,884 database round-trips is **exactly 2.00 per
message**. That is the worker path this project cut from four, confirmed here
against production traffic rather than in a test.

There was one HTTP 201 per message and no other status. So every message
succeeded on its first attempt, and no retries inflated the throughput.

Timings, same pod:

```
twilio_send_latency_seconds       408ms mean   [metric since DELETED — see note]
notif_worker_processing_seconds   418ms mean   (20895.97s / 49942)
notif_end_to_end_latency_seconds  4.30s mean   (214809.43s / 49942)
```

The provider call is 408ms of the 418ms a message spends being processed. So
97% of worker time is spent waiting on the provider, and 3% on everything this
service does. That is the correct shape. The remaining work is to keep this
service's 3% from growing.

## The instruments behind these figures were later found faulty

An audit of every metric against its increment sites ran after this campaign.
It returned six verdicts: five misleading and one wrong. Three affect the
numbers above and are marked at the point they appear.

**notif_worker_processed_total** defaulted its outcome label to "success". It
had five error returns that never reassigned it, so failures counted as
successes. Deleted, replaced by notif_message_outcome_total, which starts
empty.

**twilio_send_latency_seconds** started its clock before a token-bucket rate
limiter and a three-attempt retry loop. The 408ms is limiter queueing plus
retries plus the call, not provider latency. Deleted, replaced by
notif_provider_call_seconds wrapping only the call, with the limiter measured
separately.

**notif_db_roundtrips_total** is neither every query nor one per round-trip:

- pgx routes pool.Ping below the tracer.
- Out-of-process SQL never reaches it.
- Acquire failures produce no increment.
- A statement-cache miss is one increment covering two wire exchanges.

Renamed notif_db_query_calls_total. The 2.00 per-message ratio also used a
denominator that excludes duplicate deliveries counted in the numerator.

What survives unaffected:

- The message counts and states, which come from Postgres.
- The SQS series, which come from CloudWatch.
- The round-trip reduction itself. A dedicated counted pool in
  tests/integration/roundtrips_test.go measured it, not the production counter.

## One thing worth fixing

```
notif_db_pool_empty_acquires_total{service="worker"}  8603
notif_db_pool_connections{service="worker",state="max"}  20
```

8,603 acquisitions found the connection pool empty and waited. The pool is 20
and worker concurrency is 100, so five handlers contend for each connection.

This did not stop the run reaching the provider's ceiling. The provider call
dominates so heavily that handlers are rarely all inside the database at once.
But it is real contention, and the pool is the wrong size for the concurrency.

## What this does not show

- **A real provider.** Sends went to the mock provider, an in-cluster
  simulator, not Twilio. It models documented limits: concurrency 429s,
  per-sender MPS pacing, the ten-hour queue and its 30001 overflow.
- **Failure handling.** `MOCK_SUCCESS_RATE` was 1.0. Real traffic carries a few
  percent of failures with retries and dead-lettering, which this clean run
  deliberately excludes.

  This mattered more than "deliberately excludes" suggests. Because nothing
  failed, the "dead-letter queue 0" above does not show that failure handling
  worked. It shows that failure handling never ran. The same campaign was later
  run with failures injected. The pre-existing code discarded 8.8% of it and
  still reported an empty dead-letter queue, because a message failed
  terminally cannot reach one. See
  [retry-handling-ab-2026-08-15.md](retry-handling-ab-2026-08-15.md).
- **Multi-segment messages.** MPS is counted in segments, so a campaign of
  two-segment messages halves the effective rate.
- **Grafana.** As above.
