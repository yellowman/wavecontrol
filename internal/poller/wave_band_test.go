package poller

import (
	"testing"

	"github.com/yellowman/wavecontrol/internal/stats"
)

func TestInferWaveBand_AFCWinsEvenWithoutFrequency(t *testing.T) {
	rs := &stats.RadioStats{Frequency: 0, ChannelWidth: 0}
	raw := map[string]any{
		"id":  "backup",
		"afc": map[string]any{"status": "REQUEST_SUCCEEDED"},
	}

	band := inferWaveBand(raw, rs)
	if band != waveBand6GHz {
		t.Fatalf("expected band %v, got %v", waveBand6GHz, band)
	}
}

func TestInferWaveBand_60GHzByFrequency(t *testing.T) {
	rs := &stats.RadioStats{Frequency: 60000}
	band := inferWaveBand(map[string]any{}, rs)
	if band != waveBand60GHz {
		t.Fatalf("expected band %v, got %v", waveBand60GHz, band)
	}
}

func TestInferWaveBand_60GHzByChannelWidth(t *testing.T) {
	rs := &stats.RadioStats{Frequency: 0, ChannelWidth: 2160}
	band := inferWaveBand(map[string]any{}, rs)
	if band != waveBand60GHz {
		t.Fatalf("expected band %v, got %v", waveBand60GHz, band)
	}
}

func TestInferWaveBand_6GHzByFrequency(t *testing.T) {
	rs := &stats.RadioStats{Frequency: 6025}
	band := inferWaveBand(map[string]any{}, rs)
	if band != waveBand6GHz {
		t.Fatalf("expected band %v, got %v", waveBand6GHz, band)
	}
}

func TestInferWaveBand_5GHzDefault(t *testing.T) {
	rs := &stats.RadioStats{Frequency: 5720}
	band := inferWaveBand(map[string]any{}, rs)
	if band != waveBand5GHz {
		t.Fatalf("expected band %v, got %v", waveBand5GHz, band)
	}
}

func TestInferWaveBand_UnknownWhenNoSignal(t *testing.T) {
	rs := &stats.RadioStats{Frequency: 0, ChannelWidth: 0}
	band := inferWaveBand(map[string]any{}, rs)
	if band != waveBandUnknown {
		t.Fatalf("expected band %v, got %v", waveBandUnknown, band)
	}
}
