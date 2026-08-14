package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
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
	"github.com/yellowman/wavecontrol/internal/push"
	"github.com/yellowman/wavecontrol/internal/scheduler"
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
	if jwtSecret == "" {
		logFatal("WAVECONTROL_JWT_SECRET is required")
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

	// Ensure newer optional columns exist (safe idempotent schema tweak)
	ensureDeviceStatusReasonColumn(db)
	ensureDeviceAntennaColumns(db)
	ensureDeviceSectorPlanningColumns(db)
	ensureSiteTowerHeightColumn(db)
	ensureDeviceIdentityMismatchSchema(db)
	ensureMobilePushSchema(db)
	ensureAlertTargetPolicySchema(db)

	// Database pool sized for 2000 APs with 50 workers
	// Each worker may have 2-3 concurrent queries
	db.SetMaxOpenConns(100)
	db.SetMaxIdleConns(25)
	db.SetConnMaxLifetime(5 * time.Minute)

	// Initialize settings with defaults if not present
	initSettings(db)

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
	fwService := firmware.NewService(db, tlsManager, ultraDebug)

	// Rebuild device hierarchy from parent_mac if parent_id is NULL
	// This handles cases where wavecontrol restarted and parent devices changed IDs
	rebuildDeviceHierarchy(db)

	// Load poller config from settings
	pollerCfg := loadPollerConfig(db, debugMode, tlsManager, ultraDebug)

	// Create and start poller (with websocket hub)
	devicePoller := poller.NewPoller(db, statsStore, wsHub, pollerCfg)

	// Create scheduler
	jobScheduler := scheduler.NewScheduler(db, fwService, wsHub)

	// Create job runner for async operations
	jobRunner := jobs.NewRunner(db, fwService, wsHub)
	jobRunner.Start() // Recover pending jobs from previous runs

	// Create bulk operations controller
	bulkOps := bulkops.NewController(db)

	// Create durable mobile push service for Android/iOS notifications
	pushService, err := push.NewService(db, []byte(jwtSecret))
	if err != nil {
		logFatal("mobile push service: %v", err)
	}
	go pushService.Start(ctx)

	// Create alerting manager
	alertManager := alerting.NewManager(db, statsStore)
	alertManager.SetMobileNotifier(mobilePushAdapter{svc: pushService})

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
	r := buildRouter(db, []byte(jwtSecret), statsStore, fwService, devicePoller, wsHub, jobScheduler, jobRunner, ultraDebug, tlsManager, bulkOps, alertManager, pushService, *flagWebRoot, debugMode)

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

func initSettings(db *sql.DB) {
	// Paths are relative to the working directory (user's home dir or chroot)
	defaults := map[string]string{
		"poll_interval":        "30",
		"default_username":     "ubnt",
		"default_passwords":    `["ubnt"]`,
		"firmware_path":        "firmware",
		"backup_dir":           "backups",
		"listen_addr":          "127.0.0.1:8080",
		"zabbix_enabled":       "false",
		"zabbix_listen":        "127.0.0.1:10050",
		"cors_origins":         "", // Empty = same-origin only; "*" = allow all; or JSON array of origins
		"csp_img_sources":      "", // Additional img-src domains for map tiles (space-separated)
		"csp_connect_sources":  "", // Additional connect-src domains for map APIs (space-separated)
		"wave_peer_fallback":   "false",
		"wave_mlo_multi_radio": "false",
	}

	for key, value := range defaults {
		if _, err := db.Exec(`INSERT INTO settings (key, value) VALUES ($1, $2) ON CONFLICT (key) DO NOTHING`, key, value); err != nil {
			log.Printf("Failed to set default %s: %v", key, err)
		}
	}
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

func loadPollerConfig(db *sql.DB, debug bool, tlsMgr *tlsutil.Manager, udbg *udebug.Manager) poller.Config {
	cfg := poller.Config{
		Interval:    30 * time.Second,
		APCreds:     []poller.Credential{{Username: "ubnt", Password: "ubnt"}},
		STACreds:    []poller.Credential{{Username: "ubnt", Password: "ubnt"}},
		WorkerCount: 50, // Default for large deployments (2000 APs)
		Debug:       debug,
		TLSManager:  tlsMgr,
		UltraDebug:  udbg,
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

	// Load AP credentials (up to 3 pairs)
	var apCreds []poller.Credential
	for i := 1; i <= 3; i++ {
		var user, pass string
		userKey := fmt.Sprintf("ap_cred%d_user", i)
		passKey := fmt.Sprintf("ap_cred%d_pass", i)
		db.QueryRow(`SELECT value FROM settings WHERE key = $1`, userKey).Scan(&user)
		db.QueryRow(`SELECT value FROM settings WHERE key = $1`, passKey).Scan(&pass)
		if user != "" && pass != "" {
			apCreds = append(apCreds, poller.Credential{Username: user, Password: pass})
		}
	}
	// Fallback to legacy format if no new credentials
	if len(apCreds) == 0 {
		var apUser, apPassJSON string
		if db.QueryRow(`SELECT value FROM settings WHERE key = 'ap_username'`).Scan(&apUser) == nil && apUser != "" {
			if db.QueryRow(`SELECT value FROM settings WHERE key = 'ap_passwords'`).Scan(&apPassJSON) == nil {
				var passes []string
				if json.Unmarshal([]byte(apPassJSON), &passes) == nil {
					for _, pass := range passes {
						apCreds = append(apCreds, poller.Credential{Username: apUser, Password: pass})
					}
				}
			}
		}
	}
	// Final fallback
	if len(apCreds) == 0 {
		var defUser, defPassJSON string
		db.QueryRow(`SELECT value FROM settings WHERE key = 'default_username'`).Scan(&defUser)
		db.QueryRow(`SELECT value FROM settings WHERE key = 'default_passwords'`).Scan(&defPassJSON)
		if defUser == "" {
			defUser = "ubnt"
		}
		var passes []string
		if json.Unmarshal([]byte(defPassJSON), &passes) == nil && len(passes) > 0 {
			for _, pass := range passes {
				apCreds = append(apCreds, poller.Credential{Username: defUser, Password: pass})
			}
		}
	}
	if len(apCreds) > 0 {
		cfg.APCreds = apCreds
	}

	// Load STA credentials (up to 3 pairs)
	var staCreds []poller.Credential
	for i := 1; i <= 3; i++ {
		var user, pass string
		userKey := fmt.Sprintf("sta_cred%d_user", i)
		passKey := fmt.Sprintf("sta_cred%d_pass", i)
		db.QueryRow(`SELECT value FROM settings WHERE key = $1`, userKey).Scan(&user)
		db.QueryRow(`SELECT value FROM settings WHERE key = $1`, passKey).Scan(&pass)
		if user != "" && pass != "" {
			staCreds = append(staCreds, poller.Credential{Username: user, Password: pass})
		}
	}
	// Fallback to legacy format if no new credentials
	if len(staCreds) == 0 {
		var staUser, staPassJSON string
		if db.QueryRow(`SELECT value FROM settings WHERE key = 'sta_username'`).Scan(&staUser) == nil && staUser != "" {
			if db.QueryRow(`SELECT value FROM settings WHERE key = 'sta_passwords'`).Scan(&staPassJSON) == nil {
				var passes []string
				if json.Unmarshal([]byte(staPassJSON), &passes) == nil {
					for _, pass := range passes {
						staCreds = append(staCreds, poller.Credential{Username: staUser, Password: pass})
					}
				}
			}
		}
	}
	// Final fallback to AP credentials
	if len(staCreds) > 0 {
		cfg.STACreds = staCreds
	} else {
		cfg.STACreds = cfg.APCreds
	}

	// Allow overriding worker count from settings
	var workerCountStr string
	if db.QueryRow(`SELECT value FROM settings WHERE key = 'poller_workers'`).Scan(&workerCountStr) == nil {
		if n, err := strconv.Atoi(workerCountStr); err == nil && n > 0 {
			cfg.WorkerCount = n
		}
	}

	return cfg
}

func buildRouter(db *sql.DB, jwtSecret []byte, statsStore *stats.Store, fwService *firmware.Service, devicePoller *poller.Poller, wsHub *websocket.Hub, jobScheduler *scheduler.Scheduler, jobRunner *jobs.Runner, ultraDebug *udebug.Manager, tlsManager *tlsutil.Manager, bulkOps *bulkops.Controller, alertManager *alerting.Manager, pushService *push.Service, webRoot string, verbose bool) *chi.Mux {
	r := chi.NewRouter()

	// Middleware (applied globally)
	r.Use(chimw.RequestID)
	r.Use(chimw.RealIP)
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

	// CORS configuration
	// Default: same-origin only (empty = no CORS headers)
	// Set cors_origins to "*" for development or JSON array for production: ["https://app.example.com"]
	corsOrigins := []string{}
	var corsOriginsStr string
	if db.QueryRow(`SELECT value FROM settings WHERE key = 'cors_origins'`).Scan(&corsOriginsStr) == nil && corsOriginsStr != "" {
		if corsOriginsStr == "*" {
			corsOrigins = []string{"*"}
		} else if strings.HasPrefix(corsOriginsStr, "[") {
			// Parse as JSON array
			if err := json.Unmarshal([]byte(corsOriginsStr), &corsOrigins); err != nil {
				logError("Invalid cors_origins JSON: %v", err)
			}
		} else {
			// Single origin string
			corsOrigins = []string{corsOriginsStr}
		}
	}

	if len(corsOrigins) > 0 {
		r.Use(cors.Handler(cors.Options{
			AllowedOrigins:   corsOrigins,
			AllowedMethods:   []string{"GET", "POST", "PATCH", "DELETE", "OPTIONS"},
			AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type"},
			AllowCredentials: false, // Bearer tokens don't need credentials mode
			MaxAge:           300,
		}))
		logDebug("CORS enabled for origins: %v", corsOrigins)
	} else {
		logDebug("CORS disabled (same-origin only)")
	}

	// Configure WebSocket origin validation to match CORS policy
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
	api := NewAPI(db, jwtSecret, statsStore, fwService, devicePoller, wsHub, jobScheduler, jobRunner, ultraDebug, tlsManager, bulkOps, alertManager, pushService)
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
			priv.Patch("/settings/{key}", api.UpdateSetting)

			// Native mobile push clients
			priv.Post("/mobile/register", api.RegisterMobileDevice)
			priv.Delete("/mobile/register", api.UnregisterMobileDevice)
			priv.Get("/mobile/devices", api.ListMobileDevices)
			priv.Get("/mobile/preferences", api.GetMobilePreferences)
			priv.Patch("/mobile/preferences", api.UpdateMobilePreferences)
			priv.Get("/mobile/bootstrap", api.MobileBootstrap)
			priv.Get("/mobile/alerts", api.MobileAlerts)
			priv.Post("/mobile/test-push", api.SendMobileTestPush)

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

func buildCSP(db *sql.DB) string {
	// Base CSP - safe defaults
	imgSrc := "'self' data: https://*.tile.openstreetmap.org https://*.basemaps.cartocdn.com"
	connectSrc := "'self' wss:"

	// Load additional sources from settings
	var extraImg, extraConnect string
	db.QueryRow(`SELECT value FROM settings WHERE key = 'csp_img_sources'`).Scan(&extraImg)
	db.QueryRow(`SELECT value FROM settings WHERE key = 'csp_connect_sources'`).Scan(&extraConnect)

	// Append extra sources (space-separated domains)
	if extraImg = strings.TrimSpace(extraImg); extraImg != "" {
		imgSrc += " " + extraImg
	}
	if extraConnect = strings.TrimSpace(extraConnect); extraConnect != "" {
		connectSrc += " " + extraConnect
	}

	// Build full CSP
	return fmt.Sprintf(
		"default-src 'self'; "+
			"script-src 'self' 'unsafe-inline' https://unpkg.com https://d3js.org; "+
			"style-src 'self' 'unsafe-inline' https://unpkg.com https://fonts.googleapis.com; "+
			"font-src 'self' https://fonts.gstatic.com; "+
			"img-src %s; "+
			"connect-src %s",
		imgSrc, connectSrc)
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
			ip := r.RemoteAddr
			if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
				ip = strings.Split(fwd, ",")[0]
			}
			ip = strings.TrimSpace(ip)
			if host, _, err := net.SplitHostPort(ip); err == nil {
				ip = host
			}

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

// checkSchema verifies that the database schema has been loaded
func checkSchema(db *sql.DB) error {
	// Check for essential tables
	tables := []string{"users", "devices", "settings"}
	missing := []string{}

	for _, table := range tables {
		var exists bool
		err := db.QueryRow(`
			SELECT EXISTS (
				SELECT FROM information_schema.tables 
				WHERE table_schema = 'public' AND table_name = $1
			)`, table).Scan(&exists)
		if err != nil || !exists {
			missing = append(missing, table)
		}
	}

	if len(missing) > 0 {
		return fmt.Errorf("database schema not loaded (missing tables: %s). Run: psql -U wavecontrol wavecontrol < schema.sql",
			strings.Join(missing, ", "))
	}

	// Check for required columns added after the initial schema.
	// We fail fast with a clear message so runtime queries don't explode with
	// "column does not exist" errors.
	var hasManaged bool
	err := db.QueryRow(`
		SELECT EXISTS (
			SELECT FROM information_schema.columns
			WHERE table_schema='public' AND table_name='devices' AND column_name='managed'
		)`).Scan(&hasManaged)
	if err != nil {
		return fmt.Errorf("failed to validate database schema: %w", err)
	}
	if !hasManaged {
		return fmt.Errorf(
			"database schema is out of date (missing devices.managed column). Apply migrations/014_managed_devices.sql or re-import schema.sql")
	}

	return nil
}
