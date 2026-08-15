# Measured improvements

Every figure here came from a counter or a test that can be re-run. Where a
number is not independently measured, it says so. Claims that did not survive
checking were removed rather than softened — several were.

## 1. Retry classification — controlled A/B on AWS

Same 100,000-message campaign, same 10% failure injection (6:3:1 across HTTP
429 / 500 / 400, verified against the provider before use), same 408-second
accept duration. One variable: the worker image.

| | `sha-41c0375` | `sha-96bad7d` | change |
|---|---|---|---|
| delivered | 90,146 | 98,874 | **+8,728** |
| failed | 9,854 | 1,125 | **−8,729** |
| in dead-letter queue | 0 | 0 | — |
| attempts per message | 1 for all 100,000 | up to 6 | — |
| accepted | 100,000 | 100,000 | — |
| accept duration | 408s | 408s | — |

The last two rows are the controls: identical acceptance and identical duration
show the accept path did not move.

**What is established.** `ShouldRetry` tested `err != nil` before the HTTP
status, and `SendSMS` returns a non-nil error alongside every non-2xx response,
so the status branches were unreachable. The "attempts per message" row measures
this directly rather than inferring it: in the before arm every message was
attempted exactly once, so the three-attempt loop never ran at all.

**Why the dead-letter queue is 0 on both arms, and why that is the point.** A
message written `state='failed'` cannot be re-claimed, so its redelivery was
counted a duplicate and acknowledged. The 8,838 abandoned sends never reached
the dead-letter queue, which is why the metric that existed to catch this
reported success throughout.

**The strongest single check** is that the after arm's 1,125 failures equal the
1,125 HTTP 400s the provider returned. Every remaining failure is a permanent
rejection; no transient failure was lost.

Full method and the outage run: [campaign-100k/retry-handling-ab-2026-08-15.md](campaign-100k/retry-handling-ab-2026-08-15.md).

## 2. HTTP connection pool — controlled A/B

Same code, same 40,000-message campaign, same 500 MPS provider profile, fresh
pods on both arms so counters start at zero. One variable: the transport's
idle-connection pool.

| | default (2 idle/host) | sized (120 idle/host) | change |
|---|---|---|---|
| campaign duration | 138s | 119s | **−14%** |
| provider call latency, mean | 414 ms | 217 ms | **−48%** |
| DB pool EmptyAcquireCount | 8,961 | 191 | −98%, unexplained — see below |
| cumulative wait for a connection | 111.1s | 7.7s | **−93%** |
| messages delivered | 40,000 | 40,000 | — |
| DB round-trips | 80,000 | 80,000 | unchanged |

The last two rows are the controls: identical delivery and identical database
work prove nothing else moved.

**What is established.** Go's default transport keeps two idle connections per
host, so every provider call past the second paid a TCP and TLS handshake before
sending a byte. That accounts straightforwardly for the latency halving, and for
the shorter campaign that follows from it.

**The database row is unexplained, and should not be quoted.** pgx increments
EmptyAcquireCount in two places: once when a caller waited on the pool semaphore
before getting an idle connection, and once when it had to construct a new one.
It is therefore a mix of contention and construction, and this run cannot say
which dominated.

Three explanations were offered for it during this work and all three were
wrong. That handlers held database connections across the provider call —
contradicted by the code, which releases before the call. That connections went
idle and were reaped — impossible, since MaxConnIdleTime is ten minutes and the
run was 138 seconds. That it purely counts construction — a half-reading of the
library, which counts semaphore waits too.

The measurement is real and was taken twice under controlled conditions. The
mechanism is not known. Settling it needs the two increment sites counted
separately, which is a small change and a rebuild.

The rest of the A/B does not depend on it: the latency halving follows directly
from two idle connections per host forcing a TLS handshake on most calls, and
the shorter campaign follows from the latency.


## 3. Database round-trips per message

Measured by a pgx query tracer against real Postgres, with the previous
multi-statement sequence replayed on the same counter as the baseline.

| path | before | after |
|---|---|---|
| accept a send | 7 (9 when the daily cap was spent) | **1** |
| worker success path | 4 | **2** |
| provider callback | 2, plus an UPDATE retried up to 10 times | **1** |

Confirmed in production during a 100,000-message campaign: 200,000 worker
round-trips for 100,000 messages, and 300,000 for 300,000 callbacks — exactly
2.00 and 1.00.

## 4. SQS consumer concurrency

The consumer issued one ReceiveMessage at a time, so its ceiling was ten
messages divided by round-trip time regardless of how many handlers waited
behind it. Measured against a fake with 25ms injected receive latency:

| receivers | 400 messages drained in |
|---|---|
| 1 | ~1.04s |
| 8 | ~0.13s |

Timings vary by a few milliseconds between runs; the ratio (roughly 8x, the
receiver count) is the stable part and is what the test asserts.

Batching, same suite: 500 enqueues become 50 SendMessageBatch calls, and 200
deletions become 20 DeleteMessageBatch calls.

## 5. Queue choice

The send queue was SQS FIFO, which outside high-throughput mode is limited to
300 transactions per second per API action. Nothing required global ordering,
and FIFO's five-minute deduplication window is weaker than the
(tenant_id, idempotency_key) uniqueness constraint that already existed. Moving
to a standard queue removed the ceiling.

Not measured: no run was taken against the FIFO queue, so there is no
before-and-after here. The 300 figure is Twilio's documented limit, not an
observation.

## 6. Defects found by operating the system

Each has a mechanism, a fix, and a regression test. None was reachable by
reading the code.

**Retry classification, and the test that protected it.** Covered in §1. Worth
noting separately is how it survived: the integration suite contained a case
named "provider rejects permanently" whose fake returned HTTP 500 — a textbook
transient failure — and asserted the outcome the defect produced. It passed for
exactly that reason. The same pattern then repeated one branch over: the test
added alongside the fix asserted that a refused connection was permanent, in as
many words, and that belief also had to be corrected. Both were caught by
running the failure rather than reading the branch.

**Transport failures classified permanent.** A connection refused returns no
HTTP status, and the classifier ended the message. The reasoning — that a
refused connection will not recover inside three in-process attempts — was true
of the wrong loop: the terminal write it justified also cancels the SQS
redelivery meant for faults lasting minutes. A provider outage presents as
connection-refused, so the code discarded whatever was in flight during one.
Found while constructing an outage run, not while reading the function.

**Consumer starvation.** A 15-second HTTP client timeout killed every
20-second SQS long poll. Asymmetric and therefore invisible: SendMessage
returned in milliseconds and kept working, so the API answered 202 and looked
healthy while nothing was consumed.

**Snapshot-isolation race on accept.** ON CONFLICT DO NOTHING neither returns a
row nor waits, so a caller losing a race got nothing from the insert and
nothing from the fallback read — its snapshot predated the winner's commit. The
caller received an error for a request that had succeeded. The existing
16-goroutine test passed this 40 times in a row; a wider test (8 keys, 24
callers, 5 rounds) fails the old code 8 times out of 8.

**Shutdown abandoned in-flight work.** The worker passed its receive context to
the handler, so SIGTERM aborted in-flight provider calls and the messages were
never deleted. Separately, the delete for a handler that had already SUCCEEDED
was discarded on shutdown — the SMS went out and the recipient got a second one
after the visibility timeout. A benchmark run put 198,264 messages in the
dead-letter queue this way, every one in flight during one of six deploys.

**Provider ids collided across restarts.** Ids were a six-digit counter from
zero, so a campaign reused every id from an earlier run and could match a
callback to the wrong message. It survived only because the delivery handler
refuses to touch a message already in a terminal state — a guard written for an
unrelated reason.

**Metric labels collided with Prometheus Operator.** Labels named `endpoint`
and `service` are reserved; the Operator relabels the application's value to
`exported_*`. The counters were correct and the dashboards were empty. A test
now walks every label against the reserved set, which is how the second
collision was found.

## 7. Campaign result

100,000 messages, short-code profile at 500 MPS, on 4 vCPU of workers.

```
delivered            100,000 / 100,000
duration                        284s
dead-lettered                      0
duplicate sends                    0
messages left queued               0
```

Reconciled across three independent records: Postgres (rows and states),
CloudWatch (SQS sent, deleted, depth, age — recorded by AWS, not by this
service), and Prometheus. All three agree.

## How to re-derive every figure here

The test-measured numbers are reproducible on any machine with Postgres:

```
TEST_DB_DSN=... go test -tags=integration ./tests/integration -run RoundTrip -v
go test ./internal/queue/sqs -run 'ReceivesConcurrently|CoalescesIntoBatches|BatchesDeletes' -v
go test ./cmd/mock-provider -run SenderClass -v
```

The A/B and campaign numbers came from counters on a live cluster and are
recorded above rather than reproducible without rebuilding it. The Postgres
figures were captured before teardown; the CloudWatch series remain in AWS.

## What is not shown

- Sends went to a simulator modelling documented provider limits, not to Twilio.
- The clean run used a 100% provider success rate; real traffic carries failures.
- Message segmentation is not modelled, and provider rates are counted in
  segments.
- The 300 TPS FIFO ceiling was never measured, only cited.

## Final evidence, all campaign runs

Captured from Postgres before the environment was destroyed.

```
run     messages   started              ended
clean    100,000   2026-08-14 19:13:14  19:18:01
final    100,000   2026-08-14 20:14:35  20:19:18
prom     100,000   2026-08-14 20:41:18  20:46:01
arm       80,000   2026-08-14 21:39:25  21:46:18   (A/B, 40k per arm)
                  --------
                   380,000

final state: delivered 380,000   (no other state)

invariants           violations
sent twice                    0
sent w/o provider id          0
duplicate idem keys           0
left queued                   0
stuck processing              0
suppressed but sent           0
```

Lifetime totals on the instance: 2,358,675 messages, 2,126,179 provider
attempts, 6,384,450 delivery events.

CloudWatch retains the SQS series for fifteen months and is independent of this
service's own instrumentation; the Postgres figures above are the durable record
of message state and were captured before teardown.
