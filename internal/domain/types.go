package domain

import "errors"

type MessageState string

const (
	StateQueued     MessageState = "queued"
	StateProcessing MessageState = "processing"
	StateSuppressed MessageState = "suppressed"
	StateSubmitted  MessageState = "submitted"
	StateDelivered  MessageState = "delivered"
	StateFailed     MessageState = "failed"
)

type SendSMSRequest struct {
	TenantID       string            `json:"tenantId"`
	IdempotencyKey string            `json:"idempotencyKey"`
	To             string            `json:"to"`
	TemplateID     string            `json:"templateId"`
	Vars           map[string]string `json:"vars"`
	CampaignID     string            `json:"campaignId,omitempty"`
}

// Field limits. Generous for real use; their job is to keep one request from
// growing a queue message past what SQS accepts or a row past what is sane.
const (
	MaxIDLen     = 128 // tenantId, idempotencyKey, templateId, campaignId
	MaxPhoneLen  = 32
	MaxVars      = 32
	MaxVarKeyLen = 64
	MaxVarValLen = 512
)

func (r SendSMSRequest) Validate() error {
	if r.TenantID == "" || r.IdempotencyKey == "" || r.To == "" || r.TemplateID == "" {
		return ErrMissingFields
	}
	if len(r.TenantID) > MaxIDLen || len(r.IdempotencyKey) > MaxIDLen ||
		len(r.TemplateID) > MaxIDLen || len(r.CampaignID) > MaxIDLen || len(r.To) > MaxPhoneLen {
		return ErrFieldTooLong
	}
	if len(r.Vars) > MaxVars {
		return ErrTooManyVars
	}
	for k, v := range r.Vars {
		if len(k) > MaxVarKeyLen || len(v) > MaxVarValLen {
			return ErrFieldTooLong
		}
	}
	return nil
}

var (
	ErrMissingFields = errors.New("missing required fields")
	ErrFieldTooLong  = errors.New("a field exceeds its maximum length")
	ErrTooManyVars   = errors.New("too many template vars")
)

type CreateResponse struct {
	MessageID string `json:"messageId"`
	State     string `json:"state"`
}
