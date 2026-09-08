package safeurl

import (
	"net/url"
	"regexp"
	"strings"
)

var urlPattern = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s'"<>]+`)

// Redact hides URL userinfo while leaving non-URL and credential-free values
// unchanged.
func Redact(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return raw
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.User == nil {
		return raw
	}
	parsed.User = url.User("redacted")
	return parsed.String()
}

func RedactText(text string) string {
	return urlPattern.ReplaceAllStringFunc(text, Redact)
}
