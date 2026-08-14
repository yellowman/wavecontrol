package poller

import (
	"context"
	"crypto/tls"
	"database/sql"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"
	"github.com/yellowman/wavecontrol/internal/stats"
	"github.com/yellowman/wavecontrol/internal/udebug"
	"github.com/yellowman/wavecontrol/internal/websocket"
)

// TLSManager interface for certificate verification
type TLSManager interface {
	GetTransport(deviceID int64) *http.Transport
	GetInsecureTransport() *http.Transport
}

// Poller handles background device polling
type Poller struct {
	db    *sql.DB
	store *stats.Store
	wsHub *websocket.Hub

	// Guards configuration fields that are reloaded at runtime.
	configMu sync.RWMutex

	// Configuration (loaded from settings table)
	interval    time.Duration
	apCreds     []Credential // AP credential pairs (tried in order)
	staCreds    []Credential // STA credential pairs (tried in order)
	workerCount int
	debug       bool

	// Feature flags (loaded from settings table)
	wavePeerFallback  bool
	waveMLOMultiRadio bool

	// Management IP prefix filter (only learn IPs matching these CIDRs)
	mgmtPrefixes []*net.IPNet

	// Worker pool
	jobs chan pollJob
	wg   sync.WaitGroup

	// HTTP client for device API calls (shared, connection pooled)
	httpClient *http.Client

	// TLS manager for certificate verification
	tlsManager TLSManager

	// Ultra debug manager (optional; in-memory per-device request/response capture)
	ultraDebug *udebug.Manager

	// Circuit breaker state per device
	circuitMu    sync.RWMutex
	circuitState map[string]*circuitBreaker

	// Poll cycle counter per device (for periodic config fetch)
	pollCycleMu sync.RWMutex
	pollCycles  map[string]int

	// Interval change notification
	intervalChanged chan time.Duration

	// Control
	cancel context.CancelFunc
}

// circuitBreaker tracks failures per device for backoff
type circuitBreaker struct {
	failures    int
	unreachable int // consecutive unreachable failures (for offline threshold)
	lastFail    time.Time
	nextRetry   time.Time
}

const (
	maxFailures      = 3                // Failures before backing off
	offlineThreshold = 3                // Consecutive unreachable before "offline"
	baseBackoff      = 1 * time.Minute  // Initial backoff
	maxBackoff       = 15 * time.Minute // Maximum backoff
)

var (
	macConflictWarned sync.Map // key: "<deviceID>|<attemptedMAC>"
	truncWarned       sync.Map // key: "<deviceID>|<field>"
	truncWarnedMAC    sync.Map // key: "<mac>|<field>" (for inserts before device id exists)
)

type pollJob struct {
	DeviceID int64
	SiteID   int64
	MAC      string
	IP       string
	Username string
	Password string
	Platform string // "wave", "ltu", "airmax"
	Product  string // DB hint (best-effort) for role inference / UI
	Flavor   string // DB hint (best-effort) for role inference / UI
	Role     string // "ap", "sta" (best-effort hint for credential selection)
}

// Credential holds a username/password pair
type Credential struct {
	Username string
	Password string
}

// Config holds poller configuration
type Config struct {
	Interval          time.Duration
	APCreds           []Credential // AP credential pairs (tried in order)
	STACreds          []Credential // STA credential pairs (tried in order)
	WorkerCount       int
	Debug             bool
	WavePeerFallback  bool            // Feature flag: probe alternate endpoints for Wave/MLO peer lists when peers are missing
	WaveMLOMultiRadio bool            // Feature flag: infer Wave radio band from frequency/AFC and surface dual 5GHz radios
	TLSManager        TLSManager      // TLS certificate manager (optional)
	UltraDebug        *udebug.Manager // Ultra debug manager (optional)
}

// compactQuery condenses whitespace for readable logging without truncating context.
func compactQuery(q string) string {
	return strings.Join(strings.Fields(q), " ")
}

// truncateString truncates a string to at most max runes (PostgreSQL varchar limits are rune-based in UTF-8).
func truncateString(s string, max int) (string, bool) {
	if max <= 0 || s == "" {
		return s, false
	}
	r := []rune(s)
	if len(r) <= max {
		return s, false
	}
	return string(r[:max]), true
}

// truncateForDB enforces a varchar length and logs a warning once per device+field when truncation occurs.
func truncateForDB(field, host string, deviceID int64, value string, max int) string {
	truncatedValue, truncated := truncateString(value, max)
	if !truncated {
		return value
	}
	key := fmt.Sprintf("%d|%s", deviceID, field)
	if _, loaded := truncWarned.LoadOrStore(key, struct{}{}); !loaded {
		// Use default logger so it lands in syslog at LOG_WARNING in daemon mode.
		log.Printf("WARN: truncated %s for device id=%d host=%s from %d to %d chars", field, deviceID, host, len([]rune(value)), max)
	}
	return truncatedValue
}

// truncateForDBMAC enforces a varchar length and logs a warning once per MAC+field.
// This is useful for inserts/upserts where a device id may not exist yet.
func truncateForDBMAC(field, host, mac, value string, max int) string {
	truncatedValue, truncated := truncateString(value, max)
	if !truncated {
		return value
	}
	key := fmt.Sprintf("%s|%s", mac, field)
	if _, loaded := truncWarnedMAC.LoadOrStore(key, struct{}{}); !loaded {
		// Use default logger so it lands in syslog at LOG_WARNING in daemon mode.
		log.Printf("WARN: truncated %s for mac=%s host=%s from %d to %d chars", field, mac, host, len([]rune(value)), max)
	}
	return truncatedValue
}

// NewPoller creates a new poller
func NewPoller(db *sql.DB, store *stats.Store, wsHub *websocket.Hub, cfg Config) *Poller {
	// Scale defaults for large deployments
	if cfg.WorkerCount == 0 {
		cfg.WorkerCount = 50 // Handle 2000 APs in 30s with 50 workers
	}
	if cfg.Interval == 0 {
		cfg.Interval = 30 * time.Second
	}
	if len(cfg.APCreds) == 0 {
		cfg.APCreds = []Credential{{Username: "ubnt", Password: "ubnt"}}
	}
	if len(cfg.STACreds) == 0 {
		cfg.STACreds = cfg.APCreds // Default to AP credentials
	}

	// Calculate job queue size based on expected device count
	// Queue should hold at least one full poll cycle
	jobQueueSize := 2500 // Enough for 2000+ APs

	// Default transport (insecure for backward compatibility)
	defaultTransport := &http.Transport{
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
		MaxIdleConns:        500, // Scaled for 2000 APs
		MaxIdleConnsPerHost: 2,   // 2 per device is plenty
		MaxConnsPerHost:     4,   // Limit concurrent per device
		IdleConnTimeout:     90 * time.Second,
		DisableKeepAlives:   false,
	}

	// Use TLS manager transport if provided
	if cfg.TLSManager != nil {
		defaultTransport = cfg.TLSManager.GetInsecureTransport()
	}

	return &Poller{
		db:                db,
		store:             store,
		wsHub:             wsHub,
		interval:          cfg.Interval,
		apCreds:           cfg.APCreds,
		staCreds:          cfg.STACreds,
		workerCount:       cfg.WorkerCount,
		debug:             cfg.Debug,
		wavePeerFallback:  cfg.WavePeerFallback,
		waveMLOMultiRadio: cfg.WaveMLOMultiRadio,
		tlsManager:        cfg.TLSManager,
		ultraDebug:        cfg.UltraDebug,
		jobs:              make(chan pollJob, jobQueueSize),
		circuitState:      make(map[string]*circuitBreaker),
		pollCycles:        make(map[string]int),
		intervalChanged:   make(chan time.Duration, 1), // Buffered to avoid blocking
		httpClient: &http.Client{
			Transport: defaultTransport,
			Timeout:   15 * time.Second, // Reduced from 30s for faster failure detection
		},
	}
}

// logDebug logs only when debug mode is enabled
func (p *Poller) logDebug(format string, v ...any) {
	if p.cfgSnapshot().debug {
		log.Printf(format, v...)
	}
}

// getDeviceClient returns an HTTP client configured for a specific device.
// Uses TLS manager for per-device certificate verification when in TOFU/strict mode.
func (p *Poller) getDeviceClient(deviceID int64) *http.Client {
	// Base transport: insecure shared client when TLS manager is disabled,
	// otherwise a per-device transport from the TLS manager.
	var base http.RoundTripper
	if p.tlsManager == nil {
		base = p.httpClient.Transport
	} else {
		base = p.tlsManager.GetTransport(deviceID)
	}

	// When ultra debug is enabled for this device, return a per-device client
	// with a logging transport wrapper.
	if p.ultraDebug != nil && p.ultraDebug.IsEnabled(deviceID) {
		wrapped := udebug.WrapTransport(p.ultraDebug, deviceID, base, "poller", udebug.DefaultCaptureLimit)
		return &http.Client{Transport: wrapped, Timeout: 15 * time.Second}
	}

	// Fast path: reuse the shared pooled client when TLS manager isn't used.
	if p.tlsManager == nil {
		return p.httpClient
	}

	return &http.Client{Transport: base, Timeout: 15 * time.Second}
}

// shouldFetchConfig returns true if it's time to fetch config for this device
// Config is fetched every 2 poll cycles
func (p *Poller) shouldFetchConfig(ip string) bool {
	p.pollCycleMu.Lock()
	defer p.pollCycleMu.Unlock()

	p.pollCycles[ip]++
	cycle := p.pollCycles[ip]

	// Fetch on first poll and every 2 polls thereafter
	return cycle == 1 || cycle%2 == 0
}

// isAllowedIP checks if an IP address matches the management prefixes.
// Returns true if no prefixes configured (allow all) or if IP matches any prefix.
func (p *Poller) isAllowedIP(ipStr string) bool {
	prefixes := p.cfgSnapshot().mgmtPrefixes
	// If no prefixes configured, allow all IPs
	if len(prefixes) == 0 {
		return true
	}

	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}

	for _, prefix := range prefixes {
		if prefix.Contains(ip) {
			return true
		}
	}

	return false
}

// enrichPeersWithMAC looks up MAC addresses for peers that have allowed IPs but no MAC.
// This enables IP-based device identification when management prefixes are configured.
func (p *Poller) enrichPeersWithMAC(peers []*stats.PeerStats) {
	cfg := p.cfgSnapshot()
	// Only do IP lookups if management prefixes are configured
	if len(cfg.mgmtPrefixes) == 0 {
		return
	}

	for _, peer := range peers {
		// Skip if already has MAC
		if peer.MAC != "" {
			continue
		}

		// Skip if no IP or IP not allowed
		if peer.IP == "" || !p.isAllowedIP(peer.IP) {
			continue
		}

		// Look up MAC by IP
		var mac string
		err := p.db.QueryRow(`SELECT mac FROM devices WHERE host(ip_address) = $1`, peer.IP).Scan(&mac)
		if err == nil && mac != "" {
			peer.MAC = mac
			p.logDebug("Enriched peer IP %s with MAC %s from database", peer.IP, mac)
		}
	}
}

// shouldSkip checks if device is in backoff due to circuit breaker
func (p *Poller) shouldSkip(ip string) bool {
	p.circuitMu.RLock()
	cb, ok := p.circuitState[ip]
	p.circuitMu.RUnlock()

	if !ok {
		return false
	}

	return time.Now().Before(cb.nextRetry)
}

// recordSuccess resets circuit breaker for device
func (p *Poller) recordSuccess(ip string) {
	p.circuitMu.Lock()
	delete(p.circuitState, ip)
	p.circuitMu.Unlock()
}

// recordFailure increments failure count and sets backoff
// unreachable: true if device didn't respond at all (timeout, no route)
//
//	false if device responded but with error (TCP RST, auth fail, HTTP error)
func (p *Poller) recordFailure(ip string, unreachable bool) {
	p.circuitMu.Lock()
	defer p.circuitMu.Unlock()

	cb, ok := p.circuitState[ip]
	if !ok {
		cb = &circuitBreaker{}
		p.circuitState[ip] = cb
	}

	cb.failures++
	cb.lastFail = time.Now()

	if unreachable {
		cb.unreachable++
	} else {
		// Device responded in some way - reset unreachable counter
		cb.unreachable = 0
	}

	if cb.failures >= maxFailures {
		// Exponential backoff: 1min, 2min, 4min, 8min, 15min max
		backoff := baseBackoff * time.Duration(1<<(cb.failures-maxFailures))
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
		cb.nextRetry = time.Now().Add(backoff)
		p.logDebug("Device %s: circuit breaker open, retry in %v", ip, backoff)
	}
}

// getDeviceStatus returns "offline" only after 3+ consecutive unreachable failures
// otherwise returns "unknown"
func (p *Poller) getDeviceStatus(ip string, unreachable bool) string {
	p.circuitMu.RLock()
	cb := p.circuitState[ip]
	p.circuitMu.RUnlock()

	if !unreachable {
		// Device responded (TCP RST, auth fail, HTTP error) - always "unknown"
		return "unknown"
	}

	// Check consecutive unreachable count (will be incremented after this call)
	consecutiveUnreachable := 1
	if cb != nil {
		consecutiveUnreachable = cb.unreachable + 1
	}

	if consecutiveUnreachable >= offlineThreshold {
		return "offline"
	}
	return "unknown"
}

// batchSyncToDB syncs last_seen and status to database periodically
// This provides persistence for crash recovery without per-poll DB writes
func (p *Poller) batchSyncToDB() {
	lastSeenBatch := p.store.LastSeenBatch()
	statusBatch := p.store.OnlineStatusBatch()

	if len(lastSeenBatch) == 0 {
		return
	}

	// Build batch update - one query for online, one for offline
	onlineMACs := make([]string, 0)
	offlineMACs := make([]string, 0)

	for mac, online := range statusBatch {
		if online {
			onlineMACs = append(onlineMACs, mac)
		} else {
			offlineMACs = append(offlineMACs, mac)
		}
	}

	// Update online devices
	if len(onlineMACs) > 0 {
		_, err := dbExecCtx(p.db, dbCtxForOp("batch_sync_last_seen"), `UPDATE devices SET last_seen = NOW() WHERE mac = ANY($1)`, pq.Array(onlineMACs))
		if err != nil {
			p.logDebug("batchSyncToDB: online update failed: %v", err)
		}
	}

	// Update offline devices (with their actual last_seen time from memory)
	// This is more complex - we need individual updates or a CTE
	// For simplicity, we'll just ensure status is correct
	// Important: only transition from 'online' to 'offline', not from 'unknown' to 'offline'
	// Devices with 'unknown' status responded somehow (e.g., auth failed) so they're reachable
	if len(offlineMACs) > 0 {
		_, err := dbExecCtx(p.db, dbCtxForOp("batch_sync_mark_offline"), `UPDATE devices SET status = 'offline' WHERE mac = ANY($1) AND status = 'online'`, pq.Array(offlineMACs))
		if err != nil {
			p.logDebug("batchSyncToDB: offline update failed: %v", err)
		}
	}

	p.logDebug("batchSyncToDB: synced %d online, %d offline devices", len(onlineMACs), len(offlineMACs))
}

// cleanCircuitBreakers removes old entries
func (p *Poller) cleanCircuitBreakers() {
	p.circuitMu.Lock()
	defer p.circuitMu.Unlock()

	cutoff := time.Now().Add(-30 * time.Minute)
	for ip, cb := range p.circuitState {
		if cb.lastFail.Before(cutoff) {
			delete(p.circuitState, ip)
		}
	}
}

// Start begins the polling loop
func (p *Poller) Start(ctx context.Context) {
	ctx, p.cancel = context.WithCancel(ctx)

	// Load settings from the DB *before* the first poll cycle.
	//
	// The config watcher reloads settings periodically, but relying on it alone
	// introduces a race where the initial poll can run with default settings
	// (notably: an empty management_prefixes list). In that window, we could
	// learn and persist peer-reported IPs that should have been filtered.
	p.loadConfig()

	// Start workers
	workerCount := p.cfgSnapshot().workerCount
	for i := 0; i < workerCount; i++ {
		p.wg.Add(1)
		go p.worker(ctx)
	}

	// Start config watcher
	go p.watchConfig(ctx)

	// Start custom drilldown list poller
	go p.drilldownPoller(ctx)

	// Initial poll
	p.pollAllDevices()

	// Main poll loop with dynamic interval support
	ticker := time.NewTicker(p.cfgSnapshot().interval)
	defer ticker.Stop()

	// Cleanup stale STAs every 5 poll cycles, circuit breakers every 10, DB sync every 20
	cleanupCounter := 0

	for {
		select {
		case <-ctx.Done():
			// Final sync before shutdown
			p.batchSyncToDB()
			close(p.jobs)
			p.wg.Wait()
			return
		case newInterval := <-p.intervalChanged:
			// Reset ticker with new interval
			ticker.Reset(newInterval)
			p.logDebug("Poll interval changed to %v", newInterval)
		case <-ticker.C:
			p.pollAllDevices()

			cleanupCounter++
			// Clean stale STAs every 5 cycles (~2.5 min at 30s interval)
			if cleanupCounter%5 == 0 {
				if removed := p.store.CleanStale(); removed > 0 {
					p.logDebug("Cleaned %d stale STAs from stats store", removed)
				}
			}
			// Clean circuit breakers every 10 cycles (~5 min)
			if cleanupCounter%10 == 0 {
				p.cleanCircuitBreakers()
			}
			// Batch sync last_seen to DB every 20 cycles (~10 min)
			// This provides persistence without per-poll writes
			if cleanupCounter%20 == 0 {
				p.batchSyncToDB()
			}
		}
	}
}

// Stop stops the poller
func (p *Poller) Stop() {
	if p.cancel != nil {
		p.cancel()
	}
}

// RefreshDevice forces immediate poll of a device (bypasses circuit breaker)
func (p *Poller) RefreshDevice(ip string) {
	// Look up device credentials
	var deviceID int64
	var siteID int64
	var mac string
	var username, password, platform sql.NullString
	var product, flavor sql.NullString
	var role sql.NullString
	var parentID sql.NullInt64

	err := p.db.QueryRow(`
		SELECT id, COALESCE(site_id, 0), lower(mac), username, password, platform, role, parent_id, product, flavor
		FROM devices
		WHERE host(ip_address) = $1
		ORDER BY managed DESC, (parent_id IS NULL) DESC, id DESC
		LIMIT 1
	`, ip).Scan(&deviceID, &siteID, &mac, &username, &password, &platform, &role, &parentID, &product, &flavor)

	if err != nil {
		log.Printf("RefreshDevice %s: device not found in DB: %v", ip, err)
		return
	}

	// Clear circuit breaker on manual refresh
	p.recordSuccess(ip)
	log.Printf("RefreshDevice %s: cleared circuit breaker, queuing poll", ip)

	job := pollJob{
		DeviceID: deviceID,
		SiteID:   siteID,
		MAC:      mac,
		IP:       ip,
		Username: username.String,
		Password: password.String,
		Platform: platform.String,
		Product:  product.String,
		Flavor:   flavor.String,
		Role:     role.String,
	}

	// Default credentials if per-device creds are missing.
	// If the device is known to be a STA (role=sta OR has parent_id), prefer STA creds.
	creds := p.cfgSnapshot().apCreds
	if (strings.EqualFold(job.Role, "sta") || parentID.Valid) && len(p.cfgSnapshot().staCreds) > 0 {
		creds = p.cfgSnapshot().staCreds
	}
	if job.Username == "" && len(creds) > 0 {
		job.Username = creds[0].Username
	}
	if job.Password == "" && len(creds) > 0 {
		job.Password = creds[0].Password
	}

	select {
	case p.jobs <- job:
		log.Printf("RefreshDevice %s: queued for poll", ip)
	default:
		log.Printf("RefreshDevice %s: queue full, skipping", ip)
	}
}

// RefreshDeviceByID forces an immediate poll of a specific device by database ID.
//
// Prefer this over RefreshDevice(ip) because IP addresses can be reused.
func (p *Poller) RefreshDeviceByID(deviceID int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Look up device by ID
	var ip string
	var siteID int64
	var mac string
	var username, password, platform sql.NullString
	var product, flavor sql.NullString
	var role sql.NullString
	var parentID sql.NullInt64

	err := p.db.QueryRowContext(ctx, `
		SELECT host(ip_address), COALESCE(site_id, 0), lower(mac), username, password, platform, role, parent_id, product, flavor
		FROM devices
		WHERE id = $1
		LIMIT 1
	`, deviceID).Scan(&ip, &siteID, &mac, &username, &password, &platform, &role, &parentID, &product, &flavor)
	if err != nil {
		return fmt.Errorf("device id %d not found: %w", deviceID, err)
	}

	// Clear circuit breaker on manual refresh
	p.recordSuccess(ip)
	log.Printf("RefreshDeviceByID %d (%s): cleared circuit breaker, queuing poll", deviceID, ip)

	job := pollJob{
		DeviceID: deviceID,
		SiteID:   siteID,
		MAC:      mac,
		IP:       ip,
		Username: username.String,
		Password: password.String,
		Platform: platform.String,
		Product:  product.String,
		Flavor:   flavor.String,
		Role:     role.String,
	}

	// Default credentials if per-device creds are missing.
	creds := p.cfgSnapshot().apCreds
	if (strings.EqualFold(job.Role, "sta") || parentID.Valid) && len(p.cfgSnapshot().staCreds) > 0 {
		creds = p.cfgSnapshot().staCreds
	}
	if job.Username == "" && len(creds) > 0 {
		job.Username = creds[0].Username
	}
	if job.Password == "" && len(creds) > 0 {
		job.Password = creds[0].Password
	}

	select {
	case p.jobs <- job:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("timeout queueing poll for device id %d", deviceID)
	}
}

// pollAllDevices queues all devices that should be directly polled:
//   - All APs (parent_id IS NULL)
//   - Any explicitly "managed" device added via Add IP / Add Bulk
func (p *Poller) pollAllDevices() {
	// Guardrail: break STA->STA parent loops.
	//
	// A legitimate STA should always have an AP parent. In the field, a topology loop
	// can appear when a STA is directly polled and mis-identifies its AP as a "peer"
	// device that is then learned/updated in the DB as a STA. Once that happens the
	// (real) AP can become "stuck" as a STA under another STA, which also excludes it
	// from direct polling (because parent_id is non-null and managed may be false).
	//
	// We proactively clear the parent reference for any STA whose parent is also a STA
	// (and also clear obviously-invalid self-parent rows). This causes the device to be
	// directly polled again so the platform-specific role detection can re-verify and
	// promote it back to AP when appropriate.
	p.breakStaParentLoops()

	rows, err := p.db.Query(`
		SELECT id, COALESCE(site_id, 0), lower(mac), host(ip_address), username, password, platform, role, parent_id, product, flavor
		FROM devices
		WHERE (parent_id IS NULL OR managed = TRUE)
		ORDER BY ip_address
	`)
	if err != nil {
		log.Printf("pollAllDevices: query error: %v", err)
		return
	}
	defer rows.Close()

	queued := 0
	skipped := 0
enqueue:
	for rows.Next() {
		var job pollJob
		var username, password, platform, role sql.NullString
		var product, flavor sql.NullString
		var parentID sql.NullInt64

		err := rows.Scan(&job.DeviceID, &job.SiteID, &job.MAC, &job.IP, &username, &password, &platform, &role, &parentID, &product, &flavor)
		if err != nil {
			p.logDebug("pollAllDevices: scan error: %v", err)
			continue
		}

		// Check circuit breaker - skip devices in backoff
		if p.shouldSkip(job.IP) {
			skipped++
			continue
		}

		job.Username = username.String
		job.Password = password.String
		job.Platform = platform.String
		job.Product = product.String
		job.Flavor = flavor.String
		job.Role = role.String

		// Default credentials (role-aware)
		creds := p.cfgSnapshot().apCreds
		if (strings.EqualFold(job.Role, "sta") || parentID.Valid) && len(p.cfgSnapshot().staCreds) > 0 {
			creds = p.cfgSnapshot().staCreds
		}
		if job.Username == "" && len(creds) > 0 {
			job.Username = creds[0].Username
		}
		if job.Password == "" && len(creds) > 0 {
			job.Password = creds[0].Password
		}

		select {
		case p.jobs <- job:
			queued++
		default:
			log.Printf("pollAllDevices: queue full at %d devices", queued)
			break enqueue // Break outer loop, not just select
		}
	}

	p.store.SetLastPoll(time.Now())
	p.store.RecordThroughputSample() // Record throughput history
	if skipped > 0 {
		p.logDebug("Queued %d devices, skipped %d (backoff)", queued, skipped)
	} else {
		p.logDebug("Queued %d devices for polling", queued)
	}
}

// breakStaParentLoops clears obviously-invalid STA parent relationships so affected
// devices are directly polled again and their roles can be re-verified.
//
// Invariant we want:
//   - APs: parent_id == NULL
//   - STAs: parent_id points to an AP
//
// When a STA ends up parented by another STA, we consider it a bad topology loop
// and clear the parent reference. The next successful poll should restore the
// correct role and/or re-parenting.
func (p *Poller) breakStaParentLoops() {
	res, err := p.db.Exec(`
		UPDATE devices
		SET parent_id = NULL,
		    parent_mac = ''
		WHERE
			-- Self-parent loops are always invalid
			(parent_id = id)
			OR
			-- APs should never have a parent
			(LOWER(COALESCE(role, '')) = 'ap' AND parent_id IS NOT NULL)
			OR
			-- A STA whose parent is also a STA is almost certainly a loop/misclassification
			(LOWER(COALESCE(role, '')) = 'sta'
			 AND parent_id IS NOT NULL
			 AND EXISTS (
				SELECT 1
				FROM devices p
				WHERE p.id = devices.parent_id
				  AND LOWER(COALESCE(p.role, '')) = 'sta'
			 ))
	`)
	if err != nil {
		p.logDebug("breakStaParentLoops: update error: %v", err)
		return
	}
	if res != nil {
		if n, err := res.RowsAffected(); err == nil && n > 0 {
			p.logDebug("breakStaParentLoops: cleared %d invalid parent relationships", n)
		}
	}
}

// worker processes poll jobs
func (p *Poller) worker(ctx context.Context) {
	defer p.wg.Done()

	for {
		select {
		case <-ctx.Done():
			return
		case job, ok := <-p.jobs:
			if !ok {
				return
			}
			p.pollDevice(job)
		}
	}
}

// pollResult indicates the outcome of a poll attempt
type pollResult int

const (
	pollNotThisType pollResult = iota // Device doesn't respond to this API type
	pollFailed                        // Device responded but poll failed
	pollSuccess                       // Poll completed successfully
)

// pollDevice polls a single device
func (p *Poller) pollDevice(job pollJob) {
	// Bind inventory identity before any success/failure path mutates the in-memory stats store.
	// Alert rules key state by stats.DeviceID, so failure-only polls must not leave the
	// stats row at the zero-value device_id.
	p.store.BindIdentityByMAC(job.MAC, job.IP, int(job.DeviceID), int(job.SiteID))

	// Determine if this is AirMAX or Wave based on platform or auto-detect
	platform := strings.ToLower(job.Platform)

	// If explicitly AirMAX, only try that
	if platform == "airmax" || platform == "ac" || platform == "m" {
		if p.pollDeviceAirMAX(job) == pollSuccess {
			p.recordSuccess(job.IP)
		}
		// Failure already recorded inside pollDeviceAirMAX
		return
	}

	// Try Wave API first (for wave, ltu, or auto-detect)
	result := p.pollDeviceWave(job)
	if result == pollSuccess {
		p.recordSuccess(job.IP)
		return
	}
	if result == pollFailed {
		// Device is Wave but poll failed - failure already recorded inside pollDeviceWave
		return
	}

	// Wave didn't recognize this device - try AirMAX as fallback
	result = p.pollDeviceAirMAX(job)
	if result == pollSuccess {
		p.recordSuccess(job.IP)
		// Successfully polled as AirMAX - update platform in DB if it was wrong
		if platform == "" || platform == "wave" {
			dbExecIgnoreCtx(p.db, dbCtxForJob(job, "correct_platform_airmax"), `UPDATE devices SET platform = 'airmax' WHERE id = $1`, job.DeviceID)
			p.logDebug("Device %s: corrected platform from %s to airmax", job.IP, platform)
		}
	}
	// Failure already recorded inside pollDeviceAirMAX
}
