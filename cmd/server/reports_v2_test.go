package main

import (
	"bytes"
	"encoding/csv"
	"strings"
	"testing"

	"github.com/yellowman/wavecontrol/internal/stats"
)

func TestNormalizeReportType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  string
		ok    bool
	}{
		{input: "", want: "health", ok: true},
		{input: " Performance ", want: "performance", ok: true},
		{input: "RX_MISMATCH", want: "rx_mismatch", ok: true},
		{input: "unknown", want: "unknown", ok: false},
	}
	for _, test := range tests {
		got, ok := normalizeReportType(test.input)
		if got != test.want || ok != test.ok {
			t.Fatalf("normalizeReportType(%q) = (%q, %v), want (%q, %v)", test.input, got, ok, test.want, test.ok)
		}
	}
}

func TestReportPrimaryRadioUsesFirstRadioWithSignal(t *testing.T) {
	t.Parallel()

	wireless := stats.WirelessStats{
		Radio60GHz: &stats.RadioStats{Name: "60 GHz"},
		Radio6GHz: &stats.RadioStats{
			Name:                "MLO 6 GHz",
			DisplayBandOverride: "6 GHz",
			SignalCombined:      -67,
		},
		Radio5GHz: &stats.RadioStats{Name: "5 GHz", Signal: -58},
	}

	got := reportPrimaryRadio(wireless)
	if got.Band != "6 GHz" {
		t.Fatalf("reportPrimaryRadio band = %q, want 6 GHz", got.Band)
	}
	if got.Signal != -67 || !got.HasSignal {
		t.Fatalf("reportPrimaryRadio signal = %d, hasSignal=%v, want -67/true", got.Signal, got.HasSignal)
	}
}

func TestReportSignalQualityUsesBandThresholds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		signal int
		band   string
		want   string
	}{
		{signal: 0, band: "5 GHz", want: "no_signal"},
		{signal: -60, band: "5 GHz", want: "good"},
		{signal: -66, band: "5 GHz", want: "fair"},
		{signal: -75, band: "5 GHz", want: "poor"},
		{signal: -54, band: "60 GHz", want: "good"},
		{signal: -60, band: "60 GHz", want: "fair"},
		{signal: -70, band: "60 GHz", want: "poor"},
	}
	for _, test := range tests {
		if got := reportSignalQuality(test.signal, test.band); got != test.want {
			t.Errorf("reportSignalQuality(%d, %q) = %q, want %q", test.signal, test.band, got, test.want)
		}
	}
}

func TestWritePerformanceCSVIncludesAPAndSTA(t *testing.T) {
	t.Parallel()

	data := map[string]any{
		"ap_devices": []any{
			map[string]any{
				"hostname": "ap-1", "ip": "192.0.2.1", "site": "North", "product": "Wave AP",
				"platform": "wave60", "band": "60 GHz", "status": "online", "online": true,
				"tx_rate": int64(1000), "rx_rate": int64(2000), "signal": -50, "signal_quality": "good",
			},
		},
		"sta_devices": []any{
			map[string]any{
				"is_sta": true, "hostname": "sta-1", "ip": "192.0.2.2", "site": "North", "parent_hostname": "ap-1",
				"product": "Wave LR", "platform": "wave60", "band": "60 GHz", "status": "online", "online": true,
				"tx_rate": int64(3000), "rx_rate": int64(4000), "signal": -61, "signal_quality": "fair",
			},
		},
	}

	var output bytes.Buffer
	if err := writeReportCSV(&output, "performance", data); err != nil {
		t.Fatalf("writeReportCSV: %v", err)
	}

	rows, err := csv.NewReader(strings.NewReader(output.String())).ReadAll()
	if err != nil {
		t.Fatalf("parse generated CSV: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("generated CSV has %d rows, want header plus AP and STA", len(rows))
	}
	if rows[1][0] != "AP" || rows[1][1] != "ap-1" {
		t.Fatalf("first data row = %#v, want AP ap-1", rows[1])
	}
	if rows[2][0] != "STA" || rows[2][1] != "sta-1" || rows[2][4] != "ap-1" {
		t.Fatalf("second data row = %#v, want STA sta-1 parent ap-1", rows[2])
	}
}

func TestWriteEmptyDiagnosticCSVStillHasHeader(t *testing.T) {
	t.Parallel()

	for _, reportType := range []string{"chain", "rx_mismatch"} {
		var output bytes.Buffer
		if err := writeReportCSV(&output, reportType, map[string]any{"issues": []any{}}); err != nil {
			t.Fatalf("writeReportCSV(%s): %v", reportType, err)
		}
		rows, err := csv.NewReader(strings.NewReader(output.String())).ReadAll()
		if err != nil {
			t.Fatalf("parse %s CSV: %v", reportType, err)
		}
		if len(rows) != 1 || len(rows[0]) < 5 {
			t.Fatalf("%s empty CSV rows = %#v, want one nontrivial header", reportType, rows)
		}
	}
}

func TestCompareSummary(t *testing.T) {
	t.Parallel()

	got := compareSummary(
		map[string]any{"online": float64(10), "coverage_pct": 80},
		map[string]any{"online": float64(13), "coverage_pct": 95.5},
		[]string{"online", "coverage_pct"},
	)

	online := got["online"].(map[string]any)
	if delta := online["delta"].(float64); delta != 3 {
		t.Fatalf("online delta = %v, want 3", delta)
	}
	coverage := got["coverage_pct"].(map[string]any)
	if delta := coverage["delta"].(float64); delta != 15.5 {
		t.Fatalf("coverage delta = %v, want 15.5", delta)
	}
}
