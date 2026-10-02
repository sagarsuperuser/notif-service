package httpserver

const (
	ErrInvalidJSON      = "invalid json"
	ErrMissingID        = "missing id"
	ErrDependency       = "dependency error"
	ErrNotFound         = "not found"
	ErrBadForm          = "bad form"
	ErrInvalidSignature = "invalid signature"

	ErrBodyTooLarge        = "request body too large"
	ErrIdempotencyConflict = "idempotency key already used for a different request"
)
