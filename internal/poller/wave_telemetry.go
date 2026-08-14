package poller

import (
	"sync/atomic"
	"time"
)

// WaveParseCounters captures lightweight, in-process telemetry about Wave parsing.
//
// These counters are intended for debugging / rollout validation (no persistence).
// They must be safe for concurrent updates from poller workers.
type WaveParseCounters struct {
	startedAt time.Time

	band5GHz    atomic.Int64
	band6GHz    atomic.Int64
	band60GHz   atomic.Int64
	bandUnknown atomic.Int64

	peerFromStats         atomic.Int64
	peerFromFallback      atomic.Int64
	peerFallbackAttempted atomic.Int64
	peerNone              atomic.Int64
}

type WaveParseCountersSnapshot struct {
	StartedAt   time.Time        `json:"started_at"`
	Bands       map[string]int64 `json:"bands"`
	PeerSources map[string]int64 `json:"peer_sources"`
}

var waveParseCounters = WaveParseCounters{startedAt: time.Now().UTC()}

func recordWaveBand(b waveBand) {
	switch b {
	case waveBand5GHz:
		waveParseCounters.band5GHz.Add(1)
	case waveBand6GHz:
		waveParseCounters.band6GHz.Add(1)
	case waveBand60GHz:
		waveParseCounters.band60GHz.Add(1)
	default:
		waveParseCounters.bandUnknown.Add(1)
	}
}

func recordWavePeerSource(fromStats, fromFallback bool) {
	switch {
	case fromStats:
		waveParseCounters.peerFromStats.Add(1)
	case fromFallback:
		waveParseCounters.peerFromFallback.Add(1)
	default:
		waveParseCounters.peerNone.Add(1)
	}
}

func recordWavePeerFallbackAttempt() {
	waveParseCounters.peerFallbackAttempted.Add(1)
}

// GetWaveParseCountersSnapshot returns a point-in-time snapshot of Wave parsing telemetry.
func GetWaveParseCountersSnapshot() WaveParseCountersSnapshot {
	return WaveParseCountersSnapshot{
		StartedAt: waveParseCounters.startedAt,
		Bands: map[string]int64{
			"5ghz":    waveParseCounters.band5GHz.Load(),
			"6ghz":    waveParseCounters.band6GHz.Load(),
			"60ghz":   waveParseCounters.band60GHz.Load(),
			"unknown": waveParseCounters.bandUnknown.Load(),
		},
		PeerSources: map[string]int64{
			"stats":              waveParseCounters.peerFromStats.Load(),
			"fallback":           waveParseCounters.peerFromFallback.Load(),
			"fallback_attempted": waveParseCounters.peerFallbackAttempted.Load(),
			"none":               waveParseCounters.peerNone.Load(),
		},
	}
}
