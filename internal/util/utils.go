package util

import (
	"crypto/rand"
	"time"

	"github.com/oklog/ulid/v2"
)

func NewMessageID() string {
	// ULID is sortable (nice for DB indexes and dashboards)
	t := time.Now().UTC()
	return "msg_" + ulid.MustNew(ulid.Timestamp(t), rand.Reader).String()
}
