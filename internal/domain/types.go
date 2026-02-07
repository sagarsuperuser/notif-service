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

func (r SendSMSRequest) Validate() error {
	if r.TenantID == "" || r.IdempotencyKey == "" || r.To == "" || r.TemplateID == "" {
		return ErrMissingFields
	}
	return nil
}

var ErrMissingFields = errors.New("missing required fields")

type CreateResponse struct {
	MessageID string `json:"messageId"`
	State     string `json:"state"`
}
