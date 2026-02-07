package store

import "time"

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

type IdempotencyResult struct {
	MessageID string
	State     string
	Found     bool
}

type MessageInsert struct {
	ID         string
	TenantID   string
	IdemKey    string
	To         string
	TemplateID string
	Vars       map[string]string
	CampaignID string
	State      string
	Now        time.Time
}

type MessageStateUpdate struct {
	ID        string
	State     string
	LastError string
	Now       time.Time
}

type ProviderDetailsUpdate struct {
	ID            string
	Provider      string
	ProviderMsgID string
	State         string
	Now           time.Time
}

type MessageForWorker struct {
	TenantID      string
	To            string
	TemplateID    string
	CampaignID    string
	State         string
	ProviderMsgID string
	Vars          map[string]string
	CreatedAt     time.Time
}

type ProviderAttempt struct {
	MessageID     string
	Provider      string
	ProviderMsgID string
	HTTPStatus    int
	ErrorCode     string
	ErrorMsg      string
	RequestJSON   any
	ResponseJSON  any
}

type DeliveryEvent struct {
	Provider      string
	ProviderMsgID string
	VendorStatus  string
	ErrorCode     string
	Payload       any
	OccurredAt    *time.Time
}

type ProviderMsgUpdate struct {
	Provider      string
	ProviderMsgID string
	NewState      string
	LastError     string
	Now           time.Time
}
