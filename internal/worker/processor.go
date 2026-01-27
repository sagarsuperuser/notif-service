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
	"notif/internal/util"
)

type Store interface {
	GetMessageForWorker(ctx context.Context, msgID string) (tenantID, to, templateID, campaignID, state, providerMsgID string, vars map[string]string, err error)
	InsertAttempt(ctx context.Context, msgID, provider, providerMsgID string, httpStatus int, errCode, errMsg string, reqJSON, respJSON any) error
	SetProviderDetails(ctx context.Context, msgID, provider, providerMsgID, state string, now time.Time) error
	MarkMessageState(ctx context.Context, msgID, state, lastError string, now time.Time) error
}

type TwilioSender interface {
	SendSMS(ctx context.Context, req twilio.SendRequest) (twilio.SendResponse, int, []byte, error)
}

type Processor struct {
	Store     Store
	Sender    TwilioSender
	Templates map[string]string
	Limiter   *rate.Limiter
	Breaker   *gobreaker.CircuitBreaker
}

func (p *Processor) Process(ctx context.Context, job sqsqueue.SMSJob) error {
	tenantID, to, templateID, campaignID, state, providerMsgID, vars, err := p.Store.GetMessageForWorker(ctx, job.MessageID)
	if err != nil {
		return err
	}

	// Idempotent consumer: skip final or already submitted with SID
	if state == "suppressed" || state == "delivered" || state == "failed" {
		return nil
	}
	if providerMsgID != "" && state == "submitted" {
		return nil
	}

	bodyTmpl, ok := p.Templates[templateID]
	if !ok || bodyTmpl == "" {
		_ = p.Store.MarkMessageState(ctx, job.MessageID, "failed", "template_not_found", time.Now())
		return errors.New("template_not_found: " + templateID)
	}
	body := util.RenderTemplate(bodyTmpl, vars)

	// Send with small retries on transient issues
	var lastErr error
	start := time.Now()

	for attempt := 0; attempt < 3; attempt++ {
		// 1) Rate limit before calling Twilio (per pod)
		if p.Limiter != nil {
			waitCtx, cancelWait := context.WithTimeout(ctx, 2*time.Second)
			err := p.Limiter.Wait(waitCtx)
			cancelWait()
			if err != nil {
				// If we can't even acquire a token, treat as transient (don't mark failed)
				observability.TwilioSend.WithLabelValues("rate_limited_local", "0").Inc()
				time.Sleep(200 * time.Millisecond)
				continue
			}
		}

		// 2) Circuit breaker wraps the Twilio call
		resAny, err := p.executeWithBreaker(ctx, to, body)

		// 3) Handle breaker open (fail fast; let SQS redrive later)
		if errors.Is(err, gobreaker.ErrOpenState) || errors.Is(err, gobreaker.ErrTooManyRequests) {
			observability.TwilioSend.WithLabelValues("cb_open", "0").Inc()
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

			_ = p.Store.InsertAttempt(ctx, job.MessageID, "twilio", resp.Sid, httpStatus, "", "", map[string]any{
				"to": to, "templateId": templateID, "campaignId": campaignID, "tenantId": tenantID,
			}, jsonRaw(raw))

			if err := p.Store.SetProviderDetails(ctx, job.MessageID, "twilio", resp.Sid, "submitted", time.Now()); err != nil {
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

		_ = p.Store.InsertAttempt(ctx, job.MessageID, "twilio", "", httpStatus, "", err.Error(), map[string]any{
			"to": to, "templateId": templateID, "campaignId": campaignID, "tenantId": tenantID,
		}, map[string]any{
			"raw": string(raw),
		})

		if !twilio.ShouldRetry(err, httpStatus) {
			_ = p.Store.MarkMessageState(ctx, job.MessageID, "failed", "twilio_non_retryable", time.Now())
			return err
		}

		time.Sleep(twilio.Backoff(attempt))
	}

	_ = p.Store.MarkMessageState(ctx, job.MessageID, "failed", "twilio_retry_exhausted", time.Now())
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
