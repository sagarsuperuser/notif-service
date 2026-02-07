package pg

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"notif/internal/store"
)

type Store struct {
	DB *pgxpool.Pool
}

func New(db *pgxpool.Pool) *Store { return &Store{DB: db} }

func (s *Store) FindMessageByIdempotency(ctx context.Context, tenantID, idemKey string) (store.IdempotencyResult, error) {
	row := s.DB.QueryRow(ctx, `
		SELECT id, state FROM messages WHERE tenant_id=$1 AND idempotency_key=$2
	`, tenantID, idemKey)
	var msgID, state string
	err := row.Scan(&msgID, &state)
	if err != nil {
		if err.Error() == "no rows in result set" {
			return store.IdempotencyResult{Found: false}, nil
		}
		return store.IdempotencyResult{}, err
	}
	return store.IdempotencyResult{MessageID: msgID, State: state, Found: true}, nil
}

func (s *Store) InsertMessage(ctx context.Context, in store.MessageInsert) error {
	b, _ := json.Marshal(in.Vars)
	_, err := s.DB.Exec(ctx, `
		INSERT INTO messages (id, tenant_id, idempotency_key, to_phone, template_id, vars_json, campaign_id, state, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$9)
	`, in.ID, in.TenantID, in.IdemKey, in.To, in.TemplateID, b, nullIfEmpty(in.CampaignID), in.State, in.Now)
	return err
}

func (s *Store) MarkMessageState(ctx context.Context, in store.MessageStateUpdate) error {
	_, err := s.DB.Exec(ctx, `
		UPDATE messages SET state=$2, last_error=$3, updated_at=$4 WHERE id=$1
	`, in.ID, in.State, nullIfEmpty(in.LastError), in.Now)
	return err
}

func (s *Store) SetProviderDetails(ctx context.Context, in store.ProviderDetailsUpdate) error {
	_, err := s.DB.Exec(ctx, `
		UPDATE messages SET provider=$2, provider_msg_id=$3, state=$4, updated_at=$5 WHERE id=$1
	`, in.ID, in.Provider, in.ProviderMsgID, in.State, in.Now)
	return err
}

func (s *Store) GetMessageForWorker(ctx context.Context, msgID string) (store.MessageForWorker, error) {
	var varsJSON []byte
	row := s.DB.QueryRow(ctx, `
		SELECT tenant_id, to_phone, template_id, COALESCE(campaign_id,''), state, COALESCE(provider_msg_id,''), vars_json, created_at
		FROM messages WHERE id=$1
	`, msgID)
	var out store.MessageForWorker
	err := row.Scan(&out.TenantID, &out.To, &out.TemplateID, &out.CampaignID, &out.State, &out.ProviderMsgID, &varsJSON, &out.CreatedAt)
	if err != nil {
		return store.MessageForWorker{}, err
	}
	_ = json.Unmarshal(varsJSON, &out.Vars)
	return out, nil
}

func (s *Store) InsertAttempt(ctx context.Context, in store.ProviderAttempt) error {
	reqB, _ := json.Marshal(in.RequestJSON)
	respB, _ := json.Marshal(in.ResponseJSON)
	_, err := s.DB.Exec(ctx, `
		INSERT INTO provider_attempts (message_id, provider, provider_msg_id, http_status, error_code, error_msg, request_json, response_json)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
	`, in.MessageID, in.Provider, nullIfEmpty(in.ProviderMsgID), in.HTTPStatus, nullIfEmpty(in.ErrorCode), nullIfEmpty(in.ErrorMsg), reqB, respB)
	return err
}

func (s *Store) IsSuppressed(ctx context.Context, tenantID, phone string) (bool, error) {
	row := s.DB.QueryRow(ctx, `SELECT 1 FROM suppression_list WHERE tenant_id=$1 AND phone=$2`, tenantID, phone)
	var one int
	err := row.Scan(&one)
	if err != nil {
		if err.Error() == "no rows in result set" {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *Store) IsOptedIn(ctx context.Context, tenantID, phone string) (bool, error) {
	row := s.DB.QueryRow(ctx, `
		SELECT status FROM consents WHERE tenant_id=$1 AND phone=$2 AND channel='sms'
	`, tenantID, phone)
	var st string
	err := row.Scan(&st)
	if err != nil {
		if err.Error() == "no rows in result set" {
			return false, nil
		}
		return false, err
	}
	return st == "opted_in", nil
}

func (s *Store) IncrementDailyCap(ctx context.Context, tenantID, phone string, day time.Time, maxPerDay int) (allowed bool, newCount int, err error) {
	d := day.UTC().Truncate(24 * time.Hour)
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return false, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	row := tx.QueryRow(ctx, `
		INSERT INTO send_caps_daily (tenant_id, phone, day, count, updated_at)
		VALUES ($1,$2,$3,1,now())
		ON CONFLICT (tenant_id, phone, day)
		DO UPDATE SET count = send_caps_daily.count + 1, updated_at=now()
		RETURNING count
	`, tenantID, phone, d)
	if err := row.Scan(&newCount); err != nil {
		return false, 0, err
	}

	allowed = newCount <= maxPerDay
	if !allowed {
		_, _ = tx.Exec(ctx, `
			UPDATE send_caps_daily SET count = count - 1, updated_at=now()
			WHERE tenant_id=$1 AND phone=$2 AND day=$3
		`, tenantID, phone, d)
		if err := tx.Commit(ctx); err != nil {
			return false, 0, err
		}
		return false, newCount - 1, nil
	}

	if err := tx.Commit(ctx); err != nil {
		return false, 0, err
	}
	return true, newCount, nil
}

func (s *Store) InsertDeliveryEvent(ctx context.Context, in store.DeliveryEvent) error {
	b, _ := json.Marshal(in.Payload)
	_, err := s.DB.Exec(ctx, `
		INSERT INTO delivery_events (provider, provider_msg_id, vendor_status, error_code, payload_json, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6)
	`, in.Provider, in.ProviderMsgID, in.VendorStatus, nullIfEmpty(in.ErrorCode), b, in.OccurredAt)
	return err
}

func (s *Store) UpdateMessageByProviderMsgID(ctx context.Context, in store.ProviderMsgUpdate) (bool, error) {
	ct, err := s.DB.Exec(ctx, `
		UPDATE messages
		SET state=$3, last_error=$4, updated_at=$5
		WHERE provider=$1 AND provider_msg_id=$2
	`, in.Provider, in.ProviderMsgID, in.NewState, nullIfEmpty(in.LastError), in.Now)
	if err != nil {
		return false, err
	}
	return ct.RowsAffected() > 0, nil
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

// ClaimMessage attempts to move a message into processing state.
// It allows reclaiming if the message is still "processing" but stale.
func (s *Store) ClaimMessage(ctx context.Context, msgID string, now time.Time, staleAfter time.Duration) (bool, error) {
	staleBefore := now.Add(-staleAfter)
	ct, err := s.DB.Exec(ctx, `
		UPDATE messages
		SET state=$2, updated_at=$3
		WHERE id=$1 AND (state='queued' OR (state='processing' AND updated_at < $4))
	`, msgID, "processing", now, staleBefore)
	if err != nil {
		return false, err
	}
	return ct.RowsAffected() > 0, nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
