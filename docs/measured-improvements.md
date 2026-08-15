# Measured improvements

Every figure here came from a counter or a test that can be re-run. Where a
number is not independently measured, it says so. Claims that did not survive
checking were removed rather than softened — several were.

## 1. HTTP connection pool — controlled A/B

Same code, same 40,000-message campaign, same 500 MPS provider profile, fresh
pods on both arms so counters start at zero. One variable: the transport's
idle-connection pool.

| | default (2 idle/host) | sized (120 idle/host) | change |
|---|---|---|---|
| campaign duration | 138s | 119s | **−14%** |
| provider call latency, mean | 414 ms | 217 ms | **−48%** |
| DB pool "empty acquire" events | 8,961 | 191 | **−98%** |
| cumulative wait for a connection | 111.1s | 7.7s | **−93%** |
| messages delivered | 40,000 | 40,000 | — |
| DB round-trips | 80,000 | 80,000 | unchanged |

The last two rows are the controls: identical delivery and identical database
work prove nothing else moved.

**What is established.** Go's default transport keeps two idle connections per
host, so every provider call past the second paid a TCP and TLS handshake before
sending a byte. That accounts straightforwardly for the latency halving, and for
the shorter campaign that follows from it.

**What is not established.** Why the same change moved a DATABASE pool counter
by 98% is unexplained. An earlier version of this document asserted that slow
provider calls made handlers hold their database connections longer, which is
wrong: the worker acquires a connection for ClaimAndLoad and releases it, makes
the provider call holding nothing, then acquires again for RecordAttempt. If
anything, slower HTTP should mean handlers spend more time outside the pool and
contend for it less — the opposite of what was measured.

So the correlation is real and controlled, and the mechanism behind that one row
is not known. Quoting it as cause-and-effect would be inventing the interesting
half. Establishing it needs a rebuild with per-phase connection-acquire timing.

The same default was present in three places: the SQS client, the worker's
provider client, and the simulator's webhook client. Two carried enough traffic
to matter.

## 2. Database round-trips per message

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

## 3. SQS consumer concurrency

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

## 4. Queue choice

The send queue was SQS FIFO, which outside high-throughput mode is limited to
300 transactions per second per API action. Nothing required global ordering,
and FIFO's five-minute deduplication window is weaker than the
(tenant_id, idempotency_key) uniqueness constraint that already existed. Moving
to a standard queue removed the ceiling.

Not measured: no run was taken against the FIFO queue, so there is no
before-and-after here. The 300 figure is Twilio's documented limit, not an
observation.

## 5. Defects found by operating the system

Each has a mechanism, a fix, and a regression test. None was reachable by
reading the code.

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

## 6. Campaign result

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
