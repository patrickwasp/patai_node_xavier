package diag

import (
	"regexp"
	"strings"
)

// Nothing of the console's logs a credential. These are for what other programs print, and what people type.
var redactions = []struct {
	pattern *regexp.Regexp
	with    string
}{
	{regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----(?s:.*?)(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|$)`), "[private key removed]"},
	// A claim or recovery code, as the console makes them, with its dashes or without.
	{regexp.MustCompile(`\b[A-Z2-7]{5}-?[A-Z2-7]{5}-?[A-Z2-7]{5}-?[A-Z2-7]{5}\b`), "[code removed]"},
	{regexp.MustCompile(`\bpatai_[A-Za-z0-9_-]{16,}`), "patai_[removed]"},
	{regexp.MustCompile(`\$argon2(?:id|i|d)\$[^\s"']+`), "[hash removed]"},
	{regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^/\s:@]+:[^/\s@]*@`), "${1}[removed]@"},
	{regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`), "${1} [removed]"},
	{regexp.MustCompile(`(?i)\b(password|passwd|passphrase)(["']?\s*[:=]\s*["']?)[^\s"',;&]+`), "${1}${2}[removed]"},
	// A token or key is long. A short value is a count or an id: "control-looking token: 2".
	{regexp.MustCompile(`(?i)\b(secret|token|api[_-]?key|access[_-]?key)(["']?\s*[:=]\s*["']?)[^\s"',;&]{8,}`), "${1}${2}[removed]"},
}

// Redact takes credentials out of a line. codes are claim codes the unit holds now, which are taken out
// however they were typed.
func Redact(s string, codes []string) string {
	for _, code := range codes {
		if pattern := codePattern(code); pattern != nil {
			s = pattern.ReplaceAllString(s, "[code removed]")
		}
	}
	for _, r := range redactions {
		s = r.pattern.ReplaceAllString(s, r.with)
	}
	return s
}

// codePattern matches a code in any case, with or without its separators.
func codePattern(code string) *regexp.Regexp {
	letters := strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, code)
	if len(letters) < 12 {
		return nil
	}
	parts := make([]string, len(letters))
	for i, r := range letters {
		parts[i] = regexp.QuoteMeta(string(r))
	}
	return regexp.MustCompile(`(?i)` + strings.Join(parts, `[-\s]?`))
}
