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

type Consumer struct {
	SQS      *sqs.Client
	QueueURL string

	WaitTimeSeconds   int32
	MaxMessages       int32
	VisibilityTimeout int32
}

type Handler func(ctx context.Context, job SMSJob) error

func (c *Consumer) Poll(ctx context.Context, handler Handler) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		out, err := c.SQS.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:            &c.QueueURL,
			MaxNumberOfMessages: c.MaxMessages,
			WaitTimeSeconds:     c.WaitTimeSeconds,
			VisibilityTimeout:   c.VisibilityTimeout,
		})
		if err != nil {
			slog.Error("sqs receive message failed", "err", err)
			time.Sleep(500 * time.Millisecond)
			continue
		}
		for _, m := range out.Messages {
			var job SMSJob
			if m.Body == nil {
				continue
			}
			if err := json.Unmarshal([]byte(*m.Body), &job); err != nil {
				// bad payload => delete to avoid endless redrive
				_, _ = c.SQS.DeleteMessage(ctx, &sqs.DeleteMessageInput{
					QueueUrl:      &c.QueueURL,
					ReceiptHandle: m.ReceiptHandle,
				})
				continue
			}

			err := handler(ctx, job)
			if err == nil {
				_, _ = c.SQS.DeleteMessage(ctx, &sqs.DeleteMessageInput{
					QueueUrl:      &c.QueueURL,
					ReceiptHandle: m.ReceiptHandle,
				})
			} else {
				// If err != nil: do NOT delete => SQS redrive/DLQ handles it
				slog.Error("sqs handler error", "err", err)
			}

		}
	}
}

// PollConcurrent processes messages with a worker pool. Messages are deleted only after handler completes.
func (c *Consumer) PollConcurrent(ctx context.Context, workers int, handler Handler) error {
	if workers <= 0 {
		return c.Poll(ctx, handler)
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
				// Always handle poison / invalid messages so they don't loop forever
				if m.Body == nil {
					_, _ = c.SQS.DeleteMessage(ctx, &sqs.DeleteMessageInput{
						QueueUrl:      &c.QueueURL,
						ReceiptHandle: m.ReceiptHandle,
					})
					continue
				}

				var job SMSJob
				if err := json.Unmarshal([]byte(*m.Body), &job); err != nil {
					_, _ = c.SQS.DeleteMessage(ctx, &sqs.DeleteMessageInput{
						QueueUrl:      &c.QueueURL,
						ReceiptHandle: m.ReceiptHandle,
					})
					continue
				}

				if err := handler(ctx, job); err == nil {
					_, _ = c.SQS.DeleteMessage(ctx, &sqs.DeleteMessageInput{
						QueueUrl:      &c.QueueURL,
						ReceiptHandle: m.ReceiptHandle,
					})
				}
				// If err != nil: do NOT delete => SQS redrive/DLQ handles it
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
				slog.Error("sqs receive message failed", "err", err)
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
