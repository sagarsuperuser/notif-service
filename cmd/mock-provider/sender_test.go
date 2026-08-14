package main

import (
	"math/rand"
	"testing"
	"time"
)

// TestConcurrencyGate_RejectsAboveLimit pins the behaviour the real API has:
// the limit is on requests IN FLIGHT, not on requests per second. This is the
// distinction that makes consumer concurrency tuning a real tradeoff — past
// the account limit, more concurrency produces more 429s and less delivered
// traffic, not more throughput.
func TestConcurrencyGate_RejectsAboveLimit(t *testing.T) {
	g := newConcurrencyGate(3)

	for i := 1; i <= 3; i++ {
		ok, n := g.enter()
		if !ok {
			t.Fatalf("request %d rejected below the limit", i)
		}
		if n != int64(i) {
			t.Errorf("in-flight reported %d, want %d", n, i)
		}
	}

	ok, n := g.enter()
	if ok {
		t.Error("a fourth concurrent request was admitted with a limit of 3")
	}
	// Twilio documents that the header counts requests which received a 429.
	if n != 4 {
		t.Errorf("rejected request reported %d in flight, want 4 — the count includes rejections", n)
	}

	// Releasing one makes room again: the limit is concurrency, so it recovers
	// as soon as a request finishes rather than after a time window.
	g.leave()
	if ok, _ := g.enter(); !ok {
		t.Error("a slot did not free up after a request completed")
	}
}

// TestSenderQueue_PacesAtMPS is the limit that decides delivery latency. The
// provider accepts everything and drains at the sender's rate, so the wait a
// message inherits is its position in that queue.
func TestSenderQueue_PacesAtMPS(t *testing.T) {
	q := newSenderQueue(10, 3600) // 10 messages per second
	now := time.Now()

	// The first message goes immediately; each subsequent one waits another
	// 1/MPS.
	for i := 0; i < 5; i++ {
		wait, ok := q.admit(now)
		if !ok {
			t.Fatalf("message %d rejected well below the queue limit", i)
		}
		want := time.Duration(i) * 100 * time.Millisecond
		if wait != want {
			t.Errorf("message %d waits %v, want %v", i, wait, want)
		}
	}
}

// TestSenderQueue_OverflowsLikeTwilio covers error 30001: a sender may hold
// only so many hours of traffic, and beyond that the API rejects rather than
// queueing forever.
func TestSenderQueue_OverflowsLikeTwilio(t *testing.T) {
	// 1 MPS, 10 seconds of queue: the tenth message is the last that fits.
	q := newSenderQueue(1, 10)
	now := time.Now()

	for i := 0; i < 10; i++ {
		if _, ok := q.admit(now); !ok {
			t.Fatalf("message %d rejected before the queue was full", i)
		}
	}
	if _, ok := q.admit(now); ok {
		t.Error("the queue accepted more than its configured depth; error 30001 would never fire")
	}
	if q.overflowed.Load() != 1 {
		t.Errorf("overflow counter = %d, want 1", q.overflowed.Load())
	}
}

// TestSenderPool_QueuesAreIndependent is the property that makes separating
// urgent traffic from bulk actually work — and, read the other way, the reason
// sharing a sender cannot work.
//
// Two senders drain independently, so a bulk run that fills one leaves the
// other untouched. Put both through one sender and the urgent message inherits
// the bulk queue's depth no matter how the sending side is tuned.
func TestSenderPool_QueuesAreIndependent(t *testing.T) {
	pool := newSenderPool(10, 1, 3600, 0)
	now := time.Now()

	// Fill one sender with a hundred messages.
	bulk := pool.get("MGbulk")
	var lastBulkWait time.Duration
	for i := 0; i < 100; i++ {
		w, ok := bulk.admit(now)
		if !ok {
			t.Fatalf("bulk message %d rejected", i)
		}
		lastBulkWait = w
	}
	if lastBulkWait < 9*time.Second {
		t.Fatalf("the hundredth message at 10 MPS waits %v; expected ~9.9s", lastBulkWait)
	}

	// A different sender is unaffected.
	urgent, ok := pool.get("MGurgent").admit(now)
	if !ok {
		t.Fatal("the urgent sender rejected its first message")
	}
	if urgent != 0 {
		t.Errorf("first message on an idle sender waits %v, want 0 — sender queues must be independent", urgent)
	}

	// The same sender as the bulk run inherits its depth. This is the failure
	// mode: identical message, different sender, ~10 seconds of difference.
	shared, ok := bulk.admit(now)
	if !ok {
		t.Fatal("shared sender rejected")
	}
	if shared < 9*time.Second {
		t.Errorf("a message behind the bulk run waits %v; it should inherit the queue depth", shared)
	}
}

// TestAPILatency_HasATail checks the draw is a distribution and not a
// constant. The tail is what occupies a caller's concurrency slots, so a
// stand-in that always answers at the median makes a fixed concurrency look
// sufficient when it is not.
func TestAPILatency_HasATail(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	const median = 120

	var min, max time.Duration = time.Hour, 0
	over := 0
	for i := 0; i < 2000; i++ {
		d := apiLatency(rng, median, 0.5)
		if d < min {
			min = d
		}
		if d > max {
			max = d
		}
		if d > 2*median*time.Millisecond {
			over++
		}
	}

	if max <= 2*median*time.Millisecond {
		t.Errorf("slowest of 2000 draws was %v; a log-normal with sigma 0.5 should produce a visible tail", max)
	}
	if over == 0 {
		t.Error("no draw exceeded twice the median; the distribution is effectively constant")
	}
	if min <= 0 {
		t.Errorf("a draw came out at %v; latency must stay positive", min)
	}

	// Sigma 0 is the documented escape hatch for a deterministic run.
	if d := apiLatency(rng, median, 0); d != median*time.Millisecond {
		t.Errorf("sigma 0 produced %v, want exactly the median", d)
	}
}

// TestSenderPool_SenderClassIsTheRealLever encodes what actually makes a
// campaign faster.
//
// The intuitive lever — more numbers — is "snowshoeing", which Twilio
// discourages and which does not multiply US throughput at all, because A2P
// 10DLC allocates the rate per campaign rather than per number. The lever that
// works is a better sender class: toll-free at 3 MPS upgradable to 25+, or a
// short code at 100.
//
// The pool multiplier still exists for the cases where it is legitimate
// (non-US long codes, genuinely separate campaigns), but it is not the default
// and it is not the answer for US traffic.
func TestSenderPool_SenderClassIsTheRealLever(t *testing.T) {
	longCode := newSenderPool(1, 1, 86400, 0)    // US 10DLC: 1 MPS
	tollFree := newSenderPool(25, 1, 86400, 0)   // upgraded toll-free: 25 MPS
	shortCode := newSenderPool(100, 1, 86400, 0) // short code: 100 MPS

	if longCode.effectiveMPS() != 1 || tollFree.effectiveMPS() != 25 || shortCode.effectiveMPS() != 100 {
		t.Fatalf("sender classes wrong: %v %v %v",
			longCode.effectiveMPS(), tollFree.effectiveMPS(), shortCode.effectiveMPS())
	}

	// The same campaign, three sender classes. This is the comparison that
	// should drive the decision.
	now := time.Now()
	drain := func(p *senderPool, n int) time.Duration {
		var last time.Duration
		for i := 0; i < n; i++ {
			w, ok := p.admit("MG1", now)
			if !ok {
				t.Fatalf("rejected at %d", i)
			}
			last = w
		}
		return last
	}
	const campaign = 1000
	lc, tf, sc := drain(longCode, campaign), drain(tollFree, campaign), drain(shortCode, campaign)
	t.Logf("%d messages: long code %v, toll-free(25) %v, short code(100) %v", campaign, lc, tf, sc)

	if lc < 999*time.Second {
		t.Errorf("1000 on a 1 MPS long code waits %v, want ~999s", lc)
	}
	if tf > 41*time.Second || tf < 39*time.Second {
		t.Errorf("1000 at 25 MPS waits %v, want ~40s", tf)
	}
	if sc > 11*time.Second || sc < 9*time.Second {
		t.Errorf("1000 at 100 MPS waits %v, want ~10s", sc)
	}
}

// TestSenderPool_PoolMultipliesOnlyWhereLegitimate keeps the multiplier honest:
// it exists, it is off by default, and it is documented as inapplicable to US
// long codes.
func TestSenderPool_PoolMultipliesOnlyWhereLegitimate(t *testing.T) {
	single := newSenderPool(10, 1, 86400, 0)  // one non-US long code: 10 MPS
	pooled := newSenderPool(10, 20, 86400, 0) // twenty of them

	if single.effectiveMPS() != 10 {
		t.Errorf("one number at 10 MPS = %v, want 10", single.effectiveMPS())
	}
	if pooled.effectiveMPS() != 200 {
		t.Errorf("twenty numbers at 10 MPS = %v, want 200", pooled.effectiveMPS())
	}
	// Default is one number: the multiplier must be opted into, not inherited.
	if def := newSenderPool(10, 0, 86400, 0); def.poolSize != 1 {
		t.Errorf("default pool size is %d, want 1 — snowshoeing must not be the default", def.poolSize)
	}
}

// TestSenderPool_AccountCeilingCapsTheLever is the limit on that lever. Buying
// numbers stops helping once the account ceiling binds, and an operator who
// does not model this concludes a campaign can be made arbitrarily fast.
func TestSenderPool_AccountCeilingCapsTheLever(t *testing.T) {
	// 200 numbers would give 200/sec, but the account is capped at 50/sec.
	capped := newSenderPool(1, 200, 86400, 50)
	now := time.Now()

	var last time.Duration
	const n = 500
	for i := 0; i < n; i++ {
		w, ok := capped.admit("MG1", now)
		if !ok {
			t.Fatalf("message %d rejected", i)
		}
		last = w
	}
	// At the account ceiling of 50/sec, 500 messages take ~10s, not the ~2.5s
	// the number pool alone would suggest.
	if last < 9*time.Second {
		t.Errorf("last of %d waits %v; the account ceiling of 50/sec should dominate the 200-number pool", n, last)
	}
}
