package sqsqueue

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// WebhookEvent is an internal envelope for provider callbacks.
// Keep it small; SQS has a 256KB message size limit.
type WebhookEvent struct {
	Provider      string              `json:"provider"`
	ProviderMsgID string              `json:"providerMsgId"`
	Status        string              `json:"status"`
	ErrorCode     string              `json:"errorCode,omitempty"`
	Payload       map[string][]string `json:"payload,omitempty"`
	ReceivedAt    time.Time           `json:"receivedAt"`
}

type WebhookProducer struct {
	SQS      *sqs.Client
	QueueURL string
}

func (p *WebhookProducer) Enqueue(ctx context.Context, ev WebhookEvent) error {
	body, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	_, err = p.SQS.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:    &p.QueueURL,
		MessageBody: str(string(body)),
	})
	return err
}

type WebhookHandler func(ctx context.Context, ev WebhookEvent) error

type WebhookConsumer struct {
	SQS      *sqs.Client
	QueueURL string

	WaitTimeSeconds   int32
	MaxMessages       int32
	VisibilityTimeout int32
}

// PollConcurrent processes webhook events with a worker pool. Messages are deleted only after handler completes.
func (c *WebhookConsumer) PollConcurrent(ctx context.Context, workers int, handler WebhookHandler) error {
	if workers <= 0 {
		workers = 1
	}

	jobs := make(chan types.Message, workers*2)
	errCh := make(chan error, 1)

	sendErr := func(err error) {
		select {
		case errCh <- err:
		default:
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for m := range jobs {
				if m.Body == nil {
					_, _ = c.SQS.DeleteMessage(ctx, &sqs.DeleteMessageInput{
						QueueUrl:      &c.QueueURL,
						ReceiptHandle: m.ReceiptHandle,
					})
					continue
				}

				var ev WebhookEvent
				if err := json.Unmarshal([]byte(*m.Body), &ev); err != nil {
					// bad payload => delete to avoid endless redrive
					_, _ = c.SQS.DeleteMessage(ctx, &sqs.DeleteMessageInput{
						QueueUrl:      &c.QueueURL,
						ReceiptHandle: m.ReceiptHandle,
					})
					continue
				}

				if err := handler(ctx, ev); err == nil {
					_, _ = c.SQS.DeleteMessage(ctx, &sqs.DeleteMessageInput{
						QueueUrl:      &c.QueueURL,
						ReceiptHandle: m.ReceiptHandle,
					})
				} else {
					// If err != nil: do NOT delete => SQS redrive/DLQ handles it
					slog.Error("sqs webhook handler error", "err", err, "provider", ev.Provider, "status", ev.Status, "provider_msg_id", ev.ProviderMsgID)
				}
			}
		}()
	}

	// Producer: fetch messages and enqueue for workers
	go func() {
		defer close(jobs)

		for {
			if ctx.Err() != nil {
				sendErr(ctx.Err())
				return
			}

			out, err := c.SQS.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
				QueueUrl:            &c.QueueURL,
				MaxNumberOfMessages: c.MaxMessages,
				WaitTimeSeconds:     c.WaitTimeSeconds,
				VisibilityTimeout:   c.VisibilityTimeout,
			})
			if err != nil {
				slog.Error("sqs receive webhook message failed", "err", err)
				time.Sleep(500 * time.Millisecond)
				continue
			}

			for _, m := range out.Messages {
				select {
				case jobs <- m:
				case <-ctx.Done():
					sendErr(ctx.Err())
					return
				}
			}
		}
	}()

	// Wait for shutdown signal (ctx canceled) or producer signals error
	err := <-errCh

	// Let workers finish whatever is already in `jobs` (channel will be closed by producer)
	wg.Wait()
	return err
}

