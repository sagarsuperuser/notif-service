package util

import "strings"

// Very simple {var} replacement. Good for MVP. Keep templateId -> templateBody mapping in DB/config.
func RenderTemplate(body string, vars map[string]string) string {
	out := body
	for k, v := range vars {
		out = strings.ReplaceAll(out, "{"+k+"}", v)
	}
	return out
}
