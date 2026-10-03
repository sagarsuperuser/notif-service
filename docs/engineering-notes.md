# What this project demonstrates

Written for the two questions an interviewer actually asks: *what did you do*,
and *how do you know*. Every number below has a named source and can be
re-derived. Claims that did not survive verification are listed at the end
instead of being dropped, because how they were caught is part of the story.

---

## Headline

> Built a multi-tenant SMS notification service in Go on AWS, and proved it
> under load and under a provider outage.
>
> - **Built:** HTTP API, SQS, worker pool, provider integration and
>   delivery-receipt ingestion, on self-managed k3s across EC2 with RDS
>   Postgres.
> - **Measured:** a 100,000-message campaign delivered end to end in 293
>   seconds, at roughly 340 messages/second on 4 vCPU of workers.
> - **Reconciled:** exact across three independent records, with zero
>   duplicates, zero drops and zero dead-lettered.
> - **Under failure:** the same campaign held at zero message loss through a
>   58-second total provider outage.

---

## What was built and how it was proven

Each item states the result first, then the evidence behind it.

**Recovered 8.8% of a 100,000-message campaign that was being silently
discarded.** The retry classifier tested the error before the HTTP status.
Every provider response carries both, so the status branches were dead code
and a 429 was treated as permanent. A terminally failed message cannot be
re-claimed (taken again by a worker for processing), so its queue redelivery
was acknowledged and deleted. The sends vanished while the dead-letter queue
read zero, which is why no alarm existed to catch it. Proved with two runs on
AWS that differed only in the worker image, with the same injection and the
same duration. They delivered 90,146 against 98,874, and the failures
afterwards matched the provider's permanent rejections exactly (1,125 = 1,125).

**Proved dead-letter recovery end to end** by taking the provider down for
almost nine minutes. That was long enough for 18,067 messages to exhaust all
five SQS redeliveries and land in the dead-letter queue. A transient failure
now releases its claim instead of writing a terminal state, so every one of
those rows stayed claimable. Because of that, a single redrive command returned
all 18,067 to the main queue, and they delivered in about ninety seconds: 20,000 of 20,000
accounted, zero permanently failed. Under the previous code, the same outage
would have marked them failed and left the dead-letter queue empty. The
messages would have been unrecoverable and invisible at once.

**Kept a 58-second total provider outage at zero message loss** by moving the
retry budget out of the worker and into the queue. Transient failures now
release their claim instead of writing a terminal state, so SQS redelivery is
still available. It is the only retry that spans an incident measured in
minutes rather than seconds. During the outage, the circuit breaker released
15,434 in-flight messages back to the queue and shielded the provider from
those calls entirely. All of them were re-claimed and delivered on recovery,
and the run reconciled at 100,000 of 100,000.

**Made the worker's answer to the queue mean one thing** (2 Oct 2026). The
handler's return value decides whether SQS deletes a job. That makes it an
instruction, not a report: nil means finished, and an error means redeliver.
A permanent rejection was still written `failed` and then returned as an
error, so each one was redelivered once and skipped. This was the same
disagreement between the database and the queue that made run A's loss
invisible (run A is the pre-fix worker image in the 15 August before/after
runs, [retry-handling-ab-2026-08-15.md](campaign-100k/retry-handling-ab-2026-08-15.md)).
Final outcomes now return nil. The contract is documented on `Process` and in
`docs/architecture/03`, and pinned by
`TestProcessor_PermanentFailureIsAcknowledged`.

**Cut database round-trips per message from 13 to 4** by collapsing
multi-statement sequences into single CTEs:

- the accept path (where the API accepts and queues a message) went from 7
  statements to 1;
- the worker went from 4 to 2;
- the provider callback went from 2 plus a retried UPDATE to 1.

The 13 counts the callback at its minimum of 2; with all ten UPDATE attempts
it took 11. The first two reductions are differential: the old sequence is
replayed on the same counter. The callback's old count is read from the
previous code, not replayed. Each differential test requires identical results
from both paths, so the reduction cannot come from doing less work.

**Diagnosed a production-only consumer starvation**, in which a 15-second HTTP
client timeout silently killed every 20-second SQS long poll. The failure was
asymmetric. Sends returned in milliseconds, so the API answered 202 and looked
healthy while nothing was consumed. It is not reproducible in the test suite,
which polls a local endpoint with a one-second wait.

**Found and fixed a snapshot-isolation race on the accept path.** Concurrent
requests sharing an idempotency key could return an error for a request that
had succeeded. The existing 16-goroutine test passed it 40 times
consecutively. A wider test (8 keys, 24 callers, 5 rounds) fails the old code
8 times out of 8.

**Traced 198,264 dead-lettered messages to a shutdown path.** That path
abandoned in-flight work and discarded the deletes for work that had already
completed. As a result, rolling deploys during a campaign were both
dead-lettering messages and double-sending others. Fixed with a separate drain
context and an unconditional delete, pinned by a test that holds handlers open
across shutdown.

**Removed a hard 300 TPS ceiling** by moving the send queue off SQS FIFO,
after establishing that nothing required global ordering. A database
uniqueness constraint replaced the FIFO queue's five-minute deduplication
window, and the constraint outlives it.

**Introduced the repository's first CI test gate** and grew the test suite:

- from 401 lines in a single integration file to 4,415 lines across 19 files;
- of those, 3,103 lines across 9 files are integration tests against real
  Postgres, and one of them drives SQS through LocalStack instead.

The tests use differential testing against the previous implementation. Every
new guarantee had to fail under mutation before it was trusted.

**Built an invariant harness that gates load-test results on correctness**: no
duplicate sends, no drops, no daily-cap overshoot. Every invariant is itself
tested by injecting the violation it exists to detect.

---

## The two-minute walkthrough

**Shape.** Three services (API, worker, webhook) around one state machine:
queued, processing, submitted, delivered or failed. The API's only job is to
accept and durably queue. Everything slow lives behind the queue.

**The design decision.** Statements per request, not requests per second, set
a database's CPU. At 500 requests/second, seven statements per request means
3,500 statements/second. One statement per request means 500 statements/second
on the same database. So each state transition costs one round-trip. The
daily cap increments conditionally (ON CONFLICT DO UPDATE ... WHERE count <
max), so it can never overshoot and never needs a compensating decrement.

**The limit that isn't ours.** Providers accept above a sender's rate and queue
internally. They drain at one message per second for a US long code and a
hundred for a short code. There is a second queue behind ours that we do not
control. Draining ours faster moves messages into theirs sooner, without making
them arrive sooner. For time-sensitive traffic, the answer is sender
provisioning, not worker tuning. Adding numbers does not help either, because
A2P 10DLC allocates throughput per campaign rather than per number.

**How it is proven.** Differential tests replay the previous implementation and
require identical results. Every guarantee was made to fail under mutation
before being trusted. Throughput is reported only alongside invariants checked
against the database.

---

## Evidence

| claim | source | reproducible |
|---|---|---|
| round-trips 7→1, 4→2, 2–11→1 | pgx query tracer, old sequence replayed | `go test -tags=integration ./tests/integration -run RoundTrip -v` |
| receiver concurrency ~8x | deterministic fake, injected latency | `go test ./internal/queue/sqs -run ReceivesConcurrently -v` |
| batching 10 messages per API call | test output | `go test ./internal/queue/sqs -run 'Coalesces\|BatchesDeletes' -v` |
| connection-pool A/B (no effect; original claim withdrawn) | live cluster, fresh counters both arms | recorded in docs/measured-improvements.md |
| 380,000 delivered, 6 invariants checked by direct SQL, all zero | Postgres, captured before teardown | recorded |
| campaign reconciliation | Postgres + CloudWatch + Prometheus | CloudWatch retains 15 months |

CloudWatch matters more than its share. It is AWS's own recording of the
queue, independent of this service's instrumentation, so it cannot be wrong in
the same direction as the code.

---

## Claims that did not survive

Listed because how each was caught is part of the evidence, and because anyone who probes the numbers will find them anyway.

**"5.3x throughput from worker tuning."** An artifact. The mock provider
returned rate limits at random rather than on load, so the only thing the
tuning relaxed was our own limiter. The mock provider was rebuilt to model
documented provider behaviour, and the result did not survive.

**"2,000 sends per second."** It measured accepts. Sends were about 142/second,
set by a per-pod rate limit. The document now separates the two.

**"OTP delivered in 10 seconds behind a bulk run."** A phone-number format
error in the harness suppressed the entire bulk run, so there was no queue in
front of the OTP.

**"More numbers multiply campaign throughput."** The opposite is true. It is
snowshoeing (spreading sends across many numbers), which providers discourage,
and 10DLC allocates per campaign.

**"Connection pool sizing cut provider latency 48% and the campaign 14%."**
Withdrawn in full after re-measuring. The latency came from a histogram whose
clock started before a rate limiter and a retry loop. So it never bounded the
call it was quoted as measuring. A re-run against an instrument that wraps only
the provider call showed no effect: 218.0 ms at 2 idle connections against
218.4 ms at 120. The stated mechanism was wrong too. It blamed TLS handshakes
on a path that is plain HTTP to an in-cluster service, so there was no
handshake to save. Connection reuse does matter against a real provider over
TLS, but this environment cannot show it.

**"100,000 delivered in 284 seconds at roughly 500 messages/second."** Two
errors. The run's own output says 293 seconds, and 100,000 over 293 seconds is
341/second. The 500 was the provider's configured ceiling, not the rate
achieved against it.

**Three explanations for one database counter.** A pool metric moved 98% in the
same A/B, and each account of why was wrong:

- that handlers held connections across the provider call (the code releases
  first);
- that connections were reaped for idleness (the timeout is ten minutes, the
  run was 138 seconds);
- that the counter purely measures construction (it counts semaphore waits
  too).

The measurement is real, but the mechanism is unknown, so the figure is not
quoted.

Each was caught by verification that kept running after the claim was made.
CI found the race in code that already had tests. An adversarial review pass
disproved the throughput headline. Reading a library's source disproved the
last one.

## Correctness review, 2 October 2026

This section records what a full review of the four deployed services (API,
worker, webhook, mock provider), the schema and the infrastructure found: what
was fixed, and the gaps left open. Every finding was traced in code before it
was acted on. Fixed (see CHANGELOG):

- **Queue answer vs database state.** Permanent rejections are now
  acknowledged. A duplicate delivery of a message another worker holds is
  *not*. It used to be deleted, which could strand a row the holder later
  released for retry.
- **Claim token on every write.** The claim token proves which worker
  currently holds a message. The worker's final write had no guard, so a
  worker whose claim went stale could overwrite the new holder's result or a
  `delivered` from the webhook.
- **Sent-but-unrecorded.** A failed write after a successful send led to a
  second SMS on redelivery. The write is now retried on its own deadline.
- **Breaker and classification.** 400s no longer open the circuit breaker.
  401/403/404 are configuration errors that reach the dead-letter queue (DLQ)
  instead of failing a campaign.
- **Status callbacks.** The worker never sent `StatusCallback`. On a real
  account without a console-configured callback, nothing would report
  delivery.
- **API.**
  - Shutdown now drains before closing the database.
  - An enqueue failure no longer poisons the idempotency key.
  - A reused key with a different payload is 409.
  - Request size is bounded.
  - Errors no longer leak SQL/SQS text.
- **Infrastructure.**
  - Terraform had reverted the queues to FIFO. The next apply would have
    destroyed them and broken sending.
  - DLQ retention now outlasts the main queue's.
  - Visibility timeout is sized to the worker's real receive-to-finish time.
  - Liveness no longer depends on Postgres/SQS.
  - IMDSv2.
- **Concurrent duplicates spent two cap units** (fixed 3 Oct, after it failed
  CI on a slower runner). A duplicate arriving while the original was in
  flight ran on a snapshot that missed it. It then incremented the cap row the
  original had updated. A per-(tenant, key) advisory lock now precedes the
  accept statement in the same pipelined batch, so it is still one round-trip.
  `TestCreateMessage_InFlightDuplicateSpendsNoCap` forces the interleaving
  deterministically; it failed 3/3 before the fix.
- **Tag-bump PRs could never merge.** GitHub creates their CI run. But since
  `github-actions[bot]` opened the PR, GitHub holds the run for a maintainer's
  approval. Nobody approved, the held runs expired, the required check never
  passed, and 17 accumulated. The publish workflow now approves the held run
  and closes superseded bumps; merging remains a human decision.
  - Correction: the review first concluded that `GITHUB_TOKEN`-opened PRs
    start no CI at all, and a `workflow_dispatch` step was added on that
    basis. It ran, but branch protection counts only the PR's own run. This
    was corrected once the expired runs were found.
- **Hygiene.** 29 reachable vulnerabilities patched (stdlib, pgx, x/text);
  gofmt, staticcheck and govulncheck gates in CI.

### Known gaps, deliberately not fixed in this pass

Each needs a design decision or a deploy, not a patch:

- **No sweeper for stranded `queued` rows.** Two kinds of row wait for a
  client retry or an operator: one committed by an API that died before
  enqueueing, and one dead-lettered and never redriven. The invariant checker
  reports both. The fix is a job that re-enqueues stale `queued` rows (an
  outbox relay over the row that is already the source of truth). It has to
  coexist with DLQ semantics, because re-enqueueing dead letters
  automatically during an outage would cycle them forever. So it is a design,
  not a patch.
- **Ambiguous timeouts can double-send.** A 6 s request timeout that actually
  reached the provider is retried, in-process or by redelivery. Twilio's
  Messages API has no idempotency key. The mitigation is a recorded send
  intent plus a provider-side lookup before resending.
- **Producer head-of-line blocking.** One goroutine sends every SQS batch, so
  a slow call stalls every accept on the pod.
- **Reconcile scans all terminal events.** Bounded by the index, but it grows
  with history; needs a retention policy on `delivery_events`.
- **Webhook write failure.** A callback whose insert fails is answered 503.
  Whether Twilio retries depends on connection overrides (`#rc=`) on the
  callback URL, which are not configured.
- **Deploy-side, needs the owner:**
  - prod images are pinned to `sha-2d5b9b8`, older than the message-loss
    fixes;
  - KEDA uses static AWS keys;
  - RDS has no deletion protection or final snapshot;
  - `admin_cidr` is a /8;
  - ingress rate-limits all clients as one bucket under
    `externalTrafficPolicy: Cluster`;
  - the migration job re-runs the seed and resets test consents;
  - `PUBLIC_WEBHOOK_URL` is the in-cluster address, correct only while prod
    sends to the mock provider.
