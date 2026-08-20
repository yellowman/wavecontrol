package udebug

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
)

const redactedValue = "[REDACTED]"

var (
	jsonSecretPattern = regexp.MustCompile(`(?i)("(?:password|passwd|pass|pwd|token|access[_-]?token|refresh[_-]?token|authorization|cookie|secret|client[_-]?secret|private[_-]?key|api[_-]?key|session|csrf|x-auth-token)"\s*:\s*)"(?:\\.|[^"\\])*"`)
	formSecretPattern = regexp.MustCompile(`(?i)(^|[&;\s])((?:password|passwd|pass|pwd|token|access[_-]?token|refresh[_-]?token|authorization|cookie|secret|client[_-]?secret|private[_-]?key|api[_-]?key|session|csrf|x-auth-token)=)([^&;\s]*)`)
	bearerPattern     = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]+`)
)

// sanitizeEntry converts a debug entry to generic JSON and recursively redacts
// credentials. Applying this at the manager boundary protects every producer,
// including internal/manual debug records that do not pass through Transport.
func sanitizeEntry(entry any) any {
	b, err := json.Marshal(entry)
	if err != nil {
		return map[string]any{"type": "redaction_error", "error": "debug entry could not be serialized"}
	}
	var value any
	if err := json.Unmarshal(b, &value); err != nil {
		return map[string]any{"type": "redaction_error", "error": "debug entry could not be decoded"}
	}
	return redactValue("", value)
}

func redactValue(key string, value any) any {
	if isSensitiveKey(key) {
		return redactedValue
	}
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, child := range v {
			out[k] = redactValue(k, child)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, child := range v {
			out[i] = redactValue(key, child)
		}
		return out
	case string:
		if strings.EqualFold(strings.TrimSpace(key), "url") {
			return redactURL(v)
		}
		return redactText(v)
	default:
		return value
	}
}

func normalizeSecretKey(key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	return strings.NewReplacer("-", "", "_", "", ".", "", " ", "").Replace(key)
}

func isSensitiveKey(key string) bool {
	switch normalizeSecretKey(key) {
	case "password", "passwd", "pass", "pwd",
		"token", "authtoken", "xauthtoken", "accesstoken", "refreshtoken", "idtoken",
		"authorization", "proxyauthorization", "cookie", "setcookie",
		"secret", "clientsecret", "privatekey", "secretkey", "signingkey",
		"apikey", "session", "sessionid", "csrf", "csrftoken", "xcsrftoken":
		return true
	default:
		return false
	}
}

func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return redactText(raw)
	}
	q := u.Query()
	changed := false
	for key := range q {
		if isSensitiveKey(key) {
			q.Set(key, redactedValue)
			changed = true
		}
	}
	if changed {
		u.RawQuery = q.Encode()
	}
	// URL userinfo can contain a password even when no sensitive query exists.
	if u.User != nil {
		if _, hasPassword := u.User.Password(); hasPassword {
			u.User = url.UserPassword(u.User.Username(), redactedValue)
		}
	}
	return redactText(u.String())
}

func redactText(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return text
	}

	// Fully parse JSON when possible so nested secrets and non-string values are
	// handled correctly. Truncated/malformed payloads fall through to regexes.
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		var value any
		if json.Unmarshal([]byte(trimmed), &value) == nil {
			if b, err := json.Marshal(redactValue("", value)); err == nil {
				return string(b)
			}
		}
	}

	out := jsonSecretPattern.ReplaceAllString(text, `${1}"`+redactedValue+`"`)
	out = formSecretPattern.ReplaceAllString(out, `${1}${2}`+redactedValue)
	out = bearerPattern.ReplaceAllString(out, `${1} `+redactedValue)
	return out
}
