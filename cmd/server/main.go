package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	_ "github.com/lib/pq"

	"github.com/yellowman/wavecontrol/internal/alerting"
	"github.com/yellowman/wavecontrol/internal/bulkops"
	"github.com/yellowman/wavecontrol/internal/firmware"
	"github.com/yellowman/wavecontrol/internal/jobs"
	"github.com/yellowman/wavecontrol/internal/poller"
	"github.com/yellowman/wavecontrol/internal/scheduler"
	"github.com/yellowman/wavecontrol/internal/secrets"
	"github.com/yellowman/wavecontrol/internal/stats"
	"github.com/yellowman/wavecontrol/internal/tlsutil"
	"github.com/yellowman/wavecontrol/internal/udebug"
	"github.com/yellowman/wavecontrol/internal/websocket"
	"github.com/yellowman/wavecontrol/internal/zabbix"
)

var (
	flagDebug      = flag.Bool("d", false, "Debug mode (foreground, verbose logging)")
	flagWeb        = flag.Bool("web", false, "Run as standalone HTTP server (implies -d)")
	flagAddr       = flag.String("addr", "", "Listen address (default: from settings or 127.0.0.1:8080)")
	flagWebRoot    = flag.String("webroot", "web", "Path to web directory")
	flagPidFile    = flag.String("pidfile", "/var/run/wavecontrol.pid", "PID file path (daemon mode)")
	flagUnchrooted = flag.Bool("U", false, "Unchrooted mode (skip chroot, just chdir)")
	flagUser       = flag.String("u", "", "User to run as (default: _wavecontrol, www, or nobody)")
)

// debugMode is set based on -d or -web flags
var debugMode bool

// Loggers - errLog always logs (syslog in daemon, stderr in debug)
// debugLog only logs in debug mode
var (
	errLog   *log.Logger // Critical errors (always logged)
	debugLog *log.Logger // Debug info (only with -d)
)

// logDebug logs only in debug mode
func logDebug(format string, v ...any) {
	if debugLog != nil {
		debugLog.Printf(format, v...)
	}
}

// logError logs errors (always, to syslog in daemon mode)
func logError(format string, v ...any) {
	if errLog != nil {
		errLog.Printf(format, v...)
	}
}

// logFatal logs and exits
func logFatal(format string, v ...any) {
	if errLog != nil {
		errLog.Printf(format, v...)
	}
	os.Exit(1)
}

// initLogging is implemented in platform-specific files (logging_unix.go, logging_windows.go)

func main() {
	flag.Parse()

	// Debug mode if -d or -web specified
	debugMode = *flagDebug || *flagWeb

	// Initialize logging first
	initLogging()

	// Drop privileges and daemonize if not in debug mode
	if !debugMode {
		// Daemonize (fork to background)
		if err := daemonize(); err != nil {
			logFatal("daemonize: %v", err)
		}

		// Re-init logging after daemonize (syslog connection may need refresh)
		initLogging()

		// Write PID file
		if *flagPidFile != "" {
			if err := os.WriteFile(*flagPidFile, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0644); err != nil {
				logError("could not write pid file: %v", err)
			}
		}
	}

	// Drop privileges (Unix only, no-op on Windows)
	// Default: chroot to user's home dir when running as root
	// Use -U to skip chroot and just chdir
	if err := dropPrivileges(*flagUser, *flagUnchrooted); err != nil {
		logFatal("drop privileges: %v", err)
	}

	// Platform-specific security (pledge on OpenBSD, etc.)
	if err := platformSecure(); err != nil {
		logDebug("platform security: %v", err)
	}

	// Only DSN and JWT secret from environment
	dsn := os.Getenv("WAVECONTROL_DSN")
	if dsn == "" {
		logFatal("WAVECONTROL_DSN is required")
	}

	jwtSecret := os.Getenv("WAVECONTROL_JWT_SECRET")
	if len(jwtSecret) < 32 {
		logFatal("WAVECONTROL_JWT_SECRET must be at least 32 characters")
	}

	secretStore, err := secrets.New(os.Getenv("WAVECONTROL_DATA_KEY"))
	if err != nil {
		logFatal("data encryption key: %v", err)
	}

	// Open database
	logDebug("connecting to database...")
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		logFatal("db open: %v", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		logFatal("db ping: %v", err)
	}
	logDebug("database connected")

	// Check if schema is loaded
	if err := checkSchema(db); err != nil {
		logFatal("%v", err)
	}

	// Bring older installations to the required schema. Never continue with a
	// partially migrated database.
	if err := ensureRuntimeSchema(db); err != nil {
		logFatal("schema migration: %v", err)
	}
	if err := validateRuntimeSchema(db); err != nil {
		logFatal("schema validation: %v", err)
	}
	if err := ensureBootstrapAdmin(db); err != nil {
		logFatal("administrator bootstrap: %v", err)
	}

	// Database pool sized for 2000 APs with 50 workers
	// Each worker may have 2-3 concurrent queries
	db.SetMaxOpenConns(100)
	db.SetMaxIdleConns(25)
	db.SetConnMaxLifetime(5 * time.Minute)

	// Initialize non-secret settings, then migrate any legacy plaintext
	// operational credentials to AES-256-GCM before services read them.
	if err := initSettings(db); err != nil {
		logFatal("settings initialization: %v", err)
	}
	if err := secretStore.MigrateDatabase(context.Background(), db); err != nil {
		logFatal("credential encryption migration: %v", err)
	}

	// Load listen address from settings
	addr := "127.0.0.1:8080"
	db.QueryRow(`SELECT value FROM settings WHERE key = 'listen_addr'`).Scan(&addr)
	if *flagAddr != "" {
		addr = *flagAddr
	}

	// Create context for background services
	ctx, cancel := context.WithCancel(context.Background())

	// Create stats store
	statsStore := stats.NewStore()

	// Create websocket hub
	wsHub := websocket.NewHub()
	go wsHub.Run(ctx)

	// Create TLS manager for certificate pinning (before services that need it)
	tlsManager := tlsutil.NewManager(db)

	// Create ultra debug manager (per-device 32MB ring buffers)
	ultraDebug := udebug.NewManager(udebug.DefaultMaxBytes)

	// Create firmware service
	fwService, err := firmware.NewService(db, tlsManager, ultraDebug, secretStore)
	if err != nil {
		logFatal("firmware configuration: %v", err)
	}

	// Rebuild device hierarchy from parent_mac if parent_id is NULL
	// This handles cases where wavecontrol restarted and parent devices changed IDs
	rebuildDeviceHierarchy(db)

	// Load poller config from settings
	pollerCfg := loadPollerConfig(db, debugMode, tlsManager, ultraDebug, secretStore)

	// Create and start poller (with websocket hub)
	devicePoller := poller.NewPoller(db, statsStore, wsHub, pollerCfg)

	// Create scheduler
	jobScheduler := scheduler.NewScheduler(db, fwService, wsHub)

	// Create job runner for async operations
	jobRunner := jobs.NewRunner(db, fwService, wsHub)
	jobRunner.Start() // Recover pending jobs from previous runs

	// Create bulk operations controller
	bulkOps := bulkops.NewController(db)

	// Create alerting manager. Alert rules, state, and notification settings are
	// required runtime data; fail startup rather than running with an inert or
	// partially initialized alert engine.
	alertManager, err := alerting.NewManager(db, statsStore, secretStore)
	if err != nil {
		logFatal("alerting configuration: %v", err)
	}
	alertManager.Start(ctx)

	// Start alert evaluation loop
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				alertManager.Evaluate(ctx)
			}
		}
	}()

	// Build router
	absWebRoot, _ := filepath.Abs(*flagWebRoot)
	logDebug("Web root: %s (absolute: %s)", *flagWebRoot, absWebRoot)
	if _, err := os.Stat(filepath.Join(*flagWebRoot, "index.html")); err != nil {
		log.Printf("WARNING: index.html not found at %s/index.html", *flagWebRoot)
	}
	r := buildRouter(db, []byte(jwtSecret), statsStore, fwService, devicePoller, wsHub, jobScheduler, jobRunner, ultraDebug, tlsManager, bulkOps, alertManager, secretStore, *flagWebRoot, debugMode)

	// Create server
	srv := &http.Server{
		Addr:              addr,
		Handler:           r,
		ReadTimeout:       10 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Start poller in background
	logDebug("starting poller...")
	go devicePoller.Start(ctx)

	// Start scheduler in background
	logDebug("starting scheduler...")
	go jobScheduler.Start(ctx)

	// Start Zabbix bridge if enabled
	var zabbixBridge *zabbix.Bridge
	var zabbixEnabled string
	db.QueryRow(`SELECT value FROM settings WHERE key = 'zabbix_enabled'`).Scan(&zabbixEnabled)
	if zabbixEnabled == "true" {
		var zabbixAddr string
		db.QueryRow(`SELECT value FROM settings WHERE key = 'zabbix_listen'`).Scan(&zabbixAddr)
		if zabbixAddr == "" {
			zabbixAddr = "127.0.0.1:10050"
		}
		zabbixBridge = zabbix.NewBridge(statsStore, zabbixAddr)

		// Configure allowed hosts (security: restrict who can query)
		var allowedHosts string
		db.QueryRow(`SELECT value FROM settings WHERE key = 'zabbix_allowed_hosts'`).Scan(&allowedHosts)
		zabbixBridge.SetAllowedHosts(allowedHosts)

		if err := zabbixBridge.Start(); err != nil {
			logError("Zabbix bridge failed to start: %v", err)
		} else {
			logDebug("zabbix bridge listening on %s", zabbixAddr)
		}
	}

	// Graceful shutdown
	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-done
		logDebug("shutting down...")
		cancel()
		jobRunner.Shutdown(30 * time.Second) // Wait for jobs to complete
		if zabbixBridge != nil {
			zabbixBridge.Stop()
		}
		// Remove PID file
		if *flagPidFile != "" {
			os.Remove(*flagPidFile)
		}
		srv.Shutdown(context.Background())
	}()

	logDebug("waveControl server listening on %s", addr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		logFatal("server: %v", err)
	}
}

func ensureBootstrapAdmin(db *sql.DB) error {
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		return fmt.Errorf("count users: %w", err)
	}
	if count > 0 {
		return nil
	}

	username := strings.TrimSpace(os.Getenv("WAVECONTROL_BOOTSTRAP_USERNAME"))
	password := os.Getenv("WAVECONTROL_BOOTSTRAP_PASSWORD")
	if username == "" || password == "" {
		return errors.New("database has no users; set WAVECONTROL_BOOTSTRAP_USERNAME and WAVECONTROL_BOOTSTRAP_PASSWORD for the first startup")
	}
	if len(username) > 64 {
		return errors.New("WAVECONTROL_BOOTSTRAP_USERNAME exceeds 64 characters")
	}
	hash, err := hashPassword(password)
	if err != nil {
		return fmt.Errorf("bootstrap password: %w", err)
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id int64
	if err := tx.QueryRow(`
		INSERT INTO users (username, password, status, auth_version)
		VALUES ($1, $2, 1, 1) RETURNING id
	`, username, hash).Scan(&id); err != nil {
		return fmt.Errorf("create bootstrap user: %w", err)
	}
	result, err := tx.Exec(`
		INSERT INTO user_roles ("user", role)
		SELECT $1, id FROM roles WHERE name='administrator'
	`, id)
	if err != nil {
		return fmt.Errorf("assign bootstrap role: %w", err)
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return errors.New("administrator role is missing from the database")
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	log.Printf("Created initial administrator account %q from bootstrap environment", username)
	return nil
}

func initSettings(db *sql.DB) error {
	// Paths are relative to the working directory (user's home dir or chroot).
	defaults := map[string]string{
		"ap_cred1_user":                 "",
		"ap_cred1_pass":                 "",
		"ap_cred2_user":                 "",
		"ap_cred2_pass":                 "",
		"ap_cred3_user":                 "",
		"ap_cred3_pass":                 "",
		"poll_interval":                 "30",
		"poller_workers":                "50",
		"firmware_path":                 "firmware",
		"backup_dir":                    "backups",
		"listen_addr":                   "127.0.0.1:8080",
		"zabbix_enabled":                "false",
		"zabbix_listen":                 "127.0.0.1:10050",
		"zabbix_allowed_hosts":          "",
		"zabbix_server":                 "",
		"zabbix_sender_host":            "wavecontrol",
		"smtp_host":                     "",
		"smtp_port":                     "25",
		"smtp_username":                 "",
		"smtp_password":                 "",
		"sta_cred1_user":                "",
		"sta_cred1_pass":                "",
		"sta_cred2_user":                "",
		"sta_cred2_pass":                "",
		"sta_cred3_user":                "",
		"sta_cred3_pass":                "",
		"smtp_from":                     "",
		"scheduler_max_concurrent":      "5",
		"scheduler_check_interval":      "10",
		"scheduler_respect_maintenance": "true",
		"management_prefixes":           "[]",
		"interference_warning_pct":      "10",
		"interference_critical_pct":     "25",
		"chain_imbalance_threshold_db":  "5",
		"rx_mismatch_threshold_db":      "8",
		"cors_origins":                  "", // Empty = same-origin only; JSON array of origins is also accepted.
		"csp_img_sources":               "", // Additional img-src domains for map tiles (space-separated).
		"csp_connect_sources":           "", // Additional connect-src domains for map APIs (space-separated).
		"wave_peer_fallback":            "false",
		"wave_mlo_multi_radio":          "false",
	}

	keys := make([]string, 0, len(defaults))
	for key := range defaults {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, key := range keys {
		if _, err := tx.Exec(`INSERT INTO settings (key, value) VALUES ($1, $2) ON CONFLICT (key) DO NOTHING`, key, defaults[key]); err != nil {
			return fmt.Errorf("set default %s: %w", key, err)
		}
	}
	return tx.Commit()
}

// rebuildDeviceHierarchy re-associates STAs with their parent APs using parent_mac
// This is needed after restart when device IDs may have changed
func rebuildDeviceHierarchy(db *sql.DB) {
	// Find STAs that have parent_mac but NULL parent_id (orphaned STAs)
	rows, err := db.Query(`
		SELECT sta.id, sta.parent_mac 
		FROM devices sta 
		WHERE sta.parent_mac IS NOT NULL 
		  AND sta.parent_id IS NULL
	`)
	if err != nil {
		log.Printf("rebuildDeviceHierarchy: query failed: %v", err)
		return
	}
	defer rows.Close()

	var fixed int
	for rows.Next() {
		var staID int64
		var parentMAC string
		if rows.Scan(&staID, &parentMAC) != nil {
			continue
		}

		// Find the AP by MAC
		var apID int64
		if db.QueryRow(`SELECT id FROM devices WHERE mac = $1 AND parent_id IS NULL`, parentMAC).Scan(&apID) == nil {
			// Found the AP - update the STA's parent_id
			if _, err := db.Exec(`UPDATE devices SET parent_id = $1 WHERE id = $2`, apID, staID); err == nil {
				fixed++
			}
		}
	}

	if fixed > 0 {
		logDebug("rebuildDeviceHierarchy: re-associated %d STAs with their parent APs", fixed)
	}
}

func loadPollerConfig(db *sql.DB, debug bool, tlsMgr *tlsutil.Manager, udbg *udebug.Manager, secretStore *secrets.Manager) poller.Config {
	cfg := poller.Config{
		Interval:    30 * time.Second,
		APCreds:     nil,
		STACreds:    nil,
		WorkerCount: 50, // Default for large deployments (2000 APs)
		Debug:       debug,
		TLSManager:  tlsMgr,
		UltraDebug:  udbg,
		SecretStore: secretStore,
	}

	var intervalStr string
	if db.QueryRow(`SELECT value FROM settings WHERE key = 'poll_interval'`).Scan(&intervalStr) == nil {
		if seconds, err := time.ParseDuration(intervalStr + "s"); err == nil {
			cfg.Interval = seconds
		}
	}

	// Feature flag: Wave/MLO peer endpoint fallback (off by default)
	var wavePeerFallbackStr string
	if db.QueryRow(`SELECT value FROM settings WHERE key = 'wave_peer_fallback'`).Scan(&wavePeerFallbackStr) == nil {
		vv := strings.ToLower(strings.TrimSpace(wavePeerFallbackStr))
		switch vv {
		case "1", "true", "yes", "y", "on":
			cfg.WavePeerFallback = true
		}
	}

	// Feature flag: Wave MLO multi-radio mapping (off by default)
	// When enabled, Wave radios are slotted by inferred band (tx frequency, AFC presence, channel width),
	// and a second 5 GHz radio (MLO5) is surfaced in the 6 GHz slot.
	var waveMLOMultiRadioStr string
	if db.QueryRow(`SELECT value FROM settings WHERE key = 'wave_mlo_multi_radio'`).Scan(&waveMLOMultiRadioStr) == nil {
		vv := strings.ToLower(strings.TrimSpace(waveMLOMultiRadioStr))
		switch vv {
		case "1", "true", "yes", "y", "on":
			cfg.WaveMLOMultiRadio = true
		}
	}

	// Credentials are loaded by Poller.ReloadSettings immediately before the
	// first poll. Keeping one loader prevents encrypted values from ever being
	// copied into the runtime configuration as ciphertext.

	// Allow overriding worker count from settings
	var workerCountStr string
	if db.QueryRow(`SELECT value FROM settings WHERE key = 'poller_workers'`).Scan(&workerCountStr) == nil {
		if n, err := strconv.Atoi(workerCountStr); err == nil && n > 0 {
			cfg.WorkerCount = n
		}
	}

	return cfg
}

func parseTrustedProxies(raw string) ([]netip.Prefix, error) {
	var prefixes []netip.Prefix
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if prefix, err := netip.ParsePrefix(part); err == nil {
			prefixes = append(prefixes, prefix.Masked())
			continue
		}
		addr, err := netip.ParseAddr(part)
		if err != nil {
			return nil, fmt.Errorf("%q is not an IP address or CIDR", part)
		}
		bits := 128
		if addr.Is4() {
			bits = 32
		}
		prefixes = append(prefixes, netip.PrefixFrom(addr, bits))
	}
	return prefixes, nil
}

func requestRemoteIP(r *http.Request) (netip.Addr, bool) {
	raw := strings.TrimSpace(r.RemoteAddr)
	if host, _, err := net.SplitHostPort(raw); err == nil {
		raw = host
	}
	raw = strings.Trim(raw, "[]")
	addr, err := netip.ParseAddr(raw)
	return addr, err == nil
}

func isTrustedProxy(addr netip.Addr, prefixes []netip.Prefix) bool {
	for _, prefix := range prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func stripProxyHeaders(h http.Header) {
	for _, name := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Port", "X-Forwarded-Proto", "X-Real-IP"} {
		h.Del(name)
	}
}

// trustedProxyMiddleware accepts forwarding headers only from explicitly
// configured reverse proxies. The effective client address is selected by
// walking X-Forwarded-For from the trusted edge toward the first untrusted hop.
func trustedProxyMiddleware(prefixes []netip.Prefix) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			peer, ok := requestRemoteIP(r)
			if !ok || !isTrustedProxy(peer, prefixes) {
				stripProxyHeaders(r.Header)
				next.ServeHTTP(w, r)
				return
			}

			effective := peer
			if raw := r.Header.Get("X-Forwarded-For"); raw != "" {
				parts := strings.Split(raw, ",")
				for i := len(parts) - 1; i >= 0 && isTrustedProxy(effective, prefixes); i-- {
					candidate, err := netip.ParseAddr(strings.TrimSpace(strings.Trim(parts[i], "[]")))
					if err != nil {
						break
					}
					effective = candidate
				}
			} else if raw := strings.TrimSpace(r.Header.Get("X-Real-IP")); raw != "" {
				if candidate, err := netip.ParseAddr(strings.Trim(raw, "[]")); err == nil {
					effective = candidate
				}
			}
			r.RemoteAddr = effective.String()

			// Use only the value appended by the nearest trusted proxy. Taking the
			// first element would let a client-supplied prefix influence Secure-cookie
			// and same-origin decisions when a proxy appends rather than replaces XFP.
			proto := ""
			if values := strings.Split(r.Header.Get("X-Forwarded-Proto"), ","); len(values) > 0 {
				candidate := strings.ToLower(strings.TrimSpace(values[len(values)-1]))
				if candidate == "http" || candidate == "https" {
					proto = candidate
				}
			}
			stripProxyHeaders(r.Header)
			if proto != "" {
				r.Header.Set("X-Forwarded-Proto", proto)
			}
			next.ServeHTTP(w, r)
		})
	}
}

func clientIPFromRequest(r *http.Request) string {
	if addr, ok := requestRemoteIP(r); ok {
		return addr.String()
	}
	return strings.TrimSpace(r.RemoteAddr)
}

func normalizeOrigin(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("invalid origin %q", raw)
	}
	if u.Path != "" && u.Path != "/" {
		return "", fmt.Errorf("origin must not contain a path: %q", raw)
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host), nil
}

func loadAllowedOrigins(db *sql.DB) []string {
	var raw string
	if err := db.QueryRow(`SELECT value FROM settings WHERE key = 'cors_origins'`).Scan(&raw); err != nil || strings.TrimSpace(raw) == "" {
		return nil
	}
	var values []string
	trimmed := strings.TrimSpace(raw)
	if trimmed == "*" {
		logError("cors_origins wildcard is not supported with cookie authentication; using same-origin only")
		return nil
	}
	if strings.HasPrefix(trimmed, "[") {
		if err := json.Unmarshal([]byte(trimmed), &values); err != nil {
			logError("invalid cors_origins JSON: %v", err)
			return nil
		}
	} else {
		values = []string{trimmed}
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		origin, err := normalizeOrigin(value)
		if err != nil {
			logError("ignoring %v", err)
			continue
		}
		if _, ok := seen[origin]; ok {
			continue
		}
		seen[origin] = struct{}{}
		out = append(out, origin)
	}
	sort.Strings(out)
	return out
}

func originGuard(allowedOrigins []string) func(http.Handler) http.Handler {
	allowed := make(map[string]struct{}, len(allowedOrigins))
	for _, origin := range allowedOrigins {
		allowed[strings.ToLower(origin)] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet, http.MethodHead, http.MethodOptions:
				next.ServeHTTP(w, r)
				return
			}

			// Cookie-authenticated state changes require a non-simple custom header.
			// Cross-site forms cannot set it, and cross-origin scripts must first pass
			// the explicit CORS policy. Bearer API clients are not cookie-CSRF prone.
			_, cookieErr := r.Cookie(sessionCookieName)
			hasSessionCookie := cookieErr == nil
			hasBearer := strings.HasPrefix(strings.TrimSpace(r.Header.Get("Authorization")), "Bearer ")
			if hasSessionCookie && !hasBearer && r.Header.Get("X-WaveControl-CSRF") != "1" {
				http.Error(w, "CSRF validation failed", http.StatusForbidden)
				return
			}

			rawOrigin := strings.TrimSpace(r.Header.Get("Origin"))
			if rawOrigin == "" {
				// Non-browser clients do not generally send Origin. Their credentials
				// are still protected by authentication and role checks.
				next.ServeHTTP(w, r)
				return
			}
			origin, err := normalizeOrigin(rawOrigin)
			if err != nil {
				http.Error(w, "invalid origin", http.StatusForbidden)
				return
			}
			scheme := "http"
			if requestIsHTTPS(r) {
				scheme = "https"
			}
			sameOrigin := scheme + "://" + strings.ToLower(r.Host)
			if strings.EqualFold(origin, sameOrigin) {
				next.ServeHTTP(w, r)
				return
			}
			if _, ok := allowed[strings.ToLower(origin)]; ok {
				next.ServeHTTP(w, r)
				return
			}
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		})
	}
}

func buildRouter(db *sql.DB, jwtSecret []byte, statsStore *stats.Store, fwService *firmware.Service, devicePoller *poller.Poller, wsHub *websocket.Hub, jobScheduler *scheduler.Scheduler, jobRunner *jobs.Runner, ultraDebug *udebug.Manager, tlsManager *tlsutil.Manager, bulkOps *bulkops.Controller, alertManager *alerting.Manager, secretStore *secrets.Manager, webRoot string, verbose bool) *chi.Mux {
	r := chi.NewRouter()

	// Middleware (applied globally)
	r.Use(chimw.RequestID)
	trustedProxies, err := parseTrustedProxies(os.Getenv("WAVECONTROL_TRUSTED_PROXIES"))
	if err != nil {
		logError("invalid WAVECONTROL_TRUSTED_PROXIES: %v; proxy headers will not be trusted", err)
		trustedProxies = nil
	}
	r.Use(trustedProxyMiddleware(trustedProxies))
	if verbose {
		r.Use(chimw.Logger)
	}
	r.Use(chimw.Recoverer)
	// Note: Timeout middleware applied selectively per route group below
	// WebSocket and long operations need different timeouts

	// Build CSP header from settings
	cspHeader := buildCSP(db)
	r.Use(securityHeadersWithCSP(cspHeader))
	r.Use(rateLimiter(300, time.Minute))

	// Cross-origin cookie authentication is allowed only for explicit HTTPS/HTTP
	// origins. Wildcards are intentionally rejected because credentialed CORS
	// and wildcard origins are an unsafe combination.
	corsOrigins := loadAllowedOrigins(db)
	if len(corsOrigins) > 0 {
		r.Use(cors.Handler(cors.Options{
			AllowedOrigins:   corsOrigins,
			AllowedMethods:   []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
			AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-WaveControl-CSRF"},
			AllowCredentials: true,
			MaxAge:           300,
		}))
		logDebug("CORS enabled for origins: %v", corsOrigins)
	} else {
		logDebug("CORS disabled (same-origin only)")
	}
	r.Use(originGuard(corsOrigins))

	// Configure WebSocket origin validation to match CORS policy.
	wsHub.SetAllowedOrigins(corsOrigins)

	// Health check
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		if err := db.Ping(); err != nil {
			http.Error(w, "db", 503)
			return
		}
		w.Write([]byte("ok"))
	})

	// SPA with static file serving
	// Use os.DirFS for safe file serving that cannot escape webRoot
	webFS := os.DirFS(webRoot)
	fsHandler := http.FileServer(http.FS(webFS)) // Go 1.21 compatible

	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		indexPath := filepath.Join(webRoot, "index.html")
		if _, err := os.Stat(indexPath); err != nil {
			log.Printf("ERROR: index.html not found at %s: %v", indexPath, err)
			http.Error(w, fmt.Sprintf("index.html not found at %s", indexPath), 500)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, indexPath)
	})
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		// Clean and force relative path using path (not filepath) for URL handling
		// path.Clean normalizes the URL path, removing .. and double slashes
		urlPath := path.Clean("/" + r.URL.Path)
		// Remove leading slash to make it relative for fs.Stat
		relPath := strings.TrimPrefix(urlPath, "/")

		// If path is empty or tries to escape, serve index.html
		if relPath == "" || relPath == "." {
			http.ServeFile(w, r, filepath.Join(webRoot, "index.html"))
			return
		}

		// Check if file exists in webFS (safe - cannot escape webRoot)
		if f, err := fs.Stat(webFS, relPath); err == nil && !f.IsDir() {
			// Create modified request with cleaned path for FileServer
			r2 := new(http.Request)
			*r2 = *r
			r2.URL = new(url.URL)
			*r2.URL = *r.URL
			r2.URL.Path = "/" + relPath
			fsHandler.ServeHTTP(w, r2)
			return
		}

		// Fall back to index.html for SPA routing
		http.ServeFile(w, r, filepath.Join(webRoot, "index.html"))
	})

	// API
	api := NewAPI(db, jwtSecret, statsStore, fwService, devicePoller, wsHub, jobScheduler, jobRunner, ultraDebug, tlsManager, bulkOps, alertManager, secretStore)
	r.Route("/api/wavecontrol", func(apiR chi.Router) {
		// Public endpoints (60s timeout)
		apiR.Group(func(pub chi.Router) {
			pub.Use(chimw.Timeout(60 * time.Second))
			pub.Post("/auth/login", api.Login)
			pub.Post("/auth/logout", api.Logout) // Clears HttpOnly cookie
			pub.Get("/ping", api.Ping)
		})

		// WebSocket - NO timeout (connections are long-lived)
		apiR.Group(func(ws chi.Router) {
			ws.Use(api.AuthMiddleware)
			ws.Get("/ws", api.WebSocket)
		})

		// Long-running operations (5 minute timeout)
		// Firmware upgrades, bulk operations, config backup/restore
		apiR.Group(func(longOps chi.Router) {
			longOps.Use(api.AuthMiddleware)
			longOps.Use(chimw.Timeout(5 * time.Minute))

			// Firmware upgrades
			longOps.Post("/devices/{id}/upgrade", api.UpgradeDevice)
			longOps.Post("/devices/{id}/upgrade-fanout", api.UpgradeFanout)
			longOps.Post("/devices/bulk-upgrade", api.BulkUpgrade)
			longOps.Post("/devices/retry-upgrade", api.RetryUpgradeWithCredentials)

			// Bulk device operations
			longOps.Post("/devices", api.AddDevice)
			longOps.Post("/devices/bulk-add", api.BulkAddDevices)

			// Config backup/restore (involves device communication)
			longOps.Post("/devices/{id}/backup", api.BackupConfig)
			longOps.Post("/devices/{id}/restore", api.RestoreConfig)
			longOps.Post("/devices/bulk-backup", api.BulkBackup)
			longOps.Post("/devices/batch-config", api.BatchConfig)

			// Report generation (can be slow with many devices)
			longOps.Post("/reports/generate", api.GenerateReport)

			// Firmware upload (up to 1GB)
			longOps.Post("/firmware", api.UploadFirmware)
		})

		// Standard protected endpoints (60s timeout)
		apiR.Group(func(priv chi.Router) {
			priv.Use(api.AuthMiddleware)
			priv.Use(chimw.Timeout(60 * time.Second))

			priv.Get("/me", api.Me)

			// Devices
			priv.Get("/devices", api.ListDevices)
			priv.Get("/devices/{id}", api.GetDevice)
			priv.Get("/devices/{id}/identity-mismatch", api.GetDeviceIdentityMismatch)
			priv.Post("/devices/{id}/learn-mac", api.LearnDeviceMAC)
			priv.Delete("/devices/{id}", api.DeleteDevice)
			priv.Post("/devices/{id}/refresh", api.RefreshDevice)
			priv.Post("/devices/{id}/reboot", api.RebootDevice)
			priv.Patch("/devices/{id}/alerting", api.UpdateDeviceAlerting)
			priv.Post("/devices/bulk-alerting", api.BulkUpdateDeviceAlerting)
			priv.Patch("/devices/{id}/antenna", api.UpdateDeviceAntenna)
			// Resolve the best UI scheme (http/https) and redirect the browser.
			priv.Get("/open-ui", api.OpenDeviceUI)

			// Ultra Debug (per-device request/response ring buffers)
			priv.Post("/devices/{id}/ultra-debug", api.SetUltraDebug)
			priv.Get("/ultra-debug", api.ListUltraDebug)
			priv.Get("/ultra-debug/{id}", api.GetUltraDebug)
			priv.Get("/ultra-debug/{id}/download", api.DownloadUltraDebug)
			priv.Post("/ultra-debug/{id}/clear", api.ClearUltraDebug)
			// Host-scoped ultra debug (non-deviceID flows)
			priv.Post("/ultra-debug/host", api.SetUltraDebugHost)
			priv.Get("/ultra-debug/host/{host}", api.GetUltraDebugHost)
			priv.Get("/ultra-debug/host/{host}/download", api.DownloadUltraDebugHost)
			priv.Post("/ultra-debug/host/{host}/clear", api.ClearUltraDebugHost)

			// Debug / rollout instrumentation
			priv.Get("/debug/wave-parse-counters", api.GetWaveParseCounters)

			// Stats (real-time from memory)
			priv.Get("/stats", api.ListStats)
			priv.Get("/stats/{ip}", api.GetStats)
			priv.Get("/stats/throughput-history", api.GetThroughputHistory)
			priv.Get("/stats/stability", api.GetStabilityStats)

			// Firmware list and delete (read/delete operations are fast)
			priv.Get("/firmware", api.ListFirmware)
			priv.Get("/firmware/versions", api.ListFirmwareVersions)
			priv.Get("/firmware/{name}/download", api.DownloadFirmware)
			priv.Delete("/firmware/{name}", api.DeleteFirmware)

			// Scheduled Jobs (legacy scheduler)
			priv.Get("/jobs", api.ListJobs)
			priv.Post("/jobs", api.CreateJob)
			priv.Patch("/jobs/{id}", api.UpdateJob)
			priv.Delete("/jobs/{id}", api.CancelJob)

			// Async Job Runs (new job runner)
			priv.Get("/job-runs", api.ListJobRuns)
			priv.Post("/job-runs", api.StartJobRun)
			priv.Get("/job-runs/{id}", api.GetJobRun)
			priv.Get("/job-runs/{id}/events", api.GetJobRunEvents)
			priv.Delete("/job-runs/{id}", api.CancelJobRun)

			// Maintenance Windows
			priv.Get("/maintenance-windows", api.ListMaintenanceWindows)
			priv.Post("/maintenance-windows", api.CreateMaintenanceWindow)
			priv.Patch("/maintenance-windows/{id}", api.UpdateMaintenanceWindow)
			priv.Delete("/maintenance-windows/{id}", api.DeleteMaintenanceWindow)

			// Scheduler Settings
			priv.Get("/scheduler/settings", api.GetSchedulerSettings)
			priv.Patch("/scheduler/settings", api.UpdateSchedulerSettings)

			// Config list and download (just reading filesystem)
			priv.Get("/devices/{id}/configs", api.ListConfigs)
			priv.Get("/configs", api.ListAllConfigs)
			priv.Get("/configs/download", api.DownloadConfig)

			// Reports
			priv.Get("/reports", api.ListReports)
			priv.Get("/reports/{id}", api.GetReport)
			priv.Delete("/reports/{id}", api.DeleteReport)
			priv.Get("/reports/{id}/download", api.DownloadReport)
			priv.Post("/reports/compare", api.CompareReports)

			// Search
			priv.Get("/search", api.Search)

			// Logs
			priv.Get("/logs", api.ListLogs)

			// Users (admin only in handlers)
			priv.Get("/users", api.ListUsers)
			priv.Post("/users", api.CreateUser)
			priv.Patch("/users/{id}", api.UpdateUser)
			priv.Delete("/users/{id}", api.DeleteUser)
			priv.Get("/roles", api.ListRoles)

			// Settings
			priv.Get("/settings", api.ListSettings)
			priv.Patch("/settings", api.UpdateSettings)
			priv.Patch("/settings/{key}", api.UpdateSetting)

			// Sites
			priv.Get("/sites", api.ListSites)
			priv.Post("/sites", api.CreateSite)
			priv.Patch("/sites/{id}", api.UpdateSite)
			priv.Delete("/sites/{id}", api.DeleteSite)

			// Regions
			priv.Get("/regions", api.ListRegions)
			priv.Post("/regions", api.CreateRegion)
			priv.Patch("/regions/{id}", api.UpdateRegion)
			priv.Delete("/regions/{id}", api.DeleteRegion)

			// TLS Certificate Management
			priv.Get("/tls/mode", api.GetTLSMode)
			priv.Patch("/tls/mode", api.SetTLSMode)
			priv.Get("/tls/certs", api.GetAllCerts)
			priv.Get("/tls/certs/pending", api.GetPendingCerts)
			priv.Get("/tls/certs/stats", api.GetCertStats)
			priv.Post("/tls/certs/learn", api.BulkLearnCerts)
			priv.Post("/tls/certs/verify-all", api.BulkVerifyCerts)
			priv.Get("/devices/{id}/cert", api.GetDeviceCert)
			priv.Post("/devices/{id}/cert/pin", api.PinDeviceCert)
			priv.Post("/devices/{id}/cert/verify", api.VerifyDeviceCert)
			priv.Delete("/devices/{id}/cert", api.UnpinDeviceCert)
			priv.Delete("/sites/{id}/certs", api.UnpinSiteCerts)
			priv.Get("/devices/{id}/cert/current", api.GetCurrentDeviceCert)

			// Alert Rules
			priv.Get("/alerts/rules", api.ListAlertRules)
			priv.Post("/alerts/rules", api.CreateAlertRule)
			priv.Patch("/alerts/rules/{id}", api.UpdateAlertRule)
			priv.Delete("/alerts/rules/{id}", api.DeleteAlertRule)

			// Alerts
			priv.Get("/alerts", api.ListAlerts)
			priv.Post("/alerts/{id}/acknowledge", api.AcknowledgeAlert)
			priv.Post("/alerts/{id}/resolve", api.ResolveAlert)

			// Bulk Operations Config
			priv.Get("/bulk-ops/config", api.GetBulkOpsConfig)
			priv.Patch("/bulk-ops/config", api.UpdateBulkOpsConfig)
			priv.Get("/bulk-ops/stats", api.GetBulkOpsStats)

			// Poller Config
			priv.Get("/poller/config", api.GetPollerConfig)
			priv.Patch("/poller/config", api.UpdatePollerConfig)

			// Dry-run endpoint
			priv.Post("/devices/dry-run", api.DryRunOperation)

			// Bulk assign devices to site
			priv.Post("/devices/bulk-assign-site", api.BulkAssignSite)

			// Drilldown Lists
			priv.Get("/drilldown-lists", api.ListDrilldownLists)
			priv.Post("/drilldown-lists", api.CreateDrilldownList)
			priv.Patch("/drilldown-lists/{id}", api.UpdateDrilldownList)
			priv.Delete("/drilldown-lists/{id}", api.DeleteDrilldownList)
			priv.Get("/drilldown-lists/{id}/hosts", api.GetDrilldownHosts)
			priv.Post("/drilldown-lists/{id}/hosts", api.AddDrilldownHost)
			priv.Delete("/drilldown-lists/{id}/hosts/{hostId}", api.RemoveDrilldownHost)
		})
	})

	return r
}

func normalizeCSPSource(raw string, allowedSchemes map[string]struct{}) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsAny(raw, " \t\r\n;'\"") {
		return "", fmt.Errorf("invalid CSP source %q", raw)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("invalid CSP source %q", raw)
	}
	scheme := strings.ToLower(u.Scheme)
	if _, ok := allowedSchemes[scheme]; !ok {
		return "", fmt.Errorf("unsupported CSP source scheme in %q", raw)
	}
	if u.Path != "" && u.Path != "/" {
		return "", fmt.Errorf("CSP source must be an origin: %q", raw)
	}
	return scheme + "://" + strings.ToLower(u.Host), nil
}

func loadCSPSources(db *sql.DB, key string, allowedSchemes map[string]struct{}) []string {
	var raw string
	if err := db.QueryRow(`SELECT value FROM settings WHERE key = $1`, key).Scan(&raw); err != nil || strings.TrimSpace(raw) == "" {
		return nil
	}

	var candidates []string
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, "[") {
		if err := json.Unmarshal([]byte(trimmed), &candidates); err != nil {
			logError("ignoring invalid %s JSON: %v", key, err)
			return nil
		}
	} else {
		candidates = strings.Fields(trimmed)
	}

	seen := make(map[string]struct{}, len(candidates))
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		source, err := normalizeCSPSource(candidate, allowedSchemes)
		if err != nil {
			logError("ignoring %v from %s", err, key)
			continue
		}
		if _, ok := seen[source]; ok {
			continue
		}
		seen[source] = struct{}{}
		out = append(out, source)
	}
	sort.Strings(out)
	return out
}

func buildCSP(db *sql.DB) string {
	imgSrc := []string{"'self'", "data:", "https://*.tile.openstreetmap.org", "https://*.basemaps.cartocdn.com", "https://unpkg.com"}
	connectSrc := []string{"'self'", "ws:", "wss:"}
	imgSrc = append(imgSrc, loadCSPSources(db, "csp_img_sources", map[string]struct{}{"http": {}, "https": {}})...)
	connectSrc = append(connectSrc, loadCSPSources(db, "csp_connect_sources", map[string]struct{}{"http": {}, "https": {}, "ws": {}, "wss": {}})...)

	return fmt.Sprintf(
		"default-src 'self'; "+
			"base-uri 'self'; "+
			"object-src 'none'; "+
			"frame-ancestors 'none'; "+
			"form-action 'self'; "+
			"script-src 'self' https://unpkg.com; "+
			"script-src-attr 'none'; "+
			"style-src 'self' 'unsafe-inline' https://unpkg.com; "+
			"font-src 'self' data:; "+
			"img-src %s; "+
			"connect-src %s",
		strings.Join(imgSrc, " "), strings.Join(connectSrc, " "))
}

func securityHeadersWithCSP(csp string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("X-Frame-Options", "DENY")
			w.Header().Set("X-XSS-Protection", "1; mode=block")
			w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
			w.Header().Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
			w.Header().Set("Content-Security-Policy", csp)
			// HSTS - only set if connection is TLS (reverse proxy handles TLS)
			// Uncomment if serving directly over TLS:
			// w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
			next.ServeHTTP(w, r)
		})
	}
}

func rateLimiter(limit int, window time.Duration) func(http.Handler) http.Handler {
	const maxClients = 10000 // Prevent unbounded growth

	type client struct {
		count   int
		resetAt time.Time
	}
	var mu sync.Mutex
	clients := make(map[string]*client)

	// Cleanup goroutine
	go func() {
		ticker := time.NewTicker(window / 2) // Cleanup twice per window
		defer ticker.Stop()
		for range ticker.C {
			mu.Lock()
			now := time.Now()
			for ip, c := range clients {
				if now.After(c.resetAt) {
					delete(clients, ip)
				}
			}
			mu.Unlock()
		}
	}()

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := clientIPFromRequest(r)

			mu.Lock()
			// Prevent unbounded growth - if at max, reject new IPs
			if len(clients) >= maxClients {
				if _, ok := clients[ip]; !ok {
					mu.Unlock()
					http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
					return
				}
			}

			c, ok := clients[ip]
			now := time.Now()
			if !ok || now.After(c.resetAt) {
				c = &client{count: 0, resetAt: now.Add(window)}
				clients[ip] = c
			}
			c.count++
			count := c.count
			mu.Unlock()

			if count > limit {
				http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

func spaIndex(indexPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Check if file exists before serving
		if _, err := os.Stat(indexPath); err != nil {
			log.Printf("ERROR: index.html not found at %s: %v", indexPath, err)
			http.Error(w, fmt.Sprintf("index.html not found at %s", indexPath), 500)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, indexPath)
	}
}

// JSON helper
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// checkSchema verifies that the base database schema has been loaded before
// idempotent runtime migrations are attempted.
func checkSchema(db *sql.DB) error {
	required := []string{
		"roles", "users", "user_roles", "settings", "regions", "sites", "devices",
		"firmware_jobs", "scheduled_jobs", "changelog", "device_certs", "alert_rules",
		"alerts", "alert_states", "reports", "job_runs", "job_events",
		"maintenance_windows", "drilldown_lists", "drilldown_hosts",
	}
	missing := make([]string, 0)
	for _, table := range required {
		var exists bool
		if err := db.QueryRow(`
			SELECT to_regclass('public.' || $1) IS NOT NULL
		`, table).Scan(&exists); err != nil {
			return fmt.Errorf("validate table %s: %w", table, err)
		}
		if !exists {
			missing = append(missing, table)
		}
	}
	if len(missing) != 0 {
		return fmt.Errorf("database schema not loaded or incomplete (missing tables: %s); import schema.sql before starting waveControl", strings.Join(missing, ", "))
	}
	return nil
}
