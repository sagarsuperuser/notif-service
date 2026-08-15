package pg

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"notif/internal/store"
)

type Store struct {
	DB *pgxpool.Pool
}

func New(db *pgxpool.Pool) *Store { return &Store{DB: db} }

func (s *Store) MarkMessageState(ctx context.Context, in store.MessageStateUpdate) error {
	_, err := s.DB.Exec(ctx, `
		UPDATE messages SET state=$2, last_error=$3, updated_at=$4 WHERE id=$1
	`, in.ID, in.State, nullIfEmpty(in.LastError), in.Now)
	return err
}

// ReleaseForRetry hands a message back to the queue after a transient failure,
// and is guarded on purpose.
//
// The guard is `state='processing'`, meaning "we still hold this claim". A
// blind UPDATE would race the delivery-callback path: a send whose response we
// never saw can still produce a webhook that moves the row to delivered, and
// resetting that back to queued would send the recipient a second SMS.
//
// The bool reports whether the reset actually happened, so the caller can tell
// "handed back" from "something else owns this now" instead of assuming.
func (s *Store) ReleaseForRetry(ctx context.Context, id, lastErr string, now time.Time) (bool, error) {
	tag, err := s.DB.Exec(ctx, `
		UPDATE messages
		   SET state='queued', last_error=$2, updated_at=$3
		 WHERE id=$1 AND state='processing'
	`, id, nullIfEmpty(lastErr), now)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// CreateMessage accepts (or rejects) one send request in a SINGLE round-trip,
// replacing a 7-statement sequence — idempotency SELECT, message INSERT,
// suppression SELECT, consent SELECT, then BEGIN + cap upsert + COMMIT — that
// grew to 9 on the cap-exceeded path, because it incremented the counter first
// and then wrote a compensating decrement plus a state UPDATE.
//
// Structure, and why each piece is where it is:
//
//	gate     — suppression and consent, read once.
//	cap      — increments only when the gates pass AND this idempotency key is
//	           not already known, so an idempotent retry cannot consume a second
//	           unit of the recipient's daily allowance. The increment is
//	           CONDITIONAL (DO UPDATE ... WHERE count < max), so it never
//	           overshoots and never needs the compensating decrement the old
//	           transaction relied on. No row returned means the cap is spent.
//	decision — collapses the gates and the cap outcome into the final state, so
//	           the message is inserted with the state it ends in rather than
//	           inserted as 'queued' and then UPDATEd to 'suppressed'.
//	ins      — ON CONFLICT DO UPDATE with a no-op assignment. The update is
//	           what makes this correct under concurrency: DO NOTHING neither
//	           returns a row nor waits, so a caller that lost the race got an
//	           empty result AND could not read the winner's row, because its
//	           statement snapshot predates that commit. Both branches came back
//	           empty and the caller saw "no rows in result set" for a request
//	           that had in fact succeeded. DO UPDATE takes the row lock, waits
//	           for the other transaction, and returns the row either way.
//
// xmax distinguishes the two cases: it is 0 on a row this statement inserted
// and non-zero on one it locked, which is how a fresh accept is told apart from
// an idempotent retry without a second query.
func (s *Store) CreateMessage(ctx context.Context, in store.CreateMessageInput) (store.CreateMessageResult, error) {
	b, _ := json.Marshal(in.Vars)
	day := in.Day.UTC().Truncate(24 * time.Hour)

	var out store.CreateMessageResult
	var inserted bool
	row := s.DB.QueryRow(ctx, `
		WITH gate AS (
			SELECT
				EXISTS(SELECT 1 FROM suppression_list WHERE tenant_id=$2 AND phone=$4) AS suppressed,
				EXISTS(SELECT 1 FROM consents
				        WHERE tenant_id=$2 AND phone=$4 AND channel='sms' AND status='opted_in') AS opted_in
		), cap AS (
			INSERT INTO send_caps_daily AS s (tenant_id, phone, day, count, updated_at)
			SELECT $2,$4,$8,1,$10 FROM gate
			 WHERE NOT gate.suppressed AND gate.opted_in AND $9::int >= 1
			   AND NOT EXISTS (SELECT 1 FROM messages WHERE tenant_id=$2 AND idempotency_key=$3)
			ON CONFLICT (tenant_id, phone, day) DO UPDATE
				SET count = s.count + 1, updated_at = $10
				WHERE s.count < $9::int
			RETURNING count
		), decision AS (
			SELECT CASE
				WHEN (SELECT suppressed FROM gate)     THEN 'suppressed'
				WHEN NOT (SELECT opted_in FROM gate)   THEN 'suppressed'
				WHEN NOT EXISTS (SELECT 1 FROM cap)    THEN 'suppressed'
				ELSE 'queued' END AS state,
			CASE
				WHEN (SELECT suppressed FROM gate)     THEN 'suppressed'
				WHEN NOT (SELECT opted_in FROM gate)   THEN 'not_opted_in'
				WHEN NOT EXISTS (SELECT 1 FROM cap)    THEN 'cap_exceeded'
				ELSE '' END AS last_error
		), ins AS (
			INSERT INTO messages
				(id, tenant_id, idempotency_key, to_phone, template_id, vars_json, campaign_id,
				 state, last_error, created_at, updated_at)
			SELECT $1,$2,$3,$4,$5,$6,$7, d.state, NULLIF(d.last_error,''), $10,$10 FROM decision d
			ON CONFLICT (tenant_id, idempotency_key) DO UPDATE
				SET updated_at = messages.updated_at
			RETURNING id, state, COALESCE(last_error,'') AS last_error, (xmax = 0) AS inserted
		)
		SELECT id, state, last_error, inserted FROM ins
	`, in.ID, in.TenantID, in.IdemKey, in.To, in.TemplateID, b, nullIfEmpty(in.CampaignID),
		day, in.MaxPerDay, in.Now)

	if err := row.Scan(&out.MessageID, &out.State, &out.LastError, &inserted); err != nil {
		return store.CreateMessageResult{}, err
	}
	out.Existing = !inserted
	return out, nil
}

// RecordDeliveryEvent applies one provider callback in a SINGLE round-trip:
// the delivery event is inserted, and the message is advanced in the same
// statement when the event is terminal. Returns whether a message row matched.
//
// Three properties this shape buys, each of which was a defect in the previous
// insert-then-retry-loop version:
//
//   - The event is persisted UNCONDITIONALLY, even when no message matches yet
//     (the callback can beat the worker's provider_msg_id write). The reconcile
//     job reads delivery_events, so an event it never sees is an event it can
//     never repair — persisting first is what keeps the compensator able to
//     compensate.
//   - The UPDATE carries a state guard, so a duplicate or out-of-order terminal
//     callback cannot overwrite a message that already reached a terminal state.
//     First terminal wins here; reconcile re-asserts latest-wins for anything
//     still sitting in 'submitted'.
//   - One round-trip, holding a pooled connection for the length of one
//     statement. The previous version retried the UPDATE up to 10 times with
//     backoff sleeps — up to ~1.4s — while holding its connection, which
//     exhausted the pool under load and surfaced as database timeouts.
func (s *Store) RecordDeliveryEvent(ctx context.Context, in store.DeliveryEventRecord) (matched bool, err error) {
	b, _ := json.Marshal(in.Payload)
	var updated int
	row := s.DB.QueryRow(ctx, `
		WITH ev AS (
			INSERT INTO delivery_events
				(provider, provider_msg_id, vendor_status, error_code, payload_json, occurred_at)
			VALUES ($1,$2,$3,$4,$5,$6)
			RETURNING provider, provider_msg_id
		), upd AS (
			UPDATE messages m
			   SET state = $7, last_error = $4, updated_at = $8
			  FROM ev
			 WHERE $7::text <> ''
			   AND m.provider = ev.provider
			   AND m.provider_msg_id = ev.provider_msg_id
			   AND m.state NOT IN ('delivered','failed')
			RETURNING 1
		)
		SELECT count(*)::int FROM upd
	`, in.Provider, in.ProviderMsgID, in.VendorStatus, nullIfEmpty(in.ErrorCode), b, in.OccurredAt,
		in.NewState, in.Now)
	if err := row.Scan(&updated); err != nil {
		return false, err
	}
	return updated > 0, nil
}

func (s *Store) GetMessage(ctx context.Context, msgID string) (store.Message, bool, error) {
	var m store.Message
	row := s.DB.QueryRow(ctx, `
		SELECT id, tenant_id, to_phone, template_id, COALESCE(campaign_id,''), state,
		       COALESCE(provider,''), COALESCE(provider_msg_id,''), COALESCE(last_error,''),
		       created_at, updated_at
		FROM messages WHERE id=$1
	`, msgID)

	err := row.Scan(&m.ID, &m.TenantID, &m.ToPhone, &m.TemplateID, &m.CampaignID, &m.State,
		&m.Provider, &m.ProviderMsgID, &m.LastError, &m.CreatedAt, &m.UpdatedAt)

	if err != nil {
		if err.Error() == "no rows in result set" {
			return store.Message{}, false, nil
		}
		return store.Message{}, false, err
	}
	return m, true, nil
}

// ClaimAndLoad moves a message into processing state and returns it, in one
// round-trip, replacing a SELECT followed by a conditional UPDATE.
//
// The two CTEs read the same snapshot, so `cur` sees the message as it was
// before the claim — which is the state a caller wants for logging, since after
// the update every claimed message reads 'processing'.
//
// found=false means no such message. That is deliberately distinct from
// Claimed=false (the message exists but is terminal, already submitted, or held
// by another worker whose claim has not gone stale): the first is a job
// referencing a row that does not exist and should be surfaced, the second is
// the ordinary duplicate-delivery case and is simply skipped.
func (s *Store) ClaimAndLoad(ctx context.Context, msgID string, now time.Time, staleAfter time.Duration) (store.ClaimedMessage, bool, error) {
	staleBefore := now.Add(-staleAfter)
	var out store.ClaimedMessage
	var varsJSON []byte
	err := s.DB.QueryRow(ctx, `
		WITH cur AS (
			SELECT tenant_id, to_phone, template_id, COALESCE(campaign_id,'') AS campaign_id,
			       state, COALESCE(provider_msg_id,'') AS provider_msg_id, vars_json, created_at
			  FROM messages WHERE id=$1
		), claim AS (
			UPDATE messages
			   SET state='processing', updated_at=$2
			 WHERE id=$1
			   AND (state='queued' OR (state='processing' AND updated_at < $3))
			RETURNING 1
		)
		SELECT cur.tenant_id, cur.to_phone, cur.template_id, cur.campaign_id,
		       cur.state, cur.provider_msg_id, cur.vars_json, cur.created_at,
		       EXISTS(SELECT 1 FROM claim) AS claimed
		  FROM cur
	`, msgID, now, staleBefore).Scan(
		&out.TenantID, &out.To, &out.TemplateID, &out.CampaignID,
		&out.State, &out.ProviderMsgID, &varsJSON, &out.CreatedAt, &out.Claimed)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.ClaimedMessage{}, false, nil
		}
		return store.ClaimedMessage{}, false, err
	}
	_ = json.Unmarshal(varsJSON, &out.Vars)
	return out, true, nil
}

// RecordAttempt writes one provider attempt and, when the attempt decided the
// message's fate, the resulting state change — in a single statement, replacing
// an INSERT followed by an UPDATE.
//
// A transient failure that will be retried passes no transition, so only the
// attempt row is written; $9 gates the UPDATE so the same statement serves both
// shapes. Provider and provider_msg_id are written only when non-empty, so a
// later failed attempt cannot erase the id an earlier successful submit
// recorded.
func (s *Store) RecordAttempt(ctx context.Context, in store.AttemptRecord) error {
	reqB, _ := json.Marshal(in.Attempt.RequestJSON)
	respB, _ := json.Marshal(in.Attempt.ResponseJSON)

	t := in.Transition
	if t == nil {
		t = &store.MessageTransition{}
	}
	_, err := s.DB.Exec(ctx, `
		WITH att AS (
			INSERT INTO provider_attempts
				(message_id, provider, provider_msg_id, http_status, error_code, error_msg, request_json, response_json)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		)
		UPDATE messages
		   SET state = $10,
		       provider = COALESCE(NULLIF($11,''), provider),
		       provider_msg_id = COALESCE(NULLIF($12,''), provider_msg_id),
		       last_error = NULLIF($13,''),
		       updated_at = $14
		 WHERE id = $1 AND $9::bool
	`,
		in.Attempt.MessageID, in.Attempt.Provider, nullIfEmpty(in.Attempt.ProviderMsgID),
		in.Attempt.HTTPStatus, nullIfEmpty(in.Attempt.ErrorCode), nullIfEmpty(in.Attempt.ErrorMsg),
		reqB, respB,
		in.Transition != nil, t.State, t.Provider, t.ProviderMsgID, t.LastError, t.Now)
	return err
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
