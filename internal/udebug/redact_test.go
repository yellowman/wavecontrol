package udebug

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSanitizeEntryRedactsHeadersURLAndBodies(t *testing.T) {
	entry := map[string]any{
		"query": map[string]any{
			"url": "https://radio.local/login?token=abc&safe=yes",
			"headers": map[string]any{
				"Authorization": []any{"Bearer top-secret"},
				"X-Auth-Token":  []any{"device-token"},
				"Accept":        []any{"application/json"},
			},
			"body": map[string]any{
				"username": "ubnt",
				"password": "radio-password",
				"nested":   map[string]any{"client_secret": "oauth-secret"},
			},
		},
		"response": map[string]any{
			"headers": map[string]any{"Set-Cookie": []any{"session=secret"}},
			"body":    `password=legacy&mode=ap`,
		},
	}

	b, err := json.Marshal(sanitizeEntry(entry))
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, secret := range []string{"abc", "top-secret", "device-token", "radio-password", "oauth-secret", "session=secret", "legacy"} {
		if strings.Contains(got, secret) {
			t.Fatalf("sanitized entry still contains %q: %s", secret, got)
		}
	}
	if !strings.Contains(got, "safe=yes") || !strings.Contains(got, "ubnt") {
		t.Fatalf("non-secret context was unexpectedly removed: %s", got)
	}
}

func TestRedactTextHandlesMalformedOrTruncatedPayload(t *testing.T) {
	got := redactText(`{"password":"secret","token":"abc`)
	if strings.Contains(got, "secret") {
		t.Fatalf("password was not redacted: %s", got)
	}
}
