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

// TestSenderPool_NumberPoolMultipliesThroughput is the campaign lever. A
// Messaging Service load-balances across its numbers, so effective throughput
// is the pool size times the per-number rate — which is why "how fast can this
// campaign go out" is answered by provisioning before it is answered by code.
func TestSenderPool_NumberPoolMultipliesThroughput(t *testing.T) {
	single := newSenderPool(1, 1, 86400, 0)   // one US long code: 1/sec
	pooled := newSenderPool(1, 200, 86400, 0) // two hundred of them

	if single.effectiveMPS() != 1 {
		t.Errorf("one number at 1 MPS = %v, want 1", single.effectiveMPS())
	}
	if pooled.effectiveMPS() != 200 {
		t.Errorf("two hundred numbers at 1 MPS = %v, want 200", pooled.effectiveMPS())
	}

	// The same campaign drains 200x faster, which is the whole point.
	now := time.Now()
	const campaign = 1000
	var lastSingle, lastPooled time.Duration
	for i := 0; i < campaign; i++ {
		w1, ok1 := single.admit("MG1", now)
		w2, ok2 := pooled.admit("MG1", now)
		if !ok1 || !ok2 {
			t.Fatalf("message %d rejected", i)
		}
		lastSingle, lastPooled = w1, w2
	}
	if lastSingle < 999*time.Second {
		t.Errorf("last of %d on one number waits %v, want ~999s", campaign, lastSingle)
	}
	if lastPooled > 6*time.Second {
		t.Errorf("last of %d across 200 numbers waits %v, want ~5s", campaign, lastPooled)
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
