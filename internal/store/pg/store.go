package pg

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	DB *pgxpool.Pool
}

type Message struct {
	ID            string
	TenantID      string
	ToPhone       string
	TemplateID    string
	CampaignID    string
	State         string
	Provider      string
	ProviderMsgID string
	LastError     string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

func New(db *pgxpool.Pool) *Store { return &Store{DB: db} }

func (s *Store) FindMessageByIdempotency(ctx context.Context, tenantID, idemKey string) (msgID string, state string, found bool, err error) {
	row := s.DB.QueryRow(ctx, `
		SELECT id, state FROM messages WHERE tenant_id=$1 AND idempotency_key=$2
	`, tenantID, idemKey)
	err = row.Scan(&msgID, &state)
	if err != nil {
		// pgx returns error; check no rows by string to keep minimal
		if err.Error() == "no rows in result set" {
			return "", "", false, nil
		}
		return "", "", false, err
	}
	return msgID, state, true, nil
}

func (s *Store) InsertMessage(ctx context.Context, msgID, tenantID, idemKey, to, templateID string, vars map[string]string, campaignID string, state string, now time.Time) error {
	b, _ := json.Marshal(vars)
	_, err := s.DB.Exec(ctx, `
		INSERT INTO messages (id, tenant_id, idempotency_key, to_phone, template_id, vars_json, campaign_id, state, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$9)
	`, msgID, tenantID, idemKey, to, templateID, b, nullIfEmpty(campaignID), state, now)
	return err
}

func (s *Store) MarkMessageState(ctx context.Context, msgID, state, lastError string, now time.Time) error {
	_, err := s.DB.Exec(ctx, `
		UPDATE messages SET state=$2, last_error=$3, updated_at=$4 WHERE id=$1
	`, msgID, state, nullIfEmpty(lastError), now)
	return err
}

func (s *Store) SetProviderDetails(ctx context.Context, msgID, provider, providerMsgID, state string, now time.Time) error {
	_, err := s.DB.Exec(ctx, `
		UPDATE messages SET provider=$2, provider_msg_id=$3, state=$4, updated_at=$5 WHERE id=$1
	`, msgID, provider, providerMsgID, state, now)
	return err
}

func (s *Store) GetMessageForWorker(ctx context.Context, msgID string) (tenantID, to, templateID, campaignID, state, providerMsgID string, vars map[string]string, err error) {
	var varsJSON []byte
	row := s.DB.QueryRow(ctx, `
		SELECT tenant_id, to_phone, template_id, COALESCE(campaign_id,''), state, COALESCE(provider_msg_id,''), vars_json
		FROM messages WHERE id=$1
	`, msgID)
	err = row.Scan(&tenantID, &to, &templateID, &campaignID, &state, &providerMsgID, &varsJSON)
	if err != nil {
		return
	}
	_ = json.Unmarshal(varsJSON, &vars)
	return
}

func (s *Store) InsertAttempt(ctx context.Context, msgID, provider, providerMsgID string, httpStatus int, errCode, errMsg string, reqJSON, respJSON any) error {
	reqB, _ := json.Marshal(reqJSON)
	respB, _ := json.Marshal(respJSON)
	_, err := s.DB.Exec(ctx, `
		INSERT INTO provider_attempts (message_id, provider, provider_msg_id, http_status, error_code, error_msg, request_json, response_json)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
	`, msgID, provider, nullIfEmpty(providerMsgID), httpStatus, nullIfEmpty(errCode), nullIfEmpty(errMsg), reqB, respB)
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
		// default deny for marketing
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

	// Upsert and increment atomically
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
		// roll back increment if over cap (so you don’t burn allowance)
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

func (s *Store) InsertDeliveryEvent(ctx context.Context, provider, providerMsgID, vendorStatus, errorCode string, payload any, occurredAt *time.Time) error {
	b, _ := json.Marshal(payload)
	_, err := s.DB.Exec(ctx, `
		INSERT INTO delivery_events (provider, provider_msg_id, vendor_status, error_code, payload_json, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6)
	`, provider, providerMsgID, vendorStatus, nullIfEmpty(errorCode), b, occurredAt)
	return err
}

func (s *Store) UpdateMessageByProviderMsgID(ctx context.Context, provider, providerMsgID string, newState string, lastError string, now time.Time) (bool, error) {
	ct, err := s.DB.Exec(ctx, `
		UPDATE messages
		SET state=$3, last_error=$4, updated_at=$5
		WHERE provider=$1 AND provider_msg_id=$2
	`, provider, providerMsgID, newState, nullIfEmpty(lastError), now)
	if err != nil {
		return false, err
	}
	return ct.RowsAffected() > 0, nil
}

func (s *Store) GetMessage(ctx context.Context, msgID string) (any, bool, error) {
	var m Message
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
			return Message{}, false, nil
		}
		return Message{}, false, err
	}
	return m, true, nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
