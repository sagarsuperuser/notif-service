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
	ReleaseForRetry(ctx context.Context, id, lastErr string, now time.Time) (bool, error)
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

	// Deliberately empty. An audit found the previous version defaulted this to
	// "success" and had five error returns that left it there, so failures were
	// counted as successes. Every exit path below names its outcome, and an
	// empty one is reported as "unset" rather than silently becoming a success —
	// so a future unnamed exit shows up as a visible gap instead of inflating
	// the success rate.
	outcome := ""

	defer func() {
		if !processed {
			return
		}
		if outcome == "" {
			outcome = "unset"
		}
		observability.MessageOutcome.WithLabelValues(outcome).Inc()
		observability.WorkerProcessingSeconds.Observe(time.Since(started).Seconds())
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
		observability.ClaimResult.WithLabelValues("missing").Inc()
		// The job names a message that does not exist. Returning an error sends
		// it back to the queue and eventually to the DLQ, where it is visible,
		// rather than silently dropping it.

		return errors.New("message not found: " + job.MessageID)
	}
	if !msg.Claimed {
		// Terminal, already submitted, or held by another worker. Ordinary
		// duplicate delivery — counted as a claim result, NOT as a message
		// outcome, so redeliveries stop inflating any per-message ratio.
		observability.ClaimResult.WithLabelValues("skipped").Inc()
		return nil
	}
	observability.ClaimResult.WithLabelValues("claimed").Inc()
	processed = true

	bodyTmpl, ok := p.Templates[msg.TemplateID]
	if !ok || bodyTmpl == "" {
		// Not terminal. Templates are loaded from configuration at boot, so a
		// missing one is at least as likely to be a bad deploy as a bad
		// message — and the two are indistinguishable from here. Failing the
		// message permanently would destroy valid traffic during a config
		// glitch, while handing it back costs a few redeliveries and puts it
		// in the DLQ where it can be redriven once the template is restored.
		// Of the two ways to be wrong, only one loses messages.
		outcome = "template_not_found"
		if _, err := p.Store.ReleaseForRetry(ctx, job.MessageID, "template_not_found", util.NowUTC()); err != nil {
			return err
		}
		return errors.New("template_not_found: " + msg.TemplateID)
	}
	body := util.RenderTemplate(bodyTmpl, msg.Vars)

	// Send with small retries on transient issues.
	//
	// Note there is no timer started here. The metric this replaced began its
	// clock at this point, so every sample carried the limiter wait and any
	// retries — and was then quoted as provider latency. The provider call is
	// timed where the provider call happens, below.
	var lastErr error
	endToEndRecorded := false

	for attemptNum := 0; attemptNum < 3; attemptNum++ {
		// 1) Rate limit before calling Twilio (per pod)
		if p.Limiter != nil {
			waitCtx, cancelWait := context.WithTimeout(ctx, 2*time.Second)
			waitStart := util.NowUTC()
			err := p.Limiter.Wait(waitCtx)
			observability.ProviderWaitSeconds.Observe(time.Since(waitStart).Seconds())
			cancelWait()
			if err != nil {
				// If we can't even acquire a token, treat as transient (don't mark failed)
				observability.ProviderAttempts.WithLabelValues("rate_limit_timeout", "0").Inc()
				lastErr = err
				time.Sleep(200 * time.Millisecond)
				continue
			}
		}

		// 2) Circuit breaker wraps the provider call. The timer wraps exactly
		// this and nothing else: not the limiter above, not the backoff below.
		callStart := util.NowUTC()
		resAny, err := p.executeWithBreaker(ctx, msg.To, body)
		callSeconds := time.Since(callStart).Seconds()

		// 3) Handle breaker open (fail fast; let SQS redrive later)
		if errors.Is(err, gobreaker.ErrOpenState) || errors.Is(err, gobreaker.ErrTooManyRequests) {
			observability.ProviderAttempts.WithLabelValues("circuit_open", "0").Inc()
			outcome = "circuit_breaker_open"
			// Not a message failure — the provider is being protected, and this
			// message never reached it.
			//
			// The claim is released rather than abandoned in 'processing'. If it
			// were left held, the redelivery could only re-claim it once the row
			// aged past the stale window, which makes recovery depend on two
			// timeouts lining up. Releasing it makes the next delivery claimable
			// immediately and leaves the stale window as a backstop for crashes,
			// which is the only thing it can actually cover.
			if _, relErr := p.Store.ReleaseForRetry(ctx, job.MessageID, "circuit_breaker_open", util.NowUTC()); relErr != nil {
				return relErr
			}
			return err
		}

		var resp twilio.SendResponse
		var httpStatus int
		var raw []byte

		if err == nil {
			r := resAny.(sendResult)
			resp, httpStatus, raw = r.resp, r.httpStatus, r.raw

			observability.ProviderAttempts.WithLabelValues("ok", strconv.Itoa(httpStatus)).Inc()
			observability.ProviderCallSeconds.WithLabelValues("ok").Observe(callSeconds)
			outcome = "submitted"
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

		observability.ProviderAttempts.WithLabelValues("error", strconv.Itoa(httpStatus)).Inc()
		observability.ProviderCallSeconds.WithLabelValues("error").Observe(callSeconds)
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
			outcome = "provider_rejected"
			return err
		}

		time.Sleep(twilio.Backoff(attemptNum))
	}

	// Exhausting the in-process attempts is NOT terminal, and treating it as
	// terminal defeated the outer retry entirely.
	//
	// There are two nested loops by design: three attempts here over a couple
	// of seconds, and up to sqs_send_max_receive_count deliveries outside,
	// spaced by the visibility timeout. The outer loop is the one that matters
	// for a provider outage, since no amount of retrying within two seconds
	// survives an incident measured in minutes.
	//
	// Writing state='failed' here cut that off. ClaimAndLoad only claims
	// 'queued' or stale 'processing', so the redelivery could not re-claim a
	// failed row; it counted as skipped, returned nil, and the consumer
	// deleted the receipt. The send was abandoned after one silent redelivery
	// and never reached the DLQ — which is why "messages in DLQ: 0" could
	// never have caught this.
	//
	// The branch above for an open circuit already states this rule ("do NOT
	// mark message failed; this is transient provider protection"). This path
	// now follows it.
	if _, err := p.Store.ReleaseForRetry(ctx, job.MessageID, "twilio_retry_exhausted", util.NowUTC()); err != nil {
		return err
	}
	outcome = "retries_exhausted"
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
