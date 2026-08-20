package poller

import "github.com/yellowman/wavecontrol/internal/stats"

// waveBand represents a coarse RF band classification for Wave/Wave MLO radios.
//
// We intentionally keep this coarse (5/6/60) because WaveControl UI and stats
// aggregation are organized around those buckets.
//
// NOTE: This is a best-effort heuristic. When radios are disconnected and/or
// using auto-frequency, some firmware versions may omit frequency fields.
// In those cases we fall back to other hints (AFC, channel width, and finally
// the legacy ID mapping).
type waveBand int

const (
	waveBandUnknown waveBand = iota
	waveBand5GHz
	waveBand6GHz
	waveBand60GHz
)

func (b waveBand) String() string {
	switch b {
	case waveBand5GHz:
		return "5ghz"
	case waveBand6GHz:
		return "6ghz"
	case waveBand60GHz:
		return "60ghz"
	default:
		return ""
	}
}

// inferWaveBand attempts to determine the radio band from a Wave statistics
// radio object. The raw map is used for fields that don't currently land in
// stats.RadioStats (e.g., AFC).
func inferWaveBand(raw map[string]any, rs *stats.RadioStats) waveBand {
	// 1) Strong signal: explicit AFC object => 6 GHz
	// AFC is only present on 6 GHz radios.
	if raw != nil {
		if _, ok := raw["afc"]; ok {
			return waveBand6GHz
		}
	}

	// 2) Strong signal: 60 GHz frequencies
	if rs != nil && rs.Frequency >= 57000 {
		return waveBand60GHz
	}
	// 60 GHz often has very large channel widths; this is a helpful hint when
	// frequency is missing.
	if rs != nil && rs.ChannelWidth >= 1000 {
		return waveBand60GHz
	}

	// 3) 6 GHz frequency ranges (UNII-5..8). We use 5945 MHz as the lower bound
	// (UNII-5 starts at 5925 MHz, but 5945 is also used as a common scanlist/
	// regulatory boundary in firmware).
	if rs != nil && rs.Frequency >= 5945 {
		return waveBand6GHz
	}

	// 4) If we have any non-zero frequency but it didn't match 6/60, treat as 5.
	if rs != nil && rs.Frequency > 0 {
		return waveBand5GHz
	}

	// Unknown (caller may apply legacy fallback mapping by ID).
	return waveBandUnknown
}
