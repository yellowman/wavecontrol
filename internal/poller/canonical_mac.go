package poller

import (
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/lib/pq"
	"github.com/yellowman/wavecontrol/internal/stats"
)

// macMismatchWarned suppresses repeated MAC mismatch warnings for the same device.
// Keyed by "deviceID|expected|observed".
var macMismatchWarned sync.Map

type canonicalMACResult struct {
	Expected  string
	Canonical string
	Observed  []string
	Mismatch  bool
}

// canonicalizeDeviceMAC chooses the canonical MAC for a poll result and indicates
// whether the result appears to be for a different device than the one the job
// was scheduled for.
//
// Rule:
//   - If expected MAC is known, it is authoritative.
//   - If any observed candidate matches expected => OK.
//   - If at least one observed candidate exists but none match expected => mismatch.
//   - If expected MAC is empty, fall back to the first observed candidate.
func canonicalizeDeviceMAC(expected string, candidates ...string) canonicalMACResult {
	exp := stats.NormalizeMAC(expected)

	// Normalize + dedupe observed candidates.
	seen := make(map[string]struct{}, len(candidates))
	obs := make([]string, 0, len(candidates))
	for _, c := range candidates {
		m := stats.NormalizeMAC(strings.TrimSpace(c))
		if m == "" {
			continue
		}
		if _, ok := seen[m]; ok {
			continue
		}
		seen[m] = struct{}{}
		obs = append(obs, m)
	}

	if exp != "" {
		for _, m := range obs {
			if m == exp {
				return canonicalMACResult{Expected: exp, Canonical: exp, Observed: obs, Mismatch: false}
			}
		}
		// Observed a MAC, but it doesn't match what the DB says this device is.
		if len(obs) > 0 {
			return canonicalMACResult{Expected: exp, Canonical: exp, Observed: obs, Mismatch: true}
		}
		// No observed MAC: can't validate; keep expected.
		return canonicalMACResult{Expected: exp, Canonical: exp, Observed: obs, Mismatch: false}
	}

	// No expected MAC: pick first observed.
	if len(obs) > 0 {
		return canonicalMACResult{Expected: "", Canonical: obs[0], Observed: obs, Mismatch: false}
	}
	return canonicalMACResult{Expected: "", Canonical: "", Observed: obs, Mismatch: false}
}

// canonicalizeDeviceMACPreferred is like canonicalizeDeviceMAC, but enforces a
// preferred identity candidate when it is present.
//
// This is used when we know which MAC we want to treat as canonical (e.g. the
// Ethernet/management MAC). If the preferred MAC is observed and does not match
// the expected DB MAC, this is treated as a mismatch even if some other
// secondary candidate matches.
//
// Rule:
//   - If expected MAC is known, it is authoritative.
//   - If preferred MAC is present:
//   - preferred == expected => OK
//   - preferred != expected => mismatch
//   - If preferred is empty, fall back to canonicalizeDeviceMAC(expected, others...)
//   - If expected is empty, pick preferred (if present) else first observed.
func canonicalizeDeviceMACPreferred(expected string, preferred string, others ...string) canonicalMACResult {
	exp := stats.NormalizeMAC(expected)
	pref := stats.NormalizeMAC(strings.TrimSpace(preferred))

	// Build observed list with preferred first (if present), then others (deduped).
	seen := make(map[string]struct{}, 1+len(others))
	obs := make([]string, 0, 1+len(others))
	if pref != "" {
		seen[pref] = struct{}{}
		obs = append(obs, pref)
	}
	for _, c := range others {
		m := stats.NormalizeMAC(strings.TrimSpace(c))
		if m == "" {
			continue
		}
		if _, ok := seen[m]; ok {
			continue
		}
		seen[m] = struct{}{}
		obs = append(obs, m)
	}

	// Expected MAC provided: enforce preferred when present.
	if exp != "" {
		if pref != "" {
			if pref == exp {
				return canonicalMACResult{Expected: exp, Canonical: exp, Observed: obs, Mismatch: false}
			}
			// Preferred identity disagrees with expected.
			return canonicalMACResult{Expected: exp, Canonical: exp, Observed: obs, Mismatch: true}
		}

		// No preferred candidate: fall back to "any candidate matches" behavior.
		res := canonicalizeDeviceMAC(exp, obs...)
		// Ensure Observed includes the candidates we built.
		res.Observed = obs
		return res
	}

	// No expected MAC: choose preferred when present, otherwise first observed.
	if pref != "" {
		return canonicalMACResult{Expected: "", Canonical: pref, Observed: obs, Mismatch: false}
	}
	if len(obs) > 0 {
		return canonicalMACResult{Expected: "", Canonical: obs[0], Observed: obs, Mismatch: false}
	}
	return canonicalMACResult{Expected: "", Canonical: "", Observed: obs, Mismatch: false}
}

// handleMACMismatch marks the scheduled device as reachable-but-wrong (unknown) and logs context.
func (p *Poller) handleMACMismatch(job pollJob, api string, observed []string) pollResult {
	expected := stats.NormalizeMAC(job.MAC)
	obsStr := strings.Join(observed, ",")
	errMsg := fmt.Sprintf("%s mac mismatch: expected=%s observed=%s", api, expected, obsStr)

	// Log once per distinct mismatch; include device ID + IP for traceability.
	key := fmt.Sprintf("%d|%s|%s", job.DeviceID, expected, obsStr)
	if _, loaded := macMismatchWarned.LoadOrStore(key, struct{}{}); !loaded {
		log.Printf("WARN: %s %s id=%d: %s", strings.ToUpper(api), job.IP, job.DeviceID, errMsg)
	}

	// The device responded, so this is *not* an offline/unreachable failure.
	p.recordFailure(job.IP, false)

	// Mark expected device as unknown, but do NOT advance last_seen
	// (we did not successfully poll the expected MAC).
	p.store.SetStatusByMAC(job.MAC, job.IP, stats.StatusUnknown, "mac_mismatch", errMsg, false)
	p.store.TrackStabilityStatus(job.IP, "", stats.StatusUnknown, 0)

	if p.wsHub != nil {
		p.wsHub.BroadcastStatsUpdate(int(job.DeviceID), expected, job.IP, map[string]any{
			"online":        false,
			"status":        "unknown",
			"db_status":     "unknown",
			"status_reason": "mac_mismatch",
			"last_error":    errMsg,
			"identity_mismatch": map[string]any{
				"device_id":     job.DeviceID,
				"expected_mac":  expected,
				"observed_macs": observed,
				"observed_ip":   job.IP,
				"source":        strings.ToLower(api),
			},
		})
	}

	// Persist status_reason to DB, but do NOT touch last_seen.
	dbExecIgnoreCtx(p.db, dbCtxForJob(job, api+"_mac_mismatch"), `UPDATE devices SET status = 'unknown', status_reason = $2 WHERE id = $1`, job.DeviceID, "mac_mismatch")
	p.persistIdentityMismatch(job, api, expected, observed, errMsg)
	p.updateChildrenStatus(job.DeviceID, "unknown")

	return pollFailed
}

// persistIdentityMismatch records the observed replacement identity so the UI can
// present an explicit, auditable "learn replacement MAC" action.
func (p *Poller) persistIdentityMismatch(job pollJob, api, expected string, observed []string, errMsg string) {
	if p == nil || p.db == nil || job.DeviceID == 0 || expected == "" || len(observed) == 0 || strings.TrimSpace(job.IP) == "" {
		return
	}

	_, err := dbExecCtx(p.db, dbCtxForJob(job, api+"_identity_mismatch"), `
		INSERT INTO device_identity_mismatches
		    (device_id, expected_mac, observed_macs, observed_ip, source, observed_at, last_error)
		VALUES ($1, $2, $3, $4::inet, $5, NOW(), $6)
		ON CONFLICT (device_id) DO UPDATE SET
		    expected_mac = EXCLUDED.expected_mac,
		    observed_macs = EXCLUDED.observed_macs,
		    observed_ip = EXCLUDED.observed_ip,
		    source = EXCLUDED.source,
		    observed_at = NOW(),
		    last_error = EXCLUDED.last_error`,
		job.DeviceID, expected, pq.Array(observed), job.IP, strings.ToLower(api), errMsg)
	if err != nil {
		log.Printf("WARN: failed to persist identity mismatch for device id=%d host=%s: %v", job.DeviceID, job.IP, err)
	}
}

func (p *Poller) clearIdentityMismatch(deviceID int64) {
	if p == nil || p.db == nil || deviceID == 0 {
		return
	}
	dbExecIgnoreCtx(p.db, dbCtxForDevice(deviceID, "clear_identity_mismatch"), `DELETE FROM device_identity_mismatches WHERE device_id = $1`, deviceID)
}
