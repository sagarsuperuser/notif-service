package worker

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/sony/gobreaker"
	"golang.org/x/time/rate"

	"notif/internal/observability"
	"notif/internal/providers/twilio"
	sqsqueue "notif/internal/queue/sqs"
	"notif/internal/store"
	"notif/internal/util"
)

type Store interface {
	GetMessageForWorker(ctx context.Context, msgID string) (store.MessageForWorker, error)
	InsertAttempt(ctx context.Context, in store.ProviderAttempt) error
	SetProviderDetails(ctx context.Context, in store.ProviderDetailsUpdate) error
	MarkMessageState(ctx context.Context, in store.MessageStateUpdate) error
	ClaimMessage(ctx context.Context, msgID string, now time.Time, staleAfter time.Duration) (bool, error)
}

type TwilioSender interface {
	SendSMS(ctx context.Context, req twilio.SendRequest) (twilio.SendResponse, int, []byte, error)
}

type Processor struct {
	Store           Store
	Sender          TwilioSender
	Templates       map[string]string
	Limiter         *rate.Limiter
	Breaker         *gobreaker.CircuitBreaker
	ClaimStaleAfter time.Duration
}

func (p *Processor) Process(ctx context.Context, job sqsqueue.SMSJob) error {
	started := util.NowUTC()
	processed := false
	result := "success"

	defer func() {
		if processed {
			observability.WorkerProcessed.WithLabelValues(result).Inc()
			observability.WorkerProcessingSeconds.Observe(time.Since(started).Seconds())
		}
	}()

	msg, err := p.Store.GetMessageForWorker(ctx, job.MessageID)
	if err != nil {
		return err
	}

	// Idempotent consumer: skip final or already submitted with SID
	if msg.State == "suppressed" || msg.State == "delivered" || msg.State == "failed" {
		return nil
	}
	if msg.ProviderMsgID != "" && msg.State == "submitted" {
		return nil
	}

	// Claim before sending to avoid duplicate processing.
	claimed, err := p.Store.ClaimMessage(ctx, job.MessageID, util.NowUTC(), p.claimStaleAfter())
	if err != nil {
		return err
	}
	if !claimed {
		return nil
	}
	processed = true

	bodyTmpl, ok := p.Templates[msg.TemplateID]
	if !ok || bodyTmpl == "" {
		result = "failure_invalid_template"
		if err := p.Store.MarkMessageState(ctx, store.MessageStateUpdate{
			ID:        job.MessageID,
			State:     "failed",
			LastError: "template_not_found",
			Now:       util.NowUTC(),
		}); err != nil {
			return err
		}
		return errors.New("template_not_found: " + msg.TemplateID)
	}
	body := util.RenderTemplate(bodyTmpl, msg.Vars)

	// Send with small retries on transient issues
	var lastErr error
	start := util.NowUTC()
	endToEndRecorded := false

	for attempt := 0; attempt < 3; attempt++ {
		// 1) Rate limit before calling Twilio (per pod)
		if p.Limiter != nil {
			waitCtx, cancelWait := context.WithTimeout(ctx, 2*time.Second)
			err := p.Limiter.Wait(waitCtx)
			cancelWait()
			if err != nil {
				// If we can't even acquire a token, treat as transient (don't mark failed)
				observability.TwilioSend.WithLabelValues("rate_limited_local", "0").Inc()
				lastErr = err
				time.Sleep(200 * time.Millisecond)
				continue
			}
		}

		// 2) Circuit breaker wraps the Twilio call
		resAny, err := p.executeWithBreaker(ctx, msg.To, body)

		// 3) Handle breaker open (fail fast; let SQS redrive later)
		if errors.Is(err, gobreaker.ErrOpenState) || errors.Is(err, gobreaker.ErrTooManyRequests) {
			observability.TwilioSend.WithLabelValues("failed_cb_open", "0").Inc()
			result = "failure_throttled_cb"
			// IMPORTANT: do NOT mark message failed; this is transient provider protection.
			return err
		}

		var resp twilio.SendResponse
		var httpStatus int
		var raw []byte

		if err == nil {
			r := resAny.(sendResult)
			resp, httpStatus, raw = r.resp, r.httpStatus, r.raw

			observability.TwilioSend.WithLabelValues("ok", strconv.Itoa(httpStatus)).Inc()
			observability.TwilioLatency.Observe(time.Since(start).Seconds())
			if !endToEndRecorded {
				observability.EndToEndLatency.Observe(time.Since(msg.CreatedAt).Seconds())
				endToEndRecorded = true
			}

			if err := p.Store.InsertAttempt(ctx, store.ProviderAttempt{
				MessageID:     job.MessageID,
				Provider:      "twilio",
				ProviderMsgID: resp.Sid,
				HTTPStatus:    httpStatus,
				RequestJSON: map[string]any{
					"to": msg.To, "templateId": msg.TemplateID, "campaignId": msg.CampaignID, "tenantId": msg.TenantID,
				},
				ResponseJSON: jsonRaw(raw),
			}); err != nil {
				return err
			}

			if err := p.Store.SetProviderDetails(ctx, store.ProviderDetailsUpdate{
				ID:            job.MessageID,
				Provider:      "twilio",
				ProviderMsgID: resp.Sid,
				State:         "submitted",
				Now:           util.NowUTC(),
			}); err != nil {
				return err
			}
			return nil
		}

		// err != nil (non-breaker-open)
		lastErr = err

		// Extract httpStatus/raw if this was a twilioCallError
		var tce twilioCallError
		if errors.As(err, &tce) {
			httpStatus = tce.httpStatus
			raw = tce.raw
		}

		observability.TwilioSend.WithLabelValues("error", strconv.Itoa(httpStatus)).Inc()
		if !endToEndRecorded {
			observability.EndToEndLatency.Observe(time.Since(msg.CreatedAt).Seconds())
			endToEndRecorded = true
		}

		if err := p.Store.InsertAttempt(ctx, store.ProviderAttempt{
			MessageID:  job.MessageID,
			Provider:   "twilio",
			HTTPStatus: httpStatus,
			ErrorMsg:   err.Error(),
			RequestJSON: map[string]any{
				"to": msg.To, "templateId": msg.TemplateID, "campaignId": msg.CampaignID, "tenantId": msg.TenantID,
			},
			ResponseJSON: map[string]any{
				"raw": string(raw),
			},
		}); err != nil {
			return err
		}

		if !twilio.ShouldRetry(err, httpStatus) {
			result = "failure_non_retryable"
			if err := p.Store.MarkMessageState(ctx, store.MessageStateUpdate{
				ID:        job.MessageID,
				State:     "failed",
				LastError: "twilio_non_retryable",
				Now:       util.NowUTC(),
			}); err != nil {
				return err
			}
			return err
		}

		time.Sleep(twilio.Backoff(attempt))
	}

	if err := p.Store.MarkMessageState(ctx, store.MessageStateUpdate{
		ID:        job.MessageID,
		State:     "failed",
		LastError: "twilio_retry_exhausted",
		Now:       util.NowUTC(),
	}); err != nil {
		return err
	}
	result = "failure_retry_exhausted"
	return lastErr
}

func (p *Processor) executeWithBreaker(ctx context.Context, to, body string) (any, error) {
	call := func() (any, error) {
		reqCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
		defer cancel()

		resp, httpStatus, raw, callErr := p.Sender.SendSMS(reqCtx, twilio.SendRequest{
			To:   to,
			Body: body,
		})
		if callErr != nil {
			return nil, twilioCallError{err: callErr, httpStatus: httpStatus, raw: raw}
		}
		return sendResult{resp: resp, httpStatus: httpStatus, raw: raw}, nil
	}

	if p.Breaker == nil {
		return call()
	}
	return p.Breaker.Execute(call)
}

func (p *Processor) claimStaleAfter() time.Duration {
	if p.ClaimStaleAfter <= 0 {
		return 2 * time.Minute
	}
	return p.ClaimStaleAfter
}

func jsonRaw(b []byte) any { return map[string]any{"raw": string(b)} }

type sendResult struct {
	resp       twilio.SendResponse
	httpStatus int
	raw        []byte
}

type twilioCallError struct {
	err        error
	httpStatus int
	raw        []byte
}

func (e twilioCallError) Error() string { return e.err.Error() }
func (e twilioCallError) Unwrap() error { return e.err }
