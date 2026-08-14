//go:build integration
// +build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"notif/internal/awsutil"
	sqsqueue "notif/internal/queue/sqs"
)

// newQueue creates a fresh standard queue on the configured SQS endpoint
// (LocalStack in CI and locally) and returns a client plus its URL. Each test
// gets its own queue so leftovers from one cannot be received by another.
func newQueue(t *testing.T) (*sqs.Client, string) {
	t.Helper()
	endpoint := os.Getenv("TEST_SQS_ENDPOINT")
	if endpoint == "" {
		t.Skip("TEST_SQS_ENDPOINT not set")
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")

	ctx := context.Background()
	client, err := awsutil.NewSQSClient(ctx, "ap-south-1", endpoint)
	if err != nil {
		t.Fatalf("sqs client: %v", err)
	}

	name := fmt.Sprintf("q-%s-%d", sanitize(t.Name()), time.Now().UnixNano())
	out, err := client.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: &name})
	if err != nil {
		t.Fatalf("create queue: %v", err)
	}
	url := *out.QueueUrl
	t.Cleanup(func() {
		_, _ = client.DeleteQueue(context.Background(), &sqs.DeleteQueueInput{QueueUrl: &url})
	})
	return client, url
}

func sanitize(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
			out = append(out, r)
		default:
			out = append(out, '-')
		}
	}
	return string(out)
}

func queueDepth(t *testing.T, c *sqs.Client, url string) (visible, notVisible int) {
	t.Helper()
	out, err := c.GetQueueAttributes(context.Background(), &sqs.GetQueueAttributesInput{
		QueueUrl: &url,
		AttributeNames: []sqstypes.QueueAttributeName{
			sqstypes.QueueAttributeNameApproximateNumberOfMessages,
			sqstypes.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
		},
	})
	if err != nil {
		t.Fatalf("get queue attributes: %v", err)
	}
	fmt.Sscanf(out.Attributes[string(sqstypes.QueueAttributeNameApproximateNumberOfMessages)], "%d", &visible)
	fmt.Sscanf(out.Attributes[string(sqstypes.QueueAttributeNameApproximateNumberOfMessagesNotVisible)], "%d", &notVisible)
	return
}

// TestProducer_BatchesWithoutLosingMessages is the guarantee batching must not
// break: coalescing enqueues into SendMessageBatch calls may reduce API calls,
// but every caller that was told "sent" must have a message on the queue, and
// no caller may be told "sent" for a message that was not.
func TestProducer_BatchesWithoutLosingMessages(t *testing.T) {
	client, url := newQueue(t)
	p := &sqsqueue.Producer{SQS: client, QueueURL: url, MaxBatch: 10, MaxDelay: 20 * time.Millisecond}
	defer p.Close()

	const n = 250
	var wg sync.WaitGroup
	var ok atomic.Int64
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := p.EnqueueSMS(context.Background(), "t", fmt.Sprintf("m-%d", i),
				fmt.Sprintf("idem-%d", i), "+15550000000", "tpl", map[string]string{"n": "1"}, "")
			if err != nil {
				t.Errorf("enqueue %d: %v", i, err)
				return
			}
			ok.Add(1)
		}(i)
	}
	wg.Wait()

	if got := ok.Load(); got != n {
		t.Fatalf("%d of %d enqueues succeeded", got, n)
	}

	// Drain and count. Every acknowledged enqueue must be on the queue.
	seen := map[string]bool{}
	deadline := time.Now().Add(30 * time.Second)
	for len(seen) < n && time.Now().Before(deadline) {
		out, err := client.ReceiveMessage(context.Background(), &sqs.ReceiveMessageInput{
			QueueUrl:            &url,
			MaxNumberOfMessages: 10,
			WaitTimeSeconds:     1,
		})
		if err != nil {
			t.Fatalf("receive: %v", err)
		}
		for _, m := range out.Messages {
			seen[*m.Body] = true
			_, _ = client.DeleteMessage(context.Background(), &sqs.DeleteMessageInput{
				QueueUrl: &url, ReceiptHandle: m.ReceiptHandle,
			})
		}
	}
	if len(seen) != n {
		t.Errorf("drained %d distinct messages, want %d — batching lost messages it acknowledged", len(seen), n)
	}
}

// TestProducer_ReportsPerMessageFailure checks that a caller learns about its
// own message rather than its batch-mates'. A batch is a transport detail; a
// caller that gets nil must have had its message accepted.
func TestProducer_ReportsPerMessageFailure(t *testing.T) {
	client, url := newQueue(t)

	// Deleting the queue makes every send fail. Every caller must see an error;
	// none may be told its message was accepted.
	_, err := client.DeleteQueue(context.Background(), &sqs.DeleteQueueInput{QueueUrl: &url})
	if err != nil {
		t.Fatalf("delete queue: %v", err)
	}

	p := &sqsqueue.Producer{SQS: client, QueueURL: url, MaxBatch: 10, MaxDelay: 10 * time.Millisecond}
	defer p.Close()

	var wg sync.WaitGroup
	var silentSuccess atomic.Int64
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if err := p.EnqueueSMS(ctx, "t", fmt.Sprintf("m-%d", i), fmt.Sprintf("idem-%d", i),
				"+15550000000", "tpl", nil, ""); err == nil {
				silentSuccess.Add(1)
			}
		}(i)
	}
	wg.Wait()

	if n := silentSuccess.Load(); n != 0 {
		t.Errorf("%d callers were told their message was queued when the send failed", n)
	}
}

// TestConsumer_DeletesOnlyWhatSucceeded is the delete-batching guarantee. A
// handler that returns nil must have its message removed; a handler that
// returns an error must leave its message on the queue so SQS can redrive it.
// Batching deletes must not blur the two.
func TestConsumer_DeletesOnlyWhatSucceeded(t *testing.T) {
	client, url := newQueue(t)
	p := &sqsqueue.Producer{SQS: client, QueueURL: url, MaxBatch: 10, MaxDelay: 10 * time.Millisecond}
	defer p.Close()

	const n = 40
	for i := 0; i < n; i++ {
		if err := p.EnqueueSMS(context.Background(), "t", fmt.Sprintf("m-%d", i),
			fmt.Sprintf("idem-%d", i), "+15550000000", "tpl", nil, ""); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	c := &sqsqueue.Consumer{
		SQS: client, QueueURL: url,
		WaitTimeSeconds: 1, MaxMessages: 10, VisibilityTimeout: 3,
		Receivers: 2, DeleteBatchDelay: 10 * time.Millisecond,
	}

	// Odd-numbered messages fail. They must survive; even ones must vanish.
	var handled sync.Map
	var failures atomic.Int64
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	go func() {
		_ = c.PollConcurrent(ctx, 8, func(ctx context.Context, job sqsqueue.SMSJob) error {
			var idx int
			fmt.Sscanf(job.MessageID, "m-%d", &idx)
			if idx%2 == 1 {
				failures.Add(1)
				return errors.New("deliberate failure")
			}
			handled.Store(job.MessageID, true)
			return nil
		})
	}()

	// Wait until every even message has been handled at least once.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		count := 0
		handled.Range(func(_, _ any) bool { count++; return true })
		if count >= n/2 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	count := 0
	handled.Range(func(_, _ any) bool { count++; return true })
	if count != n/2 {
		t.Fatalf("handled %d of the %d messages that should succeed", count, n/2)
	}

	cancel()
	time.Sleep(500 * time.Millisecond)

	// The failing half must still be on the queue (visible or in flight).
	visible, notVisible := queueDepth(t, client, url)
	if total := visible + notVisible; total < n/2 {
		t.Errorf("queue holds %d messages (%d visible, %d in flight); the %d failed messages must survive for redrive",
			total, visible, notVisible, n/2)
	}
	if failures.Load() == 0 {
		t.Error("no handler failures were recorded; the test did not exercise the path it claims to")
	}
}
