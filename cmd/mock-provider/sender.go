package main

import (
	"math"
	"math/rand"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// This file models the two limits Twilio actually enforces. They are
// independent, they fail in different ways, and a stand-in that models neither
// makes a load test measure the stand-in.
//
//  1. REST API CONCURRENCY. Twilio caps how many requests an account may have
//     in flight at once — not requests per second. Exceeding it returns HTTP
//     429 with error 20429; the request is never processed and is always safe
//     to retry. The current count is reported on every response in the
//     Twilio-Concurrent-Requests header.
//
//     This is the limit that interacts with worker concurrency. Raising a
//     consumer's concurrency raises in-flight requests, so past the account
//     limit more concurrency produces more 429s and less delivered traffic.
//     A stand-in that returns 429 at random cannot show that, and will make
//     raising concurrency look free.
//
//  2. MESSAGES PER SECOND, PER SENDER. Twilio does NOT reject a send that
//     exceeds the sender's MPS. It accepts the request, answers 201 with
//     status "queued", and drains its own queue at the sender's rate — one
//     segment per second for a US long code, ten for a UK long code, a hundred
//     or more for a short code.
//
//     The consequence is the one that matters for anything time-sensitive:
//     there is a second queue behind yours that you do not control. Draining
//     your own queue faster moves messages into Twilio's queue sooner; it does
//     not make them arrive sooner. Delivery latency is set by queue depth
//     divided by MPS.
//
//     Overfilling it fails with error 30001 (queue overflow) once more than
//     MOCK_QUEUE_MAX_SECONDS of traffic is enqueued for a sender — four hours
//     by default, which is 14,400 segments on a 1 MPS long code.
//
// Sources: twilio.com/docs/api/errors/20429, /30001, /21611,
// twilio.com/docs/messaging/guides/scaling-queueing-latency

// concurrencyGate models the REST API concurrency limit.
type concurrencyGate struct {
	max     int64
	current atomic.Int64
	// rejected counts 429s so a test can assert it exercised the limit rather
	// than assuming it did.
	rejected atomic.Int64
}

func newConcurrencyGate(max int) *concurrencyGate {
	return &concurrencyGate{max: int64(max)}
}

// enter reports whether the request may proceed, and the concurrency count to
// report back. The count includes this request and rejected ones, matching
// Twilio's documented behaviour that the header counts requests which received
// a 429.
func (g *concurrencyGate) enter() (allowed bool, count int64) {
	n := g.current.Add(1)
	if g.max > 0 && n > g.max {
		g.current.Add(-1)
		g.rejected.Add(1)
		return false, n
	}
	return true, n
}

func (g *concurrencyGate) leave() { g.current.Add(-1) }

// senderQueue models one sender's outbound queue, drained at a fixed MPS.
//
// Accepting a message returns the delay before it will actually be sent, which
// is what the delivery webhooks are then scheduled against. That delay is the
// whole point: it is how "the provider accepted it instantly" and "the
// recipient got it twenty minutes later" coexist.
type senderQueue struct {
	mps        float64
	maxDepth   int
	mu         sync.Mutex
	depth      int
	nextSlotAt time.Time

	overflowed atomic.Int64
	accepted   atomic.Int64
}

func newSenderQueue(mps float64, maxSeconds int) *senderQueue {
	depth := int(mps * float64(maxSeconds))
	if depth < 1 {
		depth = 1
	}
	return &senderQueue{mps: mps, maxDepth: depth}
}

// admit takes one message. It returns the wait before this message's turn, or
// ok=false when the queue is full — Twilio's error 30001.
func (q *senderQueue) admit(now time.Time) (wait time.Duration, ok bool) {
	if q.mps <= 0 {
		return 0, true // unpaced: the sender has no MPS limit configured
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	if q.depth >= q.maxDepth {
		q.overflowed.Add(1)
		return 0, false
	}

	interval := time.Duration(float64(time.Second) / q.mps)
	if q.nextSlotAt.Before(now) {
		q.nextSlotAt = now
	}
	wait = q.nextSlotAt.Sub(now)
	q.nextSlotAt = q.nextSlotAt.Add(interval)

	q.depth++
	q.accepted.Add(1)

	// The slot is consumed when its turn arrives, so depth falls then rather
	// than when the message was admitted.
	time.AfterFunc(wait+interval, func() {
		q.mu.Lock()
		if q.depth > 0 {
			q.depth--
		}
		q.mu.Unlock()
	})

	return wait, true
}

func (q *senderQueue) currentDepth() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.depth
}

// accountPacer is the ceiling that sits above every sender.
//
// Providers cap total account throughput as well as per-sender throughput, so
// adding senders stops helping at some point. Without this the model would
// suggest a campaign can be made arbitrarily fast by buying more numbers, which
// is the one conclusion an operator must not draw.
type accountPacer struct {
	mps        float64
	mu         sync.Mutex
	nextSlotAt time.Time
}

func newAccountPacer(mps float64) *accountPacer { return &accountPacer{mps: mps} }

// reserve returns the additional wait imposed by the account ceiling.
func (a *accountPacer) reserve(now time.Time) time.Duration {
	if a == nil || a.mps <= 0 {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	interval := time.Duration(float64(time.Second) / a.mps)
	if a.nextSlotAt.Before(now) {
		a.nextSlotAt = now
	}
	wait := a.nextSlotAt.Sub(now)
	a.nextSlotAt = a.nextSlotAt.Add(interval)
	return wait
}

// senderPool keeps one queue per sender, because MPS is per sender. Two
// Messaging Services with their own numbers drain independently — which is the
// only reason separating time-sensitive traffic from bulk actually works. Put
// both through one sender and the bulk fills the queue that the urgent traffic
// has to wait in, no matter how the sending side is tuned.
type senderPool struct {
	mps        float64 // per NUMBER, not per sender
	poolSize   int     // numbers behind each sender
	maxSeconds int
	account    *accountPacer
	mu         sync.Mutex
	queues     map[string]*senderQueue
}

// newSenderPool models a sender as a Messaging Service holding poolSize
// numbers, each running at mps.
//
// This is the lever a campaign actually pulls. A single US long code does one
// message per second, so a hundred thousand messages take over a day; the same
// hundred thousand across a pool of two hundred numbers takes about eight
// minutes. Provider documentation describes exactly this — load-balancing a
// Messaging Service across a number pool — and it is why "how fast can we send
// this campaign" is a sender-provisioning question before it is an engineering
// one.
//
// accountMPS caps the total across all senders, because buying numbers stops
// helping once the account ceiling is reached.
func newSenderPool(mps float64, poolSize, maxSeconds int, accountMPS float64) *senderPool {
	if poolSize < 1 {
		poolSize = 1
	}
	return &senderPool{
		mps:        mps,
		poolSize:   poolSize,
		maxSeconds: maxSeconds,
		account:    newAccountPacer(accountMPS),
		queues:     map[string]*senderQueue{},
	}
}

// effectiveMPS is what one sender delivers with its whole number pool working.
func (p *senderPool) effectiveMPS() float64 { return p.mps * float64(p.poolSize) }

func (p *senderPool) get(sender string) *senderQueue {
	if sender == "" {
		sender = "default"
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	q, ok := p.queues[sender]
	if !ok {
		q = newSenderQueue(p.effectiveMPS(), p.maxSeconds)
		p.queues[sender] = q
	}
	return q
}

// admit places a message on its sender's queue and applies the account
// ceiling on top, returning whichever wait is longer. A sender with spare
// capacity still waits when the account as a whole is saturated.
func (p *senderPool) admit(sender string, now time.Time) (time.Duration, bool) {
	wait, ok := p.get(sender).admit(now)
	if !ok {
		return 0, false
	}
	if acct := p.account.reserve(now); acct > wait {
		wait = acct
	}
	return wait, true
}

func (p *senderPool) snapshot() map[string]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]int, len(p.queues))
	for k, q := range p.queues {
		out[k] = q.currentDepth()
	}
	return out
}

// apiLatency draws a call duration with a right tail, because a fixed delay
// understates what a consumer has to absorb. Real API latency is roughly
// log-normal: most calls near the median, a small fraction several times
// slower, and the slow ones are what occupy a worker's concurrency slots.
//
// A stand-in that always answers in exactly the median makes a fixed
// concurrency look sufficient when in production the tail is what saturates it.
func apiLatency(rng *rand.Rand, medianMs int, tailSigma float64) time.Duration {
	if medianMs <= 0 {
		return 0
	}
	if tailSigma <= 0 {
		return time.Duration(medianMs) * time.Millisecond
	}
	// Median of a log-normal is exp(mu), so mu = ln(median).
	mu := math.Log(float64(medianMs))
	ms := math.Exp(mu + tailSigma*rng.NormFloat64())
	if ms < 1 {
		ms = 1
	}
	if max := float64(medianMs) * 20; ms > max {
		ms = max // keep a pathological draw from stalling the whole run
	}
	return time.Duration(ms) * time.Millisecond
}

// writeConcurrencyHeader reports in-flight requests the way Twilio does, so a
// client can see how close it is to the limit before it starts being rejected.
func writeConcurrencyHeader(w http.ResponseWriter, count int64) {
	w.Header().Set("Twilio-Concurrent-Requests", strconv.FormatInt(count, 10))
}
