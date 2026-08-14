package sqsqueue

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// SQSAPI is the slice of the SQS client this package uses, named so tests can
// substitute a fake without reaching for the whole client.
type SQSAPI interface {
	SendMessageBatch(ctx context.Context, in *sqs.SendMessageBatchInput, optFns ...func(*sqs.Options)) (*sqs.SendMessageBatchOutput, error)
}

type SMSJob struct {
	TenantID       string            `json:"tenantId"`
	MessageID      string            `json:"messageId"`
	IdempotencyKey string            `json:"idempotencyKey"`
	To             string            `json:"to"`
	TemplateID     string            `json:"templateId"`
	Vars           map[string]string `json:"vars"`
	CampaignID     string            `json:"campaignId,omitempty"`
}

// Producer sends jobs to a STANDARD queue, coalescing concurrent enqueues into
// SendMessageBatch calls of up to ten.
//
// Why standard and not FIFO: a FIFO queue outside high-throughput mode is
// limited to 300 transactions per second per API action, and 3,000 messages per
// second even with batching. Nothing in this system needs global ordering — two
// SMS to different recipients have no relationship, and two to the same
// recipient are ordered by when the customer sent them, not by the queue. The
// ordering guarantee was buying nothing and costing a hard ceiling.
//
// What replaces FIFO's deduplication: the message row's (tenant_id,
// idempotency_key) unique constraint decides whether a send exists at all, and
// the worker's claim decides who may send it. Both are stronger than SQS's
// 5-minute deduplication window, which would also have silently dropped a
// legitimate resend of the same key on minute four.
//
// Batching does not weaken the accept guarantee: a caller blocks until the
// batch containing its message has been acknowledged by SQS, so a 202 still
// means the job is durably queued.
type Producer struct {
	SQS      SQSAPI
	QueueURL string

	// MaxBatch is how many messages one SendMessageBatch may carry (SQS allows
	// 10). MaxDelay is how long the first message in a batch waits for company.
	// Both fall back to sane defaults when zero.
	MaxBatch int
	MaxDelay time.Duration

	startOnce sync.Once
	inbox     chan *enqueueReq
	closeOnce sync.Once
	done      chan struct{}
}

type enqueueReq struct {
	body string
	err  chan error
}

func (p *Producer) maxBatch() int {
	if p.MaxBatch <= 0 || p.MaxBatch > 10 {
		return 10
	}
	return p.MaxBatch
}

func (p *Producer) maxDelay() time.Duration {
	if p.MaxDelay <= 0 {
		return 5 * time.Millisecond
	}
	return p.MaxDelay
}

func (p *Producer) start() {
	p.startOnce.Do(func() {
		p.inbox = make(chan *enqueueReq, 1024)
		p.done = make(chan struct{})
		go p.loop()
	})
}

// Close flushes nothing by itself — it stops the batching loop. In-flight
// callers are released with an error so no request hangs on shutdown.
func (p *Producer) Close() {
	p.closeOnce.Do(func() {
		if p.done != nil {
			close(p.done)
		}
	})
}

func (p *Producer) loop() {
	batch := make([]*enqueueReq, 0, p.maxBatch())
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	timerArmed := false

	flush := func() {
		if len(batch) == 0 {
			return
		}
		p.send(batch)
		batch = batch[:0]
		if timerArmed {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timerArmed = false
		}
	}

	for {
		select {
		case <-p.done:
			flush()
			// Release anyone still queued rather than leaving them blocked.
			for {
				select {
				case req := <-p.inbox:
					req.err <- context.Canceled
				default:
					return
				}
			}
		case req := <-p.inbox:
			batch = append(batch, req)
			if len(batch) >= p.maxBatch() {
				flush()
				continue
			}
			if !timerArmed {
				timer.Reset(p.maxDelay())
				timerArmed = true
			}
		case <-timer.C:
			timerArmed = false
			flush()
		}
	}
}

// send issues one SendMessageBatch and fans the per-message result back to the
// caller that submitted it. SQS reports success and failure per entry, so a
// caller is told about its own message and not about its batch-mates.
func (p *Producer) send(batch []*enqueueReq) {
	entries := make([]types.SendMessageBatchRequestEntry, len(batch))
	for i, req := range batch {
		id := strconv.Itoa(i)
		body := req.body
		entries[i] = types.SendMessageBatchRequestEntry{
			Id:          &id,
			MessageBody: &body,
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	out, err := p.SQS.SendMessageBatch(ctx, &sqs.SendMessageBatchInput{
		QueueUrl: &p.QueueURL,
		Entries:  entries,
	})
	if err != nil {
		for _, req := range batch {
			req.err <- err
		}
		return
	}

	failed := make(map[string]string, len(out.Failed))
	for _, f := range out.Failed {
		msg := ""
		if f.Message != nil {
			msg = *f.Message
		}
		if f.Id != nil {
			failed[*f.Id] = msg
		}
	}
	for i, req := range batch {
		if msg, bad := failed[strconv.Itoa(i)]; bad {
			req.err <- &BatchEntryError{Reason: msg}
			continue
		}
		req.err <- nil
	}
}

// BatchEntryError is one entry rejected inside an otherwise successful batch.
type BatchEntryError struct{ Reason string }

func (e *BatchEntryError) Error() string { return "sqs batch entry failed: " + e.Reason }

func (p *Producer) EnqueueSMS(ctx context.Context, tenantID, messageID, idempotencyKey, to, templateID string, vars map[string]string, campaignID string) error {
	job := SMSJob{
		TenantID: tenantID, MessageID: messageID, IdempotencyKey: idempotencyKey,
		To: to, TemplateID: templateID, Vars: vars, CampaignID: campaignID,
	}
	body, err := json.Marshal(job)
	if err != nil {
		return err
	}

	p.start()
	req := &enqueueReq{body: string(body), err: make(chan error, 1)}

	select {
	case p.inbox <- req:
	case <-ctx.Done():
		return ctx.Err()
	}

	select {
	case err := <-req.err:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
