package poller

import (
	"database/sql"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/lib/pq"
	"github.com/yellowman/wavecontrol/internal/stats"
	"github.com/yellowman/wavecontrol/internal/websocket"
)

// updateChildrenStatus updates status of all STAs associated with an AP
// When an AP has issues, its children should inherit the same status
func (p *Poller) updateChildrenStatus(apID int64, status string) {
	// Update all devices that have this AP as their parent
	result, err := dbExecCtx(p.db, dbCtxForDevice(apID, "update_children_status"), `UPDATE devices SET status = $1 WHERE parent_id = $2`, status, apID)
	if err != nil {
		p.logDebug("updateChildrenStatus: failed to update children of AP %d: %v", apID, err)
		return
	}

	if rows, _ := result.RowsAffected(); rows > 0 {
		p.logDebug("updateChildrenStatus: updated %d children of AP %d to status '%s'", rows, apID, status)
	}
}
func (p *Poller) updateSTAsInDB(apID int64, peers []*stats.PeerStats, ipChanges map[string]string) {
	// Get AP's MAC, site_id, SSID and platform for inheritance
	var apMAC sql.NullString
	var apSiteID sql.NullInt64
	var apSSID sql.NullString
	var apPlatform sql.NullString
	p.db.QueryRow(`SELECT lower(mac), site_id, ssid, platform FROM devices WHERE id = $1`, apID).Scan(&apMAC, &apSiteID, &apSSID, &apPlatform)
	if apMAC.Valid && apMAC.String != "" {
		apMAC.String = stats.NormalizeMAC(apMAC.String)
	}

	// Track associated peer MACs for this poll so we can mark missing STAs offline.
	peerMACs := make([]string, 0, len(peers))

	for _, peer := range peers {
		// MAC is authoritative - skip peers without MAC
		hasMAC := peer.MAC != ""
		if !hasMAC {
			continue
		}

		// Normalize MAC for consistent lookup
		peerMAC := stats.NormalizeMAC(peer.MAC)
		peerMACs = append(peerMACs, peerMAC)

		// Check if store updated the IP for this MAC
		// If so, we should update DB. If not, don't touch IP in DB.
		newIP, ipChanged := ipChanges[peerMAC]

		if ipChanged {
			p.logDebug("STA %s: syncing IP %s to database (store updated)", peerMAC, newIP)
		}

		// Extract flavor/platform from raw firmware when available.
		// Wave peers often report a clean firmwareVersion ("vX.Y.Z....") which
		// does NOT include the flavor prefix (gmc/mgmp/gmp). Keep using the clean
		// version for display/storage, but detect flavor from the full string.
		fwDetect := peer.Firmware
		if peer.FirmwareFull != "" {
			fwDetect = peer.FirmwareFull
		}
		flavor := extractFlavor(fwDetect)
		platform := ""
		fwLower := strings.ToLower(fwDetect)
		if strings.HasPrefix(fwLower, "afltu") || strings.HasPrefix(fwLower, "af5xhd") {
			platform = "ltu"
		} else if strings.HasPrefix(fwLower, "xc.") || strings.HasPrefix(fwLower, "2xc.") ||
			strings.HasPrefix(fwLower, "wa.") || strings.HasPrefix(fwLower, "2wa.") ||
			strings.HasPrefix(fwLower, "xm.") || strings.HasPrefix(fwLower, "xw.") ||
			strings.HasPrefix(fwLower, "ti.") {
			platform = "airmax"
		} else if strings.HasPrefix(fwLower, "af11.") || strings.HasPrefix(fwLower, "af5x.") ||
			strings.HasPrefix(fwLower, "af5u.") || strings.HasPrefix(fwLower, "af5.") ||
			strings.HasPrefix(fwLower, "af3x.") || strings.HasPrefix(fwLower, "af2x.") {
			platform = "airmax" // AirFiber with prefixed firmware
		} else {
			// Check model name for AirFiber with plain "v4.x" firmware
			modelLower := strings.ToLower(peer.Model)
			if strings.Contains(modelLower, "airfiber") || strings.HasPrefix(modelLower, "af") {
				platform = "airmax"
			}
		}

		// If platform couldn't be determined from firmware/model, inherit from parent AP
		// This handles "stuck" STAs that report empty remote data
		if platform == "" {
			if apPlatform.Valid && apPlatform.String != "" {
				platform = apPlatform.String
				p.logDebug("STA %s: inheriting platform %q from parent AP", peerMAC, platform)
			} else {
				platform = "wave" // Final fallback
			}
		}

		// Check if device exists - lookup by MAC
		var existingID sql.NullInt64
		var existingParentID sql.NullInt64
		var existingParentMAC sql.NullString
		var existingSSID sql.NullString
		var existingSiteID sql.NullInt64
		var existingIP sql.NullString
		var existingRole sql.NullString
		var existingManaged sql.NullBool

		err := p.db.QueryRow(`
			SELECT id, parent_id, lower(parent_mac), ssid, site_id, host(ip_address), role, managed
			FROM devices WHERE lower(mac) = $1
		`, peerMAC).Scan(&existingID, &existingParentID, &existingParentMAC, &existingSSID, &existingSiteID, &existingIP, &existingRole, &existingManaged)
		if existingParentMAC.Valid && existingParentMAC.String != "" {
			existingParentMAC.String = stats.NormalizeMAC(existingParentMAC.String)
		}

		if err == nil {
			// Device exists in DB

			// Seed memory store from DB if needed (important after restart)
			// If DB has a valid management IP that the store doesn't have, use it
			storeIP := p.store.GetIPByMAC(peerMAC)
			if existingIP.Valid && existingIP.String != "" && storeIP == "" {
				// DB has IP, store doesn't - seed from DB if it passes filter
				if p.isAllowedIP(existingIP.String) {
					if p.store.SetIPForMAC(peerMAC, existingIP.String) {
						p.logDebug("STA %s: seeded memory store with DB IP %s", peerMAC, existingIP.String)
					}
				}
			}

			// Check for role or SSID change
			//
			// Safety: never demote or re-parent a device already known as an AP based only on
			// being seen as a "peer" under some other device. We have seen cases where a STA
			// poll can erroneously surface its AP as a peer, and this logic would otherwise
			// convert the AP into a STA (creating parent loops and causing the AP to stop being
			// directly polled).
			existingRoleNorm := stats.NormalizeRole(existingRole.String)
			if existingRole.Valid && existingRoleNorm == "ap" {
				// Defensive cleanup: APs should never have a parent relationship.
				if existingParentID.Valid || (existingParentMAC.Valid && existingParentMAC.String != "") {
					dbExecIgnoreCtx(p.db, dbCtxForMAC(peerMAC, peer.IP, "ap_peer_clear_parent", existingID.Int64), `
						UPDATE devices SET parent_id = NULL, parent_mac = NULL WHERE id = $1
					`, existingID.Int64)
				}
				p.logDebug("Ignoring STA association for %s: device is role=ap in DB (possible loop)", peerMAC)
				continue
			}

			wasStandalone := !existingParentID.Valid // parent_id NULL means it was standalone (AP or manually added)

			// Determine if we should clear site_id
			shouldClearSite := false
			apChanged := false

			// Role change: was standalone, now STA (has parent_id)
			if wasStandalone {
				shouldClearSite = true
				log.Printf("Device %s (%s) is now a STA of AP %d - will stop direct polling", peerMAC, peer.IP, apID)
			}

			// AP change: STA moved to different AP (roaming or reconnect)
			if existingParentID.Valid && existingParentID.Int64 != apID {
				apChanged = true
				log.Printf("STA %s (%s) moved from AP %d (%s) to AP %d (%s)",
					peerMAC, peer.Hostname, existingParentID.Int64, existingParentMAC.String, apID, apMAC.String)
			}

			// SSID change: moved to different network
			if existingSSID.Valid && apSSID.Valid && existingSSID.String != apSSID.String {
				shouldClearSite = true
				p.logDebug("Device %s changed SSID from %s to %s, clearing site",
					peerMAC, existingSSID.String, apSSID.String)
			}

			staHost := peer.IP
			if ipChanged {
				staHost = newIP
			} else if existingIP.Valid && existingIP.String != "" {
				staHost = existingIP.String
			}

			// Truncate DB-bound fields to schema limits (avoid varchar errors)
			hostname := truncateForDB("hostname", staHost, existingID.Int64, peer.Hostname, 128)
			model := truncateForDB("model", staHost, existingID.Int64, peer.Model, 32)
			fw := truncateForDB("firmware", staHost, existingID.Int64, peer.Firmware, 128)
			plat := truncateForDB("platform", staHost, existingID.Int64, platform, 16)
			flv := truncateForDB("flavor", staHost, existingID.Int64, flavor, 16)
			ssid := ""
			if apSSID.Valid {
				ssid = apSSID.String
			}
			ssid = truncateForDB("ssid", staHost, existingID.Int64, ssid, 64)

			if shouldClearSite {
				// Clear site on role/SSID change - device was repurposed
				if ipChanged {
					dbExecIgnoreCtx(p.db, dbCtxForMAC(peerMAC, staHost, "sta_update_clear_site", existingID.Int64), `
						UPDATE devices SET
							ip_address = $1,
							hostname = COALESCE(NULLIF($2, ''), hostname),
							model = COALESCE(NULLIF($3, ''), model),
							platform = COALESCE(NULLIF($4, ''), platform),
							flavor = COALESCE(NULLIF($5, ''), flavor),
							firmware = COALESCE(NULLIF($6, ''), firmware),
							parent_id = $7,
							parent_mac = $8,
							ssid = COALESCE(NULLIF($9, ''), ssid),
							role = 'sta',
							site_id = NULL,
							status = 'online',
							status_reason = NULL,
							last_seen = NOW()
						WHERE lower(mac) = $10
					`, newIP, hostname, model, plat, flv, fw, apID, apMAC.String, ssid, peerMAC)
				} else {
					dbExecIgnoreCtx(p.db, dbCtxForMAC(peerMAC, staHost, "sta_update_clear_site", existingID.Int64), `
						UPDATE devices SET
							hostname = COALESCE(NULLIF($1, ''), hostname),
							model = COALESCE(NULLIF($2, ''), model),
							platform = COALESCE(NULLIF($3, ''), platform),
							flavor = COALESCE(NULLIF($4, ''), flavor),
							firmware = COALESCE(NULLIF($5, ''), firmware),
							parent_id = $6,
							parent_mac = $7,
							ssid = COALESCE(NULLIF($8, ''), ssid),
							role = 'sta',
							site_id = NULL,
							status = 'online',
							status_reason = NULL,
							last_seen = NOW()
						WHERE lower(mac) = $9
					`, hostname, model, plat, flv, fw, apID, apMAC.String, ssid, peerMAC)
				}
			} else {
				// Normal update - inherit site from AP if STA has no site
				newSiteID := existingSiteID
				if !existingSiteID.Valid && apSiteID.Valid {
					newSiteID = apSiteID // Inherit from AP
				}
				if ipChanged {
					dbExecIgnoreCtx(p.db, dbCtxForMAC(peerMAC, staHost, "sta_update_normal", existingID.Int64), `
						UPDATE devices SET
							ip_address = $1,
							hostname = COALESCE(NULLIF($2, ''), hostname),
							model = COALESCE(NULLIF($3, ''), model),
							platform = COALESCE(NULLIF($4, ''), platform),
							flavor = COALESCE(NULLIF($5, ''), flavor),
							firmware = COALESCE(NULLIF($6, ''), firmware),
							parent_id = $7,
							parent_mac = $8,
							ssid = COALESCE(NULLIF($9, ''), ssid),
							site_id = COALESCE(site_id, $10),
							role = 'sta',
							status = 'online',
							status_reason = NULL,
							last_seen = NOW()
						WHERE lower(mac) = $11
					`, newIP, hostname, model, plat, flv, fw, apID, apMAC.String, ssid, newSiteID, peerMAC)
				} else {
					dbExecIgnoreCtx(p.db, dbCtxForMAC(peerMAC, staHost, "sta_update_normal", existingID.Int64), `
						UPDATE devices SET
							hostname = COALESCE(NULLIF($1, ''), hostname),
							model = COALESCE(NULLIF($2, ''), model),
							platform = COALESCE(NULLIF($3, ''), platform),
							flavor = COALESCE(NULLIF($4, ''), flavor),
							firmware = COALESCE(NULLIF($5, ''), firmware),
							parent_id = $6,
							parent_mac = $7,
							ssid = COALESCE(NULLIF($8, ''), ssid),
							site_id = COALESCE(site_id, $9),
							role = 'sta',
							status = 'online',
							status_reason = NULL,
							last_seen = NOW()
						WHERE lower(mac) = $10
					`, hostname, model, plat, flv, fw, apID, apMAC.String, ssid, newSiteID, peerMAC)
				}
			}

			// Broadcast AP change via WebSocket so UI updates hierarchy
			if apChanged && p.wsHub != nil && existingID.Valid {
				p.wsHub.Broadcast(websocket.Message{
					Type: websocket.MsgDeviceUpdate,
					// Use the effective STA host IP (management IP if filtered) so the
					// client can reliably match the device row.
					DeviceIP:  staHost,
					Timestamp: time.Now().Unix(),
					Data: map[string]any{
						"id":         existingID.Int64,
						"parent_id":  apID,
						"parent_mac": apMAC.String,
					},
				})
			}
		} else if errors.Is(err, sql.ErrNoRows) {
			// New device - get the effective IP from the store
			// (which has already applied management prefix filtering)
			effectiveIP := p.store.GetIPByMAC(peerMAC)

			// When management prefixes are configured, NEVER insert a new device with an
			// untrusted/non-management IP. This is explicitly required by the
			// "Management IP Prefixes" behavior documented in README/SPEC.
			ipToStore := effectiveIP
			if ipToStore == "" {
				if len(p.cfgSnapshot().mgmtPrefixes) > 0 {
					// Peer-reported IP is either empty or outside allowed management ranges.
					// Skip inserting this STA until we learn a valid management IP.
					p.logDebug("Auto-discovered new STA: %s has no management-prefix IP (peer reported %q); skipping insert", peerMAC, peer.IP)
					continue
				}
				// No prefixes configured (backward compatible): accept any peer IP.
				ipToStore = peer.IP
			}
			if ipToStore == "" {
				p.logDebug("Auto-discovered new STA: %s has no usable IP; skipping insert", peerMAC)
				continue
			}

			staHost := ipToStore
			// Truncate DB-bound fields to schema limits
			hostname := truncateForDBMAC("hostname", staHost, peerMAC, peer.Hostname, 128)
			model := truncateForDBMAC("model", staHost, peerMAC, peer.Model, 32)
			fw := truncateForDBMAC("firmware", staHost, peerMAC, peer.Firmware, 128)
			plat := truncateForDBMAC("platform", staHost, peerMAC, platform, 16)
			flv := truncateForDBMAC("flavor", staHost, peerMAC, flavor, 16)
			ssid := ""
			if apSSID.Valid {
				ssid = apSSID.String
			}
			ssid = truncateForDBMAC("ssid", staHost, peerMAC, ssid, 64)

			// Upsert STA by MAC to avoid repeated unique-violation spam.
			// Only update ip_address on conflict when the store reported an IP change.
			inserted := false
			var newID int64
			ctx := dbCtxForMAC(peerMAC, staHost, "sta_upsert", 0)
			if ipChanged {
				err = dbQueryRowCtx(p.db, ctx, `
					INSERT INTO devices (mac, ip_address, hostname, model, platform, flavor, firmware, parent_id, parent_mac, ssid, site_id, role, alertable, status, status_reason, last_seen)
					VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 'sta', FALSE, 'online', NULL, NOW())
					ON CONFLICT (mac) DO UPDATE SET
							ip_address = CASE WHEN devices.role = 'ap' THEN devices.ip_address ELSE EXCLUDED.ip_address END,
							hostname = CASE WHEN devices.role = 'ap' THEN devices.hostname ELSE COALESCE(NULLIF(EXCLUDED.hostname, ''), devices.hostname) END,
							model = CASE WHEN devices.role = 'ap' THEN devices.model ELSE COALESCE(NULLIF(EXCLUDED.model, ''), devices.model) END,
							platform = CASE WHEN devices.role = 'ap' THEN devices.platform ELSE COALESCE(NULLIF(EXCLUDED.platform, ''), devices.platform) END,
							flavor = CASE WHEN devices.role = 'ap' THEN devices.flavor ELSE COALESCE(NULLIF(EXCLUDED.flavor, ''), devices.flavor) END,
							firmware = CASE WHEN devices.role = 'ap' THEN devices.firmware ELSE COALESCE(NULLIF(EXCLUDED.firmware, ''), devices.firmware) END,
							parent_id = CASE WHEN devices.role = 'ap' THEN NULL ELSE EXCLUDED.parent_id END,
							parent_mac = CASE WHEN devices.role = 'ap' THEN NULL ELSE EXCLUDED.parent_mac END,
							ssid = CASE WHEN devices.role = 'ap' THEN devices.ssid ELSE COALESCE(NULLIF(EXCLUDED.ssid, ''), devices.ssid) END,
							site_id = CASE WHEN devices.role = 'ap' THEN devices.site_id ELSE COALESCE(devices.site_id, EXCLUDED.site_id) END,
							role = CASE WHEN devices.role = 'ap' THEN devices.role ELSE 'sta' END,
							status = CASE WHEN devices.role = 'ap' THEN devices.status ELSE 'online' END,
							status_reason = CASE WHEN devices.role = 'ap' THEN devices.status_reason ELSE NULL END,
							last_seen = CASE WHEN devices.role = 'ap' THEN devices.last_seen ELSE NOW() END
					RETURNING id, (xmax = 0) AS inserted
				`, []any{&newID, &inserted}, peerMAC, newIP, hostname, model, plat, flv, fw, apID, apMAC.String, ssid, apSiteID)
			} else {
				err = dbQueryRowCtx(p.db, ctx, `
					INSERT INTO devices (mac, ip_address, hostname, model, platform, flavor, firmware, parent_id, parent_mac, ssid, site_id, role, alertable, status, status_reason, last_seen)
					VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 'sta', FALSE, 'online', NULL, NOW())
					ON CONFLICT (mac) DO UPDATE SET
							hostname = CASE WHEN devices.role = 'ap' THEN devices.hostname ELSE COALESCE(NULLIF(EXCLUDED.hostname, ''), devices.hostname) END,
							model = CASE WHEN devices.role = 'ap' THEN devices.model ELSE COALESCE(NULLIF(EXCLUDED.model, ''), devices.model) END,
							platform = CASE WHEN devices.role = 'ap' THEN devices.platform ELSE COALESCE(NULLIF(EXCLUDED.platform, ''), devices.platform) END,
							flavor = CASE WHEN devices.role = 'ap' THEN devices.flavor ELSE COALESCE(NULLIF(EXCLUDED.flavor, ''), devices.flavor) END,
							firmware = CASE WHEN devices.role = 'ap' THEN devices.firmware ELSE COALESCE(NULLIF(EXCLUDED.firmware, ''), devices.firmware) END,
							parent_id = CASE WHEN devices.role = 'ap' THEN NULL ELSE EXCLUDED.parent_id END,
							parent_mac = CASE WHEN devices.role = 'ap' THEN NULL ELSE EXCLUDED.parent_mac END,
							ssid = CASE WHEN devices.role = 'ap' THEN devices.ssid ELSE COALESCE(NULLIF(EXCLUDED.ssid, ''), devices.ssid) END,
							site_id = CASE WHEN devices.role = 'ap' THEN devices.site_id ELSE COALESCE(devices.site_id, EXCLUDED.site_id) END,
							role = CASE WHEN devices.role = 'ap' THEN devices.role ELSE 'sta' END,
							status = CASE WHEN devices.role = 'ap' THEN devices.status ELSE 'online' END,
							status_reason = CASE WHEN devices.role = 'ap' THEN devices.status_reason ELSE NULL END,
							last_seen = CASE WHEN devices.role = 'ap' THEN devices.last_seen ELSE NOW() END
					RETURNING id, (xmax = 0) AS inserted
				`, []any{&newID, &inserted}, peerMAC, ipToStore, hostname, model, plat, flv, fw, apID, apMAC.String, ssid, apSiteID)
			}

			if err != nil {
				p.logDebug("Failed to upsert STA %s: %v", peerMAC, err)
				continue
			}

			p.logDebug("STA %s upserted with ID %d (inserted=%v)", peerMAC, newID, inserted)

			// Only broadcast new-device events. Existing STA rows are updated frequently and
			// should not trigger expensive full client refreshes.
			if p.wsHub != nil && inserted {
				p.wsHub.Broadcast(websocket.Message{
					Type:      websocket.MsgDeviceAdd,
					DeviceIP:  staHost,
					Timestamp: time.Now().Unix(),
					Data: map[string]any{
						"id":         newID,
						"mac":        peerMAC,
						"ip_address": staHost,
						"hostname":   hostname,
						"model":      model,
						"platform":   plat,
						"firmware":   fw,
						"parent_id":  apID,
						"parent_mac": apMAC.String,
						"status":     "online",
					},
				})
			}
		} else {
			// Lookup failed for reasons other than "no rows". Log with context and skip insert.
			logDBExecError(dbCtxForMAC(peerMAC, peer.IP, "sta_lookup", 0), err, `
				SELECT id, parent_id, lower(parent_mac), ssid, site_id, host(ip_address)
				FROM devices WHERE lower(mac) = $1
			`, []any{peerMAC}, nil)
			continue
		}
	}

	// Any STA previously associated with this AP but missing from the current peer list is
	// treated as disconnected. This prevents "unknown" from persisting indefinitely.
	p.markMissingSTAsOffline(apID, peerMACs)
}

func (p *Poller) markMissingSTAsOffline(apID int64, associatedMACs []string) {
	ctx := dbCtxForDevice(apID, "mark_missing_stas_offline")

	// Use a case-insensitive match for safety when legacy rows have uppercase MACs.
	// If associatedMACs is empty, this will mark all child STAs as offline which is
	// correct when an AP reports no peers.
	_, err := dbExecCtx(p.db, ctx, `
		UPDATE devices
		SET status = 'offline',
		    status_reason = 'not_associated'
		WHERE parent_id = $1
		  AND role = 'sta'
		  AND NOT (lower(mac) = ANY($2::text[]))
		  AND status <> 'offline'
	`, apID, pq.Array(associatedMACs))
	_ = err // dbExecCtx already logs errors with full query + args + context.
}
