package sqsqueue

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

type fakeSender struct {
	calls    atomic.Int64
	messages atomic.Int64
	latency  time.Duration

	// failEntry, when set, marks that batch index as failed in every batch.
	failEntry int
	failAll   error

	mu     sync.Mutex
	bodies []string
}

func (f *fakeSender) SendMessageBatch(ctx context.Context, in *sqs.SendMessageBatchInput, _ ...func(*sqs.Options)) (*sqs.SendMessageBatchOutput, error) {
	f.calls.Add(1)
	if f.latency > 0 {
		time.Sleep(f.latency)
	}
	if f.failAll != nil {
		return nil, f.failAll
	}
	f.messages.Add(int64(len(in.Entries)))

	out := &sqs.SendMessageBatchOutput{}
	f.mu.Lock()
	for i, e := range in.Entries {
		if f.failEntry > 0 && i == f.failEntry {
			id := *e.Id
			msg := "deliberate entry failure"
			out.Failed = append(out.Failed, types.BatchResultErrorEntry{Id: &id, Message: &msg})
			continue
		}
		f.bodies = append(f.bodies, *e.MessageBody)
		out.Successful = append(out.Successful, types.SendMessageBatchResultEntry{Id: e.Id})
	}
	f.mu.Unlock()
	return out, nil
}

// TestProducer_CoalescesIntoBatches is the API-call reduction. Concurrent
// enqueues should leave in batches of ten rather than one call each — the same
// number of messages for a tenth of the SQS transactions.
func TestProducer_CoalescesIntoBatches(t *testing.T) {
	f := &fakeSender{latency: 5 * time.Millisecond}
	p := &Producer{SQS: f, QueueURL: "q", MaxBatch: 10, MaxDelay: 10 * time.Millisecond}
	defer p.Close()

	const n = 500
	var wg sync.WaitGroup
	var failures atomic.Int64
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := p.EnqueueSMS(context.Background(), "t", fmt.Sprintf("m-%d", i),
				fmt.Sprintf("idem-%d", i), "+15550000000", "tpl", nil, ""); err != nil {
				failures.Add(1)
			}
		}(i)
	}
	wg.Wait()

	calls := f.calls.Load()
	msgs := f.messages.Load()
	t.Logf("%d enqueues sent as %d SendMessageBatch calls (%.1f messages per call)",
		msgs, calls, float64(msgs)/float64(calls))

	if failures.Load() != 0 {
		t.Errorf("%d enqueues failed", failures.Load())
	}
	if msgs != n {
		t.Errorf("%d messages reached SQS, want %d", msgs, n)
	}
	if calls >= n {
		t.Errorf("%d API calls for %d messages; enqueues are not being coalesced", calls, n)
	}
	if avg := float64(msgs) / float64(calls); avg < 5 {
		t.Errorf("only %.1f messages per call; batching is not filling batches under concurrent load", avg)
	}
}

// TestProducer_CallerLearnsItsOwnResult is what makes batching safe to put on
// the accept path. SQS reports success per entry, so one rejected message in a
// batch must fail exactly one caller — not all ten, and not none.
func TestProducer_CallerLearnsItsOwnResult(t *testing.T) {
	f := &fakeSender{failEntry: 3}
	p := &Producer{SQS: f, QueueURL: "q", MaxBatch: 10, MaxDelay: 50 * time.Millisecond}
	defer p.Close()

	var wg sync.WaitGroup
	results := make([]error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = p.EnqueueSMS(context.Background(), "t", fmt.Sprintf("m-%d", i),
				fmt.Sprintf("idem-%d", i), "+15550000000", "tpl", nil, "")
		}(i)
	}
	wg.Wait()

	failed := 0
	for _, err := range results {
		if err != nil {
			failed++
			var entryErr *BatchEntryError
			if !errors.As(err, &entryErr) {
				t.Errorf("expected a BatchEntryError, got %T: %v", err, err)
			}
		}
	}
	if failed != 1 {
		t.Errorf("%d of 10 callers saw an error, want exactly 1 — a per-entry failure must not be reported to the whole batch", failed)
	}
}

// TestProducer_TransportFailureFailsEveryCaller is the other direction: when
// the call itself fails, nobody may be told their message was queued. A false
// 202 is a message the customer believes was sent and that no worker will ever
// see.
func TestProducer_TransportFailureFailsEveryCaller(t *testing.T) {
	f := &fakeSender{failAll: errors.New("network is down")}
	p := &Producer{SQS: f, QueueURL: "q", MaxBatch: 10, MaxDelay: 10 * time.Millisecond}
	defer p.Close()

	var wg sync.WaitGroup
	var silentSuccess atomic.Int64
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := p.EnqueueSMS(context.Background(), "t", fmt.Sprintf("m-%d", i),
				fmt.Sprintf("idem-%d", i), "+15550000000", "tpl", nil, ""); err == nil {
				silentSuccess.Add(1)
			}
		}(i)
	}
	wg.Wait()

	if n := silentSuccess.Load(); n != 0 {
		t.Errorf("%d callers were told their message was queued while every send failed", n)
	}
}

// TestProducer_SingleEnqueueDoesNotWaitForever checks the linger is a ceiling,
// not a requirement: one enqueue with no company must still go out promptly.
func TestProducer_SingleEnqueueDoesNotWaitForever(t *testing.T) {
	f := &fakeSender{}
	p := &Producer{SQS: f, QueueURL: "q", MaxBatch: 10, MaxDelay: 20 * time.Millisecond}
	defer p.Close()

	start := time.Now()
	if err := p.EnqueueSMS(context.Background(), "t", "m-solo", "idem-solo",
		"+15550000000", "tpl", nil, ""); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Errorf("a lone enqueue took %v; the linger must bound the wait", elapsed)
	}
	if f.messages.Load() != 1 {
		t.Errorf("%d messages sent, want 1", f.messages.Load())
	}
}

// TestProducer_ContextCancellationReleasesCaller makes sure a caller whose
// request is cancelled does not stay parked in the batcher.
func TestProducer_ContextCancellationReleasesCaller(t *testing.T) {
	f := &fakeSender{latency: 2 * time.Second}
	p := &Producer{SQS: f, QueueURL: "q", MaxBatch: 10, MaxDelay: time.Second}
	defer p.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- p.EnqueueSMS(ctx, "t", "m-cancel", "idem-cancel", "+15550000000", "tpl", nil, "")
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Error("a cancelled enqueue reported success")
		}
	case <-time.After(3 * time.Second):
		t.Error("a cancelled enqueue never returned; the caller is stuck in the batcher")
	}
}
