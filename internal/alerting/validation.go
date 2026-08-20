package alerting

import (
	"fmt"
	"net/mail"
	"strings"
)

var validMetrics = map[string]struct{}{
	MetricSignal60GHz: {}, MetricSignal5GHz: {}, MetricSignalLTU: {}, MetricCPU: {},
	MetricTemperature: {}, MetricRAM: {}, MetricOfflineDuration: {}, MetricCapacity: {},
	MetricPeerCount: {}, MetricLinkScore: {},
}

var validOperators = map[string]struct{}{OpLT: {}, OpLTE: {}, OpGT: {}, OpGTE: {}, OpEQ: {}, OpNE: {}}

func ValidateRule(rule *Rule) error {
	if rule == nil {
		return fmt.Errorf("rule is required")
	}
	normalizeRule(rule)
	if rule.Name == "" || len([]rune(rule.Name)) > 100 {
		return fmt.Errorf("name is required and must be at most 100 characters")
	}
	if !finite(rule.Threshold) {
		return fmt.Errorf("threshold must be finite")
	}
	switch rule.Scope {
	case "all":
		if rule.ScopeID != nil {
			return fmt.Errorf("scope_id must be omitted for all scope")
		}
	case "site", "device":
		if rule.ScopeID == nil || *rule.ScopeID <= 0 {
			return fmt.Errorf("positive scope_id is required for %s scope", rule.Scope)
		}
	default:
		return fmt.Errorf("scope must be all, site, or device")
	}
	if rule.TargetRole != "all" && rule.TargetRole != "ap" && rule.TargetRole != "sta" {
		return fmt.Errorf("target_role must be all, ap, or sta")
	}
	if _, ok := validMetrics[rule.Metric]; !ok {
		return fmt.Errorf("unsupported metric %q", rule.Metric)
	}
	if _, ok := validOperators[rule.Operator]; !ok {
		return fmt.Errorf("unsupported operator %q", rule.Operator)
	}
	const maxSeconds = 31 * 24 * 60 * 60
	if rule.DurationSeconds < 0 || rule.DurationSeconds > maxSeconds {
		return fmt.Errorf("duration_seconds must be between 0 and %d", maxSeconds)
	}
	if rule.CooldownSeconds < 0 || rule.CooldownSeconds > maxSeconds {
		return fmt.Errorf("cooldown_seconds must be between 0 and %d", maxSeconds)
	}

	channels := map[string]bool{}
	for _, ch := range rule.NotifyChannels {
		switch ch {
		case "email", "webhook", "zabbix":
			channels[ch] = true
		default:
			return fmt.Errorf("unsupported notification channel %q", ch)
		}
	}
	if channels["email"] {
		if len(rule.NotifyEmails) == 0 {
			return fmt.Errorf("notify_emails is required for email notifications")
		}
		for _, address := range rule.NotifyEmails {
			parsed, err := mail.ParseAddress(address)
			if err != nil || !strings.EqualFold(parsed.Address, strings.TrimSpace(address)) {
				return fmt.Errorf("invalid notification email %q", address)
			}
		}
	}
	if channels["webhook"] {
		if rule.WebhookURL == "" {
			return fmt.Errorf("webhook_url is required for webhook notifications")
		}
		if err := validateWebhookURL(rule.WebhookURL); err != nil {
			return fmt.Errorf("invalid webhook_url: %w", err)
		}
	} else if rule.WebhookURL != "" {
		// A stored URL with the channel disabled is harmless but still validate it
		// so enabling the channel later cannot activate an SSRF payload.
		if err := validateWebhookURL(rule.WebhookURL); err != nil {
			return fmt.Errorf("invalid webhook_url: %w", err)
		}
	}
	return nil
}
