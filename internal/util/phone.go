package util

import "strings"

func NormalizePhone(p string) string {
	// keep it simple for MVP
	// TODO -  may use libphonenumber
	return strings.ReplaceAll(strings.TrimSpace(p), " ", "")
}
