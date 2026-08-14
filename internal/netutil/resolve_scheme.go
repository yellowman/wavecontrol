package netutil

import (
	"sync"
	"time"
)

// SchemeHint provides optional hints to ResolveScheme.
//
// The goal is to keep scheme selection consistent across the codebase without
// sprinkling hard-coded "http://" / "https://" in many places.
//
// Precedence (highest first):
//  1. TLSPinned=true   -> https
//  2. PreferHTTPS=true -> https
//  3. Cached decision  -> cached scheme
//  4. ProbeScheme()    -> "https" if 443 reachable, "http" if only 80 reachable
//     (and defaults to "https" if neither answers).
//
// NOTE: Platform is a hint only; callers can translate platform to PreferHTTPS.
type SchemeHint struct {
	// Platform is optional context (e.g. "wave", "ltu", "airmax").
	Platform string

	// PreferHTTPS forces https even if probe results are inconclusive.
	PreferHTTPS bool

	// TLSPinned indicates we already have a pinned certificate for this host.
	// If true, we always return https.
	TLSPinned bool

	// Timeout is forwarded to ProbeScheme when probing is needed.
	// If zero, a reasonable default is used.
	Timeout time.Duration
}

type schemeCacheEntry struct {
	scheme  string
	expires time.Time
}

var (
	schemeCacheTTL = 5 * time.Minute
	schemeCache    sync.Map // map[string]schemeCacheEntry
)

// ResolveScheme returns "http" or "https" for the given host.
//
// This is the central, shared scheme-selection helper.
func ResolveScheme(host string, hint SchemeHint) string {
	if host == "" {
		return "https"
	}

	// Strongest hint: we have a pinned cert for this host.
	if hint.TLSPinned {
		// Cache it for a while to prevent repeated probes.
		schemeCache.Store(host, schemeCacheEntry{scheme: "https", expires: time.Now().Add(schemeCacheTTL)})
		return "https"
	}

	if hint.PreferHTTPS {
		schemeCache.Store(host, schemeCacheEntry{scheme: "https", expires: time.Now().Add(schemeCacheTTL)})
		return "https"
	}

	// Cache hit?
	if v, ok := schemeCache.Load(host); ok {
		if ent, ok2 := v.(schemeCacheEntry); ok2 {
			if time.Now().Before(ent.expires) {
				if ent.scheme == "http" || ent.scheme == "https" {
					return ent.scheme
				}
			}
		}
	}

	timeout := hint.Timeout
	if timeout <= 0 {
		timeout = 750 * time.Millisecond
	}
	scheme := ProbeScheme(host, timeout)
	schemeCache.Store(host, schemeCacheEntry{scheme: scheme, expires: time.Now().Add(schemeCacheTTL)})
	return scheme
}

// ClearSchemeCache clears any cached scheme decision for the given host.
// This is mainly useful for debugging.
func ClearSchemeCache(host string) {
	if host == "" {
		return
	}
	schemeCache.Delete(host)
}
