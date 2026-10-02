package worker

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/sony/gobreaker"

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
	ReleaseForRetry(ctx context.Context, id, lastErr string, claimedAt, now time.Time) (bool, error)
}

type TwilioSender interface {
	SendSMS(ctx context.Context, req twilio.SendRequest) (twilio.SendResponse, int, []byte, error)
}

type Processor struct {
	Store           Store
	Sender          TwilioSender
	Templates       map[string]string
	Breaker         *gobreaker.CircuitBreaker
	ClaimStaleAfter time.Duration
	// StatusCallbackURL is sent with every message so the provider reports
	// delivery to the webhook service. Without it, delivery reports depend on
	// a callback configured out-of-band in the provider console — and if that
	// is missing, every message stays 'submitted' forever.
	StatusCallbackURL string
}

// ErrHeldElsewhere is returned for a delivery whose message another worker is
// still processing. It is deliberately an error: the job is not finished, so
// SQS must keep a copy until the holder settles the row.
var ErrHeldElsewhere = errors.New("message is held by another worker; leaving it for redelivery")

// workerIsDone reports whether a message needs nothing further from the
// worker. 'submitted' counts: the provider has it, and the delivery callback
// (or reconcile) finishes it, not another send.
func workerIsDone(state string) bool {
	switch state {
	case "submitted", "delivered", "failed", "suppressed":
		return true
	}
	return false
}

// Process handles one delivery of a job. Its return value is an instruction to
// the queue, not a report on the message:
//
//   - nil   — this job is finished; delete it. The message reached a state the
//     database has recorded (submitted, or failed permanently), or this
//     delivery was a duplicate of one that did.
//   - error — this job is NOT finished; leave it for SQS to redeliver, and to
//     dead-letter after maxReceiveCount. Only returned when the row is still
//     claimable on the next delivery (released to 'queued') or nothing was
//     written at all.
//
// Never return an error after writing a terminal state. The redelivery cannot
// re-claim a terminal row, so it is skipped and acknowledged: the error buys a
// wasted round-trip at best and, when the terminal write was wrong, a message
// discarded without ever reaching the DLQ.
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
		if workerIsDone(msg.State) {
			// Already submitted, delivered, failed or suppressed: an ordinary
			// duplicate delivery of a job that is finished. Counted as a claim
			// result, NOT as a message outcome, so redeliveries stop inflating
			// any per-message ratio — and acknowledged, because it is done.
			observability.ClaimResult.WithLabelValues("skipped").Inc()
			return nil
		}
		// Held by another worker whose claim is still fresh (or racing ours:
		// the pre-claim read can still say 'queued' when another claim lands
		// first). This copy must NOT be acknowledged. The holder may yet hand
		// the row back for retry — retries exhausted, breaker open — and its
		// own SQS copy may already be gone, so deleting this one would leave a
		// 'queued' row with nothing left on the queue to ever send it.
		// Returning an error lets SQS redeliver it; once the holder finishes,
		// the next delivery sees a finished row and is acknowledged above.
		observability.ClaimResult.WithLabelValues("held").Inc()
		return ErrHeldElsewhere
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
		return p.release(ctx, job.MessageID, msg.ClaimedAt, "template_not_found",
			errors.New("template_not_found: "+msg.TemplateID))
	}
	body := util.RenderTemplate(bodyTmpl, msg.Vars)

	// Send with small retries on transient issues.
	//
	// Note there is no timer started here. The metric this replaced began its
	// clock at this point, so every sample carried the whole retry loop and was
	// then quoted as provider latency. The provider call is timed where the
	// provider call happens, below.
	var lastErr error
	endToEndRecorded := false

	for attemptNum := 0; attemptNum < 3; attemptNum++ {
		// The circuit breaker wraps the provider call. The timer wraps exactly
		// this and nothing else — not the backoff below.
		callStart := util.NowUTC()
		resAny, err := p.executeWithBreaker(ctx, msg.To, body)
		callSeconds := time.Since(callStart).Seconds()

		// Breaker open: fail fast and let SQS redrive.
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
			return p.release(ctx, job.MessageID, msg.ClaimedAt, "circuit_breaker_open", err)
		}

		var resp twilio.SendResponse
		var httpStatus int
		var raw []byte

		if err == nil {
			r := resAny.(sendResult)
			resp, httpStatus, raw = r.resp, r.httpStatus, r.raw

			observability.ProviderAttempts.WithLabelValues("ok", strconv.Itoa(httpStatus)).Inc()
			observability.ProviderCallSeconds.WithLabelValues("ok").Observe(callSeconds)
			if !endToEndRecorded {
				observability.EndToEndLatency.Observe(time.Since(msg.CreatedAt).Seconds())
				endToEndRecorded = true
			}

			// The attempt and the state it produces are written together: one
			// round-trip, and no window in which an attempt exists for a message
			// still reading 'processing'.
			//
			// This is the one write that must not be lost. The SMS has already
			// gone out; if the row stays 'processing', the redelivery re-claims
			// it once the claim goes stale and sends it AGAIN, and the provider
			// id that would tie delivery callbacks to the row is gone. So it is
			// retried, on a deadline of its own that survives cancellation of
			// the job context (shutdown drain, request timeouts).
			err := p.recordSubmit(ctx, store.AttemptRecord{
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
					ClaimedAt:     msg.ClaimedAt,
				},
			})
			if errors.Is(err, store.ErrClaimLost) {
				// Sent, but the row is no longer ours: another worker re-claimed
				// it after our claim went stale, or a delivery callback already
				// moved it on. The attempt row is recorded; the row's state is
				// the other party's to settle. Nothing left for this job to do.
				outcome = "claim_lost"
				slog.Warn("sent, but claim was lost before the result was recorded",
					"message_id", job.MessageID, "provider_msg_id", resp.Sid)
				return nil
			}
			if err != nil {
				return err
			}
			outcome = "submitted"
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
				ClaimedAt: msg.ClaimedAt,
			}
		}
		if recErr := p.Store.RecordAttempt(ctx, attempt); recErr != nil {
			if errors.Is(recErr, store.ErrClaimLost) {
				outcome = "claim_lost"
				return nil
			}
			return recErr
		}
		if nonRetryable {
			// Final, and durably recorded: the row is 'failed' and the attempt
			// that failed it is stored. So the job is done and returns nil.
			//
			// It used to return err, which told the consumer the opposite — "not
			// done, redeliver" — while the database said "done". SQS redelivered
			// it after the visibility timeout, ClaimAndLoad refused the failed
			// row, and the redelivery was acknowledged as a duplicate. Harmless
			// for a real 400, but it cost every permanent failure one wasted
			// receive, one claim query and a misleading "sqs handler error" log.
			// And it was the same pairing — terminal write plus error return —
			// that silently discarded transient failures when they were
			// misclassified as permanent (docs/campaign-100k/retry-handling-ab-2026-08-15.md).
			//
			// The failure is still reported: the outcome metric, the stored
			// attempt and last_error, and this log line.
			outcome = "provider_rejected"
			slog.Warn("provider rejected message permanently",
				"message_id", job.MessageID, "http_status", httpStatus, "err", err)
			return nil
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
	outcome = "retries_exhausted"
	return p.release(ctx, job.MessageID, msg.ClaimedAt, "twilio_retry_exhausted", lastErr)
}

// release hands a claimed message back to the queue and returns the error that
// tells the consumer to leave the job for redelivery. If the release did not
// apply — the claim was lost to another worker, or a callback already finished
// the row — there is nothing left for this job to retry, so it returns nil and
// the job is acknowledged; the row's current owner settles it.
func (p *Processor) release(ctx context.Context, id string, claimedAt time.Time, reason string, cause error) error {
	released, err := p.Store.ReleaseForRetry(ctx, id, reason, claimedAt, util.NowUTC())
	if err != nil {
		return err
	}
	if !released {
		slog.Info("release skipped: message no longer held by this claim", "message_id", id, "reason", reason)
		return nil
	}
	return cause
}

// recordSubmit writes a successful send's attempt and transition, retrying a
// failed write a few times on its own deadline. A claim-lost result is final
// and returned at once.
func (p *Processor) recordSubmit(ctx context.Context, rec store.AttemptRecord) error {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	var err error
	for i, wait := range []time.Duration{0, 200 * time.Millisecond, time.Second, 3 * time.Second} {
		if wait > 0 {
			select {
			case <-time.After(wait):
			case <-wctx.Done():
				return err
			}
		}
		err = p.Store.RecordAttempt(wctx, rec)
		if err == nil || errors.Is(err, store.ErrClaimLost) {
			return err
		}
		slog.Warn("recording a sent message failed; retrying",
			"message_id", rec.Attempt.MessageID, "try", i+1, "err", err)
	}
	return err
}

func (p *Processor) executeWithBreaker(ctx context.Context, to, body string) (any, error) {
	call := func() (any, error) {
		reqCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
		defer cancel()

		resp, httpStatus, raw, callErr := p.Sender.SendSMS(reqCtx, twilio.SendRequest{
			To:                to,
			Body:              body,
			StatusCallbackURL: p.StatusCallbackURL,
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

// IsPermanentRejection reports whether err is the provider answering with a
// status that will not improve on retry (e.g. 400) — a correct answer about one
// message, not a sign the provider is unhealthy.
func IsPermanentRejection(err error) bool {
	var tce twilioCallError
	if !errors.As(err, &tce) || tce.httpStatus == 0 {
		return false
	}
	return !twilio.ShouldRetry(err, tce.httpStatus)
}
func (e twilioCallError) Unwrap() error { return e.err }
