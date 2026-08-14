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
	ClaimAndLoad(ctx context.Context, msgID string, now time.Time, staleAfter time.Duration) (store.ClaimedMessage, bool, error)
	RecordAttempt(ctx context.Context, in store.AttemptRecord) error
	MarkMessageState(ctx context.Context, in store.MessageStateUpdate) error
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

	// Claiming is what makes this consumer idempotent, and it now also loads the
	// message, so a duplicate delivery costs one round-trip instead of two. The
	// claim's own condition — queued, or processing past the stale window — is
	// the same test the separate state checks used to make before it.
	msg, found, err := p.Store.ClaimAndLoad(ctx, job.MessageID, util.NowUTC(), p.claimStaleAfter())
	if err != nil {
		return err
	}
	if !found {
		// The job names a message that does not exist. Returning an error sends
		// it back to the queue and eventually to the DLQ, where it is visible,
		// rather than silently dropping it.
		observability.WorkerProcessed.WithLabelValues("failure_message_missing").Inc()
		return errors.New("message not found: " + job.MessageID)
	}
	if !msg.Claimed {
		// Terminal, already submitted, or held by another worker. Ordinary
		// duplicate delivery — acknowledge and move on.
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

	for attemptNum := 0; attemptNum < 3; attemptNum++ {
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

			// The attempt and the state it produces are written together: one
			// round-trip, and no window in which an attempt exists for a message
			// still reading 'processing'.
			if err := p.Store.RecordAttempt(ctx, store.AttemptRecord{
				Attempt: store.ProviderAttempt{
					MessageID:     job.MessageID,
					Provider:      "twilio",
					ProviderMsgID: resp.Sid,
					HTTPStatus:    httpStatus,
					RequestJSON: map[string]any{
						"to": msg.To, "templateId": msg.TemplateID, "campaignId": msg.CampaignID, "tenantId": msg.TenantID,
					},
					ResponseJSON: jsonRaw(raw),
				},
				Transition: &store.MessageTransition{
					State:         "submitted",
					Provider:      "twilio",
					ProviderMsgID: resp.Sid,
					Now:           util.NowUTC(),
				},
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

		attempt := store.AttemptRecord{
			Attempt: store.ProviderAttempt{
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
			},
		}

		// A retryable error records the attempt and nothing else — the message
		// stays in processing for the next pass. A non-retryable one ends the
		// message, so the attempt and the failure are written together.
		nonRetryable := !twilio.ShouldRetry(err, httpStatus)
		if nonRetryable {
			attempt.Transition = &store.MessageTransition{
				State:     "failed",
				LastError: "twilio_non_retryable",
				Now:       util.NowUTC(),
			}
		}
		if recErr := p.Store.RecordAttempt(ctx, attempt); recErr != nil {
			return recErr
		}
		if nonRetryable {
			result = "failure_non_retryable"
			return err
		}

		time.Sleep(twilio.Backoff(attemptNum))
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
