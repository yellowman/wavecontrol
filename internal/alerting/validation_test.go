package alerting

import (
	"math"
	"net"
	"testing"
)

func validTestRule() Rule {
	return Rule{
		Name:             "offline AP",
		Enabled:          true,
		Scope:            "all",
		TargetRole:       "ap",
		RequireAlertable: true,
		Metric:           MetricOfflineDuration,
		Operator:         OpGTE,
		Threshold:        120,
		DurationSeconds:  30,
		CooldownSeconds:  300,
		NotifyChannels:   []string{"email"},
		NotifyEmails:     []string{"ops@example.com"},
	}
}

func TestValidateRule(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		rule := validTestRule()
		if err := ValidateRule(&rule); err != nil {
			t.Fatalf("ValidateRule() error = %v", err)
		}
	})

	tests := []struct {
		name   string
		mutate func(*Rule)
	}{
		{"unknown metric", func(r *Rule) { r.Metric = "signal_typo" }},
		{"unknown operator", func(r *Rule) { r.Operator = "approximately" }},
		{"invalid target", func(r *Rule) { r.TargetRole = "client" }},
		{"site without id", func(r *Rule) { r.Scope = "site" }},
		{"all with id", func(r *Rule) { id := 1; r.ScopeID = &id }},
		{"negative duration", func(r *Rule) { r.DurationSeconds = -1 }},
		{"nonfinite threshold", func(r *Rule) { r.Threshold = math.NaN() }},
		{"unsupported channel", func(r *Rule) { r.NotifyChannels = []string{"mobile"} }},
		{"email without recipient", func(r *Rule) { r.NotifyEmails = nil }},
		{"invalid email", func(r *Rule) { r.NotifyEmails = []string{"not-an-address"} }},
		{"private webhook", func(r *Rule) {
			r.NotifyChannels = []string{"webhook"}
			r.NotifyEmails = nil
			r.WebhookURL = "http://127.0.0.1/hook"
		}},
		{"credentialed webhook", func(r *Rule) {
			r.NotifyChannels = []string{"webhook"}
			r.NotifyEmails = nil
			r.WebhookURL = "https://user:pass@example.com/hook"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rule := validTestRule()
			tt.mutate(&rule)
			if err := ValidateRule(&rule); err == nil {
				t.Fatal("ValidateRule() unexpectedly succeeded")
			}
		})
	}
}

func TestEvaluateCondition(t *testing.T) {
	tests := []struct {
		op        string
		value     float64
		threshold float64
		want      bool
	}{
		{OpLT, 1, 2, true}, {OpLTE, 2, 2, true}, {OpGT, 3, 2, true},
		{OpGTE, 2, 2, true}, {OpEQ, 2, 2, true}, {OpNE, 1, 2, true},
		{"invalid", 1, 2, false},
	}
	for _, tt := range tests {
		if got := evaluateCondition(tt.op, tt.value, tt.threshold); got != tt.want {
			t.Errorf("evaluateCondition(%q, %v, %v) = %v, want %v", tt.op, tt.value, tt.threshold, got, tt.want)
		}
	}
}

func TestUnsafeWebhookIP(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "192.168.1.1", "::1", "fc00::1", "2001:db8::1"} {
		if !unsafeWebhookIP(net.ParseIP(raw)) {
			t.Errorf("unsafeWebhookIP(%s) = false", raw)
		}
	}
	for _, raw := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if unsafeWebhookIP(net.ParseIP(raw)) {
			t.Errorf("unsafeWebhookIP(%s) = true", raw)
		}
	}
}
