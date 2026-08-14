package poller

import (
	"encoding/json"
	"testing"
)

func TestParseStats_MLO6RadioBandMapping(t *testing.T) {
	p := &Poller{}

	raw := []any{
		map[string]any{
			"wireless": map[string]any{
				"radios": []any{
					map[string]any{
						"id":            "main",
						"frequency":     map[string]any{"tx": 5720.0, "type": "channels"},
						"channelWidth":  map[string]any{"tx": 20.0, "rx": 20.0},
						"linkState":     "disconnected",
						"outputPower":   map[string]any{"autoPower": false, "conducted": 20.0, "eirp": 30.0},
						"dfs":           map[string]any{"enabled": true, "cacDuration": 60.0, "cacRemaining": 0.0},
						"servingAp":     nil,
						"serviceUptime": 0.0,
					},
					map[string]any{
						"id":            "backup",
						"frequency":     map[string]any{"tx": 6025.0, "type": "channels"},
						"channelWidth":  map[string]any{"tx": 160.0, "rx": 160.0},
						"linkState":     "disconnected",
						"outputPower":   map[string]any{"autoPower": false, "conducted": 23.0, "eirp": 33.0},
						"afc":           map[string]any{"status": "REQUEST_SUCCEEDED", "label": "Ready"},
						"serviceUptime": 0.0,
					},
				},
				"peers": []any{},
			},
		},
	}

	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("json.Marshal raw stats failed: %v", err)
	}

	ds, _ := p.parseStats(b, "wave")
	if ds == nil || ds.Wireless == nil {
		t.Fatalf("expected wireless stats to be present")
	}
	if ds.Wireless.Radio5GHz == nil {
		t.Fatalf("expected Radio5GHz to be set")
	}
	if ds.Wireless.Radio6GHz == nil {
		t.Fatalf("expected Radio6GHz to be set")
	}
	if ds.Wireless.Radio5GHz.DisplayBandOverride != "5ghz" {
		t.Fatalf("expected Radio5GHz override '5ghz', got %q", ds.Wireless.Radio5GHz.DisplayBandOverride)
	}
	if ds.Wireless.Radio6GHz.DisplayBandOverride != "6ghz" {
		t.Fatalf("expected Radio6GHz override '6ghz', got %q", ds.Wireless.Radio6GHz.DisplayBandOverride)
	}
	if ds.Wireless.Radio6GHz.AFC == nil {
		t.Fatalf("expected Radio6GHz to include AFC info")
	}
	if ds.Wireless.Radio6GHz.AFC.Status != "REQUEST_SUCCEEDED" {
		t.Fatalf("expected AFC status REQUEST_SUCCEEDED, got %q", ds.Wireless.Radio6GHz.AFC.Status)
	}
}

func TestParseStats_MLO5Two5GHzRadiosUsesSecondSlot(t *testing.T) {
	p := &Poller{}

	raw := []any{
		map[string]any{
			"wireless": map[string]any{
				"radios": []any{
					map[string]any{
						"id":           "main",
						"frequency":    map[string]any{"tx": 5200.0, "type": "channels"},
						"channelWidth": map[string]any{"tx": 40.0, "rx": 40.0},
					},
					map[string]any{
						"id":           "backup",
						"frequency":    map[string]any{"tx": 5745.0, "type": "channels"},
						"channelWidth": map[string]any{"tx": 80.0, "rx": 80.0},
					},
				},
				"peers": []any{},
			},
		},
	}

	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("json.Marshal raw stats failed: %v", err)
	}

	ds, _ := p.parseStats(b, "wave")
	if ds == nil || ds.Wireless == nil {
		t.Fatalf("expected wireless stats to be present")
	}
	if ds.Wireless.Radio5GHz == nil {
		t.Fatalf("expected Radio5GHz to be set")
	}
	if ds.Wireless.Radio6GHz == nil {
		t.Fatalf("expected second 5GHz radio to occupy the secondary slot (Radio6GHz)")
	}
	if ds.Wireless.Radio6GHz.DisplayBandOverride != "5ghz#2" {
		t.Fatalf("expected secondary slot override '5ghz#2', got %q", ds.Wireless.Radio6GHz.DisplayBandOverride)
	}
	// The secondary slot is a 2nd 5GHz radio for MLO5; it must not be misclassified as 6GHz.
	if ds.Wireless.Radio6GHz.Frequency >= 5945 {
		t.Fatalf("expected secondary slot to remain 5GHz (<5945), got freq=%d", ds.Wireless.Radio6GHz.Frequency)
	}
}
