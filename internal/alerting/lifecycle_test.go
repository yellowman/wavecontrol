package alerting

import (
	"testing"
	"time"

	"github.com/yellowman/wavecontrol/internal/stats"
)

func TestDetermineSeverity(t *testing.T) {
	m := &Manager{}
	tests := []struct {
		name  string
		rule  Rule
		value float64
		want  string
	}{
		{"explicit info", Rule{Metric: MetricOfflineDuration, Severity: SeverityInfo}, 7200, SeverityInfo},
		{"explicit critical", Rule{Metric: MetricSignal60GHz, Severity: SeverityCritical}, -60, SeverityCritical},
		{"auto weak signal critical", Rule{Metric: MetricSignal60GHz, Severity: SeverityAuto}, -81, SeverityCritical},
		{"auto weak signal warning", Rule{Metric: MetricSignal6GHz, Severity: SeverityAuto}, -75, SeverityWarning},
		{"auto offline critical", Rule{Metric: MetricOfflineDuration, Severity: SeverityAuto}, 3601, SeverityCritical},
		{"auto capacity critical", Rule{Metric: MetricCapacity, Severity: SeverityAuto}, 20, SeverityCritical},
		{"auto interference warning", Rule{Metric: MetricInterference, Severity: SeverityAuto}, 30, SeverityWarning},
		{"auto interference critical", Rule{Metric: MetricInterference, Severity: SeverityAuto}, 60, SeverityCritical},
		{"auto chain imbalance warning", Rule{Metric: MetricChainImbalance, Severity: SeverityAuto}, 7, SeverityWarning},
		{"auto chain imbalance critical", Rule{Metric: MetricChainImbalance, Severity: SeverityAuto}, 14, SeverityCritical},
		{"auto GPS sync lost", Rule{Metric: MetricGPSSync, Severity: SeverityAuto}, 0, SeverityCritical},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := m.determineSeverity(tt.rule, tt.value); got != tt.want {
				t.Fatalf("determineSeverity() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSixGHzMetricRequiresSixGHzFrequency(t *testing.T) {
	m := &Manager{}
	ds := &stats.DeviceStats{}
	ds.Wireless.Radio6GHz = &stats.RadioStats{Frequency: 5955, Signal: -73}
	if got, ok := m.getMetricValue(MetricSignal6GHz, ds, deviceAlertPolicy{}, time.Time{}); !ok || got != -73 {
		t.Fatalf("6 GHz metric = (%v,%v), want (-73,true)", got, ok)
	}

	// MLO5 can use Radio6GHz as a compatibility slot for a second 5 GHz
	// interface. That must not appear as a 6 GHz alert metric.
	ds.Wireless.Radio6GHz.Frequency = 5805
	if got, ok := m.getMetricValue(MetricSignal6GHz, ds, deviceAlertPolicy{}, time.Time{}); ok {
		t.Fatalf("5 GHz compatibility slot returned (%v,true) as 6 GHz", got)
	}
}

func TestMaxRadioInterference(t *testing.T) {
	ds := &stats.DeviceStats{}
	ds.Wireless.Radio60GHz = &stats.RadioStats{Utilization: &stats.Utilization{Interference: 12}}
	ds.Wireless.Radio5GHz = &stats.RadioStats{Utilization: &stats.Utilization{Interference: 37.5}}
	ds.Wireless.Radios = []stats.RadioStats{{Utilization: &stats.Utilization{Interference: 22}}}
	if got, ok := maxRadioInterference(ds); !ok || got != 37.5 {
		t.Fatalf("maxRadioInterference() = (%v,%v), want (37.5,true)", got, ok)
	}
	if _, ok := maxRadioInterference(&stats.DeviceStats{}); ok {
		t.Fatal("maxRadioInterference() reported a value with no utilization data")
	}
}

func TestChainImbalanceAndGPSSyncMetrics(t *testing.T) {
	m := &Manager{}
	ds := &stats.DeviceStats{}
	ds.Wireless.Radio5GHz = &stats.RadioStats{SignalPerChain: []int{-61, -70}}
	ds.Wireless.Radio60GHz = &stats.RadioStats{SignalPerChain: []int{-55, 0}}
	if got, ok := m.getMetricValue(MetricChainImbalance, ds, deviceAlertPolicy{}, time.Time{}); !ok || got != 9 {
		t.Fatalf("chain imbalance = (%v,%v), want (9,true)", got, ok)
	}

	ds.Wireless.Radio60GHz.GPSSyncState = 3
	if got, ok := m.getMetricValue(MetricGPSSync, ds, deviceAlertPolicy{}, time.Time{}); !ok || got != 0 {
		t.Fatalf("unsynchronized GPS metric = (%v,%v), want (0,true)", got, ok)
	}
	ds.Wireless.Radio60GHz.GPSSyncState = 2
	if got, ok := m.getMetricValue(MetricGPSSync, ds, deviceAlertPolicy{}, time.Time{}); !ok || got != 1 {
		t.Fatalf("synchronized GPS metric = (%v,%v), want (1,true)", got, ok)
	}
	ds.Wireless.Radio60GHz.GPSSyncState = 0
	if _, ok := m.getMetricValue(MetricGPSSync, ds, deviceAlertPolicy{}, time.Time{}); ok {
		t.Fatal("GPS metric was available without a live state or positive parsed configuration")
	}
}

func TestGPSAlertMessage(t *testing.T) {
	m := &Manager{}
	rule := Rule{Name: "GPS sync lost", Metric: MetricGPSSync, Operator: OpLT, Threshold: 1}
	ds := &stats.DeviceStats{Hostname: "sector-ap"}
	if got := m.formatAlertMessage(rule, ds, 0); got != "GPS sync lost: GPS synchronization is not synchronized on sector-ap" {
		t.Fatalf("GPS alert message = %q", got)
	}
}

func TestSummarizeDeliveries(t *testing.T) {
	tests := []struct {
		name       string
		deliveries []NotificationDelivery
		event      string
		empty      string
		channels   int
		status     string
	}{
		{"in app only", nil, "triggered", "in_app", 0, "in_app"},
		{"pending", []NotificationDelivery{{Channel: "email", Event: "triggered", Status: "pending"}}, "triggered", "in_app", 1, "pending"},
		{"sending", []NotificationDelivery{{Channel: "email", Event: "triggered", Status: "sending"}}, "triggered", "in_app", 1, "sending"},
		{"retrying", []NotificationDelivery{{Channel: "email", Event: "triggered", Status: "failed"}}, "triggered", "in_app", 1, "retrying"},
		{"sent", []NotificationDelivery{{Channel: "email", Event: "triggered", Status: "sent"}}, "triggered", "in_app", 1, "sent"},
		{"partial", []NotificationDelivery{{Channel: "email", Event: "triggered", Status: "sent"}, {Channel: "sysmon", Event: "triggered", Status: "dead"}}, "triggered", "in_app", 2, "partial"},
		{"failed", []NotificationDelivery{{Channel: "sysmon", Event: "resolved", Status: "dead"}}, "resolved", "not_requested", 1, "failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			channels, status := summarizeDeliveries(tt.deliveries, tt.event, tt.empty)
			if len(channels) != tt.channels || status != tt.status {
				t.Fatalf("summarizeDeliveries() = (%v,%q), want %d channels and %q", channels, status, tt.channels, tt.status)
			}
		})
	}
}

func TestSysmonNotificationMapping(t *testing.T) {
	ruleID := 7
	payload := notificationPayload{
		Event:  "triggered",
		Rule:   Rule{ID: ruleID, Name: "Device down"},
		Alert:  Alert{ID: 19, RuleID: &ruleID, Severity: SeverityCritical, Message: "offline"},
		Device: notificationDevice{ID: 12, Hostname: "tower-ap"},
	}
	if got := sysmonProtocolStatus(payload); got != "CRITICAL" {
		t.Fatalf("sysmonProtocolStatus(triggered) = %q", got)
	}
	if got := sysmonObject(payload); got != "device-12-rule-7" {
		t.Fatalf("sysmonObject() = %q", got)
	}
	payload.Event = "resolved"
	payload.ClearReason = "condition returned to normal"
	if got := sysmonProtocolStatus(payload); got != "OK" {
		t.Fatalf("sysmonProtocolStatus(resolved) = %q", got)
	}
	if got := notificationText(payload); got != "Device down cleared on tower-ap: condition returned to normal" {
		t.Fatalf("notificationText(resolved) = %q", got)
	}
}

func TestNotificationFailurePolicy(t *testing.T) {
	status, delay := notificationFailurePolicy("sysmon", 1)
	if status != "failed" || delay != 5*time.Second {
		t.Fatalf("sysmon first failure = (%q,%s), want (failed,5s)", status, delay)
	}
	status, delay = notificationFailurePolicy("sysmon", 100)
	if status != "failed" || delay != time.Minute {
		t.Fatalf("sysmon sustained failure = (%q,%s), want (failed,1m)", status, delay)
	}
	status, delay = notificationFailurePolicy("email", 8)
	if status != "dead" || delay != 30*time.Minute {
		t.Fatalf("email terminal failure = (%q,%s), want (dead,30m)", status, delay)
	}
}
