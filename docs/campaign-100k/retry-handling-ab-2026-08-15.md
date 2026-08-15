# Failure handling under load — controlled A/B, 15 August 2026

Three 100,000-message campaigns on the same AWS stack, run to answer one
question: **what happens to a message when the provider says no?**

Every earlier campaign on this project ran with `MOCK_SUCCESS_RATE=1.0`. Nothing
failed, so nothing about failure handling was ever exercised, and "messages in
DLQ: 0" was quoted as evidence of correctness. It was not evidence of anything.

## Stack

Unchanged from `benchmark-2026-08-14.md`: k3s on 8 EC2 nodes, 2 worker pods
(`WORKER_CONCURRENCY=100`), RDS `db.m7g.xlarge` via RDS Proxy, SQS standard with
`maxReceiveCount=5` and a 60s visibility timeout. Sends go to the in-cluster
provider simulator. The load generator sends 100,000 accepts and then waits for
the message-state histogram to stop moving for a minute.

---

## Runs A and B — the same 10% failure injection, before and after

`MOCK_SUCCESS_RATE=0.90`, failures weighted 6:3:1 across HTTP 429 / 500 / 400.
Verified before use by driving 400 requests directly at the provider: 89.75% ok,
7.0% 429, 2.25% 500, 1.0% 400.

Only the worker image differs between the runs.

| | A — `sha-41c0375` | B — `sha-96bad7d` |
|---|---|---|
| accepted | 100,000 | 100,000 |
| accept duration | 408s | 408s |
| **delivered** | **90,146** | **98,874** |
| **failed** | **9,854** | **1,125** |
| in dead-letter queue | 0 | 0 |
| provider 429s answered | 5,922 | 6,439 |
| provider 500s answered | 2,916 | 3,334 |
| provider 400s answered | 1,016 | 1,125 |
| attempts per message | **1 for all 100,000** | 1→91,129 · 2→8,062 · 3→725 · 4→72 · 5→10 · 6→1 |

### What run A shows

Every message was attempted **exactly once**. The three-attempt retry loop was
present in the code and never executed, because `ShouldRetry` tested the error
before the status and `SendSMS` returns a non-nil error alongside every non-2xx
response. The status branches were unreachable.

So 8,838 messages that answered 429 or 500 — both documented by the provider as
safe to retry — were marked `failed` on first contact. A terminal message cannot
be re-claimed, so the SQS redelivery was counted as a duplicate, acknowledged,
and deleted.

**8.8% of the campaign was discarded, and the dead-letter queue stayed at zero
throughout.** No alarm could have fired. That is the part worth keeping: the
defect was invisible to the metric that existed to catch it.

### What run B shows

`failed` is **1,125**, and the provider answered **1,125** HTTP 400s. Those two
numbers matching exactly means every remaining failure is a genuine permanent
rejection and no transient failure was lost.

The attempts histogram is the mechanism made visible. The in-process loop caps
at three, so the 83 messages at 4, 5 and 6 attempts can only have got there by
being released back to the queue and re-delivered by SQS — the outer retry loop
that run A's terminal writes had disabled.

**+8,728 messages delivered, same code path, same injection, same duration.**

---

## Run C — a provider outage

10% independent per-request failures is not what a provider outage looks like.
Real failures arrive **correlated** and at the **connection** level: everything
in flight fails at once, and the client sees connection-refused rather than a
polite 429.

So run C uses a realistic steady state — `MOCK_SUCCESS_RATE=0.995`, weighted
toward bad numbers, which is what a real recipient list contains — and then
takes the provider away completely by scaling it to zero for **58 seconds**
mid-campaign (08:04:02–08:05:00 UTC).

Worker image `sha-da47d34`.

```
accepted            100,000  in 347s
delivered            97,987
submitted             1,655   (see caveat)
failed                  358
dead-letter queue         0
total               100,000   ✓

provider answers    201 → 99,642
                    400 →    358
                    429 →    102
                      0 →     70   ← connection refused, during the outage
                    500 →     56
```

`failed` is 358 and the provider answered 358 HTTP 400s. Again exact: **the
outage cost zero messages.**

### What the outage actually did

Peak state, measured mid-recovery:

```
queued      15,434   last_error = circuit_breaker_open
delivered   35,269
failed         146   last_error = twilio_non_retryable
```

15,434 messages had their claim **released back to the queue** rather than being
failed. The circuit breaker opened within seconds and shielded the provider from
roughly fifteen thousand pointless calls; only 70 requests reached it and got
connection-refused before the breaker tripped. Every released message was
re-claimed and sent once the provider returned.

Those 70 are the ones that matter for classification. Until `sha-da47d34`, a
transport failure with no HTTP status was classified permanent — the reasoning
being that a refused connection would not recover inside the three in-process
attempts. True, and irrelevant: the code path that reasoning justified marks the
message `failed`, which also cancels the SQS redelivery that exists precisely for
faults outlasting a couple of seconds. Preparing this run is what surfaced it;
the test covering that branch had asserted the wrong belief in as many words.

### Caveat: the 1,655 `submitted`

All 1,655 were submitted between 08:03:48 and 08:03:57 — a nine-second window
ending five seconds before the provider pod was killed. The provider had issued
each a SID and scheduled its delivery callback in memory; scaling to zero
destroyed that state.

This is an artifact of how the outage was injected, not a property of the
service. A real provider does not lose its callback queue when it recovers. It
is left in the table rather than excluded, because excluding it would make the
run look tidier than it was.

---

## What these runs do not show

- **A real provider.** Still the in-cluster simulator. It models documented
  limits (concurrency 429s, per-sender MPS, queue overflow) and now failure
  injection, but it is not Twilio.
- **A DLQ under stress.** Every run ended with an empty dead-letter queue,
  because no message exhausted all five redeliveries. The redrive path to the
  DLQ is covered by integration tests, not by these campaigns. An outage longer
  than `5 × visibility timeout` would be needed to exercise it here.
- **Multi-segment messages**, which halve effective throughput per segment.

## One procedural note

Run B ended with a single message accepted but never attempted — `queued`, no
error, no provider attempt, absent from both queues. The accept path cannot
produce that state on an enqueue failure: it marks `enqueue_failed` and returns
an error, and the batching producer blocks each caller until SQS acknowledges
its specific entry, propagating per-entry batch failures individually.

The likely cause is procedural. `PurgeQueue` was issued seconds before the run,
and AWS documents that the purge can take up to 60 seconds and may delete
messages sent while it is in progress. Run C waited out that window and lost
nothing, which supports the explanation without proving it. Recorded rather than
rounded away.
