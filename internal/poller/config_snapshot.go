package poller

import (
	"net"
	"time"
)

// pollerConfigSnapshot is an immutable view of Poller's runtime configuration.
// The returned slices must be treated as read-only.
type pollerConfigSnapshot struct {
	interval          time.Duration
	workerCount       int
	debug             bool
	wavePeerFallback  bool
	waveMLOMultiRadio bool
	mgmtPrefixes      []*net.IPNet
	apCreds           []Credential
	staCreds          []Credential
}

func (p *Poller) cfgSnapshot() pollerConfigSnapshot {
	p.configMu.RLock()
	defer p.configMu.RUnlock()

	return pollerConfigSnapshot{
		interval:          p.interval,
		workerCount:       p.workerCount,
		debug:             p.debug,
		wavePeerFallback:  p.wavePeerFallback,
		waveMLOMultiRadio: p.waveMLOMultiRadio,
		mgmtPrefixes:      p.mgmtPrefixes,
		apCreds:           p.apCreds,
		staCreds:          p.staCreds,
	}
}
