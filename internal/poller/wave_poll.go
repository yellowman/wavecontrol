package poller

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/yellowman/wavecontrol/internal/netutil"
	"github.com/yellowman/wavecontrol/internal/stats"
)

// pollDeviceWave polls using Wave API
func (p *Poller) pollDeviceWave(job pollJob) pollResult {
	// Centralized scheme selection to avoid hard-coded http/https drift.
	// Wave/LTU are effectively https-first, but we still resolve for consistency.
	scheme := netutil.ResolveScheme(job.IP, netutil.SchemeHint{
		Platform:    job.Platform,
		PreferHTTPS: job.Platform == "wave" || job.Platform == "ltu",
	})
	baseURL := fmt.Sprintf("%s://%s", scheme, job.IP)

	// Get HTTP client for this device (uses TLS manager for cert verification)
	client := p.getDeviceClient(job.DeviceID)

	// Try to authenticate - first with job credentials, then AP credential pairs
	token, err := p.login(client, baseURL, job.Username, job.Password)
	if err != nil && !isNetworkUnreachable(err) {
		// Try all AP credential pairs, but stop on network errors
		for _, cred := range p.cfgSnapshot().apCreds {
			if cred.Username == job.Username && cred.Password == job.Password {
				continue // Already tried this one
			}
			token, err = p.login(client, baseURL, cred.Username, cred.Password)
			if err == nil || isNetworkUnreachable(err) {
				break
			}
		}
	}

	if err != nil {
		// AirMAX devices may return 401/403 on this Wave login URL, often with HTML.
		// Treat JSON-ish 401/403 as a reachable Wave endpoint with bad creds (unknown),
		// otherwise fall back to AirMAX probing.
		if hs, ok := err.(*httpStatusError); ok {
			if (hs.StatusCode == 401 || hs.StatusCode == 403) && hs.likelyJSON() {
				reason := "auth_failed"
				p.logDebug("pollDeviceWave %s: Wave login denied (%v); marking unknown", job.IP, hs)
				p.recordFailure(job.IP, false)
				leftOnline := p.store.SetUnknownByMAC(job.MAC, job.IP, reason, fmt.Sprintf("wave auth failed: %v", hs))
				p.store.TrackStabilityStatus(job.IP, "", stats.StatusUnknown, 0)
				if p.wsHub != nil {
					p.wsHub.BroadcastStatsUpdate(int(job.DeviceID), job.MAC, job.IP, map[string]any{"online": false, "status": "unknown", "db_status": "unknown", "status_reason": reason, "last_error": hs.Error()})
				}
				if leftOnline {
					p.updateChildrenStatus(job.DeviceID, "unknown")
				}
				dbExecIgnoreCtx(p.db, dbCtxForJob(job, "wave_mark_unknown_auth"), `UPDATE devices SET status = 'unknown', status_reason = $2, last_seen = NOW() WHERE id = $1`, job.DeviceID, reason)
				return pollFailed
			}
		}

		// Could be AirMAX (or any non-Wave device), return to try that.
		return pollNotThisType
	}

	// Fetch statistics
	statsData, err := p.fetchStats(client, baseURL, token)
	if err != nil {
		p.logDebug("pollDeviceWave %s: stats fetch failed: %v", job.IP, err)
		// Device authenticated so it's reachable - mark as unknown, not offline
		p.recordFailure(job.IP, false) // false = device responded (authenticated)
		leftOnline := p.store.SetUnknownByMAC(job.MAC, job.IP, "stats_failed", fmt.Sprintf("stats failed: %v", err))
		p.store.TrackStabilityStatus(job.IP, "", stats.StatusUnknown, 0)
		// Broadcast unknown status via WebSocket (device reachable but not responding properly)
		if p.wsHub != nil {
			p.wsHub.BroadcastStatsUpdate(int(job.DeviceID), job.MAC, job.IP, map[string]any{"online": false, "status": "unknown", "db_status": "unknown", "status_reason": "stats_failed", "last_error": err.Error()})
		}
		// Always update to "unknown" when device authenticated - it's clearly reachable
		if leftOnline {
			p.updateChildrenStatus(job.DeviceID, "unknown")
		}
		dbExecIgnoreCtx(p.db, dbCtxForJob(job, "wave_mark_unknown_stats_failed"), `UPDATE devices SET status = 'unknown', status_reason = $2, last_seen = NOW() WHERE id = $1`, job.DeviceID, "stats_failed")
		return pollFailed // We authenticated, so it's a Wave device, but stats failed
	}

	// Parse and store stats
	deviceStats, peers := p.parseStats(statsData, job.Platform)
	deviceStats.IP = job.IP

	// Canonicalize MAC identity.
	// The DB/job MAC is authoritative. If the device responds with a different MAC,
	// do not apply stats to the wrong device (common when an IP is reassigned).
	macRes := canonicalizeDeviceMAC(job.MAC, deviceStats.MAC)
	if macRes.Mismatch {
		return p.handleMACMismatch(job, "wave", macRes.Observed)
	}
	if macRes.Canonical != "" {
		deviceStats.MAC = macRes.Canonical
	}

	// Fetch config periodically (every 2 polls)
	if p.shouldFetchConfig(job.IP) {
		cfg, netCfg := p.fetchWaveConfig(client, baseURL, token)
		if cfg != nil {
			deviceStats.Config = cfg
		}
		if netCfg != nil {
			deviceStats.Network = netCfg
		}
	} else {
		// Preserve existing config from store
		if oldStats := func() *stats.DeviceStats {
			if s := p.store.GetByMAC(job.MAC); s != nil {
				return s
			}
			return p.store.Get(job.IP)
		}(); oldStats != nil {
			if oldStats.Config != nil {
				deviceStats.Config = oldStats.Config
			}
			if oldStats.Network != nil {
				deviceStats.Network = oldStats.Network
			}
		}
	}

	// Update GPS sync in config from live radio stats (gpsSyncState == 2 means synced)
	if deviceStats.Config != nil {
		// Reset and check all radio types for GPS sync
		deviceStats.Config.GPSSync = false
		if deviceStats.Wireless.Radio60GHz != nil && deviceStats.Wireless.Radio60GHz.GPSSyncState == 2 {
			deviceStats.Config.GPSSync = true
		} else if deviceStats.Wireless.RadioLTU != nil && deviceStats.Wireless.RadioLTU.GPSSyncState == 2 {
			deviceStats.Config.GPSSync = true
		} else if deviceStats.Wireless.Radio5GHz != nil && deviceStats.Wireless.Radio5GHz.GPSSyncState == 2 {
			deviceStats.Config.GPSSync = true
		}
		p.logDebug("pollDeviceWave %s: config mode=%s-%s ssid=%s waveai=%v sync=%v",
			job.IP, deviceStats.Config.Mode, deviceStats.Config.NetMode,
			deviceStats.Config.SSID, deviceStats.Config.WaveAI, deviceStats.Config.GPSSync)
	}

	// Phase 7: Wave peer discovery fallback (only when peers are missing on APs)
	// - When Ultra Debug is enabled and peers are missing, we probe candidate peer endpoints so we can
	//   see HTTP status/body in the debug bundle.
	// - When WavePeerFallback is enabled (feature flag), we also try to *use* the first endpoint that
	//   returns a non-empty peer list.
	isAP := false
	if deviceStats.Config != nil && deviceStats.Config.Mode == "ap" {
		isAP = true
	} else if strings.ToLower(job.Role) == "ap" {
		isAP = true
	}
	peersFromStats := len(peers) > 0
	fallbackAttempted := false
	fallbackUsed := false
	fallbackEndpoint := ""
	fallbackRawPeerCount := 0

	if !peersFromStats && isAP {
		if p.cfgSnapshot().wavePeerFallback {
			recordWavePeerFallbackAttempt()
			fallbackAttempted = true
			rawPeers, usedEndpoint, err := p.fetchWavePeersFallback(client, baseURL, token)
			fallbackEndpoint = usedEndpoint
			if err == nil {
				fallbackRawPeerCount = len(rawPeers)
			}
			if err == nil && len(rawPeers) > 0 {
				// Parse fallback peers using the same peer parser as /statistics.
				parsedPeers := make([]*stats.PeerStats, 0, len(rawPeers))
				ts := deviceStats.LastSeen
				if ts.IsZero() {
					ts = time.Now()
				}
				for _, peerObj := range rawPeers {
					pm, ok := peerObj.(map[string]any)
					if !ok {
						continue
					}
					peer := p.parsePeer(pm, job.Platform, nil)
					if peer == nil {
						continue
					}
					parsedPeers = append(parsedPeers, peer)
				}
				peers = parsedPeers
				fallbackUsed = len(peers) > 0
				if len(peers) > 0 && p.cfgSnapshot().debug {
					log.Printf("Wave peer fallback: device_id=%d endpoint=%s peers=%d", job.DeviceID, usedEndpoint, len(peers))
				}
			}
		} else if p.ultraDebug != nil && p.ultraDebug.IsEnabled(job.DeviceID) {
			// Probe candidate peer endpoints so Ultra Debug captures the HTTP status/body for diagnosis.
			p.probeWavePeerEndpoints(client, baseURL, token)
		}
	}
	if isAP {
		recordWavePeerSource(peersFromStats, fallbackUsed)
	}

	// Ultra Debug: attach a Wave parse report per poll so operators can see
	// band inference + slot mapping decisions and whether peer fallback was used.
	if p.ultraDebug != nil {
		enabledDevice := p.ultraDebug.IsEnabled(job.DeviceID)
		enabledHost := p.ultraDebug.IsHostEnabled(job.IP)
		if enabledDevice || enabledHost {
			// statsData is returned as raw JSON bytes; unmarshal a copy for the
			// debug report builder.
			var statsSnapshots []map[string]interface{}
			_ = json.Unmarshal(statsData, &statsSnapshots)

			peersStatsCount := 0
			for _, snap := range statsSnapshots {
				wirelessMap, ok := snap["wireless"].(map[string]interface{})
				if !ok {
					continue
				}
				if rawPeers, ok := wirelessMap["peers"].([]interface{}); ok {
					peersStatsCount = len(rawPeers)
					break
				}
			}

			report := buildWaveParseReport(
				job.DeviceID,
				job.IP,
				job.Platform,
				statsSnapshots,
				deviceStats,
				peersStatsCount,
				len(peers),
				isAP,
				fallbackAttempted,
				fallbackUsed,
				fallbackEndpoint,
				fallbackRawPeerCount,
			)

			entry := map[string]any{
				"time":        time.Now().UTC(),
				"type":        "wave_parse_report",
				"duration_ms": 0,
				"query": map[string]any{
					"method": "INTERNAL",
					"url":    "wave_parse_report",
				},
				"response": map[string]any{
					"status":        0,
					"body":          report,
					"body_encoding": "utf-8",
					"bytes":         len(report),
				},
			}

			if enabledDevice {
				p.ultraDebug.Record(job.DeviceID, entry)
			}
			if enabledHost {
				p.ultraDebug.RecordHost(job.IP, entry)
			}
		}
	}
	// Check if hostname changed before updating store
	oldStats := p.store.GetByMAC(job.MAC)
	if oldStats == nil {
		oldStats = p.store.Get(job.IP)
	}
	hostnameChanged := deviceStats.Hostname != "" && (oldStats == nil || oldStats.Hostname != deviceStats.Hostname)

	// Update memory store - returns true if state changed (offline->online)
	becameOnline := p.store.Update(job.IP, deviceStats)
	p.store.TrackStabilityStatus(job.IP, deviceStats.Hostname, stats.StatusOnline, deviceStats.Uptime)

	// Update peers first so AP websocket updates include peer_count/peers.
	// This is important for real-time STA updates in the UI.
	var ipChanges map[string]string
	if len(peers) > 0 {
		// Enrich peers with MAC from database when using IP fallback
		p.enrichPeersWithMAC(peers)
		ipChanges = p.store.UpdatePeers(deviceStats.MAC, job.IP, peers)
	} else {
		// No peers - still update peer count to 0 (and clear any previous peers)
		ipChanges = p.store.UpdatePeers(deviceStats.MAC, job.IP, nil)
	}

	// Broadcast stats update via WebSocket (after peer update)
	if p.wsHub != nil {
		p.wsHub.BroadcastStatsUpdate(int(job.DeviceID), deviceStats.MAC, job.IP, deviceStats)
	}

	// Determine role (AP vs STA) from the most reliable source available.
	//
	// Why: LTU stations include their AP in /statistics peers, and when those STAs are polled directly
	// (e.g., for firmware upgrades), WaveControl must NOT treat that peer list as "child STAs" or
	// downgrade a real AP to a STA.
	//
	// Precedence:
	//   1) LTU: infer from product/flavor (e.g., LTU-Rocket is always an AP)
	//   2) Config mode (Wave devices)
	//   3) Product/flavor heuristic (Wave devices when config isn't available)
	detectedRole := ""
	roleSource := ""
	if strings.EqualFold(job.Platform, "ltu") {
		if inf := stats.InferRoleFromIdentity(job.Platform, job.Product, job.Flavor); inf.Role != "" {
			detectedRole = inf.Role
			roleSource = inf.Source
		}
	}
	if detectedRole == "" && deviceStats.Config != nil && deviceStats.Config.Mode != "" {
		if r := stats.NormalizeRole(deviceStats.Config.Mode); r != "" {
			detectedRole = r
			roleSource = "config.mode"
		}
	}
	if detectedRole == "" {
		if inf := stats.InferRoleFromIdentity(job.Platform, job.Product, job.Flavor); inf.Role != "" {
			detectedRole = inf.Role
			roleSource = inf.Source
		}
	}
	if detectedRole != "" {
		p.logDebug("pollDeviceWave %s: role=%s (source=%s, jobRole=%s)", job.IP, detectedRole, roleSource, job.Role)
	}

	// Defensive cleanup: APs should never have a parent relationship. If a previous bad
	// association pass (or manual edits) left a parent_id/parent_mac on an AP, clear it
	// as soon as we positively identify the device as an AP.
	if detectedRole == "ap" {
		if res, err := p.db.Exec(`UPDATE devices SET parent_id = NULL, parent_mac = NULL WHERE id = $1 AND (parent_id IS NOT NULL OR parent_mac IS NOT NULL)`, job.DeviceID); err == nil {
			if rows, _ := res.RowsAffected(); rows > 0 {
				p.logDebug("pollDeviceWave %s: cleared stale parent for AP device %d", job.IP, job.DeviceID)
				if p.wsHub != nil {
					patch := map[string]interface{}{`id`: job.DeviceID, `parent_id`: nil, `parent_mac`: nil}
					p.wsHub.BroadcastDeviceUpdate(int(job.DeviceID), job.IP, patch)
				}
			}
		}
	}

	// Persist/update STA rows in DB (AP only). Run after broadcast to keep UI responsive.
	// NOTE: When peers is empty/nil for an AP, this call will mark all child STAs as offline
	// in the DB ("not_associated").
	if detectedRole == "ap" {
		p.updateSTAsInDB(job.DeviceID, peers, ipChanges)
	}

	// Update device role (AP vs STA) in the DB when our detection differs from the stored role.
	// This is important when a device was previously discovered as a STA via another poll's peer list,
	// but is later added directly (managed) and is actually configured/identified as an AP.
	if detectedRole != "" && detectedRole != job.Role {
		if detectedRole == "ap" {
			dbExecIgnoreCtx(p.db, dbCtxForJob(job, "wave_role_to_ap"), `UPDATE devices SET role = 'ap', parent_id = NULL, parent_mac = NULL WHERE id = $1`, job.DeviceID)
		} else {
			dbExecIgnoreCtx(p.db, dbCtxForJob(job, "wave_role_to_sta"), `UPDATE devices SET role = 'sta' WHERE id = $1`, job.DeviceID)
		}
		if p.wsHub != nil {
			// device_update websocket messages should contain an id so the UI can apply
			// the patch in-place without forcing a full /api/devices refresh.
			patch := map[string]interface{}{"id": job.DeviceID, "role": detectedRole}
			if detectedRole == "ap" {
				patch["parent_id"] = nil
				patch["parent_mac"] = nil
			}
			p.wsHub.BroadcastDeviceUpdate(int(job.DeviceID), job.IP, patch)
		}
	}

	// Only write to DB on state transition or hostname change (not every poll)
	if becameOnline {
		p.clearIdentityMismatch(job.DeviceID)
		// Device came online - update status in DB
		if hostnameChanged {
			dbExecIgnoreCtx(p.db, dbCtxForJob(job, "wave_mark_online"), `UPDATE devices SET status = 'online', status_reason = NULL, last_seen = NOW(), hostname = $2 WHERE id = $1`, job.DeviceID, deviceStats.Hostname)
		} else {
			dbExecIgnoreCtx(p.db, dbCtxForJob(job, "wave_mark_online"), `UPDATE devices SET status = 'online', status_reason = NULL, last_seen = NOW() WHERE id = $1`, job.DeviceID)
		}
	} else if hostnameChanged {
		// Already online but hostname changed
		dbExecIgnoreCtx(p.db, dbCtxForJob(job, "wave_update_hostname"), `UPDATE devices SET hostname = $2 WHERE id = $1`, job.DeviceID, deviceStats.Hostname)
	}
	// If already online and nothing changed, no DB write needed

	// Check if static info changed (firmware, additional hostname sources)
	p.checkStaticInfo(client, baseURL, token, job.DeviceID, job.MAC)

	return pollSuccess
}
