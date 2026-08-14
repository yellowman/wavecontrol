package poller

import (
	"context"
	"crypto/tls"
	"database/sql"
	"net/http"
	"time"

	"github.com/yellowman/wavecontrol/internal/airmax"

	"github.com/yellowman/wavecontrol/internal/udebug"
)

// drilldownPoller runs a separate polling loop for custom drilldown lists
// It only polls hosts that are NOT already APs being polled by the main loop
func (p *Poller) drilldownPoller(ctx context.Context) {
	// Check every 10 seconds for lists that need polling
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	// Track last poll time per list
	lastPoll := make(map[int]time.Time)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.pollDrilldownLists(lastPoll)
		}
	}
}

// pollDrilldownLists checks all enabled drilldown lists and polls if interval elapsed
func (p *Poller) pollDrilldownLists(lastPoll map[int]time.Time) {
	// Get enabled drilldown lists
	rows, err := p.db.Query(`
		SELECT id, name, poll_interval FROM drilldown_lists WHERE enabled = true
	`)
	if err != nil {
		return
	}
	defer rows.Close()

	var lists []struct {
		ID           int
		Name         string
		PollInterval int
	}
	for rows.Next() {
		var l struct {
			ID           int
			Name         string
			PollInterval int
		}
		if rows.Scan(&l.ID, &l.Name, &l.PollInterval) == nil {
			// Enforce minimum 30 second interval
			if l.PollInterval < 30 {
				l.PollInterval = 30
			}
			lists = append(lists, l)
		}
	}

	if len(lists) == 0 {
		return
	}

	// Build set of AP IPs (these are already polled by main loop)
	apIPs := make(map[string]bool)
	apRows, err := p.db.Query(`SELECT host(ip_address) FROM devices WHERE parent_id IS NULL`)
	if err == nil {
		defer apRows.Close()
		for apRows.Next() {
			var ip string
			if apRows.Scan(&ip) == nil {
				apIPs[ip] = true
			}
		}
	}

	now := time.Now()

	for _, list := range lists {
		// Check if enough time has passed since last poll
		interval := time.Duration(list.PollInterval) * time.Second
		if now.Sub(lastPoll[list.ID]) < interval {
			continue
		}
		lastPoll[list.ID] = now

		// Get hosts for this list
		hostRows, err := p.db.Query(`
			SELECT dh.id, dh.host, dh.device_id, d.username, d.password
			FROM drilldown_hosts dh
			LEFT JOIN devices d ON dh.device_id = d.id
			WHERE dh.list_id = $1
		`, list.ID)
		if err != nil {
			continue
		}

		for hostRows.Next() {
			var hostID int
			var host string
			var deviceID sql.NullInt64
			var username, password sql.NullString

			if hostRows.Scan(&hostID, &host, &deviceID, &username, &password) != nil {
				continue
			}

			// Skip if this is an AP (already polled by main loop)
			if apIPs[host] {
				p.logDebug("drilldown: skipping AP %s (already polled)", host)
				continue
			}

			// Poll this host - credentials come from linked device or global STA creds
			go p.pollDrilldownHost(list.ID, hostID, host, username.String, password.String, deviceID.Int64)
		}
		hostRows.Close()
	}
}

// pollDrilldownHost polls a single host from a drilldown list
func (p *Poller) pollDrilldownHost(listID, hostID int, host, username, password string, deviceID int64) {
	// Use STA credentials if no override provided
	cfg := p.cfgSnapshot()
	if username == "" && len(cfg.staCreds) > 0 {
		username = cfg.staCreds[0].Username
	}
	if password == "" && len(cfg.staCreds) > 0 {
		password = cfg.staCreds[0].Password
	}

	// Create HTTP client (use insecure transport for drilldown hosts)
	baseTransport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}

	// If ultra debug is enabled for this device OR this host, wrap the transport so
	// all request/response cycles are captured into the in-memory ring buffer.
	debugOn := false
	if p.ultraDebug != nil {
		if deviceID > 0 && p.ultraDebug.IsEnabled(deviceID) {
			debugOn = true
		} else if p.ultraDebug.IsHostEnabled(host) {
			debugOn = true
		}
	}

	waveTransport := http.RoundTripper(baseTransport)
	if debugOn {
		waveTransport = udebug.WrapTransportTargets(p.ultraDebug, deviceID, host, baseTransport, "drilldown:wave", udebug.DefaultCaptureLimit)
	}

	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: waveTransport,
	}

	var lastError string
	var success bool

	// Try Wave API first
	baseURL := "https://" + host
	token, err := p.login(client, baseURL, username, password)
	if err == nil {
		// Wave device - fetch stats
		statsData, err := p.fetchStats(client, baseURL, token)
		if err == nil {
			// Parse and store stats
			deviceStats, _ := p.parseStats(statsData, "wave")
			deviceStats.IP = host

			// Fetch config
			cfg, netCfg := p.fetchWaveConfig(client, baseURL, token)
			if cfg != nil {
				deviceStats.Config = cfg
			}
			if netCfg != nil {
				deviceStats.Network = netCfg
			}

			// Update memory store
			p.store.Update(host, deviceStats)

			// Broadcast via WebSocket
			if p.wsHub != nil {
				p.wsHub.BroadcastStatsUpdate(0, "", host, deviceStats)
			}

			success = true
			p.logDebug("drilldown: polled Wave host %s", host)
		} else {
			lastError = "stats: " + err.Error()
		}
	} else {
		// Try AirMAX
		airTransport := http.RoundTripper(baseTransport)
		if debugOn {
			airTransport = udebug.WrapTransportTargets(p.ultraDebug, deviceID, host, baseTransport, "drilldown:airmax", udebug.DefaultCaptureLimit)
		}
		airClient := airmax.NewClientWithTransport(host, 10*time.Second, airTransport)

		err := airClient.Login(username, password)
		if err == nil {
			status, err := airClient.GetStatus()
			if err == nil && status != nil {
				deviceStats := p.convertAirMAXStats(status)
				deviceStats.IP = host

				// Parse config
				if cfg := p.parseAirMAXConfig(status); cfg != nil {
					deviceStats.Config = cfg
				}

				// Update memory store
				p.store.Update(host, deviceStats)

				// Broadcast via WebSocket
				if p.wsHub != nil {
					p.wsHub.BroadcastStatsUpdate(0, "", host, deviceStats)
				}

				success = true
				p.logDebug("drilldown: polled AirMAX host %s", host)
			} else {
				lastError = "status: " + err.Error()
			}
		} else {
			lastError = "login: " + err.Error()
		}
	}

	// Update drilldown host record
	if success {
		dbExecIgnoreCtx(p.db, dbCtx{Op: "drilldown_host_update", Host: host}, `UPDATE drilldown_hosts SET last_poll = NOW(), last_error = NULL WHERE id = $1`, hostID)
	} else {
		dbExecIgnoreCtx(p.db, dbCtx{Op: "drilldown_host_update", Host: host}, `UPDATE drilldown_hosts SET last_poll = NOW(), last_error = $2 WHERE id = $1`, hostID, lastError)
	}
}
