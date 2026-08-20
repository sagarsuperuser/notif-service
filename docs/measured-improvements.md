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

The two deltas differ by one because the after arm's own rows do: 98,874 + 1,125
= 99,999. One accepted message was never attempted and appears in neither queue.
`PurgeQueue` was issued seconds before that run, and AWS documents that a purge
can take up to 60 seconds and may delete messages sent while it is in progress.
Recorded rather than rounded away; the full note is at the end of the linked
timeline.

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

## 2. HTTP connection pool — re-measured, and the original claim withdrawn

This section previously reported a 48% fall in provider call latency and a 14%
shorter campaign from raising the transport's idle-connection pool from Go's
default of 2 to 120. **Neither reproduces.** Re-run on 15 August 2026 with the
same 40,000-message campaign, fresh pods per arm, one variable:

| | 2 idle/host | 120 idle/host | change |
|---|---|---|---|
| provider call, mean | 218.0 ms | 218.4 ms | **+0.2% — none** |
| campaign duration | 143s | 138s | −3.5%, within noise |
| messages | 40,000 | 40,000 | control |

Mean is `notif_provider_call_seconds_sum / _count` summed across both worker
pods: 8721.82s / 40,000 against 8735.82s / 40,000.

**Why the original number was wrong.** It came from
`twilio_send_latency_seconds`, which started its clock before a token-bucket
rate limiter and a three-attempt retry loop. It measured limiter queueing plus
retries plus the call, and pool size changes throughput, which changes queueing.
That metric was later deleted as misleading; this section kept quoting it. The
replacement, `notif_provider_call_seconds`, wraps the provider call and nothing
else, and the limiter has since been removed entirely.

**Why the stated mechanism was also wrong.** The old text explained the gain as
"every provider call past the second paid a TCP and TLS handshake". There is no
TLS on this path: `TWILIO_BASE_URL` is `http://notif-mock-provider-svc`, plain
HTTP to an in-cluster service one hop away. There was no handshake cost for a
larger pool to save, which is exactly what re-measuring shows.

**What is still true.** Connection reuse matters when a handshake and a real
round-trip are involved — against a provider over TLS on the public internet,
where a fresh connection costs two round-trips before the first byte. That case
is real and is why `PROVIDER_MAX_IDLE_CONNS` remains configurable. This
environment cannot demonstrate it, and the number that claimed to was measuring
something else.

The `EmptyAcquireCount` figures previously quoted here (8,961 → 191) came from
the same run and the same conditions and are withdrawn with it. That counter
also mixes semaphore waits with connection construction, so it could not have
settled the question either way.

## 3. Database round-trips per message

Measured by a pgx query tracer against real Postgres. For the accept and worker
paths the previous multi-statement sequence is replayed on the same counter, so
both columns are measured; the callback row's "before" is counted from the
previous implementation rather than replayed, and is marked accordingly.

| path | before | after | before measured? |
|---|---|---|---|
| accept a send | 7 (9 when the daily cap was spent) | **1** | yes — replayed |
| worker success path | 4 | **2** | yes — replayed |
| provider callback | 2, plus an UPDATE retried up to 10 times | **1** | no — read from the old code |

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

## 7. Dead-letter recovery — measured

20,000 messages, provider removed for 8m51s, which is longer than
`maxReceiveCount (5) × visibility timeout (60s)` and therefore long enough to
exhaust the queue's own retries.

```
during the outage, with everything dead-lettered
  messages in dead-letter queue      18,067
  message rows state='failed'             0
  message rows state='queued'        18,067   last_error=circuit_breaker_open

after one `sqs start-message-move-task` back to the main queue
  delivered                          18,666
  submitted                           1,334   provider-restart artifact, see timeline
  failed                                  0
  dead-letter queue                       0
  total accounted             20,000 / 20,000
```

The two rows that matter are `failed = 0` alongside `dead-letter = 18,067`. The
dead-letter queue holds a redrivable copy only because the message row stayed
claimable; a terminal row would have made the redrive a no-op. Recovery took
about ninety seconds from issuing the command.

Full timeline: [campaign-100k/retry-handling-ab-2026-08-15.md](campaign-100k/retry-handling-ab-2026-08-15.md).

## 8. Campaign result

100,000 messages, short-code profile at 500 MPS, on 4 vCPU of workers.

```
delivered            100,000 / 100,000
duration                        293s
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
run     messages   started (UTC)        ended (UTC)
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
