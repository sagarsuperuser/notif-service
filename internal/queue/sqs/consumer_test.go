package sqsqueue

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// fakeSQS serves a fixed backlog with an injected per-call latency, and records
// the highest number of ReceiveMessage calls that were ever in flight at once.
//
// The latency is the point. Against a local endpoint a receive round-trip is
// sub-millisecond, so a single receiver looks infinitely fast and the thing
// under test — how many receives overlap — is invisible. Real SQS answers in
// tens of milliseconds, which is what makes one receiver a ceiling.
type fakeSQS struct {
	latency time.Duration

	mu        sync.Mutex
	remaining int
	next      int
	inFlight  int
	maxFlight int

	deleted   atomic.Int64
	deleteAPI atomic.Int64
}

func (f *fakeSQS) ReceiveMessage(ctx context.Context, in *sqs.ReceiveMessageInput, _ ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	f.mu.Lock()
	f.inFlight++
	if f.inFlight > f.maxFlight {
		f.maxFlight = f.inFlight
	}
	f.mu.Unlock()

	defer func() {
		f.mu.Lock()
		f.inFlight--
		f.mu.Unlock()
	}()

	select {
	case <-time.After(f.latency):
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	n := int(in.MaxNumberOfMessages)
	if n > f.remaining {
		n = f.remaining
	}
	msgs := make([]types.Message, 0, n)
	for i := 0; i < n; i++ {
		body, _ := json.Marshal(SMSJob{MessageID: fmt.Sprintf("m-%d", f.next)})
		s := string(body)
		h := strconv.Itoa(f.next)
		msgs = append(msgs, types.Message{Body: &s, ReceiptHandle: &h})
		f.next++
	}
	f.remaining -= n
	return &sqs.ReceiveMessageOutput{Messages: msgs}, nil
}

func (f *fakeSQS) DeleteMessageBatch(ctx context.Context, in *sqs.DeleteMessageBatchInput, _ ...func(*sqs.Options)) (*sqs.DeleteMessageBatchOutput, error) {
	f.deleteAPI.Add(1)
	f.deleted.Add(int64(len(in.Entries)))
	return &sqs.DeleteMessageBatchOutput{}, nil
}

func (f *fakeSQS) maxConcurrentReceives() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxFlight
}

// TestConsumer_ReceivesConcurrently is the guarantee this consumer was rewritten
// for. One receiver fetches at most MaxNumberOfMessages per round-trip, so its
// rate is 10 ÷ round-trip-time no matter how many handlers wait behind it. The
// old consumer had exactly one, which capped the worker pool at roughly 200-300
// messages a second and left handlers idle.
func TestConsumer_ReceivesConcurrently(t *testing.T) {
	run := func(receivers, workers int) (time.Duration, int, *fakeSQS) {
		f := &fakeSQS{latency: 25 * time.Millisecond, remaining: 400}
		c := &Consumer{
			SQS: f, QueueURL: "q",
			MaxMessages: 10, WaitTimeSeconds: 0, VisibilityTimeout: 30,
			Receivers: receivers, DeleteBatchDelay: time.Millisecond,
		}

		var done atomic.Int64
		finished := make(chan struct{})
		var once sync.Once
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		start := time.Now()
		go func() {
			_ = c.PollConcurrent(ctx, workers, func(ctx context.Context, job SMSJob) error {
				if done.Add(1) == 400 {
					once.Do(func() { close(finished) })
				}
				return nil
			})
		}()

		select {
		case <-finished:
		case <-ctx.Done():
			t.Fatalf("drained only %d of 400 with %d receivers", done.Load(), receivers)
		}
		elapsed := time.Since(start)
		cancel()
		return elapsed, f.maxConcurrentReceives(), f
	}

	oneDur, oneFlight, _ := run(1, 64)
	manyDur, manyFlight, _ := run(8, 64)

	t.Logf("400 messages at 25ms/receive: 1 receiver %v (max %d in flight), 8 receivers %v (max %d in flight)",
		oneDur, oneFlight, manyDur, manyFlight)

	if oneFlight != 1 {
		t.Errorf("Receivers=1 ran %d concurrent receives, want 1", oneFlight)
	}
	if manyFlight < 2 {
		t.Errorf("Receivers=8 ran only %d concurrent receive, want several overlapping", manyFlight)
	}
	// 400 messages, 10 per receive, 25ms per receive: one receiver needs ~40
	// sequential round-trips (~1s); eight need ~5 rounds (~125ms). Anything less
	// than a 2x improvement means the receivers are not actually overlapping.
	if manyDur*2 > oneDur {
		t.Errorf("8 receivers took %v against 1 receiver's %v; concurrent receiving is not working", manyDur, oneDur)
	}
}

// TestConsumer_DefaultReceiversScaleWithWorkers pins the default: one receiver
// per ten handler slots, so a pool sized for throughput is not starved by a
// setting nobody remembered to change.
func TestConsumer_DefaultReceiversScaleWithWorkers(t *testing.T) {
	c := &Consumer{}
	for _, tc := range []struct{ workers, want int }{
		{1, 1}, {8, 1}, {10, 1}, {40, 4}, {80, 8}, {200, 20},
	} {
		if got := c.receivers(tc.workers); got != tc.want {
			t.Errorf("receivers(%d workers) = %d, want %d", tc.workers, got, tc.want)
		}
	}
	explicit := &Consumer{Receivers: 3}
	if got := explicit.receivers(500); got != 3 {
		t.Errorf("an explicit Receivers setting must win: got %d, want 3", got)
	}
}

// TestConsumer_BatchesDeletes checks the other half of the API-call reduction:
// completed messages leave in batches of ten rather than one call each.
func TestConsumer_BatchesDeletes(t *testing.T) {
	f := &fakeSQS{latency: time.Millisecond, remaining: 200}
	c := &Consumer{
		SQS: f, QueueURL: "q",
		MaxMessages: 10, VisibilityTimeout: 30,
		Receivers: 2, DeleteBatchDelay: 20 * time.Millisecond,
	}

	var done atomic.Int64
	finished := make(chan struct{})
	var once sync.Once
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	go func() {
		_ = c.PollConcurrent(ctx, 16, func(ctx context.Context, job SMSJob) error {
			if done.Add(1) == 200 {
				once.Do(func() { close(finished) })
			}
			return nil
		})
	}()

	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatalf("drained only %d of 200", done.Load())
	}
	// Let the last partial batch flush.
	time.Sleep(200 * time.Millisecond)
	cancel()

	deleted := f.deleted.Load()
	calls := f.deleteAPI.Load()
	t.Logf("deleted %d messages in %d DeleteMessageBatch calls (%.1f per call)",
		deleted, calls, float64(deleted)/float64(calls))

	if deleted != 200 {
		t.Errorf("deleted %d messages, want 200 — every handled message must be removed", deleted)
	}
	if calls >= 200 {
		t.Errorf("%d delete API calls for 200 messages; deletes are not being batched", calls)
	}
}

// TestConsumer_FailedHandlerLeavesMessage is the safety half of delete
// batching: a message whose handler failed must not be swept into a batch with
// its successful neighbours.
func TestConsumer_FailedHandlerLeavesMessage(t *testing.T) {
	f := &fakeSQS{latency: time.Millisecond, remaining: 100}
	c := &Consumer{
		SQS: f, QueueURL: "q",
		MaxMessages: 10, VisibilityTimeout: 30,
		Receivers: 1, DeleteBatchDelay: 5 * time.Millisecond,
	}

	var seen atomic.Int64
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	go func() {
		_ = c.PollConcurrent(ctx, 4, func(ctx context.Context, job SMSJob) error {
			seen.Add(1)
			var i int
			fmt.Sscanf(job.MessageID, "m-%d", &i)
			if i%2 == 1 {
				return fmt.Errorf("deliberate failure")
			}
			return nil
		})
	}()

	for seen.Load() < 100 && ctx.Err() == nil {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	cancel()

	if got := f.deleted.Load(); got != 50 {
		t.Errorf("deleted %d messages, want 50 — only the successful half may be removed", got)
	}
}

// TestConsumer_DrainsInFlightOnShutdown is the guarantee a rolling deploy
// depends on.
//
// A message being handled when shutdown begins must still be deleted. If it is
// abandoned, SQS redelivers it after the visibility timeout, and a message
// unlucky enough to be in flight across five deploys reaches maxReceiveCount
// and lands in the dead-letter queue. That is not hypothetical: a benchmark run
// here put 198,264 messages in the DLQ, and every one of them was in flight
// during one of six worker restarts.
//
// The consumer's contract is that cancelling the context stops the RECEIVERS.
// Handlers already running keep going, the job channel closes behind them, and
// the pending delete batch is flushed before PollConcurrent returns.
func TestConsumer_DrainsInFlightOnShutdown(t *testing.T) {
	f := &fakeSQS{latency: time.Millisecond, remaining: 40}
	c := &Consumer{
		SQS: f, QueueURL: "q",
		MaxMessages: 10, VisibilityTimeout: 30,
		Receivers: 1, DeleteBatchDelay: 5 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())

	var started, finished atomic.Int64
	release := make(chan struct{})
	returned := make(chan struct{})

	go func() {
		_ = c.PollConcurrent(ctx, 4, func(hctx context.Context, job SMSJob) error {
			started.Add(1)
			// Hold here so shutdown is guaranteed to begin mid-handler.
			<-release
			finished.Add(1)
			return nil
		})
		close(returned)
	}()

	// Wait until handlers are genuinely in flight.
	deadline := time.Now().Add(5 * time.Second)
	for started.Load() < 4 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if started.Load() < 4 {
		t.Fatalf("only %d handlers started; the test never reached the state it checks", started.Load())
	}

	// Shutdown begins while those handlers are still running.
	cancel()
	time.Sleep(50 * time.Millisecond)
	close(release)

	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("PollConcurrent did not return after shutdown")
	}
	time.Sleep(100 * time.Millisecond)

	done := finished.Load()
	deleted := f.deleted.Load()
	t.Logf("handlers finished after shutdown: %d, messages deleted: %d", done, deleted)

	if done < 4 {
		t.Errorf("%d handlers completed, want at least the 4 that were in flight — "+
			"shutdown must not abandon work in progress", done)
	}
	if deleted < done {
		t.Errorf("%d handlers succeeded but only %d messages were deleted; the rest will be "+
			"redelivered and eventually dead-lettered", done, deleted)
	}
}

// TestConsumer_HandlerConcurrencyIsBounded pins the guarantee that carries the
// whole load story now that the per-pod rate limiter is gone.
//
// The provider limits how many requests an account may have IN FLIGHT, not how
// many it may start per second, so the worker's control has to be over in-flight
// requests. That control is this bound and nothing else: if PollConcurrent ever
// ran more handlers than it was given, the service would have no way to stay
// under a provider concurrency allowance.
//
// A rate limiter cannot substitute. Rate and concurrency are only equivalent at
// a fixed latency, and provider latency is exactly what moves during an
// incident: at 130ms a 70/s limit implies ~9 in flight, and at 6s the same limit
// implies 420.
func TestConsumer_HandlerConcurrencyIsBounded(t *testing.T) {
	const workers = 8
	f := &fakeSQS{latency: time.Millisecond, remaining: 600}
	c := &Consumer{
		SQS: f, QueueURL: "q",
		MaxMessages: 10, WaitTimeSeconds: 0, VisibilityTimeout: 30,
		Receivers: 4, DeleteBatchDelay: time.Millisecond,
	}

	var inFlight, peak, done atomic.Int64
	finished := make(chan struct{})
	var once sync.Once
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	go func() {
		_ = c.PollConcurrent(ctx, workers, func(ctx context.Context, job SMSJob) error {
			n := inFlight.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			// Hold the slot long enough that an unbounded pool would pile up
			// visibly rather than racing through one at a time.
			time.Sleep(2 * time.Millisecond)
			inFlight.Add(-1)
			if done.Add(1) == 600 {
				once.Do(func() { close(finished) })
			}
			return nil
		})
	}()

	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatalf("drained only %d of 600", done.Load())
	}

	if got := peak.Load(); got > workers {
		t.Errorf("peak in-flight handlers = %d with a bound of %d; the only control over provider concurrency "+
			"does not hold", got, workers)
	}
	// A bound nothing reaches proves nothing, so require the pool to have been
	// genuinely saturated.
	if got := peak.Load(); got < workers {
		t.Errorf("peak in-flight handlers = %d, never reached the bound of %d — the test did not exercise it", got, workers)
	}
}
