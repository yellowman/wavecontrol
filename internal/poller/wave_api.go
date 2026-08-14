package poller

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

// httpStatusError carries enough info to differentiate Wave auth failures from non-Wave endpoints.
type httpStatusError struct {
	StatusCode  int
	URL         string
	ContentType string
	BodySnippet string
}

func (e *httpStatusError) Error() string {
	snippet := strings.TrimSpace(e.BodySnippet)
	// Keep logs bounded
	if len(snippet) > 256 {
		snippet = snippet[:256] + "..."
	}
	if e.ContentType != "" && snippet != "" {
		return fmt.Sprintf("status %d (%s): %s", e.StatusCode, e.ContentType, snippet)
	}
	if snippet != "" {
		return fmt.Sprintf("status %d: %s", e.StatusCode, snippet)
	}
	if e.ContentType != "" {
		return fmt.Sprintf("status %d (%s)", e.StatusCode, e.ContentType)
	}
	return fmt.Sprintf("status %d", e.StatusCode)
}

func (e *httpStatusError) likelyJSON() bool {
	ct := strings.ToLower(e.ContentType)
	if strings.Contains(ct, "application/json") || strings.Contains(ct, "application/problem+json") {
		return true
	}
	s := strings.TrimSpace(e.BodySnippet)
	return strings.HasPrefix(s, "{") || strings.HasPrefix(s, "[")
}

// login authenticates with the device using provided client
func (p *Poller) login(client *http.Client, baseURL, username, password string) (string, error) {
	body, _ := json.Marshal(map[string]string{
		"username": username,
		"password": password,
	})

	url := baseURL + "/api/v1.0/user/login"
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		ct := resp.Header.Get("Content-Type")
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		snippet := strings.TrimSpace(string(b))
		return "", &httpStatusError{
			StatusCode:  resp.StatusCode,
			URL:         url,
			ContentType: ct,
			BodySnippet: snippet,
		}
	}

	token := resp.Header.Get("x-auth-token")
	if token == "" {
		return "", fmt.Errorf("no token")
	}

	return token, nil
}

// fetchStats gets /api/v1.0/statistics using provided client
func (p *Poller) fetchStats(client *http.Client, baseURL, token string) ([]byte, error) {
	req, err := http.NewRequest("GET", baseURL+"/api/v1.0/statistics", nil)
	if err != nil {
		return nil, fmt.Errorf("create stats request: %w", err)
	}
	req.Header.Set("x-auth-token", token)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}

// FetchDeviceBackup retrieves config backup from a device using the same auth as polling
// Returns the backup file bytes or an error
// checkStaticInfo checks if device firmware/hostname changed
func (p *Poller) checkStaticInfo(client *http.Client, baseURL, token string, deviceID int64, expectedMAC string) {
	req, err := http.NewRequest("GET", baseURL+"/api/v1.0/device", nil)
	if err != nil {
		return
	}
	req.Header.Set("x-auth-token", token)

	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		io.Copy(io.Discard, resp.Body)
		return
	}

	var device map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&device); err != nil {
		return
	}

	ident, ok := device["identification"].(map[string]any)
	if !ok {
		return
	}

	// Wave devices expose both a full firmware identifier (often including flavor
	// like "GMC..."/"MGMP...") and a clean version string ("vX.Y.Z...").
	// Store the full identifier in devices.firmware and the clean version in devices.firmware_version.
	firmwareFull := getString(ident, "firmware")
	firmware := firmwareFull
	firmwareVersion := getString(ident, "firmwareVersion")
	if firmwareVersion == "" {
		// Some devices omit firmwareVersion; fall back to parsing the full identifier.
		firmwareVersion = extractFirmwareVersion(firmwareFull)
	}
	// If firmware is missing but firmwareVersion exists, keep something for display/debug.
	if firmware == "" && firmwareVersion != "" {
		firmware = firmwareVersion
	}
	product := getString(ident, "product")
	model := getString(ident, "model")
	mac := strings.ToLower(getString(ident, "mac"))

	hostname := getString(ident, "hostname")
	if hostname == "" {
		hostname = getString(ident, "name")
	}

	// Check host block for hostname
	if hostname == "" {
		if host, ok := device["host"].(map[string]any); ok {
			hostname = getString(host, "hostname")
			if hostname == "" {
				hostname = getString(host, "name")
			}
		}
	}

	// Check device block for name
	if hostname == "" {
		if dev, ok := device["device"].(map[string]any); ok {
			hostname = getString(dev, "name")
			if hostname == "" {
				hostname = getString(dev, "hostname")
			}
		}
	}

	// Check general block for name (common on Wave/LTU)
	if hostname == "" {
		if general, ok := device["general"].(map[string]any); ok {
			hostname = getString(general, "name")
			if hostname == "" {
				hostname = getString(general, "deviceName")
			}
		}
	}

	// Try /api/v1.0/system endpoint for device name (common on Wave/LTU)
	if hostname == "" {
		hostname = p.fetchSystemName(client, baseURL, token)
	}

	// Debug log if still no hostname
	if hostname == "" {
		p.logDebug("checkStaticInfo %s: no hostname found in /api/v1.0/device, keys: %v", baseURL, getMapKeys(device))
	}

	// Host IP without scheme (used for store lookup and log context)
	hostIP := strings.TrimPrefix(baseURL, "https://")

	// Determine Wave firmware flavor (gmc/mgmp/gmp) if possible.
	// This is required for upgrade selection. Prefer capabilities.supportedFirmwares
	// when available, otherwise fall back to parsing the full firmware identifier.
	flavor := ""
	if caps, ok := device["capabilities"].(map[string]any); ok {
		if devCaps, ok := caps["device"].(map[string]any); ok {
			if sfw, ok := devCaps["supportedFirmwares"].([]any); ok && len(sfw) > 0 {
				if first, ok := sfw[0].(map[string]any); ok {
					flavor = strings.ToLower(getString(first, "flavor"))
				}
			}
		}
	}
	if flavor == "" {
		flavor = extractFlavor(firmwareFull)
	}

	// Canonicalize MAC identity.
	// The DB/job MAC is authoritative. We should never silently mutate a device's
	// identity based on a different interface MAC returned by the API.
	// If the device reports a different MAC here, we treat it as a mismatch and
	// skip MAC updates (but we still update other static fields).
	macRes := canonicalizeDeviceMAC(expectedMAC, mac)
	if macRes.Mismatch {
		// Log once; do not update the DB MAC.
		key := fmt.Sprintf("%d|%s|%s", deviceID, macRes.Expected, strings.Join(macRes.Observed, ","))
		if _, loaded := macMismatchWarned.LoadOrStore(key, struct{}{}); !loaded {
			log.Printf("WARN: WAVE %s id=%d: static info MAC mismatch: expected=%s observed=%s; skipping MAC update", hostIP, deviceID, macRes.Expected, strings.Join(macRes.Observed, ","))
		}
		mac = ""
	} else if macRes.Canonical != "" {
		mac = macRes.Canonical
	}

	// Enforce DB column limits (avoid pq: value too long for type... errors)
	firmware = truncateForDB("firmware", hostIP, deviceID, firmware, 128)
	firmwareVersion = truncateForDB("firmware_version", hostIP, deviceID, firmwareVersion, 32)
	hostname = truncateForDB("hostname", hostIP, deviceID, hostname, 128)
	product = truncateForDB("product", hostIP, deviceID, product, 64)
	model = truncateForDB("model", hostIP, deviceID, model, 32)
	mac = truncateForDB("mac", hostIP, deviceID, mac, 17)
	flavor = truncateForDB("flavor", hostIP, deviceID, flavor, 16)

	macConflict := false

	// Detect MAC conflicts to avoid unique constraint spam.
	// If the reported MAC already belongs to another device record, we skip updating MAC
	// but still update other static fields, and log a warning once with context.
	if mac != "" {
		var conflictID int64
		var conflictIP, conflictHost sql.NullString
		err := p.db.QueryRow(`
			SELECT id, host(ip_address), hostname
			FROM devices
			WHERE mac = $1 AND id <> $2
			LIMIT 1
		`, mac, deviceID).Scan(&conflictID, &conflictIP, &conflictHost)
		if err == nil {
			macConflict = true
			key := fmt.Sprintf("%d|%s", deviceID, mac)
			if _, loaded := macConflictWarned.LoadOrStore(key, struct{}{}); !loaded {
				log.Printf("WARN: device %s id=%d attempted to claim MAC %s already used by device id=%d ip=%s hostname=%s; skipping MAC update",
					hostIP, deviceID, mac, conflictID, conflictIP.String, conflictHost.String)
			}
		}
	}

	// Update hostname, firmware, product, model if changed.
	// Note: firmware contains the clean version (e.g., "v2.4.0.00017") for consistent display.
	// MAC update is conditional: only update if not already owned by another device record.
	// Update hostname/firmware/product/model/flavor if changed.
	// MAC update is conditional: only update if not already owned by another device record.
	res, err := dbExecCtx(p.db, dbCtxForMAC(mac, hostIP, "wave_static_info_update", deviceID), `
		UPDATE devices SET
			firmware = COALESCE(NULLIF($1, ''), firmware),
			firmware_version = COALESCE(NULLIF($2, ''), firmware_version),
			hostname = COALESCE(NULLIF($3, ''), hostname),
			product = COALESCE(NULLIF($4, ''), product),
			model = COALESCE(NULLIF($5, ''), model),
			mac = CASE
				WHEN NULLIF($6, '') IS NULL THEN mac
				WHEN EXISTS (SELECT 1 FROM devices d2 WHERE d2.mac = $6 AND d2.id <> $8) THEN mac
				ELSE $6
			END,
			flavor = COALESCE(NULLIF($7, ''), flavor)
		WHERE id = $8 AND (
			firmware IS DISTINCT FROM $1 OR
			firmware_version IS DISTINCT FROM $2 OR
			hostname IS DISTINCT FROM $3 OR
			product IS DISTINCT FROM $4 OR
			model IS DISTINCT FROM $5 OR
			(NULLIF($7, '') IS NOT NULL AND flavor IS DISTINCT FROM $7) OR
			(
				NULLIF($6, '') IS NOT NULL
				AND NOT EXISTS (SELECT 1 FROM devices d2 WHERE d2.mac = $6 AND d2.id <> $8)
				AND mac IS DISTINCT FROM $6
			)
		)
	`, firmware, firmwareVersion, hostname, product, model, mac, flavor, deviceID)
	if err != nil {
		p.logDebug("checkStaticInfo %s id=%d: DB update error: %v", hostIP, deviceID, err)
		return
	}

	// Propagate static DB changes to UI without forcing a full refresh
	if res != nil && p.wsHub != nil {
		if rows, _ := res.RowsAffected(); rows > 0 {
			patch := map[string]any{
				"id":               deviceID,
				"firmware":         firmware,
				"firmware_version": firmwareVersion,
				"hostname":         hostname,
				"product":          product,
				"model":            model,
				"flavor":           flavor,
			}
			if !macConflict && mac != "" {
				patch["mac"] = mac
			}
			p.wsHub.BroadcastDeviceUpdate(int(deviceID), hostIP, patch)
		}
	}

}

// fetchSystemName gets device name from /api/v1.0/system endpoint
func (p *Poller) fetchSystemName(client *http.Client, baseURL, token string) string {
	req, err := http.NewRequest("GET", baseURL+"/api/v1.0/system", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("x-auth-token", token)

	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		io.Copy(io.Discard, resp.Body)
		return ""
	}

	var system map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&system); err != nil {
		return ""
	}

	// Try common locations for device name
	// Top-level fields
	if name := getString(system, "name"); name != "" {
		return name
	}
	if name := getString(system, "hostname"); name != "" {
		return name
	}
	if name := getString(system, "deviceName"); name != "" {
		return name
	}

	// Nested in 'general' block (common on Wave/LTU)
	if general, ok := system["general"].(map[string]any); ok {
		if name := getString(general, "name"); name != "" {
			return name
		}
		if name := getString(general, "deviceName"); name != "" {
			return name
		}
		if name := getString(general, "hostname"); name != "" {
			return name
		}
	}

	// Nested in 'device' block
	if device, ok := system["device"].(map[string]any); ok {
		if name := getString(device, "name"); name != "" {
			return name
		}
		if name := getString(device, "deviceName"); name != "" {
			return name
		}
	}

	// Nested in 'identification' block
	if ident, ok := system["identification"].(map[string]any); ok {
		if name := getString(ident, "hostname"); name != "" {
			return name
		}
		if name := getString(ident, "name"); name != "" {
			return name
		}
	}

	// Debug: log what we received if we couldn't find a name
	p.logDebug("fetchSystemName %s: no device name found in system response, keys: %v", baseURL, getMapKeys(system))

	return ""
}

func getMapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// updateSTAsInDB ensures STAs exist in database with site inheritance.
// ipChanges is a map of MAC -> newIP for STAs that had their IP updated in the memory store.
// Only updates DB IP for MACs in ipChanges (to avoid redundant writes).
