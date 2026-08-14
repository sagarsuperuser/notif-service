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

type MessageStateUpdate struct {
	ID        string
	State     string
	LastError string
	Now       time.Time
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

// ClaimedMessage is the result of claiming a message and loading it in one
// round-trip. Claimed reports whether this caller won the claim; when it is
// false the message is still returned, so a caller can log or count why it was
// skipped without a second query.
type ClaimedMessage struct {
	MessageForWorker
	Claimed bool
}

// MessageTransition is a state change applied to the message row in the same
// statement as the attempt that caused it. Provider and ProviderMsgID are only
// written when non-empty, so a failed attempt cannot erase the provider id a
// previous successful submit recorded.
type MessageTransition struct {
	State         string
	Provider      string
	ProviderMsgID string
	LastError     string
	Now           time.Time
}

// AttemptRecord is one provider attempt plus, optionally, the message-state
// change it implies — written together so the pair costs one round-trip and
// cannot half-apply.
type AttemptRecord struct {
	Attempt    ProviderAttempt
	Transition *MessageTransition
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

// CreateMessageInput is one accept-a-message decision: the suppression and
// consent gates, the daily-cap increment, the idempotency check and the message
// insert, resolved in a single round-trip.
type CreateMessageInput struct {
	ID         string
	TenantID   string
	IdemKey    string
	To         string
	TemplateID string
	Vars       map[string]string
	CampaignID string
	Day        time.Time
	MaxPerDay  int
	Now        time.Time
}

// CreateMessageResult reports the row that now represents this request —
// either the one just created, or the pre-existing one an idempotent retry
// resolved to (Existing=true).
type CreateMessageResult struct {
	MessageID string
	State     string
	LastError string
	Existing  bool
}

// DeliveryEventRecord is one provider callback, applied in a single round-trip:
// the event is always persisted, and the message row is advanced only when the
// event is terminal (NewState != "").
type DeliveryEventRecord struct {
	Provider      string
	ProviderMsgID string
	VendorStatus  string
	ErrorCode     string
	Payload       any
	OccurredAt    *time.Time
	// NewState is the message state this event implies: "delivered", "failed",
	// or "" for a non-terminal status (queued/sent), which records the event
	// without touching the message.
	NewState string
	Now      time.Time
}
