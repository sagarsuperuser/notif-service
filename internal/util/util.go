package util

import (
	"crypto/rand"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

func NormalizePhone(p string) string {
	// keep it simple for MVP
	// TODO -  may use libphonenumber
	return strings.ReplaceAll(strings.TrimSpace(p), " ", "")
}

// Very simple {var} replacement. Good for MVP. Keep templateId -> templateBody mapping in DB/config.
func RenderTemplate(body string, vars map[string]string) string {
	out := body
	for k, v := range vars {
		out = strings.ReplaceAll(out, "{"+k+"}", v)
	}
	return out
}

func NewMessageID() string {
	// ULID is sortable (nice for DB indexes and dashboards)
	t := time.Now().UTC()
	return "msg_" + ulid.MustNew(ulid.Timestamp(t), rand.Reader).String()
}

func NowUTC() time.Time {
	return time.Now().UTC()
}
