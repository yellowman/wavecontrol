package poller

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"time"
)

func (p *Poller) decryptSecret(value string) (string, error) {
	if value == "" || p.secretStore == nil {
		return value, nil
	}
	return p.secretStore.Decrypt(value)
}

func dedupePollerCredentials(in []Credential) []Credential {
	out := make([]Credential, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, c := range in {
		c.Username = strings.TrimSpace(c.Username)
		if c.Username == "" || c.Password == "" {
			continue
		}
		key := c.Username + "\x00" + c.Password
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, c)
	}
	return out
}

func (p *Poller) loadSettingCredentialPairs(prefix string) ([]Credential, error) {
	pairs := make([]Credential, 0, 3)
	for i := 1; i <= 3; i++ {
		var username, stored string
		userKey := fmt.Sprintf("%s_cred%d_user", prefix, i)
		passKey := fmt.Sprintf("%s_cred%d_pass", prefix, i)
		if err := p.db.QueryRow(`SELECT value FROM settings WHERE key=$1`, userKey).Scan(&username); err != nil && err != sql.ErrNoRows {
			return nil, fmt.Errorf("read %s: %w", userKey, err)
		}
		if err := p.db.QueryRow(`SELECT value FROM settings WHERE key=$1`, passKey).Scan(&stored); err != nil && err != sql.ErrNoRows {
			return nil, fmt.Errorf("read %s: %w", passKey, err)
		}
		if strings.TrimSpace(username) == "" || stored == "" {
			continue
		}
		password, err := p.decryptSecret(stored)
		if err != nil {
			return nil, fmt.Errorf("decrypt %s: %w", passKey, err)
		}
		pairs = append(pairs, Credential{Username: username, Password: password})
	}
	return dedupePollerCredentials(pairs), nil
}

func (p *Poller) loadLegacySettingCredentialPairs(prefix string) ([]Credential, error) {
	var username, stored string
	if err := p.db.QueryRow(`SELECT value FROM settings WHERE key=$1`, prefix+"_username").Scan(&username); err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if err := p.db.QueryRow(`SELECT value FROM settings WHERE key=$1`, prefix+"_passwords").Scan(&stored); err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	username = strings.TrimSpace(username)
	if username == "" || stored == "" {
		return nil, nil
	}
	plain, err := p.decryptSecret(stored)
	if err != nil {
		return nil, err
	}
	var passwords []string
	if err := json.Unmarshal([]byte(plain), &passwords); err != nil {
		return nil, fmt.Errorf("parse %s_passwords: %w", prefix, err)
	}
	pairs := make([]Credential, 0, len(passwords))
	for _, password := range passwords {
		pairs = append(pairs, Credential{Username: username, Password: password})
	}
	return dedupePollerCredentials(pairs), nil
}

func (p *Poller) loadCredentialTable() (apCreds, staCreds []Credential, err error) {
	var exists bool
	if err := p.db.QueryRow(`SELECT to_regclass('public.device_credentials') IS NOT NULL`).Scan(&exists); err != nil {
		return nil, nil, err
	}
	if !exists {
		return nil, nil, nil
	}
	rows, err := p.db.Query(`SELECT username, password, role FROM device_credentials WHERE enabled = true ORDER BY id`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var username, stored, role string
		if err := rows.Scan(&username, &stored, &role); err != nil {
			return nil, nil, err
		}
		password, err := p.decryptSecret(stored)
		if err != nil {
			return nil, nil, err
		}
		cred := Credential{Username: username, Password: password}
		switch strings.ToLower(strings.TrimSpace(role)) {
		case "sta":
			staCreds = append(staCreds, cred)
		case "", "ap":
			apCreds = append(apCreds, cred)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return dedupePollerCredentials(apCreds), dedupePollerCredentials(staCreds), nil
}

// watchConfig reloads config from settings table periodically
func (p *Poller) watchConfig(ctx context.Context) {
	// Load settings immediately on startup.
	//
	// Without this, the first settings load would only happen on the first
	// ticker tick (~60s), leaving a window after restart where settings such as
	// management_prefixes were effectively disabled.
	p.loadConfig()

	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.loadConfig()
		}
	}
}

func (p *Poller) loadConfig() {
	oldCfg := p.cfgSnapshot()

	// Poll interval
	newInterval := oldCfg.interval
	var intervalStr string
	if err := p.db.QueryRow(`SELECT value FROM settings WHERE key = 'poll_interval'`).Scan(&intervalStr); err == nil {
		if seconds, err := strconv.Atoi(intervalStr); err == nil && seconds > 0 {
			newInterval = time.Duration(seconds) * time.Second
		}
	} else if err != sql.ErrNoRows {
		log.Printf("Error loading poll_interval: %v", err)
	}

	// Credentials. Credential pairs are loaded intact and decrypted before use.
	// When a legacy device_credentials table exists, its entries are prepended;
	// the current settings-backed pairs remain the canonical configuration.
	newAPCreds := oldCfg.apCreds
	newSTACreds := oldCfg.staCreds

	settingsAP, credErr := p.loadSettingCredentialPairs("ap")
	if credErr == nil && len(settingsAP) == 0 {
		settingsAP, credErr = p.loadLegacySettingCredentialPairs("ap")
	}
	settingsSTA, staErr := p.loadSettingCredentialPairs("sta")
	if staErr == nil && len(settingsSTA) == 0 {
		settingsSTA, staErr = p.loadLegacySettingCredentialPairs("sta")
	}
	tableAP, tableSTA, tableErr := p.loadCredentialTable()

	if credErr != nil || staErr != nil || tableErr != nil {
		log.Printf("Error loading credentials: ap=%v sta=%v legacy_table=%v", credErr, staErr, tableErr)
	} else {
		apCreds := dedupePollerCredentials(append(tableAP, settingsAP...))
		staCreds := dedupePollerCredentials(append(tableSTA, settingsSTA...))
		if len(staCreds) == 0 {
			staCreds = append([]Credential(nil), apCreds...)
		}
		newAPCreds = apCreds
		newSTACreds = staCreds
	}

	// Management IP prefixes
	newMgmtPrefixes := oldCfg.mgmtPrefixes
	mgmtPrefixesLoaded := false

	var prefixesVal string
	if err := p.db.QueryRow(`SELECT value FROM settings WHERE key = 'management_prefixes'`).Scan(&prefixesVal); err == nil {
		mgmtPrefixesLoaded = true

		if prefixesVal == "" {
			newMgmtPrefixes = nil
		} else {
			var prefixList []string
			if err := json.Unmarshal([]byte(prefixesVal), &prefixList); err != nil {
				log.Printf("Error parsing management prefixes JSON: %v", err)
				newMgmtPrefixes = nil
			} else {
				prefixes := make([]*net.IPNet, 0, len(prefixList))
				for _, prefix := range prefixList {
					_, ipNet, err := net.ParseCIDR(prefix)
					if err != nil {
						log.Printf("Invalid CIDR prefix %s: %v", prefix, err)
						continue
					}
					prefixes = append(prefixes, ipNet)
				}
				newMgmtPrefixes = prefixes
			}
		}
	} else if err != sql.ErrNoRows {
		log.Printf("Error loading management_prefixes: %v", err)
	}

	// Wave feature flags
	newWavePeerFallback := oldCfg.wavePeerFallback
	var peerFallbackVal string
	if err := p.db.QueryRow(`SELECT value FROM settings WHERE key = 'wave_peer_fallback'`).Scan(&peerFallbackVal); err == nil && peerFallbackVal != "" {
		if enabled, err := strconv.ParseBool(peerFallbackVal); err == nil {
			newWavePeerFallback = enabled
		}
	} else if err != nil && err != sql.ErrNoRows {
		log.Printf("Error loading wave_peer_fallback: %v", err)
	}

	newWaveMLOMultiRadio := oldCfg.waveMLOMultiRadio
	var mloVal string
	if err := p.db.QueryRow(`SELECT value FROM settings WHERE key = 'wave_mlo_multi_radio'`).Scan(&mloVal); err == nil && mloVal != "" {
		if enabled, err := strconv.ParseBool(mloVal); err == nil {
			newWaveMLOMultiRadio = enabled
		}
	} else if err != nil && err != sql.ErrNoRows {
		log.Printf("Error loading wave_mlo_multi_radio: %v", err)
	}

	// Adjust worker count based on AP count in the DB.
	//
	// This intentionally uses direct DB queries instead of stats.Store methods so
	// settings reload does not depend on optional helper APIs on the in-memory
	// store. We count persisted AP records here because worker sizing is based on
	// inventory, not whatever subset is currently present in memory.
	newWorkerCount := oldCfg.workerCount
	var apCount int
	if err := p.db.QueryRow(`SELECT COUNT(*) FROM devices WHERE role = 'ap'`).Scan(&apCount); err != nil {
		log.Printf("Error counting AP devices for worker sizing: %v", err)
	} else if apCount > 0 {
		apsPerWorker := 8
		var apsPerWorkerVal string
		if err := p.db.QueryRow(`SELECT value FROM settings WHERE key = 'aps_per_worker'`).Scan(&apsPerWorkerVal); err == nil {
			if n, err := strconv.Atoi(strings.TrimSpace(apsPerWorkerVal)); err == nil && n > 0 {
				apsPerWorker = n
			}
		} else if err != sql.ErrNoRows {
			log.Printf("Error loading aps_per_worker: %v", err)
		}

		optimalWorkers := apCount / apsPerWorker
		if apCount%apsPerWorker != 0 {
			optimalWorkers++
		}

		// Ensure reasonable bounds
		if optimalWorkers < 4 {
			optimalWorkers = 4
		} else if optimalWorkers > 32 {
			optimalWorkers = 32
		}

		newWorkerCount = optimalWorkers
	}

	// Apply updates atomically
	p.configMu.Lock()
	p.interval = newInterval
	p.apCreds = newAPCreds
	p.staCreds = newSTACreds
	if mgmtPrefixesLoaded {
		p.mgmtPrefixes = newMgmtPrefixes
	}
	p.wavePeerFallback = newWavePeerFallback
	p.waveMLOMultiRadio = newWaveMLOMultiRadio
	p.workerCount = newWorkerCount
	p.configMu.Unlock()

	// Side effects after unlock
	if newInterval != oldCfg.interval {
		// Best-effort: ensure the latest interval wins even if the channel already has a pending value.
		select {
		case p.intervalChanged <- newInterval:
		default:
			select {
			case <-p.intervalChanged:
			default:
			}
			select {
			case p.intervalChanged <- newInterval:
			default:
			}
		}

		log.Printf("Poll interval changed: %v -> %v", oldCfg.interval, newInterval)
	}

	if mgmtPrefixesLoaded {
		if len(newMgmtPrefixes) > 0 {
			p.store.SetIPFilter(p.isAllowedIP)
			p.logDebug("Loaded %d management prefixes", len(newMgmtPrefixes))
		} else {
			p.store.SetIPFilter(nil)
		}
	}

	if newWavePeerFallback != oldCfg.wavePeerFallback {
		log.Printf("Wave peer fallback changed: %v -> %v", oldCfg.wavePeerFallback, newWavePeerFallback)
	}
	if newWaveMLOMultiRadio != oldCfg.waveMLOMultiRadio {
		log.Printf("Wave MLO multi-radio changed: %v -> %v", oldCfg.waveMLOMultiRadio, newWaveMLOMultiRadio)
	}
	if newWorkerCount != oldCfg.workerCount {
		log.Printf("Worker count changed: %d -> %d (auto)", oldCfg.workerCount, newWorkerCount)
	}
}

func parseInt(s string) int {
	var n int
	fmt.Sscanf(s, "%d", &n)
	return n
}

// Helper functions
func getFloat(m map[string]any, key string) float64 {
	if v, ok := m[key].(float64); ok {
		return v
	}
	return 0
}

func getString(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func getBool(m map[string]any, key string) bool {
	if v, ok := m[key].(bool); ok {
		return v
	}
	return false
}

func extractFlavor(firmware string) string {
	parts := strings.Split(firmware, ".")
	if len(parts) > 0 {
		prefix := strings.ToUpper(parts[0])
		switch prefix {
		// Wave flavors (includes AF60 which uses Wave API)
		case "GMC", "GMP", "MGMP", "GP", "MW":
			return prefix
		// LTU flavors (includes AF5XHD which uses LTU API)
		case "AFLTUROCKET", "AFLTU", "AF5XHD":
			return prefix
		// AirMAX prefixes (AC: XC/WA, M: XM/XW/TI)
		case "XC", "WA", "2XC", "2WA", "XM", "XW", "TI":
			return prefix
		// AirFiber prefixes (AF2/AF3/AF5/AF11 use AirMAX-style API)
		case "AF11", "AF5X", "AF5U", "AF5", "AF3X", "AF2X":
			return prefix
		}
	}
	return ""
}

// RuntimeConfig holds runtime-configurable poller settings
type RuntimeConfig struct {
	PollInterval int `json:"poll_interval"` // seconds
	ApsPerWorker int `json:"aps_per_worker"`
	WorkerCount  int `json:"worker_count"` // read-only, calculated
}

// GetRuntimeConfig returns current poller configuration
func (p *Poller) GetRuntimeConfig() RuntimeConfig {
	// Read current interval from DB settings
	var intervalStr, apsPerWorkerStr string
	cfgSnap := p.cfgSnapshot()
	interval := int(cfgSnap.interval.Seconds())
	apsPerWorker := 30

	if p.db.QueryRow(`SELECT value FROM settings WHERE key = 'poll_interval'`).Scan(&intervalStr) == nil {
		if v, err := strconv.Atoi(intervalStr); err == nil && v > 0 {
			interval = v
		}
	}
	if p.db.QueryRow(`SELECT value FROM settings WHERE key = 'aps_per_worker'`).Scan(&apsPerWorkerStr) == nil {
		if v, err := strconv.Atoi(apsPerWorkerStr); err == nil && v > 0 {
			apsPerWorker = v
		}
	}

	return RuntimeConfig{
		PollInterval: interval,
		ApsPerWorker: apsPerWorker,
		WorkerCount:  cfgSnap.workerCount,
	}
}

// UpdateRuntimeConfig updates poller configuration
func (p *Poller) UpdateRuntimeConfig(cfg RuntimeConfig) error {
	// Validate
	if cfg.PollInterval < 10 || cfg.PollInterval > 300 {
		return fmt.Errorf("poll_interval must be 10-300 seconds")
	}
	if cfg.ApsPerWorker < 5 || cfg.ApsPerWorker > 100 {
		return fmt.Errorf("aps_per_worker must be 5-100")
	}

	// Update settings in DB
	_, err := dbExecCtx(p.db, dbCtxForOp("update_runtime_config_poll_interval"), `INSERT INTO settings (key, value) VALUES ('poll_interval', $1) 
		ON CONFLICT (key) DO UPDATE SET value = $1`, strconv.Itoa(cfg.PollInterval))
	if err != nil {
		return err
	}

	_, err = dbExecCtx(p.db, dbCtxForOp("update_runtime_config_aps_per_worker"), `INSERT INTO settings (key, value) VALUES ('aps_per_worker', $1) 
		ON CONFLICT (key) DO UPDATE SET value = $1`, strconv.Itoa(cfg.ApsPerWorker))
	if err != nil {
		return err
	}

	// Apply interval change immediately
	newInterval := time.Duration(cfg.PollInterval) * time.Second
	oldInterval := p.cfgSnapshot().interval
	if newInterval != oldInterval {
		p.configMu.Lock()
		p.interval = newInterval
		p.configMu.Unlock()

		// Notify the poll loop if it's waiting (best-effort latest-wins)
		select {
		case p.intervalChanged <- newInterval:
		default:
			select {
			case <-p.intervalChanged:
			default:
			}
			select {
			case p.intervalChanged <- newInterval:
			default:
			}
		}
	}

	return nil
}

// extractFirmwareVersion extracts a clean version from a firmware string
// Examples:
//
//	"AFLTU.ar934x.v2.4.0.hash123.date" -> "v2.4.0"
//	"GMC.ipq5018.v4.1.0.deda4ab.251212.0922" -> "v4.1.0"
//	"v4.1.0" -> "v4.1.0"
func extractFirmwareVersion(firmware string) string {
	// Find "v" followed by version number
	idx := strings.Index(firmware, ".v")
	if idx > 0 {
		firmware = firmware[idx+1:] // Skip the dot, keep the v
	} else if !strings.HasPrefix(firmware, "v") {
		// No version prefix found
		return ""
	}

	// Parse version parts
	if strings.HasPrefix(firmware, "v") {
		parts := strings.Split(firmware, ".")
		var result []string
		for i, p := range parts {
			if i == 0 {
				// First part starts with v
				result = append(result, p)
				continue
			}
			// Keep numeric version parts, stop at hash/date
			if len(p) > 0 {
				// Check if it's a numeric version part or version suffix (like -beta)
				if p[0] >= '0' && p[0] <= '9' {
					// Check if it looks like a hash (6+ hex chars) or date (6 digits)
					if len(p) >= 6 && isHexString(p[:6]) {
						break
					}
					result = append(result, p)
				} else if strings.HasPrefix(parts[i-1], "v") || (i > 1 && len(result) > 0) {
					// Could be suffix like "-beta" attached to version
					break
				} else {
					break
				}
			}
		}
		if len(result) >= 2 {
			return strings.Join(result, ".")
		}
	}

	return ""
}

// isHexString checks if a string contains only hex characters
func isHexString(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// DeviceCredentials holds credentials for a device
type DeviceCredentials struct {
	Credentials []Credential
}

// GetCredentialsForDevice returns credentials to try for a device.
// For APs (parent_id IS NULL), returns AP creds first then STA creds.
// For STAs (has parent_id), returns STA creds first then AP creds.
func (p *Poller) GetCredentialsForDevice(deviceID int64) DeviceCredentials {
	// Check if device is an AP or STA
	var parentID sql.NullInt64
	var storedUser, storedPass sql.NullString
	p.db.QueryRow(`SELECT parent_id, username, password FROM devices WHERE id = $1`, deviceID).Scan(&parentID, &storedUser, &storedPass)

	isAP := !parentID.Valid

	cfg := p.cfgSnapshot()

	result := DeviceCredentials{}
	seen := make(map[string]bool) // key = "user:pass"

	// Add stored credentials first if present
	if storedUser.Valid && storedUser.String != "" && storedPass.Valid && storedPass.String != "" {
		if password, err := p.decryptSecret(storedPass.String); err == nil && password != "" {
			key := storedUser.String + ":" + password
			result.Credentials = append(result.Credentials, Credential{Username: storedUser.String, Password: password})
			seen[key] = true
		} else if err != nil {
			log.Printf("GetCredentialsForDevice %d: decrypt stored password: %v", deviceID, err)
		}
	}

	if isAP {
		// AP: try AP creds first, then STA creds
		for _, cred := range cfg.apCreds {
			key := cred.Username + ":" + cred.Password
			if !seen[key] {
				result.Credentials = append(result.Credentials, cred)
				seen[key] = true
			}
		}
		for _, cred := range cfg.staCreds {
			key := cred.Username + ":" + cred.Password
			if !seen[key] {
				result.Credentials = append(result.Credentials, cred)
				seen[key] = true
			}
		}
	} else {
		// STA: try STA creds first, then AP creds
		for _, cred := range cfg.staCreds {
			key := cred.Username + ":" + cred.Password
			if !seen[key] {
				result.Credentials = append(result.Credentials, cred)
				seen[key] = true
			}
		}
		for _, cred := range cfg.apCreds {
			key := cred.Username + ":" + cred.Password
			if !seen[key] {
				result.Credentials = append(result.Credentials, cred)
				seen[key] = true
			}
		}
	}

	return result
}

// GetAPCredentials returns AP credential pairs
func (p *Poller) GetAPCredentials() []Credential {
	cfg := p.cfgSnapshot()
	out := make([]Credential, len(cfg.apCreds))
	copy(out, cfg.apCreds)
	return out
}

// GetSTACredentials returns STA credential pairs
func (p *Poller) GetSTACredentials() []Credential {
	cfg := p.cfgSnapshot()
	out := make([]Credential, len(cfg.staCreds))
	copy(out, cfg.staCreds)
	return out
}

// ReloadSettings applies database-backed runtime settings immediately. It is
// safe to call after an administrator updates settings; the periodic watcher
// remains as a reconciliation fallback.
func (p *Poller) ReloadSettings() {
	p.loadConfig()
}
