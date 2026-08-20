package poller

import (
	"fmt"
	"strings"
	"time"

	"github.com/yellowman/wavecontrol/internal/stats"
)

// buildWaveParseReport generates a human-readable parse report intended for inclusion
// in Ultra Debug bundles.
//
// The goal is to help debug:
//   - Band inference (5/6/60 GHz) for Wave MLO devices.
//   - Slot assignment (UI mapping) decisions.
//   - Peer discovery path (statistics vs fallback endpoint).
//
// Keep it short enough to be readable in one screen (~10-30 lines).
func buildWaveParseReport(
	deviceID int64,
	host string,
	platform string,
	statsData []map[string]interface{},
	deviceStats *stats.DeviceStats,
	peersStatsCount int,
	peersFinalCount int,
	apLikely bool,
	fallbackAttempted bool,
	fallbackUsed bool,
	fallbackEndpoint string,
	fallbackRawPeerCount int,
) string {
	var lines []string

	lines = append(lines, fmt.Sprintf("wave_parse_report (%s)", time.Now().UTC().Format(time.RFC3339Nano)))
	lines = append(lines, fmt.Sprintf("device_id=%d host=%s platform=%s", deviceID, host, platform))

	radios, warnings := extractWaveWirelessRadios(statsData)
	if len(warnings) > 0 {
		lines = append(lines, "warnings:")
		for _, w := range warnings {
			lines = append(lines, "- "+w)
		}
	}

	lines = append(lines, fmt.Sprintf("radios in statistics.wireless.radios: %d", len(radios)))
	for _, r := range radios {
		id := getString(r, "id")
		freqTx := "n/a"
		if fm, ok := r["frequency"].(map[string]interface{}); ok {
			if v, ok := fm["tx"]; ok && v != nil {
				freqTx = fmt.Sprintf("%v", v)
			} else if _, ok := fm["tx"]; ok {
				// present but null
				freqTx = "null"
			}
		}

		cwTx := "n/a"
		cwRx := "n/a"
		if cwm, ok := r["channelWidth"].(map[string]interface{}); ok {
			if v, ok := cwm["tx"]; ok && v != nil {
				cwTx = fmt.Sprintf("%v", v)
			} else if _, ok := cwm["tx"]; ok {
				cwTx = "null"
			}
			if v, ok := cwm["rx"]; ok && v != nil {
				cwRx = fmt.Sprintf("%v", v)
			} else if _, ok := cwm["rx"]; ok {
				cwRx = "null"
			}
		}

		hasAFC := false
		if v, ok := r["afc"]; ok && v != nil {
			hasAFC = true
		}

		inferred := waveInferBandFromStats(r).String()
		if inferred == "" {
			inferred = "unknown"
		}

		lines = append(lines, fmt.Sprintf("- id=%s freq.tx=%s cw(tx/rx)=%s/%s afc=%t -> inferred=%s", id, freqTx, cwTx, cwRx, hasAFC, inferred))
	}

	// Slot mapping summary
	lines = append(lines, "slot mapping summary:")
	if deviceStats == nil {
		lines = append(lines, "- deviceStats=nil")
	} else {
		// Fixed compatibility slots
		slot5 := "(none)"
		if deviceStats.Wireless.Radio5GHz != nil {
			slot5 = deviceStats.Wireless.Radio5GHz.ID
		}
		slot6 := "(none)"
		slot6Label := "6ghz"
		if deviceStats.Wireless.Radio6GHz != nil {
			slot6 = deviceStats.Wireless.Radio6GHz.ID
			if deviceStats.Wireless.Radio6GHz.DisplayBandOverride != "" {
				slot6Label = deviceStats.Wireless.Radio6GHz.DisplayBandOverride
			}
		}
		slot60 := "(none)"
		if deviceStats.Wireless.Radio60GHz != nil {
			slot60 = deviceStats.Wireless.Radio60GHz.ID
		}

		lines = append(lines, fmt.Sprintf("- slot_5ghz=%s", slot5))
		lines = append(lines, fmt.Sprintf("- slot_%s=%s", strings.ReplaceAll(slot6Label, " ", "_"), slot6))
		lines = append(lines, fmt.Sprintf("- slot_60ghz=%s", slot60))

		// All radios slice (stable order) to confirm nothing got dropped.
		if len(deviceStats.Wireless.Radios) > 0 {
			lines = append(lines, fmt.Sprintf("all radios stored=%d:", len(deviceStats.Wireless.Radios)))
			for _, rs := range deviceStats.Wireless.Radios {
				if rs.ID == "" {
					continue
				}
				inferred := waveInferBandFromRadioStats(rs).String()
				bandLabel := inferred
				if rs.DisplayBandOverride != "" {
					bandLabel = rs.DisplayBandOverride
				}
				lines = append(lines, fmt.Sprintf("- id=%s band=%s", rs.ID, bandLabel))
			}
		}
	}

	// Peer discovery path summary
	lines = append(lines, "peer discovery:")
	lines = append(lines, fmt.Sprintf("- ap_likely=%t", apLikely))
	lines = append(lines, fmt.Sprintf("- stats.wireless.peers=%d", peersStatsCount))
	lines = append(lines, fmt.Sprintf("- fallback_attempted=%t", fallbackAttempted))
	if fallbackAttempted {
		if fallbackEndpoint == "" {
			fallbackEndpoint = "(unknown)"
		}
		lines = append(lines, fmt.Sprintf("- fallback_endpoint=%s", fallbackEndpoint))
		lines = append(lines, fmt.Sprintf("- fallback_raw_peers=%d", fallbackRawPeerCount))
		lines = append(lines, fmt.Sprintf("- fallback_used=%t", fallbackUsed))
	}
	lines = append(lines, fmt.Sprintf("- peers_final=%d", peersFinalCount))

	return strings.Join(lines, "\n")
}

func extractWaveWirelessRadios(statsData []map[string]interface{}) ([]map[string]interface{}, []string) {
	warnings := make([]string, 0, 4)
	if len(statsData) == 0 {
		return nil, []string{"statistics payload is empty"}
	}

	// Find the first element that has a wireless.radios section.
	for idx, section := range statsData {
		wm, ok := section["wireless"].(map[string]interface{})
		if !ok {
			continue
		}

		rv, ok := wm["radios"]
		if !ok {
			warnings = append(warnings, fmt.Sprintf("statsData[%d].wireless missing radios", idx))
			continue
		}
		arr, ok := rv.([]interface{})
		if !ok {
			warnings = append(warnings, fmt.Sprintf("statsData[%d].wireless.radios not an array", idx))
			continue
		}

		radios := make([]map[string]interface{}, 0, len(arr))
		for i, item := range arr {
			rm, ok := item.(map[string]interface{})
			if !ok {
				warnings = append(warnings, fmt.Sprintf("wireless.radios[%d] not an object", i))
				continue
			}
			radios = append(radios, rm)
		}
		return radios, warnings
	}

	return nil, []string{"no wireless.radios section found in statistics payload"}
}

// waveInferBandFromStats infers the band for a radio object from the raw
// /statistics wireless.radios element.
func waveInferBandFromStats(r map[string]interface{}) waveBand {
	return inferWaveBand(r, nil)
}

// waveInferBandFromRadioStats infers the band for a stored RadioStats entry.
// This is used only for debugging output.
func waveInferBandFromRadioStats(rs stats.RadioStats) waveBand {
	if rs.AFC != nil {
		return waveBand6GHz
	}
	if rs.ChannelWidth >= 1000 || rs.Frequency >= 57000 {
		return waveBand60GHz
	}
	if rs.Frequency >= 5925 {
		return waveBand6GHz
	}
	// 5GHz (rough heuristic)
	if rs.Frequency >= 4900 {
		return waveBand5GHz
	}
	if rs.Frequency >= 2400 && rs.Frequency < 3000 {
		return waveBandUnknown
	}
	return waveBandUnknown
}
