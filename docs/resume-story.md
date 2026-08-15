# What this project demonstrates

Written for the two questions an interviewer actually asks: *what did you do*,
and *how do you know*. Every number below has a named source and can be
re-derived. Claims that did not survive verification are listed at the end
rather than quietly dropped, because how they were caught is part of the story.

---

## Headline

> Built a multi-tenant SMS notification service in Go on AWS — HTTP API,
> SQS, worker pool, provider integration, delivery-receipt ingestion — on
> self-managed k3s across EC2 with RDS Postgres. Delivered a 100,000-message
> campaign end to end in 284 seconds at roughly 500 messages/second on 4 vCPU
> of workers, reconciled exactly across three independent records, with zero
> duplicates, zero drops and zero dead-lettered.

---

## Resume bullets

Pick three or four. Each is defensible under follow-up.

**Cut database round-trips per message from 13 to 4** by collapsing
multi-statement sequences into single CTEs — the accept path from 7 statements
to 1, the worker from 4 to 2, the provider callback from as many as 11 to 1.
Verified by differential tests that replay the original sequence and require
identical results, with both paths measured on the same query counter.

**Diagnosed a production-only consumer starvation** in which a 15-second HTTP
client timeout silently killed every 20-second SQS long poll. The failure was
asymmetric — sends returned in milliseconds, so the API answered 202 and looked
healthy while nothing was consumed. Not reproducible in the test suite, which
polls a local endpoint with a one-second wait.

**Found and fixed a snapshot-isolation race on the accept path** where
concurrent requests sharing an idempotency key could return an error for a
request that had succeeded. The existing 16-goroutine test passed it 40 times
consecutively; a wider test (8 keys, 24 callers, 5 rounds) fails the old code 8
times out of 8.

**Traced 198,264 dead-lettered messages to a shutdown path** that abandoned
in-flight work and discarded the deletes for work that had already completed —
so rolling deploys during a campaign were both dead-lettering messages and
double-sending others. Fixed with a separate drain context and an unconditional
delete, pinned by a test that holds handlers open across shutdown.

**Sized HTTP transport connection pools** after finding three services on Go's
two-idle-connections-per-host default. Controlled A/B with identical load and
fresh counters: provider call latency down 48% (414ms to 217ms), database
connections constructed down 98% (8,961 to 191), campaign completion down 14%.

**Removed a hard 300 TPS ceiling** by moving the send queue off SQS FIFO after
establishing that nothing required global ordering, replacing its five-minute
deduplication window with a database uniqueness constraint that outlives it.

**Introduced the repository's first CI test gate** and grew integration
coverage from 405 lines to roughly 3,300, using differential testing against
the previous implementation and requiring every new guarantee to fail under
mutation before trusting it.

**Built an invariant harness that gates load-test results on correctness** — no
duplicate sends, no drops, no daily-cap overshoot — with every invariant itself
tested by injecting the violation it exists to detect.

---

## The two-minute walkthrough

**Shape.** Three services around one state machine: queued, processing,
submitted, delivered or failed. The API's only job is to accept and durably
queue; everything slow lives behind the queue.

**The design decision.** Statements per request, not requests per second, is
what sets a database's CPU. Seven statements at 500 requests/second is 3,500
statements/second against the same database that one statement would ask 500 of.
So each state transition costs one round-trip. The daily cap increments
conditionally — ON CONFLICT DO UPDATE ... WHERE count < max — so it can never
overshoot and never needs a compensating decrement.

**The limit that isn't ours.** Providers accept above a sender's rate and queue
internally, draining at one message per second for a US long code and a hundred
for a short code. There is a second queue behind ours that we do not control, so
draining ours faster moves messages into theirs sooner without making them
arrive sooner. For time-sensitive traffic the answer is sender provisioning, not
worker tuning — and adding numbers does not help, because A2P 10DLC allocates
throughput per campaign rather than per number.

**How it is proven.** Differential tests replay the previous implementation and
require identical results. Every guarantee was made to fail under mutation
before being trusted. Throughput is reported only alongside invariants checked
against the database.

---

## Evidence

| claim | source | reproducible |
|---|---|---|
| round-trips 7→1, 4→2, 11→1 | pgx query tracer, old sequence replayed | `go test -tags=integration ./tests/integration -run RoundTrip -v` |
| receiver concurrency ~8x | deterministic fake, injected latency | `go test ./internal/queue/sqs -run ReceivesConcurrently -v` |
| batching 10 messages per API call | test output | `go test ./internal/queue/sqs -run 'Coalesces\|BatchesDeletes' -v` |
| connection-pool A/B | live cluster, fresh counters both arms | recorded in docs/measured-improvements.md |
| 380,000 delivered, 6/6 invariants zero | Postgres, captured before teardown | recorded |
| campaign reconciliation | Postgres + CloudWatch + Prometheus | CloudWatch retains 15 months |

CloudWatch matters disproportionately: it is AWS's own recording of the queue,
independent of this service's instrumentation, so it cannot be wrong in the same
direction as the code.

---

## Claims that did not survive

Listed because the discipline is the point, and because an interviewer who
probes will find them anyway.

**"5.3x throughput from worker tuning."** An artifact. The provider simulator
returned rate limits at random rather than on load, so the only thing the tuning
relaxed was our own limiter. Rebuilt the simulator to model documented provider
behaviour, and the result did not survive.

**"2,000 sends per second."** It measured accepts. Sends were about 142/second,
set by a per-pod rate limit. The document now separates the two.

**"OTP delivered in 10 seconds behind a bulk run."** The bulk was entirely
suppressed by a phone-number format error in the harness, so there was no queue
in front of it.

**"More numbers multiply campaign throughput."** The opposite: it is
snowshoeing, which providers discourage, and 10DLC allocates per campaign.

**"Slow provider calls held database connections longer."** Contradicted by the
code — the connection is released before the provider call. The metric was
measuring connection construction, not contention, which reading pgx's source
settled without another experiment.

Each was caught by verification that kept running after the claim was made: CI
found the race in code that already had tests, an adversarial review pass killed
the throughput headline, and reading a library's source killed the last one.
