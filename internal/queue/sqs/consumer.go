package sqsqueue

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// ConsumerAPI is the slice of the SQS client the consumer uses.
type ConsumerAPI interface {
	ReceiveMessage(ctx context.Context, in *sqs.ReceiveMessageInput, optFns ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessageBatch(ctx context.Context, in *sqs.DeleteMessageBatchInput, optFns ...func(*sqs.Options)) (*sqs.DeleteMessageBatchOutput, error)
}

type Consumer struct {
	SQS      ConsumerAPI
	QueueURL string

	WaitTimeSeconds   int32
	MaxMessages       int32
	VisibilityTimeout int32

	// Receivers is how many ReceiveMessage calls are in flight at once.
	//
	// This is the setting that decides the consumer's ceiling, and it used to be
	// fixed at one. A single receive loop fetches at most MaxMessages (10) per
	// round-trip, so its throughput is 10 ÷ round-trip-time no matter how many
	// handler goroutines wait behind it — roughly 200-300 messages/second, which
	// is below what even a modest worker pool can process. Handlers then sit
	// idle waiting to be fed.
	//
	// Defaults to one receiver per ten handler slots, which keeps the pool fed
	// without spending API calls on empty polls.
	Receivers int

	// DeleteBatchDelay is how long a completed message waits for company before
	// its delete is sent. Deletes are batched ten at a time, turning one API
	// call per message into one per ten.
	DeleteBatchDelay time.Duration
}

type Handler func(ctx context.Context, job SMSJob) error

func (c *Consumer) receivers(workers int) int {
	if c.Receivers > 0 {
		return c.Receivers
	}
	n := workers / 10
	if n < 1 {
		n = 1
	}
	return n
}

func (c *Consumer) deleteBatchDelay() time.Duration {
	if c.DeleteBatchDelay <= 0 {
		return 20 * time.Millisecond
	}
	return c.DeleteBatchDelay
}

// Poll runs a single receiver with a single handler slot. Kept for callers that
// want the simplest possible loop; the service uses PollConcurrent.
func (c *Consumer) Poll(ctx context.Context, handler Handler) error {
	return c.PollConcurrent(ctx, 1, handler)
}

// PollConcurrent runs `workers` handlers fed by several concurrent receivers,
// deleting completed messages in batches.
//
// A message is deleted only after its handler returns nil. A handler error
// leaves the message to reappear after the visibility timeout, which is what
// drives SQS redrive and eventually the DLQ.
func (c *Consumer) PollConcurrent(ctx context.Context, workers int, handler Handler) error {
	if workers <= 0 {
		workers = 1
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan types.Message, workers*2)
	deletes := make(chan string, workers*4)

	var handlers sync.WaitGroup
	for i := 0; i < workers; i++ {
		handlers.Add(1)
		go func() {
			defer handlers.Done()
			for m := range jobs {
				c.handleOne(ctx, m, handler, deletes)
			}
		}()
	}

	var deleter sync.WaitGroup
	deleter.Add(1)
	go func() {
		defer deleter.Done()
		c.deleteLoop(deletes)
	}()

	var receivers sync.WaitGroup
	for i := 0; i < c.receivers(workers); i++ {
		receivers.Add(1)
		go func() {
			defer receivers.Done()
			c.receiveLoop(ctx, jobs)
		}()
	}

	receivers.Wait()
	close(jobs)
	handlers.Wait()
	close(deletes)
	deleter.Wait()

	return ctx.Err()
}

func (c *Consumer) receiveLoop(ctx context.Context, jobs chan<- types.Message) {
	for {
		if ctx.Err() != nil {
			return
		}
		out, err := c.SQS.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:            &c.QueueURL,
			MaxNumberOfMessages: c.MaxMessages,
			WaitTimeSeconds:     c.WaitTimeSeconds,
			VisibilityTimeout:   c.VisibilityTimeout,
		})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("sqs receive message failed", "err", err)
			select {
			case <-time.After(500 * time.Millisecond):
			case <-ctx.Done():
				return
			}
			continue
		}
		for _, m := range out.Messages {
			select {
			case jobs <- m:
			case <-ctx.Done():
				return
			}
		}
	}
}

func (c *Consumer) handleOne(ctx context.Context, m types.Message, handler Handler, deletes chan<- string) {
	receipt := ""
	if m.ReceiptHandle != nil {
		receipt = *m.ReceiptHandle
	}

	// A message that cannot be parsed will never parse. Deleting it stops an
	// endless redrive; leaving it would occupy a receive slot forever.
	if m.Body == nil {
		c.queueDelete(deletes, receipt)
		return
	}
	var job SMSJob
	if err := json.Unmarshal([]byte(*m.Body), &job); err != nil {
		slog.Error("sqs message body is not a job; dropping", "err", err)
		c.queueDelete(deletes, receipt)
		return
	}

	if err := handler(ctx, job); err != nil {
		// Left undeleted on purpose: SQS redrive and the DLQ handle it.
		slog.Error("sqs handler error", "err", err, "message_id", job.MessageID)
		return
	}
	c.queueDelete(deletes, receipt)
}

// queueDelete hands a completed message to the delete batcher.
//
// Deliberately not cancellable. An earlier version selected on ctx.Done() here,
// which meant a handler that SUCCEEDED during shutdown had its delete silently
// discarded — the send had gone out, but the message stayed on the queue and
// came back after the visibility timeout, so the recipient got a second SMS and
// the message crept toward the dead-letter queue.
//
// The send cannot block forever and cannot race a closed channel: PollConcurrent
// closes `deletes` only after handlers.Wait() returns, so the batcher is alive
// for as long as any handler can still call this.
func (c *Consumer) queueDelete(deletes chan<- string, receipt string) {
	if receipt == "" {
		return
	}
	deletes <- receipt
}

// deleteLoop coalesces receipt handles into DeleteMessageBatch calls of ten. It
// deliberately does not take a cancellable context for the delete itself: a
// message whose handler already succeeded must be removed, or it will be
// redelivered and the recipient will get a second SMS.
func (c *Consumer) deleteLoop(deletes <-chan string) {
	batch := make([]string, 0, 10)
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	armed := false

	flush := func() {
		if len(batch) == 0 {
			return
		}
		c.deleteBatch(batch)
		batch = batch[:0]
		if armed {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			armed = false
		}
	}

	for {
		select {
		case receipt, ok := <-deletes:
			if !ok {
				flush()
				return
			}
			batch = append(batch, receipt)
			if len(batch) >= 10 {
				flush()
				continue
			}
			if !armed {
				timer.Reset(c.deleteBatchDelay())
				armed = true
			}
		case <-timer.C:
			armed = false
			flush()
		}
	}
}

func (c *Consumer) deleteBatch(receipts []string) {
	entries := make([]types.DeleteMessageBatchRequestEntry, len(receipts))
	for i := range receipts {
		id := strconv.Itoa(i)
		r := receipts[i]
		entries[i] = types.DeleteMessageBatchRequestEntry{Id: &id, ReceiptHandle: &r}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	out, err := c.SQS.DeleteMessageBatch(ctx, &sqs.DeleteMessageBatchInput{
		QueueUrl: &c.QueueURL,
		Entries:  entries,
	})
	if err != nil {
		slog.Error("sqs delete batch failed; messages will be redelivered", "err", err, "count", len(receipts))
		return
	}
	for _, f := range out.Failed {
		reason := ""
		if f.Message != nil {
			reason = *f.Message
		}
		slog.Error("sqs delete entry failed; message will be redelivered", "reason", reason)
	}
}
