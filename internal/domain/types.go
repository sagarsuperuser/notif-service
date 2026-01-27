package domain

type MessageState string

const (
	StateQueued     MessageState = "queued"
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

type CreateResponse struct {
	MessageID string `json:"messageId"`
	State     string `json:"state"`
}
