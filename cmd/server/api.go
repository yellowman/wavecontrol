package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"math"
	"net"
	"net/http"
	"net/mail"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	"github.com/lib/pq"
	"golang.org/x/crypto/bcrypt"

	"github.com/yellowman/wavecontrol/internal/alerting"
	"github.com/yellowman/wavecontrol/internal/bulkops"
	"github.com/yellowman/wavecontrol/internal/configbackup"
	"github.com/yellowman/wavecontrol/internal/firmware"
	"github.com/yellowman/wavecontrol/internal/jobs"
	"github.com/yellowman/wavecontrol/internal/netutil"
	"github.com/yellowman/wavecontrol/internal/poller"
	"github.com/yellowman/wavecontrol/internal/scheduler"
	"github.com/yellowman/wavecontrol/internal/secrets"
	"github.com/yellowman/wavecontrol/internal/stats"
	"github.com/yellowman/wavecontrol/internal/sysmonalerter"
	"github.com/yellowman/wavecontrol/internal/tlsutil"
	"github.com/yellowman/wavecontrol/internal/udebug"
	"github.com/yellowman/wavecontrol/internal/websocket"
)

type API struct {
	DB         *sql.DB
	JWTSecret  []byte
	Stats      *stats.Store
	Firmware   *firmware.Service
	Poller     *poller.Poller
	WSHub      *websocket.Hub
	Scheduler  *scheduler.Scheduler
	Jobs       *jobs.Runner
	TLS        *tlsutil.Manager
	UltraDebug *udebug.Manager
	BulkOps    *bulkops.Controller
	Alerts     *alerting.Manager
	Secrets    *secrets.Manager
}

type Claims struct {
	UserID      int64    `json:"user_id"`
	Username    string   `json:"username"`
	Roles       []string `json:"roles"`
	AuthVersion int64    `json:"auth_version"`
	jwt.RegisteredClaims
}

type contextKey string

const claimsKey contextKey = "claims"

func NewAPI(db *sql.DB, secret []byte, statsStore *stats.Store, fwService *firmware.Service, devicePoller *poller.Poller, wsHub *websocket.Hub, jobScheduler *scheduler.Scheduler, jobRunner *jobs.Runner, ultraDebug *udebug.Manager, tlsManager *tlsutil.Manager, bulkOps *bulkops.Controller, alertManager *alerting.Manager, secretStore *secrets.Manager) *API {
	return &API{
		DB:         db,
		JWTSecret:  secret,
		Stats:      statsStore,
		Firmware:   fwService,
		Poller:     devicePoller,
		WSHub:      wsHub,
		Scheduler:  jobScheduler,
		Jobs:       jobRunner,
		TLS:        tlsManager,
		UltraDebug: ultraDebug,
		BulkOps:    bulkOps,
		Alerts:     alertManager,
		Secrets:    secretStore,
	}
}

const sessionCookieName = "wavecontrol_session"

func tokenFromRequest(r *http.Request) string {
	if auth := strings.TrimSpace(r.Header.Get("Authorization")); strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	}
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		return strings.TrimSpace(cookie.Value)
	}
	return ""
}

func (a *API) parseToken(tokenStr string) (*Claims, error) {
	if tokenStr == "" {
		return nil, errors.New("missing token")
	}
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (any, error) {
		if t.Method.Alg() != jwt.SigningMethodHS256.Alg() {
			return nil, fmt.Errorf("unexpected signing method %q", t.Method.Alg())
		}
		return a.JWTSecret, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}), jwt.WithIssuer("wavecontrol"), jwt.WithAudience("wavecontrol-web"))
	if err != nil || token == nil || !token.Valid || claims.ExpiresAt == nil {
		return nil, errors.New("invalid token")
	}
	return claims, nil
}

func recognizedRoles(roles []string) []string {
	seen := make(map[string]struct{}, len(roles))
	out := make([]string, 0, len(roles))
	for _, role := range roles {
		role = strings.ToLower(strings.TrimSpace(role))
		switch role {
		case RoleViewer, RoleCreator, RoleEditor, RoleAdmin:
			if _, ok := seen[role]; !ok {
				seen[role] = struct{}{}
				out = append(out, role)
			}
		}
	}
	sort.Strings(out)
	return out
}

func (a *API) currentIdentity(ctx context.Context, userID int64) (string, int64, []string, error) {
	var username string
	var status int
	var authVersion int64
	var roles pq.StringArray
	err := a.DB.QueryRowContext(ctx, `
		SELECT u.username, u.status, u.auth_version,
		       COALESCE(array_agg(r.name ORDER BY r.name) FILTER (WHERE r.name IS NOT NULL), '{}')
		FROM users u
		LEFT JOIN user_roles ur ON ur."user" = u.id
		LEFT JOIN roles r ON r.id = ur.role
		WHERE u.id = $1
		GROUP BY u.id, u.username, u.status, u.auth_version
	`, userID).Scan(&username, &status, &authVersion, &roles)
	if err != nil {
		return "", 0, nil, err
	}
	if status != 1 {
		return "", 0, nil, errors.New("account disabled")
	}
	validRoles := recognizedRoles([]string(roles))
	if len(validRoles) == 0 {
		return "", 0, nil, errors.New("account has no application role")
	}
	return username, authVersion, validRoles, nil
}

func (a *API) AuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, err := a.parseToken(tokenFromRequest(r))
		if err != nil || claims.UserID <= 0 || claims.AuthVersion <= 0 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		username, authVersion, roles, err := a.currentIdentity(r.Context(), claims.UserID)
		if err != nil || authVersion != claims.AuthVersion || username != claims.Username {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		claims.Username = username
		claims.Roles = roles
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), claimsKey, claims)))
	})
}

func getClaims(r *http.Request) *Claims {
	if c, ok := r.Context().Value(claimsKey).(*Claims); ok {
		return c
	}
	return nil
}

// logChangelog logs an action to the changelog table (non-critical, logs errors)
func (a *API) logChangelog(change string, userID int64) {
	if _, err := a.DB.Exec(`INSERT INTO changelog (change, "user") VALUES ($1, $2)`, change, userID); err != nil {
		log.Printf("Failed to log changelog: %v", err)
	}
}

// logChangelogByUsername logs an action using username lookup (non-critical)
func (a *API) logChangelogByUsername(change, username string) {
	if _, err := a.DB.Exec(`INSERT INTO changelog (change, "user") SELECT $1, id FROM users WHERE username = $2`, change, username); err != nil {
		log.Printf("Failed to log changelog: %v", err)
	}
}

// logChangelogDevice logs a device-specific action (non-critical)
func (a *API) logChangelogDevice(mac, change string, userID int64) {
	if _, err := a.DB.Exec(`INSERT INTO changelog (device_mac, change, "user") VALUES ($1, $2, $3)`, mac, change, userID); err != nil {
		log.Printf("Failed to log device changelog: %v", err)
	}
}

// logChangelogDeviceByUsername logs a device action using username lookup (non-critical)
func (a *API) logChangelogDeviceByUsername(mac, change, username string) {
	if _, err := a.DB.Exec(`INSERT INTO changelog (device_mac, change, "user") SELECT $1, $2, id FROM users WHERE username = $3`, mac, change, username); err != nil {
		log.Printf("Failed to log device changelog: %v", err)
	}
}

// Role constants
const (
	RoleAdmin   = "administrator"
	RoleEditor  = "editor"
	RoleCreator = "creator"
	RoleViewer  = "viewer"
)

// hasRole checks if user has a specific role
func (a *API) hasRole(r *http.Request, role string) bool {
	claims := getClaims(r)
	if claims == nil {
		return false
	}
	for _, r := range claims.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// hasAnyRole checks if user has any of the specified roles
func (a *API) hasAnyRole(r *http.Request, roles ...string) bool {
	claims := getClaims(r)
	if claims == nil {
		return false
	}
	for _, userRole := range claims.Roles {
		for _, required := range roles {
			if userRole == required {
				return true
			}
		}
	}
	return false
}

// Permission levels (each level includes permissions of levels below it)
// - viewer: read-only access to devices, stats, reports, logs
// - creator: viewer + add devices
// - editor: creator + modify devices, firmware, jobs, config backup/restore, sites/regions
// - administrator: editor + users, settings, sensitive data

func (a *API) canView(r *http.Request) bool {
	return a.hasAnyRole(r, RoleViewer, RoleCreator, RoleEditor, RoleAdmin)
}

// requireAuth is kept for backwards compatibility with older handlers.
// In wavecontrol, "authenticated" effectively means the caller has at least
// viewer permissions.
func (a *API) requireAuth(w http.ResponseWriter, r *http.Request) bool {
	return a.requireView(w, r)
}

func (a *API) canCreate(r *http.Request) bool {
	return a.hasAnyRole(r, RoleCreator, RoleEditor, RoleAdmin)
}

func (a *API) canEdit(r *http.Request) bool {
	return a.hasAnyRole(r, RoleEditor, RoleAdmin)
}

func (a *API) isAdmin(r *http.Request) bool {
	return a.hasRole(r, RoleAdmin)
}

// requireView returns 403 if user cannot view
func (a *API) requireView(w http.ResponseWriter, r *http.Request) bool {
	if !a.canView(r) {
		http.Error(w, "forbidden: viewer role required", 403)
		return false
	}
	return true
}

// requireCreate returns 403 if user cannot create
func (a *API) requireCreate(w http.ResponseWriter, r *http.Request) bool {
	if !a.canCreate(r) {
		http.Error(w, "forbidden: creator role required", 403)
		return false
	}
	return true
}

// requireEdit returns 403 if user cannot edit
func (a *API) requireEdit(w http.ResponseWriter, r *http.Request) bool {
	if !a.canEdit(r) {
		http.Error(w, "forbidden: editor role required", 403)
		return false
	}
	return true
}

// requireAdmin returns 403 if user is not admin
func (a *API) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if !a.isAdmin(r) {
		http.Error(w, "forbidden: administrator role required", 403)
		return false
	}
	return true
}

func requestIsHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: requestIsHTTPS(r), SameSite: http.SameSiteStrictMode,
	})
}

func (a *API) Login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" || len(req.Username) > 64 || req.Password == "" || len([]byte(req.Password)) > 72 {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}

	clientIP := clientIPFromRequest(r)
	var id int64
	var storedHash string
	var status int
	var authVersion int64
	err := a.DB.QueryRow(`SELECT id, password, status, auth_version FROM users WHERE username = $1`, req.Username).
		Scan(&id, &storedHash, &status, &authVersion)
	if err != nil || status != 1 || !verifyPassword(req.Password, storedHash) {
		logError("LOGIN FAILED: username=%q ip=%s", req.Username, clientIP)
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	username, currentVersion, roles, err := a.currentIdentity(r.Context(), id)
	if err != nil || currentVersion != authVersion {
		logError("LOGIN FAILED: role/status lookup username=%q ip=%s: %v", req.Username, clientIP, err)
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}

	now := time.Now()
	claims := &Claims{
		UserID: id, Username: username, Roles: roles, AuthVersion: authVersion,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: "wavecontrol", Subject: strconv.FormatInt(id, 10), Audience: jwt.ClaimStrings{"wavecontrol-web"},
			ExpiresAt: jwt.NewNumericDate(now.Add(24 * time.Hour)), IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now.Add(-time.Minute)),
		},
	}
	tokenStr, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(a.JWTSecret)
	if err != nil {
		logError("Login: JWT signing failed: %v", err)
		http.Error(w, "authentication error", http.StatusInternalServerError)
		return
	}
	setSessionCookie(w, r, tokenStr, 86400)
	logError("LOGIN SUCCESS: username=%q ip=%s", req.Username, clientIP)
	writeJSON(w, map[string]any{"username": username, "roles": roles})
}

func (a *API) Logout(w http.ResponseWriter, r *http.Request) {
	claims, err := a.parseToken(tokenFromRequest(r))
	if err == nil && claims.UserID > 0 {
		if _, err := a.DB.ExecContext(r.Context(), `UPDATE users SET auth_version = auth_version + 1 WHERE id = $1`, claims.UserID); err != nil {
			// Clear this browser's cookie, but do not claim that all copies of the
			// session were revoked when the durable version bump failed.
			setSessionCookie(w, r, "", -1)
			logError("logout token revocation failed for user %d: %v", claims.UserID, err)
			http.Error(w, "logout revocation failed", http.StatusInternalServerError)
			return
		}
	}
	setSessionCookie(w, r, "", -1)
	writeJSON(w, map[string]any{"status": "ok"})
}

func (a *API) Me(w http.ResponseWriter, r *http.Request) {
	claims := getClaims(r)
	if claims == nil {
		http.Error(w, "unauthorized", 401)
		return
	}
	writeJSON(w, map[string]any{"id": claims.UserID, "username": claims.Username, "roles": claims.Roles})
}

func (a *API) Ping(w http.ResponseWriter, r *http.Request) {
	online, offline, unknown, total := a.Stats.CountsByStatus()
	writeJSON(w, map[string]any{"status": "ok", "time": time.Now().Unix(), "last_poll": a.Stats.GetLastPoll(), "devices": total, "online": online, "offline": offline, "unknown": unknown})
}

func (a *API) ListDevices(w http.ResponseWriter, r *http.Request) {
	rows, err := a.DB.Query(`
		SELECT d.id,
		       lower(d.mac) AS mac,
		       host(d.ip_address) AS ip_address,
		       d.hostname, d.product, d.model, d.platform, d.flavor, d.firmware, d.firmware_version,
		       d.parent_id,
		       lower(d.parent_mac) AS parent_mac,
		       d.status, d.status_reason, d.last_seen, d.role, d.managed, d.alertable, d.alert_silenced_until, d.alert_notes, d.ssid, d.frequency, d.channel_width, d.gps_lat, d.gps_lon,
		       d.antenna_model, d.antenna_override, d.antenna_azimuth_deg, d.antenna_downtilt_deg, d.antenna_electrical_downtilt_deg, d.antenna_beamwidth_h_deg, d.antenna_beamwidth_v_deg,
		       d.radius_m, d.tech, d.down_mbps, d.up_mbps, d.latency_ms, d.bizres,
		       d.site_id, s.name as site_name, r.name as region_name
		FROM devices d
		LEFT JOIN sites s ON d.site_id = s.id
		LEFT JOIN regions r ON s.region_id = r.id
		ORDER BY COALESCE(d.parent_id, d.id), d.parent_id NULLS FIRST, d.hostname`)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()

	parentIPs := make(map[int64]string)
	parentMACs := make(map[int64]string)
	var devices []map[string]any

	for rows.Next() {
		var id int64
		var mac, ipAddr, hostname, product, model, platform, flavor, fw, fwVer, parentMAC, dbStatus, dbStatusReason, role, ssid sql.NullString
		var managed, alertable bool
		var alertSilencedUntil sql.NullTime
		var alertNotes sql.NullString
		var siteName, regionName sql.NullString
		var parentID, siteID sql.NullInt64
		var lastSeen sql.NullTime
		var frequency, channelWidth sql.NullInt64
		var gpsLat, gpsLon sql.NullFloat64
		var antennaModel sql.NullString
		var antennaOverride sql.NullBool
		var antennaAzimuthDeg, antennaDowntiltDeg, antennaElectricalDowntiltDeg, antennaBeamH, antennaBeamV sql.NullFloat64
		var radiusM, downMbps, upMbps, latencyMS sql.NullFloat64
		var tech sql.NullInt64
		var bizres sql.NullString
		if rows.Scan(&id, &mac, &ipAddr, &hostname, &product, &model, &platform, &flavor, &fw, &fwVer,
			&parentID, &parentMAC, &dbStatus, &dbStatusReason, &lastSeen, &role, &managed, &alertable, &alertSilencedUntil, &alertNotes, &ssid, &frequency, &channelWidth, &gpsLat, &gpsLon,
			&antennaModel, &antennaOverride, &antennaAzimuthDeg, &antennaDowntiltDeg, &antennaElectricalDowntiltDeg, &antennaBeamH, &antennaBeamV,
			&radiusM, &tech, &downMbps, &upMbps, &latencyMS, &bizres,
			&siteID, &siteName, &regionName) != nil {
			continue
		}

		ipHost := ipAddr.String // host(ip_address) ensures no "/32" suffix

		d := map[string]any{"id": id, "mac": mac.String, "ip_address": ipHost, "hostname": hostname.String,
			"product": product.String, "model": model.String, "platform": platform.String, "flavor": flavor.String,
			"firmware": fw.String, "firmware_version": fwVer.String,
			// Persisted DB status (source of truth when we have no live stats)
			"db_status":        dbStatus.String,
			"db_status_reason": dbStatusReason.String,
			// Live/computed status defaults to DB status and may be overridden below
			"status":        dbStatus.String,
			"status_reason": dbStatusReason.String,
			"role":          role.String,
			"managed":       managed,
			"alertable":     alertable,
			"alert_notes":   alertNotes.String,
			"ssid":          ssid.String}
		if parentID.Valid {
			d["parent_id"] = parentID.Int64
		} else {
			parentIPs[id] = ipHost
			if mac.Valid {
				parentMACs[id] = mac.String
			}
		}
		if parentMAC.Valid {
			d["parent_mac"] = parentMAC.String
		}
		if siteID.Valid {
			d["site_id"] = siteID.Int64
		}
		if siteName.Valid {
			d["site_name"] = siteName.String
		}
		if regionName.Valid {
			d["region_name"] = regionName.String
		}
		if lastSeen.Valid {
			d["last_seen"] = lastSeen.Time
		}
		if alertSilencedUntil.Valid {
			d["alert_silenced_until"] = alertSilencedUntil.Time
		}
		if frequency.Valid {
			d["frequency"] = frequency.Int64
		}
		if channelWidth.Valid {
			d["channel_width"] = channelWidth.Int64
		}
		if gpsLat.Valid && gpsLon.Valid {
			d["gps_lat"] = gpsLat.Float64
			d["gps_lon"] = gpsLon.Float64
		}

		// Optional antenna modeling / planning fields (Config UI)
		d["antenna_model"] = antennaModel.String
		d["antenna_override"] = antennaOverride.Bool
		if antennaAzimuthDeg.Valid {
			d["antenna_azimuth_deg"] = antennaAzimuthDeg.Float64
		}
		if antennaDowntiltDeg.Valid {
			d["antenna_downtilt_deg"] = antennaDowntiltDeg.Float64
		}
		// Electrical downtilt defaults to 0; return 0 when unset for consistent UI behavior.
		if antennaElectricalDowntiltDeg.Valid {
			d["antenna_electrical_downtilt_deg"] = antennaElectricalDowntiltDeg.Float64
		} else {
			d["antenna_electrical_downtilt_deg"] = 0.0
		}
		if antennaBeamH.Valid {
			d["antenna_beamwidth_h_deg"] = antennaBeamH.Float64
		}
		if antennaBeamV.Valid {
			d["antenna_beamwidth_v_deg"] = antennaBeamV.Float64
		}

		// Optional sector planning/export fields
		if radiusM.Valid {
			d["radius_m"] = radiusM.Float64
		}
		if tech.Valid {
			d["tech"] = tech.Int64
		}
		if downMbps.Valid {
			d["down_mbps"] = downMbps.Float64
		}
		if upMbps.Valid {
			d["up_mbps"] = upMbps.Float64
		}
		if latencyMS.Valid {
			d["latency_ms"] = latencyMS.Float64
		}
		if bizres.Valid {
			d["bizres"] = bizres.String
		}

		// Live stats merge MUST be keyed by MAC when a MAC exists.
		// Falling back to IP for MAC-bearing DB rows causes cross-linking when an IP is reused
		// by a different device (different MAC/platform). Only fall back to IP when the DB row
		// truly has no MAC.
		var liveStats *stats.DeviceStats
		if mac.Valid && mac.String != "" {
			liveStats = a.Stats.GetByMAC(mac.String)
		} else {
			liveStats = a.Stats.Get(ipHost)
		}

		if liveStats != nil {
			d["online"] = liveStats.Online
			// Prefer live tri-state status over DB status
			d["status"] = string(liveStats.Status)
			if liveStats.StatusReason != "" {
				d["status_reason"] = liveStats.StatusReason
			} else {
				// Clear stale DB reasons when device is currently healthy
				if liveStats.Online {
					d["status_reason"] = ""
				}
			}
			d["uptime"] = liveStats.Uptime
			d["peer_count"] = liveStats.PeerCount
			if liveStats.Wireless.Radio60GHz != nil && liveStats.Wireless.Radio60GHz.Capacity != nil {
				d["capacity_60ghz"] = liveStats.Wireless.Radio60GHz.Capacity.Combined
			}
			if liveStats.Wireless.RadioLTU != nil && liveStats.Wireless.RadioLTU.Capacity != nil {
				d["capacity_ltu"] = liveStats.Wireless.RadioLTU.Capacity.Combined
			}
			if liveStats.Wireless.Radio5GHz != nil {
				d["signal"] = liveStats.Wireless.Radio5GHz.Signal
				if liveStats.Wireless.Radio5GHz.Capacity != nil {
					d["capacity_5ghz"] = liveStats.Wireless.Radio5GHz.Capacity.Combined
				}
			}
			// Use GPS from live stats if available
			if liveStats.GPS.Fix && (liveStats.GPS.Lat != 0 || liveStats.GPS.Lon != 0) {
				d["gps_lat"] = liveStats.GPS.Lat
				d["gps_lon"] = liveStats.GPS.Lon
			}

			// AP hardware stats
			if len(liveStats.CPU) > 0 {
				d["cpu"] = liveStats.CPU
			}
			if liveStats.RAM.Total > 0 {
				d["ram"] = liveStats.RAM
			}
			if liveStats.Temperature.CPU > 0 {
				d["temperature"] = liveStats.Temperature.CPU
			}
			if liveStats.PowerTime > 0 {
				d["power_time"] = liveStats.PowerTime
			}
			if liveStats.Wireless.ServiceUptime > 0 {
				d["service_uptime"] = liveStats.Wireless.ServiceUptime
			}
			if len(liveStats.Interfaces) > 0 {
				d["interfaces"] = liveStats.Interfaces
			}
			// Radio details for APs
			if liveStats.Wireless.Radio60GHz != nil {
				d["radio_60ghz"] = liveStats.Wireless.Radio60GHz
			}
			if liveStats.Wireless.Radio5GHz != nil {
				d["radio_5ghz"] = liveStats.Wireless.Radio5GHz
			}
			if liveStats.Wireless.RadioLTU != nil {
				d["radio_ltu"] = liveStats.Wireless.RadioLTU
			}
			// Orientation for 60GHz APs
			if liveStats.Orientation != nil {
				d["orientation"] = liveStats.Orientation
			}
			// Wireless config features
			if liveStats.Config != nil {
				d["config"] = liveStats.Config
			}
		} else {
			// No live stats - set online based on db_status for consistent client-side checking
			d["online"] = dbStatus.String == "online"
		}
		devices = append(devices, d)
	}

	// Add STA signal from parent AP peer stats
	// Check if management prefixes are configured - if so, IPs are trustworthy for matching
	var mgmtPrefixesEnabled bool
	var prefixJSON string
	if a.DB.QueryRow(`SELECT value FROM settings WHERE key = 'management_prefixes'`).Scan(&prefixJSON) == nil {
		var prefixes []string
		if json.Unmarshal([]byte(prefixJSON), &prefixes) == nil && len(prefixes) > 0 {
			mgmtPrefixesEnabled = true
		}
	}

	for i, d := range devices {
		parentID, ok := d["parent_id"].(int64)
		if !ok {
			continue
		}
		parentStats := a.Stats.GetByMAC(parentMACs[parentID])
		if parentStats == nil {
			parentStats = a.Stats.Get(parentIPs[parentID])
		}
		if parentStats == nil {
			// Parent AP not being polled - can't determine STA status
			continue
		}
		mac := d["mac"].(string)
		ip := d["ip_address"].(string)
		platform, _ := d["platform"].(string)
		found := false
		for _, peer := range parentStats.Peers {
			// peer.MAC from AP is authoritative - match on MAC first
			matched := false
			peerMAC := stats.NormalizeMAC(peer.MAC)
			if peerMAC != "" && peerMAC == mac {
				matched = true
			} else if mgmtPrefixesEnabled && platform == "airmax" && peer.IP != "" && peer.IP == ip {
				// IP fallback only for airMAX when management prefixes are enabled
				// (prefixes ensure stored IPs are trustworthy management addresses)
				matched = true
			}

			if matched {
				found = true
				devices[i]["online"] = true
				devices[i]["status"] = "online"
				devices[i]["status_reason"] = ""
				devices[i]["signal_level"] = getPeerSignal(peer)
				devices[i]["signal_per_chain"] = getPeerSignalPerChain(peer)
				devices[i]["distance"] = peer.Distance
				devices[i]["tx_rate"] = peer.TxRate
				devices[i]["rx_rate"] = peer.RxRate
				devices[i]["uptime"] = peer.Uptime

				// New fields
				if peer.PowerTime > 0 {
					devices[i]["power_time"] = peer.PowerTime
				}
				if peer.ServiceUptime > 0 {
					devices[i]["service_uptime"] = peer.ServiceUptime
				}
				if peer.NetMode != "" {
					devices[i]["net_mode"] = peer.NetMode
				}
				devices[i]["carrier_drop"] = peer.CarrierDrop

				// Orientation
				if peer.Orientation != nil {
					devices[i]["orientation"] = peer.Orientation
				}

				// Hardware stats
				if len(peer.CPU) > 0 {
					devices[i]["cpu"] = peer.CPU
				}
				if peer.RAM.Total > 0 {
					devices[i]["ram"] = peer.RAM
				}
				if peer.Temperature > 0 {
					devices[i]["temperature"] = peer.Temperature
				}

				// Interfaces
				if len(peer.Interfaces) > 0 {
					devices[i]["interfaces"] = peer.Interfaces
				}

				if peer.LinkScore != nil {
					devices[i]["link_score"] = peer.LinkScore
				}
				if peer.Radio60GHz != nil {
					devices[i]["radio_60ghz"] = peer.Radio60GHz
					// Use SignalCombined (MRC computed) for consistency
					if peer.Radio60GHz.SignalCombined != 0 {
						devices[i]["signal_60ghz"] = peer.Radio60GHz.SignalCombined
					} else {
						devices[i]["signal_60ghz"] = peer.Radio60GHz.Signal
					}
					if peer.Radio60GHz.Capacity != nil {
						devices[i]["capacity_60ghz"] = peer.Radio60GHz.Capacity.Combined
					}
				}
				if peer.Radio5GHz != nil {
					devices[i]["radio_5ghz"] = peer.Radio5GHz
					// Use SignalCombined (MRC computed) for consistency
					if peer.Radio5GHz.SignalCombined != 0 {
						devices[i]["signal_5ghz"] = peer.Radio5GHz.SignalCombined
					} else {
						devices[i]["signal_5ghz"] = peer.Radio5GHz.Signal
					}
					if peer.Radio5GHz.Capacity != nil {
						devices[i]["capacity_5ghz"] = peer.Radio5GHz.Capacity.Combined
					}
				}
				if peer.RadioLTU != nil {
					devices[i]["radio_ltu"] = peer.RadioLTU
					// Use SignalCombined (MRC computed) for consistency
					if peer.RadioLTU.SignalCombined != 0 {
						devices[i]["signal_ltu"] = peer.RadioLTU.SignalCombined
					} else {
						devices[i]["signal_ltu"] = peer.RadioLTU.Signal
					}
					if peer.RadioLTU.Capacity != nil {
						devices[i]["capacity_ltu"] = peer.RadioLTU.Capacity.Combined
					}
				}
				// airMAX AC direct signal (separate from Wave 5GHz)
				if peer.Signal != 0 {
					devices[i]["signal_airmax"] = peer.Signal
				}
				// airMAX remote signal (what STA receives from AP)
				if peer.RemoteSignal != 0 {
					devices[i]["remote_signal"] = peer.RemoteSignal
					if peer.RemoteNoiseFloor != 0 {
						devices[i]["remote_noise_floor"] = peer.RemoteNoiseFloor
					}
					if len(peer.RemoteSignalPerChain) > 0 {
						devices[i]["remote_signal_per_chain"] = peer.RemoteSignalPerChain
					}
				}
				// GPS from peer
				if peer.GPS.Fix && (peer.GPS.Lat != 0 || peer.GPS.Lon != 0) {
					devices[i]["gps_lat"] = peer.GPS.Lat
					devices[i]["gps_lon"] = peer.GPS.Lon
				}
				break
			}
		}
		// STA not in parent's peer list - mark as offline (disconnected from AP)
		if !found {
			devices[i]["online"] = false
			// If the parent AP is online, we can definitively say the STA is offline (not associated)
			if parentStats.Status == stats.StatusOnline {
				devices[i]["status"] = "offline"
				devices[i]["status_reason"] = "not_associated"
			} else {
				// Parent AP not online -> unknown
				devices[i]["status"] = "unknown"
				devices[i]["status_reason"] = fmt.Sprintf("parent_%s", parentStats.Status)
			}
		}
	}

	a.attachIdentityMismatches(devices)
	if devices == nil {
		devices = []map[string]any{}
	}
	writeJSON(w, devices)
}

func getPeerSignal(peer *stats.PeerStats) int {
	if peer.Radio60GHz != nil && peer.Radio60GHz.Active {
		return peer.Radio60GHz.Signal
	}
	if peer.RadioLTU != nil && peer.RadioLTU.Signal != 0 {
		return peer.RadioLTU.Signal
	}
	if peer.Radio5GHz != nil && peer.Radio5GHz.Signal != 0 {
		return peer.Radio5GHz.Signal
	}
	// airMAX direct signal field
	if peer.Signal != 0 {
		return peer.Signal
	}
	return 0
}

func getPeerSignalPerChain(peer *stats.PeerStats) []int {
	if peer.Radio5GHz != nil && len(peer.Radio5GHz.SignalPerChain) > 0 {
		return peer.Radio5GHz.SignalPerChain
	}
	if peer.RadioLTU != nil && len(peer.RadioLTU.SignalPerChain) > 0 {
		return peer.RadioLTU.SignalPerChain
	}
	return nil
}

func getPeerRemoteSignalPerChain(peer *stats.PeerStats) []int {
	if peer.Radio60GHz != nil && len(peer.Radio60GHz.RemoteSignalPerChain) > 0 {
		return peer.Radio60GHz.RemoteSignalPerChain
	}
	if peer.Radio6GHz != nil && len(peer.Radio6GHz.RemoteSignalPerChain) > 0 {
		return peer.Radio6GHz.RemoteSignalPerChain
	}
	if peer.Radio5GHz != nil && len(peer.Radio5GHz.RemoteSignalPerChain) > 0 {
		return peer.Radio5GHz.RemoteSignalPerChain
	}
	if peer.RadioLTU != nil && len(peer.RadioLTU.RemoteSignalPerChain) > 0 {
		return peer.RadioLTU.RemoteSignalPerChain
	}
	if len(peer.RemoteSignalPerChain) > 0 {
		return peer.RemoteSignalPerChain
	}
	return nil
}

type peerRadioEntry struct {
	name string
	r    *stats.PeerRadioStats
}

func getPeerRadioEntries(peer *stats.PeerStats) []peerRadioEntry {
	entries := make([]peerRadioEntry, 0, 4)
	if peer.Radio60GHz != nil {
		entries = append(entries, peerRadioEntry{name: "60 GHz", r: peer.Radio60GHz})
	}
	if peer.Radio6GHz != nil {
		entries = append(entries, peerRadioEntry{name: "6 GHz", r: peer.Radio6GHz})
	}
	if peer.Radio5GHz != nil {
		entries = append(entries, peerRadioEntry{name: "5 GHz", r: peer.Radio5GHz})
	}
	if peer.RadioLTU != nil {
		entries = append(entries, peerRadioEntry{name: "LTU", r: peer.RadioLTU})
	}
	return entries
}

func getPeerRadioSignalValue(pr *stats.PeerRadioStats) int {
	if pr == nil {
		return 0
	}
	if pr.SignalCombined != 0 {
		return pr.SignalCombined
	}
	if len(pr.SignalPerChain) > 0 {
		return stats.CombineSignals(pr.SignalPerChain)
	}
	return pr.Signal
}

func getPeerRadioRemoteSignalValue(pr *stats.PeerRadioStats) int {
	if pr == nil {
		return 0
	}
	if pr.RemoteSignalCombined != 0 {
		return pr.RemoteSignalCombined
	}
	if len(pr.RemoteSignalPerChain) > 0 {
		return stats.CombineSignals(pr.RemoteSignalPerChain)
	}
	return pr.RemoteSignal
}

func (a *API) getSettingIntDefault(key string, def int) int {
	if a == nil || a.DB == nil {
		return def
	}
	var raw string
	if err := a.DB.QueryRow(`SELECT value FROM settings WHERE key = $1`, key).Scan(&raw); err != nil {
		return def
	}
	v, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return def
	}
	return v
}

func chainMismatchSide(apSpread, staSpread, threshold int) string {
	apBad := apSpread > threshold
	staBad := staSpread > threshold
	switch {
	case apBad && staBad:
		return "both"
	case apBad:
		return "ap_only"
	case staBad:
		return "sta_only"
	default:
		return "none"
	}
}

func sanitizeReportSignalChains(chains []int, noiseFloor int) []int {
	if len(chains) == 0 {
		return nil
	}
	cleaned := make([]int, 0, len(chains))
	for _, v := range chains {
		if v == 0 {
			continue
		}
		// Negative values are valid dBm chain readings, including values close to
		// the reported noise floor. Do not treat noise-floor-adjacent values as
		// placeholders; only explicit zero entries are empty-chain placeholders.
		cleaned = append(cleaned, v)
	}
	if len(cleaned) == 0 {
		return nil
	}
	return cleaned
}

func signalChainSpread(chains []int) int {
	if len(chains) < 2 {
		return 0
	}
	minV, maxV := chains[0], chains[0]
	for _, v := range chains[1:] {
		if v < minV {
			minV = v
		}
		if v > maxV {
			maxV = v
		}
	}
	return maxV - minV
}

func radioBandLabel(r *stats.RadioStats, fallback string) string {
	if r == nil {
		return fallback
	}
	if strings.TrimSpace(r.DisplayBandOverride) != "" {
		return strings.TrimSpace(r.DisplayBandOverride)
	}
	if r.Frequency >= 57000 {
		return "60 GHz"
	}
	if r.Frequency >= 5925 {
		return "6 GHz"
	}
	if r.Frequency > 0 {
		return "5 GHz"
	}
	if strings.TrimSpace(r.Name) != "" {
		return strings.TrimSpace(r.Name)
	}
	if fallback != "" {
		return fallback
	}
	return "Radio"
}

func buildCredentialCandidates(explicitUser, explicitPass string, apCreds, staCreds []Credential) []Credential {
	creds := make([]Credential, 0)
	seen := make(map[string]struct{})
	add := func(c Credential) {
		if strings.TrimSpace(c.Username) == "" || strings.TrimSpace(c.Password) == "" {
			return
		}
		key := c.Username + "\x00" + c.Password
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		creds = append(creds, c)
	}
	if explicitUser != "" && explicitPass != "" {
		add(Credential{Username: explicitUser, Password: explicitPass})
	}
	for _, c := range apCreds {
		add(c)
	}
	for _, c := range staCreds {
		add(c)
	}
	return creds
}

func (a *API) discoverWithCredentials(ip, explicitUser, explicitPass string) (*DeviceInfo, string, string, error) {
	apCreds, staCreds := a.loadCredentials()
	creds := buildCredentialCandidates(explicitUser, explicitPass, apCreds, staCreds)
	if len(creds) == 0 {
		return nil, "", "", fmt.Errorf("no credentials available")
	}

	var lastErr error
	for _, c := range creds {
		device, err := discoverDevice(ip, c.Username, c.Password, a.TLS, a.UltraDebug)
		if err == nil && device != nil {
			return device, c.Username, c.Password, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("all discovery methods failed")
	}
	return nil, "", "", lastErr
}

type discoveredUpsertResult struct {
	ID       int64
	MAC      string
	Hostname string
	Warnings []string
	Message  string
}

func (a *API) upsertDiscoveredDevice(device *DeviceInfo, ip, username, password string, siteID *int64, createdBy string) (*discoveredUpsertResult, error) {
	if device == nil {
		return nil, fmt.Errorf("nil device")
	}

	device.MAC = stats.NormalizeMAC(device.MAC)

	var existingID int64
	var existingParentID sql.NullInt64
	var parentHostname sql.NullString
	selErr := a.DB.QueryRow(`
		SELECT d.id, d.parent_id, p.hostname
		FROM devices d
		LEFT JOIN devices p ON d.parent_id = p.id
		WHERE lower(d.mac) = $1
		ORDER BY d.id
		LIMIT 1
	`, device.MAC).Scan(&existingID, &existingParentID, &parentHostname)

	staReaddNote := ""
	if selErr == nil && existingParentID.Valid {
		if parentHostname.Valid && parentHostname.String != "" {
			staReaddNote = fmt.Sprintf("Device was previously STA of %s; marked managed for direct polling", parentHostname.String)
		} else {
			staReaddNote = "Device was previously STA; marked managed for direct polling"
		}
	}

	warnings := []string{}
	ctx := fmt.Sprintf("add_device ip=%s mac=%s", ip, device.MAC)
	mac := truncateWithWarning(&warnings, ctx, "mac", device.MAC, 17)
	hostname := truncateWithWarning(&warnings, ctx, "hostname", device.Hostname, 128)
	product := truncateWithWarning(&warnings, ctx, "product", device.Product, 64)
	model := truncateWithWarning(&warnings, ctx, "model", device.Model, 32)
	platform := truncateWithWarning(&warnings, ctx, "platform", device.Platform, 16)
	flavor := truncateWithWarning(&warnings, ctx, "flavor", device.Flavor, 16)
	firmware := truncateWithWarning(&warnings, ctx, "firmware", device.Firmware, 128)
	firmwareVersion := truncateWithWarning(&warnings, ctx, "firmware_version", device.FirmwareVersion, 32)
	username = truncateWithWarning(&warnings, ctx, "username", username, 64)
	if len(password) > 4096 {
		return nil, fmt.Errorf("device password exceeds 4096 bytes")
	}
	storedPassword, err := a.Secrets.Encrypt(password)
	if err != nil {
		return nil, fmt.Errorf("encrypt device password: %w", err)
	}

	var id int64
	switch {
	case selErr == nil:
		err := a.DB.QueryRow(`
			UPDATE devices SET
				mac = $2,
				ip_address = $3,
				hostname = $4,
				product = $5,
				model = $6,
				platform = $7,
				flavor = $8,
				firmware = $9,
				firmware_version = $10,
				site_id = COALESCE($11, devices.site_id),
				managed = TRUE,
				alertable = TRUE,
				status = 'online',
				last_seen = NOW(),
				username = $12,
				password = $13
			WHERE id = $1
			RETURNING id
		`, existingID, mac, ip, hostname, product, model, platform, flavor, firmware, firmwareVersion, siteID, username, storedPassword).Scan(&id)
		if err != nil {
			return nil, err
		}
	case selErr == sql.ErrNoRows:
		err := a.DB.QueryRow(`
			INSERT INTO devices (mac, ip_address, hostname, product, model, platform, flavor, firmware, firmware_version, site_id, managed, alertable, status, last_seen, username, password)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, TRUE, TRUE, 'online', NOW(), $11, $12)
			RETURNING id
		`, mac, ip, hostname, product, model, platform, flavor, firmware, firmwareVersion, siteID, username, storedPassword).Scan(&id)
		if err != nil {
			return nil, err
		}
	default:
		return nil, selErr
	}

	if inf := stats.InferRoleFromIdentity(platform, product, flavor); inf.Role != "" {
		switch inf.Role {
		case "ap":
			if _, err := a.DB.Exec(`UPDATE devices SET role='ap', parent_id=NULL, parent_mac=NULL WHERE id=$1`, id); err != nil {
				logDebug("upsertDiscoveredDevice: role promote to ap failed id=%d: %v", id, err)
			}
		case "sta":
			if _, err := a.DB.Exec(`UPDATE devices SET role='sta' WHERE id=$1 AND (role IS NULL OR role='')`, id); err != nil {
				logDebug("upsertDiscoveredDevice: role set to sta failed id=%d: %v", id, err)
			}
		}
	}

	if createdBy != "" {
		a.logChangelogDeviceByUsername(mac, fmt.Sprintf("added device %s (%s)", hostname, ip), createdBy)
	}

	go a.Poller.RefreshDeviceByID(id)

	return &discoveredUpsertResult{
		ID:       id,
		MAC:      mac,
		Hostname: hostname,
		Warnings: warnings,
		Message:  staReaddNote,
	}, nil
}

func (a *API) AddDevice(w http.ResponseWriter, r *http.Request) {
	if !a.requireCreate(w, r) {
		return
	}
	var req struct {
		IP       string `json:"ip"`
		Username string `json:"username"`
		Password string `json:"password"`
		SiteID   *int64 `json:"site_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}
	if strings.TrimSpace(req.IP) == "" {
		http.Error(w, "ip required", 400)
		return
	}

	device, workingUser, workingPass, err := a.discoverWithCredentials(req.IP, req.Username, req.Password)
	if err != nil || device == nil {
		http.Error(w, fmt.Sprintf("discovery failed: %v", err), 400)
		return
	}

	logDebug("AddDevice: discovered %s MAC=%s hostname=%s platform=%s", req.IP, device.MAC, device.Hostname, device.Platform)

	createdBy := ""
	if claims := getClaims(r); claims != nil {
		createdBy = claims.Username
	}

	result, err := a.upsertDiscoveredDevice(device, req.IP, workingUser, workingPass, req.SiteID, createdBy)
	if err != nil {
		logError("AddDevice: upsert failed: %v", err)
		http.Error(w, fmt.Sprintf("db error: %v", err), 500)
		return
	}

	resp := map[string]any{"id": result.ID, "mac": result.MAC, "hostname": result.Hostname}
	if len(result.Warnings) > 0 {
		resp["warnings"] = result.Warnings
	}
	if result.Message != "" {
		resp["message"] = result.Message
	}
	writeJSON(w, resp)
}

func (a *API) BulkAddDevices(w http.ResponseWriter, r *http.Request) {
	if !a.requireCreate(w, r) {
		return
	}
	var req struct {
		IPs      []string `json:"ips"`
		Username string   `json:"username"`
		Password string   `json:"password"`
		SiteID   *int64   `json:"site_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}

	var ips []string
	for _, ip := range req.IPs {
		ip = strings.TrimSpace(ip)
		if ip != "" {
			ips = append(ips, ip)
		}
	}
	if len(ips) == 0 {
		http.Error(w, "no IPs provided", 400)
		return
	}

	jobID := fmt.Sprintf("bulk-add-%d", time.Now().UnixNano())

	var createdBy string
	if claims := getClaims(r); claims != nil {
		createdBy = claims.Username
	}

	go func() {
		if a.WSHub != nil {
			a.WSHub.Broadcast(websocket.Message{
				Type: "job_update",
				Data: map[string]any{
					"job_id":      jobID,
					"job_type":    "bulk_add",
					"status":      "running",
					"progress":    0,
					"total_steps": len(ips),
					"target":      fmt.Sprintf("%d devices", len(ips)),
				},
			})
		}

		var results []map[string]any
		var failCount int

		for i, ip := range ips {
			result := map[string]any{"ip": ip}

			device, workingUser, workingPass, err := a.discoverWithCredentials(ip, req.Username, req.Password)
			if err != nil || device == nil {
				result["status"] = "failed"
				result["message"] = fmt.Sprint(err)
				failCount++
			} else {
				upsertResult, dbErr := a.upsertDiscoveredDevice(device, ip, workingUser, workingPass, req.SiteID, createdBy)
				if dbErr != nil {
					result["status"] = "failed"
					result["message"] = dbErr.Error()
					failCount++
				} else {
					result["status"] = "success"
					result["id"] = upsertResult.ID
					result["mac"] = upsertResult.MAC
					result["hostname"] = upsertResult.Hostname
					if len(upsertResult.Warnings) > 0 {
						result["warnings"] = upsertResult.Warnings
					}
					if upsertResult.Message != "" {
						result["message"] = upsertResult.Message
					}
				}
			}

			results = append(results, result)
			progress := ((i + 1) * 100) / len(ips)
			var eventMsg string
			if result["status"] == "success" {
				eventMsg = fmt.Sprintf("Discovered %s (%s)", result["hostname"], ip)
			} else {
				eventMsg = fmt.Sprintf("Failed %s: %s", ip, result["message"])
			}

			if a.WSHub != nil {
				a.WSHub.Broadcast(websocket.Message{
					Type: "job_event",
					Data: map[string]any{
						"job_id":     jobID,
						"event_type": "step_complete",
						"message":    eventMsg,
						"data":       result,
					},
				})
				a.WSHub.Broadcast(websocket.Message{
					Type: "job_progress",
					Data: map[string]any{
						"job_id":          jobID,
						"progress":        progress,
						"completed_steps": i + 1,
						"total_steps":     len(ips),
					},
				})
			}
		}

		status := "completed"
		if failCount == len(ips) {
			status = "failed"
		}
		if a.WSHub != nil {
			a.WSHub.Broadcast(websocket.Message{
				Type: "job_update",
				Data: map[string]any{
					"job_id":          jobID,
					"job_type":        "bulk_add",
					"status":          status,
					"progress":        100,
					"total_steps":     len(ips),
					"completed_steps": len(ips),
					"results":         results,
				},
			})
		}
	}()

	writeJSON(w, map[string]any{
		"job_id": jobID,
		"total":  len(ips),
		"status": "started",
	})
}

// Credential holds a username/password pair
type Credential struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// loadCredentials loads AP and STA credentials from settings
func (a *API) loadCredentials() (apCreds, staCreds []Credential) {
	loadPairs := func(prefix string) []Credential {
		pairs := make([]Credential, 0, 3)
		for i := 1; i <= 3; i++ {
			var username, stored string
			userKey := fmt.Sprintf("%s_cred%d_user", prefix, i)
			passKey := fmt.Sprintf("%s_cred%d_pass", prefix, i)
			if err := a.DB.QueryRow(`SELECT value FROM settings WHERE key=$1`, userKey).Scan(&username); err != nil && err != sql.ErrNoRows {
				logError("load credential %s: %v", userKey, err)
				continue
			}
			if err := a.DB.QueryRow(`SELECT value FROM settings WHERE key=$1`, passKey).Scan(&stored); err != nil && err != sql.ErrNoRows {
				logError("load credential %s: %v", passKey, err)
				continue
			}
			username = strings.TrimSpace(username)
			if username == "" || stored == "" {
				continue
			}
			password, err := a.Secrets.Decrypt(stored)
			if err != nil {
				logError("decrypt credential %s: %v", passKey, err)
				continue
			}
			if password != "" {
				pairs = append(pairs, Credential{Username: username, Password: password})
			}
		}
		return pairs
	}

	apCreds = loadPairs("ap")
	staCreds = loadPairs("sta")
	if len(staCreds) == 0 {
		staCreds = append([]Credential(nil), apCreds...)
	}
	return apCreds, staCreds
}

func (a *API) GetDevice(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	var mac, ipAddr, hostname, product, model, platform, flavor, fw, fwVer, parentMAC, dbStatus, dbStatusReason sql.NullString
	var managed, alertable bool
	var alertSilencedUntil sql.NullTime
	var alertNotes sql.NullString
	var antennaModel sql.NullString
	var antennaOverride sql.NullBool
	var antennaAzimuthDeg, antennaDowntiltDeg, antennaElectricalDowntiltDeg, antennaBeamH, antennaBeamV sql.NullFloat64
	var radiusM, downMbps, upMbps, latencyMS sql.NullFloat64
	var tech sql.NullInt64
	var bizres sql.NullString
	var parentID sql.NullInt64
	var lastSeen sql.NullTime
	err := a.DB.QueryRow(`SELECT mac, host(ip_address), hostname, product, model, platform, flavor, firmware, firmware_version,
		managed, alertable, alert_silenced_until, alert_notes,
		parent_id, parent_mac, status, status_reason, last_seen,
		antenna_model, antenna_override, antenna_azimuth_deg, antenna_downtilt_deg, antenna_electrical_downtilt_deg, antenna_beamwidth_h_deg, antenna_beamwidth_v_deg,
		radius_m, tech, down_mbps, up_mbps, latency_ms, bizres
		FROM devices WHERE id = $1`, id).
		Scan(&mac, &ipAddr, &hostname, &product, &model, &platform, &flavor, &fw, &fwVer, &managed, &alertable, &alertSilencedUntil, &alertNotes, &parentID, &parentMAC, &dbStatus, &dbStatusReason, &lastSeen,
			&antennaModel, &antennaOverride, &antennaAzimuthDeg, &antennaDowntiltDeg, &antennaElectricalDowntiltDeg, &antennaBeamH, &antennaBeamV,
			&radiusM, &tech, &downMbps, &upMbps, &latencyMS, &bizres)
	if err == sql.ErrNoRows {
		http.Error(w, "not found", 404)
		return
	}
	d := map[string]any{"id": id, "mac": mac.String, "ip_address": ipAddr.String, "hostname": hostname.String, "product": product.String, "model": model.String, "platform": platform.String, "flavor": flavor.String, "firmware": fw.String, "firmware_version": fwVer.String, "db_status": dbStatus.String, "db_status_reason": dbStatusReason.String, "status": dbStatus.String, "status_reason": dbStatusReason.String, "managed": managed, "alertable": alertable, "alert_notes": alertNotes.String}

	// Optional antenna modeling fields
	d["antenna_model"] = antennaModel.String
	d["antenna_override"] = antennaOverride.Bool
	if antennaAzimuthDeg.Valid {
		d["antenna_azimuth_deg"] = antennaAzimuthDeg.Float64
	}
	if antennaDowntiltDeg.Valid {
		d["antenna_downtilt_deg"] = antennaDowntiltDeg.Float64
	}
	// Electrical downtilt defaults to 0; return 0 when unset for consistent UI behavior.
	if antennaElectricalDowntiltDeg.Valid {
		d["antenna_electrical_downtilt_deg"] = antennaElectricalDowntiltDeg.Float64
	} else {
		d["antenna_electrical_downtilt_deg"] = 0.0
	}
	if antennaBeamH.Valid {
		d["antenna_beamwidth_h_deg"] = antennaBeamH.Float64
	}
	if antennaBeamV.Valid {
		d["antenna_beamwidth_v_deg"] = antennaBeamV.Float64
	}

	// Optional sector planning/export fields
	if radiusM.Valid {
		d["radius_m"] = radiusM.Float64
	}
	if tech.Valid {
		d["tech"] = tech.Int64
	}
	if downMbps.Valid {
		d["down_mbps"] = downMbps.Float64
	}
	if upMbps.Valid {
		d["up_mbps"] = upMbps.Float64
	}
	if latencyMS.Valid {
		d["latency_ms"] = latencyMS.Float64
	}
	if bizres.Valid {
		d["bizres"] = bizres.String
	}
	if parentID.Valid {
		d["parent_id"] = parentID.Int64
	}
	if parentMAC.Valid {
		d["parent_mac"] = parentMAC.String
	}
	if lastSeen.Valid {
		d["last_seen"] = lastSeen.Time
	}
	if alertSilencedUntil.Valid {
		d["alert_silenced_until"] = alertSilencedUntil.Time
	}
	// Prefer MAC lookup (authoritative). Only fall back to IP when the DB row has no MAC.
	// Falling back to IP for MAC-bearing rows causes cross-linking when IPs are reused.
	var liveStats *stats.DeviceStats
	if mac.String != "" {
		liveStats = a.Stats.GetByMAC(mac.String)
	} else {
		liveStats = a.Stats.Get(ipAddr.String)
	}
	if liveStats != nil {
		d["live_stats"] = liveStats
	}
	a.attachIdentityMismatch(d, id)
	writeJSON(w, d)
}

type deviceAlertingRequest struct {
	Alertable          *bool   `json:"alertable"`
	AlertSilencedUntil *string `json:"alert_silenced_until"`
	AlertNotes         *string `json:"alert_notes"`
	SilenceSeconds     *int    `json:"silence_seconds"`
	ClearSilence       bool    `json:"clear_silence"`
}

func parseAlertSilenceUntil(req deviceAlertingRequest) (*time.Time, bool, error) {
	selectors := 0
	if req.ClearSilence {
		selectors++
	}
	if req.SilenceSeconds != nil {
		selectors++
	}
	if req.AlertSilencedUntil != nil {
		selectors++
	}
	if selectors > 1 {
		return nil, false, fmt.Errorf("specify only one silence option")
	}
	if req.AlertNotes != nil && len([]rune(*req.AlertNotes)) > 2000 {
		return nil, false, fmt.Errorf("alert_notes must be at most 2000 characters")
	}
	if req.ClearSilence {
		return nil, true, nil
	}
	if req.SilenceSeconds != nil {
		const maxSilenceSeconds = 31 * 24 * 60 * 60
		if *req.SilenceSeconds < 0 || *req.SilenceSeconds > maxSilenceSeconds {
			return nil, false, fmt.Errorf("silence_seconds must be between 0 and %d", maxSilenceSeconds)
		}
		if *req.SilenceSeconds == 0 {
			return nil, true, nil
		}
		t := time.Now().Add(time.Duration(*req.SilenceSeconds) * time.Second)
		return &t, true, nil
	}
	if req.AlertSilencedUntil != nil {
		raw := strings.TrimSpace(*req.AlertSilencedUntil)
		if raw == "" {
			return nil, true, nil
		}
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return nil, false, fmt.Errorf("alert_silenced_until must be RFC3339")
		}
		return &t, true, nil
	}
	return nil, false, nil
}

func (a *API) updateDeviceAlertPolicy(ctx context.Context, deviceID int64, req deviceAlertingRequest, userID int64) (map[string]any, error) {
	if deviceID <= 0 {
		return nil, fmt.Errorf("invalid device id")
	}
	until, setSilence, err := parseAlertSilenceUntil(req)
	if err != nil {
		return nil, err
	}

	sets := []string{"updated_at = NOW()"}
	args := []any{}
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if req.Alertable != nil {
		sets = append(sets, "alertable = "+arg(*req.Alertable))
	}
	if req.AlertNotes != nil {
		sets = append(sets, "alert_notes = "+arg(strings.TrimSpace(*req.AlertNotes)))
	}
	if setSilence {
		if until == nil {
			sets = append(sets, "alert_silenced_until = NULL")
		} else {
			sets = append(sets, "alert_silenced_until = "+arg(*until))
		}
	}
	idRef := arg(deviceID)
	query := fmt.Sprintf(`UPDATE devices SET %s WHERE id = %s
		RETURNING lower(mac), COALESCE(alertable, false), alert_silenced_until, alert_notes`, strings.Join(sets, ", "), idRef)

	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var mac sql.NullString
	var alertable bool
	var silenced sql.NullTime
	var notes sql.NullString
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&mac, &alertable, &silenced, &notes); err != nil {
		return nil, err
	}

	shouldResolve := !alertable || (silenced.Valid && time.Now().Before(silenced.Time))
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if shouldResolve && a.Alerts != nil {
		if err := a.Alerts.ResolveDeviceAlerts(ctx, int(deviceID), "device alerting was disabled or silenced"); err != nil {
			return nil, fmt.Errorf("alert policy saved but active alert resolution failed: %w", err)
		}
	}

	changeParts := []string{}
	if req.Alertable != nil {
		changeParts = append(changeParts, fmt.Sprintf("alertable=%v", *req.Alertable))
	}
	if silenced.Valid {
		changeParts = append(changeParts, "silenced_until="+silenced.Time.Format(time.RFC3339))
	} else if setSilence {
		changeParts = append(changeParts, "silence=cleared")
	}
	if req.AlertNotes != nil {
		changeParts = append(changeParts, "notes updated")
	}
	if len(changeParts) > 0 && mac.Valid {
		a.logChangelogDevice(mac.String, "alert policy: "+strings.Join(changeParts, ", "), userID)
	}

	patch := map[string]any{"id": deviceID, "alertable": alertable, "alert_notes": notes.String}
	if silenced.Valid {
		patch["alert_silenced_until"] = silenced.Time
	} else {
		patch["alert_silenced_until"] = nil
	}
	if a.WSHub != nil {
		a.WSHub.BroadcastDeviceUpdate(int(deviceID), "", patch)
	}
	return patch, nil
}

func (a *API) UpdateDeviceAlerting(w http.ResponseWriter, r *http.Request) {
	if !a.requireEdit(w, r) {
		return
	}
	claims := getClaims(r)
	deviceID, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if deviceID <= 0 {
		http.Error(w, "invalid device id", 400)
		return
	}
	var req deviceAlertingRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid json", 400)
		return
	}
	patch, err := a.updateDeviceAlertPolicy(r.Context(), deviceID, req, claims.UserID)
	if err == sql.ErrNoRows {
		http.Error(w, "not found", 404)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "device": patch})
}

func (a *API) BulkUpdateDeviceAlerting(w http.ResponseWriter, r *http.Request) {
	if !a.requireEdit(w, r) {
		return
	}
	claims := getClaims(r)
	var req struct {
		DeviceIDs          []int64 `json:"device_ids"`
		Alertable          *bool   `json:"alertable"`
		AlertSilencedUntil *string `json:"alert_silenced_until"`
		AlertNotes         *string `json:"alert_notes"`
		SilenceSeconds     *int    `json:"silence_seconds"`
		ClearSilence       bool    `json:"clear_silence"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid json", 400)
		return
	}
	policyReq := deviceAlertingRequest{
		Alertable:          req.Alertable,
		AlertSilencedUntil: req.AlertSilencedUntil,
		AlertNotes:         req.AlertNotes,
		SilenceSeconds:     req.SilenceSeconds,
		ClearSilence:       req.ClearSilence,
	}
	if len(req.DeviceIDs) == 0 {
		http.Error(w, "device_ids required", 400)
		return
	}
	if len(req.DeviceIDs) > 1000 {
		http.Error(w, "too many device_ids", 400)
		return
	}

	results := []map[string]any{}
	for _, id := range req.DeviceIDs {
		patch, err := a.updateDeviceAlertPolicy(r.Context(), id, policyReq, claims.UserID)
		if err != nil {
			results = append(results, map[string]any{"device_id": id, "ok": false, "error": err.Error()})
			continue
		}
		results = append(results, map[string]any{"device_id": id, "ok": true, "device": patch})
	}
	writeJSON(w, map[string]any{"ok": true, "results": results})
}

// OpenDeviceUI redirects to the device's UI using a best-effort scheme.
//
// This keeps scheme selection in one place instead of scattering hardcoded http/https across
// the frontend.
func (a *API) OpenDeviceUI(w http.ResponseWriter, r *http.Request) {
	if !a.requireView(w, r) {
		return
	}
	// Resolve the destination strictly from inventory. Accepting a caller-supplied
	// IP here would turn the server into an authenticated internal-network probe.
	deviceID, err := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("device_id")), 10, 64)
	if err != nil || deviceID <= 0 {
		http.Error(w, "valid device_id required", http.StatusBadRequest)
		return
	}
	var ip string
	var platformNS sql.NullString
	if err := a.DB.QueryRowContext(r.Context(), `
		SELECT host(ip_address), platform
		FROM devices
		WHERE id = $1 AND ip_address IS NOT NULL
	`, deviceID).Scan(&ip, &platformNS); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "device not found or has no management IP", http.StatusNotFound)
		} else {
			http.Error(w, "database error", http.StatusInternalServerError)
		}
		return
	}
	if net.ParseIP(ip) == nil {
		http.Error(w, "device has invalid management IP", http.StatusInternalServerError)
		return
	}
	platform := strings.ToLower(strings.TrimSpace(platformNS.String))
	scheme := netutil.ResolveScheme(ip, netutil.SchemeHint{Platform: platform})
	if scheme == "" {
		scheme = "https"
	}
	http.Redirect(w, r, fmt.Sprintf("%s://%s/", scheme, ip), http.StatusFound)
}

func (a *API) DeleteDevice(w http.ResponseWriter, r *http.Request) {
	if !a.requireEdit(w, r) {
		return
	}
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	var mac, ip string
	if err := a.DB.QueryRow(`SELECT mac, host(ip_address) FROM devices WHERE id = $1`, id).Scan(&mac, &ip); err != nil {
		http.Error(w, "device not found", 404)
		return
	}
	// Delete children first
	if _, err := a.DB.Exec(`DELETE FROM devices WHERE parent_id = $1`, id); err != nil {
		log.Printf("Failed to delete child devices for %d: %v", id, err)
		http.Error(w, "database error", 500)
		return
	}
	// Delete the device
	if _, err := a.DB.Exec(`DELETE FROM devices WHERE id = $1`, id); err != nil {
		log.Printf("Failed to delete device %d: %v", id, err)
		http.Error(w, "database error", 500)
		return
	}
	a.Stats.RemoveByMAC(mac)
	// Changelog is non-critical
	if claims := getClaims(r); claims != nil {
		if _, err := a.DB.Exec(`INSERT INTO changelog (device_mac, change, "user") SELECT $1, $2, id FROM users WHERE username = $3`, mac, "deleted device", claims.Username); err != nil {
			log.Printf("Failed to log device deletion: %v", err)
		}
	}
	writeJSON(w, map[string]any{"status": "ok"})
}

// UpdateDeviceAntenna updates optional antenna modeling parameters for a device.
//
// These fields are used for future RF planning and are edited from the Config UI.
func (a *API) UpdateDeviceAntenna(w http.ResponseWriter, r *http.Request) {
	if !a.requireEdit(w, r) {
		return
	}
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)

	// Note: we accept nulls for numeric fields to clear them.
	var req struct {
		AntennaModel      string   `json:"antenna_model"`
		AntennaOverride   bool     `json:"antenna_override"`
		AntennaAzimuthDeg *float64 `json:"antenna_azimuth_deg"`
		// antenna_downtilt_deg is treated as *mechanical* downtilt when electrical downtilt is present.
		AntennaDowntiltDeg           *float64 `json:"antenna_downtilt_deg"`
		AntennaElectricalDowntiltDeg *float64 `json:"antenna_electrical_downtilt_deg"`
		AntennaBeamwidthHDeg         *float64 `json:"antenna_beamwidth_h_deg"`
		AntennaBeamwidthVDeg         *float64 `json:"antenna_beamwidth_v_deg"`

		// Optional sector planning/export fields
		RadiusM    *float64 `json:"radius_m"`
		Tech       *int64   `json:"tech"`
		DownMbps   *float64 `json:"down_mbps"`
		UpMbps     *float64 `json:"up_mbps"`
		LatencyMS  *float64 `json:"latency_ms"`
		BizResCode *string  `json:"bizres"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", 400)
		return
	}

	// Basic validation (allow nil to mean "unset")
	if req.AntennaAzimuthDeg != nil {
		if *req.AntennaAzimuthDeg < 0 || *req.AntennaAzimuthDeg > 360 {
			http.Error(w, "antenna_azimuth_deg must be between 0 and 360", 400)
			return
		}
	}
	if req.AntennaDowntiltDeg != nil {
		if *req.AntennaDowntiltDeg < -90 || *req.AntennaDowntiltDeg > 90 {
			http.Error(w, "antenna_downtilt_deg must be between -90 and 90", 400)
			return
		}
	}
	if req.AntennaElectricalDowntiltDeg != nil {
		if *req.AntennaElectricalDowntiltDeg < -90 || *req.AntennaElectricalDowntiltDeg > 90 {
			http.Error(w, "antenna_electrical_downtilt_deg must be between -90 and 90", 400)
			return
		}
	}
	if req.AntennaBeamwidthHDeg != nil {
		if *req.AntennaBeamwidthHDeg < 0 || *req.AntennaBeamwidthHDeg > 360 {
			http.Error(w, "antenna_beamwidth_h_deg must be between 0 and 360", 400)
			return
		}
	}
	if req.AntennaBeamwidthVDeg != nil {
		if *req.AntennaBeamwidthVDeg < 0 || *req.AntennaBeamwidthVDeg > 360 {
			http.Error(w, "antenna_beamwidth_v_deg must be between 0 and 360", 400)
			return
		}
	}
	if req.RadiusM != nil {
		if *req.RadiusM < 0 {
			http.Error(w, "radius_m must be >= 0", 400)
			return
		}
	}
	if req.Tech != nil {
		if *req.Tech < 0 {
			http.Error(w, "tech must be >= 0", 400)
			return
		}
	}
	if req.DownMbps != nil {
		if *req.DownMbps < 0 {
			http.Error(w, "down_mbps must be >= 0", 400)
			return
		}
	}
	if req.UpMbps != nil {
		if *req.UpMbps < 0 {
			http.Error(w, "up_mbps must be >= 0", 400)
			return
		}
	}
	if req.LatencyMS != nil {
		if *req.LatencyMS < 0 {
			http.Error(w, "latency_ms must be >= 0", 400)
			return
		}
	}
	if req.BizResCode != nil {
		code := strings.TrimSpace(*req.BizResCode)
		if code != "" {
			code = strings.ToUpper(code[:1])
			if code != "B" && code != "R" && code != "X" {
				http.Error(w, "bizres must be one of: B (Business), R (Residential), X (Both)", 400)
				return
			}
		}
	}

	// Convert pointers to driver-friendly values
	var az any = nil
	if req.AntennaAzimuthDeg != nil {
		az = *req.AntennaAzimuthDeg
	}
	var dt any = nil
	if req.AntennaDowntiltDeg != nil {
		dt = *req.AntennaDowntiltDeg
	}
	// Electrical downtilt defaults to 0 (many antennas have built-in electrical downtilt).
	// If the client omits it (or sends null), we persist 0.
	var edt any = 0.0
	if req.AntennaElectricalDowntiltDeg != nil {
		edt = *req.AntennaElectricalDowntiltDeg
	}
	var bwH any = nil
	if req.AntennaBeamwidthHDeg != nil {
		bwH = *req.AntennaBeamwidthHDeg
	}
	var bwV any = nil
	if req.AntennaBeamwidthVDeg != nil {
		bwV = *req.AntennaBeamwidthVDeg
	}
	var radius any = nil
	if req.RadiusM != nil {
		radius = *req.RadiusM
	}
	var tech any = nil
	if req.Tech != nil {
		tech = *req.Tech
	}
	var down any = nil
	if req.DownMbps != nil {
		down = *req.DownMbps
	}
	var up any = nil
	if req.UpMbps != nil {
		up = *req.UpMbps
	}
	var latency any = nil
	if req.LatencyMS != nil {
		latency = *req.LatencyMS
	}
	// Biz/Res classification defaults to Both (X).
	var bizres any = "X"
	if req.BizResCode != nil {
		code := strings.TrimSpace(*req.BizResCode)
		if code == "" {
			bizres = "X"
		} else {
			code = strings.ToUpper(code[:1])
			if code != "B" && code != "R" && code != "X" {
				// Should have been caught by validation, but be defensive.
				bizres = "X"
			} else {
				bizres = code
			}
		}
	}

	if _, err := a.DB.Exec(`
		UPDATE devices
		SET antenna_model = $1,
		    antenna_override = $2,
		    antenna_azimuth_deg = $3,
		    antenna_downtilt_deg = $4,
		    antenna_electrical_downtilt_deg = $5,
		    antenna_beamwidth_h_deg = $6,
		    antenna_beamwidth_v_deg = $7,
		    radius_m = $8,
		    tech = $9,
		    down_mbps = $10,
		    up_mbps = $11,
		    latency_ms = $12,
		    bizres = $13,
		    updated_at = NOW()
		WHERE id = $14
	`, req.AntennaModel, req.AntennaOverride, az, dt, edt, bwH, bwV, radius, tech, down, up, latency, bizres, id); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	// Return updated fields
	var model sql.NullString
	var override sql.NullBool
	var azOut, dtOut, edtOut, bwHOut, bwVOut sql.NullFloat64
	var radiusOut, downOut, upOut, latencyOut sql.NullFloat64
	var techOut sql.NullInt64
	var bizresOut sql.NullString
	if err := a.DB.QueryRow(`SELECT antenna_model, antenna_override, antenna_azimuth_deg, antenna_downtilt_deg, antenna_electrical_downtilt_deg, antenna_beamwidth_h_deg, antenna_beamwidth_v_deg,
		radius_m, tech, down_mbps, up_mbps, latency_ms, bizres
		FROM devices WHERE id = $1`, id).
		Scan(&model, &override, &azOut, &dtOut, &edtOut, &bwHOut, &bwVOut, &radiusOut, &techOut, &downOut, &upOut, &latencyOut, &bizresOut); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	resp := map[string]any{
		"id":               id,
		"antenna_model":    model.String,
		"antenna_override": override.Bool,
	}
	// Always include optional fields (null when unset) so the UI can clear values.
	resp["antenna_azimuth_deg"] = nil
	if azOut.Valid {
		resp["antenna_azimuth_deg"] = azOut.Float64
	}
	resp["antenna_downtilt_deg"] = nil
	if dtOut.Valid {
		resp["antenna_downtilt_deg"] = dtOut.Float64
	}
	// Always return electrical downtilt (default 0) for consistent UI behavior.
	if edtOut.Valid {
		resp["antenna_electrical_downtilt_deg"] = edtOut.Float64
	} else {
		resp["antenna_electrical_downtilt_deg"] = 0.0
	}
	resp["antenna_beamwidth_h_deg"] = nil
	if bwHOut.Valid {
		resp["antenna_beamwidth_h_deg"] = bwHOut.Float64
	}
	resp["antenna_beamwidth_v_deg"] = nil
	if bwVOut.Valid {
		resp["antenna_beamwidth_v_deg"] = bwVOut.Float64
	}
	resp["radius_m"] = nil
	if radiusOut.Valid {
		resp["radius_m"] = radiusOut.Float64
	}
	resp["tech"] = nil
	if techOut.Valid {
		resp["tech"] = techOut.Int64
	}
	resp["down_mbps"] = nil
	if downOut.Valid {
		resp["down_mbps"] = downOut.Float64
	}
	resp["up_mbps"] = nil
	if upOut.Valid {
		resp["up_mbps"] = upOut.Float64
	}
	resp["latency_ms"] = nil
	if latencyOut.Valid {
		resp["latency_ms"] = latencyOut.Float64
	}
	// Always return a value for consistent UI behavior.
	resp["bizres"] = "X"
	if bizresOut.Valid {
		code := strings.TrimSpace(bizresOut.String)
		if code != "" {
			resp["bizres"] = strings.ToUpper(code[:1])
		}
	}

	// Changelog is best-effort
	if claims := getClaims(r); claims != nil {
		_, _ = a.DB.Exec(`
			INSERT INTO changelog (device_mac, change, "user")
			SELECT d.mac, $2, u.id
			FROM devices d, users u
			WHERE d.id = $1 AND u.username = $3
		`, id, "updated antenna parameters", claims.Username)
	}

	writeJSON(w, resp)
}

func (a *API) RefreshDevice(w http.ResponseWriter, r *http.Request) {
	// Refresh only polls the device and does not change its configuration.
	if !a.requireView(w, r) {
		return
	}
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	logDebug("RefreshDevice API: id=%d", id)
	if err := a.Poller.RefreshDeviceByID(id); err != nil {
		logDebug("RefreshDevice API: device %d not found (or not pollable): %v", id, err)
		http.Error(w, "device not found", 404)
		return
	}
	writeJSON(w, map[string]any{"status": "refreshing"})
}

func (a *API) ListStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"devices": a.Stats.List(), "updated_at": a.Stats.GetLastPoll()})
}

func (a *API) GetStats(w http.ResponseWriter, r *http.Request) {
	ip := chi.URLParam(r, "ip")
	s := a.Stats.Get(ip)
	if s == nil {
		s = a.Stats.GetByMAC(ip)
	}
	if s == nil {
		http.Error(w, "not found", 404)
		return
	}
	writeJSON(w, s)
}

// GetThroughputHistory returns the network throughput history
func (a *API) GetThroughputHistory(w http.ResponseWriter, r *http.Request) {
	history := a.Stats.GetThroughputHistory()
	writeJSON(w, map[string]any{
		"samples": history,
		"count":   len(history),
	})
}

// GetStabilityStats returns flapping and reboot stats for all devices
func (a *API) GetStabilityStats(w http.ResponseWriter, r *http.Request) {
	stability := a.Stats.GetStabilityStats()

	// Filter to only devices with issues
	flapping := make([]*stats.StabilityStats, 0)
	rebooting := make([]*stats.StabilityStats, 0)

	for _, st := range stability {
		if st.Flaps1h > 0 || st.Flaps24h > 0 {
			flapping = append(flapping, st)
		}
		if st.Reboots1h > 0 || st.Reboots24h > 0 {
			rebooting = append(rebooting, st)
		}
	}

	writeJSON(w, map[string]any{
		"flapping":  flapping,
		"rebooting": rebooting,
		"total":     len(stability),
	})
}

func (a *API) ListFirmware(w http.ResponseWriter, r *http.Request) {
	files, err := a.Firmware.ListFirmwarePublic()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, files)
}

func (a *API) ListFirmwareVersions(w http.ResponseWriter, r *http.Request) {
	versions, err := a.Firmware.ListVersions()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, versions)
}

func (a *API) UploadFirmware(w http.ResponseWriter, r *http.Request) {
	if !a.requireEdit(w, r) {
		return
	}

	// Limit upload size to 1GB
	r.Body = http.MaxBytesReader(w, r.Body, 1<<30)

	// Parse multipart form
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "file too large or invalid form", 400)
		return
	}

	file, header, err := r.FormFile("firmware")
	if err != nil {
		http.Error(w, "missing firmware file", 400)
		return
	}
	defer file.Close()

	// Save firmware
	fw, err := a.Firmware.SaveFirmware(header.Filename, file)
	if err != nil {
		if strings.Contains(err.Error(), "already exists") {
			http.Error(w, err.Error(), 409)
			return
		}
		http.Error(w, err.Error(), 400)
		return
	}

	// Absolute server paths are internal implementation details. Firmware is
	// subsequently addressed through its stable ID/relative path.
	fw.Path = ""
	writeJSON(w, fw)
}

func (a *API) DeleteFirmware(w http.ResponseWriter, r *http.Request) {
	if !a.requireEdit(w, r) {
		return
	}

	name := chi.URLParam(r, "name")
	if name == "" {
		http.Error(w, "missing firmware name", 400)
		return
	}

	if err := a.Firmware.DeleteFirmware(name); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "not found") {
			http.Error(w, "firmware not found", http.StatusNotFound)
		} else {
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
		return
	}

	writeJSON(w, map[string]any{"ok": true, "deleted": name})
}

func (a *API) DownloadFirmware(w http.ResponseWriter, r *http.Request) {
	if !a.requireView(w, r) {
		return
	}
	reference := chi.URLParam(r, "name")
	if reference == "" {
		http.Error(w, "missing firmware reference", http.StatusBadRequest)
		return
	}
	fw, err := a.Firmware.GetFirmwareInfo(reference)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "not found") {
			http.Error(w, "firmware not found", http.StatusNotFound)
		} else {
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", fw.Name))
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, r, fw.Path)
}

func (a *API) UpgradeDevice(w http.ResponseWriter, r *http.Request) {
	if !a.requireEdit(w, r) {
		return
	}
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	var req struct {
		Firmware string `json:"firmware"`
		Force    bool   `json:"force"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}
	if id <= 0 || strings.TrimSpace(req.Firmware) == "" {
		http.Error(w, "valid device id and firmware are required", http.StatusBadRequest)
		return
	}
	result, err := a.Firmware.UpgradeDevice(r.Context(), id, req.Firmware, req.Force)
	if err != nil {
		logError("Upgrade error for device %d: %v", id, err)
		status := http.StatusBadGateway
		switch {
		case errors.Is(err, sql.ErrNoRows), strings.Contains(strings.ToLower(err.Error()), "device not found"), strings.Contains(strings.ToLower(err.Error()), "firmware not found"):
			status = http.StatusNotFound
		case strings.Contains(strings.ToLower(err.Error()), "invalid"):
			status = http.StatusBadRequest
		}
		http.Error(w, err.Error(), status)
		return
	}
	if result == nil {
		http.Error(w, "firmware service returned no result", http.StatusInternalServerError)
		return
	}
	if claims := getClaims(r); claims != nil {
		a.logChangelogDeviceByUsername(result.DeviceMAC, fmt.Sprintf("upgrade: %s", result.Status), claims.Username)
	}
	writeJSON(w, result)
}

func (a *API) UpgradeFanout(w http.ResponseWriter, r *http.Request) {
	if !a.requireEdit(w, r) {
		return
	}
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	var req struct {
		Force bool `json:"force"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}
	results, err := a.Firmware.UpgradeFanout(r.Context(), id, req.Force)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if claims := getClaims(r); claims != nil {
		for _, result := range results {
			if result != nil {
				a.logChangelogDeviceByUsername(result.DeviceMAC, fmt.Sprintf("fanout: %s", result.Status), claims.Username)
			}
		}
	}
	writeJSON(w, map[string]any{"results": results})
}

func (a *API) BulkUpgrade(w http.ResponseWriter, r *http.Request) {
	if !a.requireEdit(w, r) {
		return
	}
	var req struct {
		DeviceIDs []int64 `json:"device_ids"`
		Firmware  string  `json:"firmware"`
		Force     bool    `json:"force"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}
	writeJSON(w, map[string]any{"results": a.Firmware.UpgradeBulk(r.Context(), req.DeviceIDs, req.Firmware, req.Force)})
}

func (a *API) RetryUpgradeWithCredentials(w http.ResponseWriter, r *http.Request) {
	if !a.requireEdit(w, r) {
		return
	}
	var req struct {
		DeviceIDs []int64 `json:"device_ids"`
		Username  string  `json:"username"`
		Password  string  `json:"password"`
		Force     bool    `json:"force"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}
	if len(req.DeviceIDs) == 0 {
		http.Error(w, "device_ids required", 400)
		return
	}
	if req.Username == "" {
		http.Error(w, "username required", 400)
		return
	}
	results := a.Firmware.RetryUpgradeWithCredentials(r.Context(), req.DeviceIDs, req.Username, req.Password, req.Force)
	if claims := getClaims(r); claims != nil {
		for _, result := range results {
			if result != nil {
				a.logChangelogDeviceByUsername(result.DeviceMAC, fmt.Sprintf("retry upgrade: %s", result.Status), claims.Username)
			}
		}
	}
	writeJSON(w, map[string]any{"results": results})
}

func (a *API) Search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		writeJSON(w, map[string]any{"results": []any{}})
		return
	}
	rows, err := a.DB.Query(`SELECT id, mac, ip_address, hostname, product, status, parent_id FROM devices WHERE hostname ILIKE $1 OR ip_address::text ILIKE $1 OR mac ILIKE $1 OR product ILIKE $1 ORDER BY hostname LIMIT 50`, "%"+q+"%")
	if err != nil {
		logError("Search query error: %v", err)
		http.Error(w, "search failed", 500)
		return
	}
	defer rows.Close()
	var results []map[string]any
	for rows.Next() {
		var id int64
		var mac, ip, hostname, product, status sql.NullString
		var parentID sql.NullInt64
		if err := rows.Scan(&id, &mac, &ip, &hostname, &product, &status, &parentID); err != nil {
			continue
		}
		result := map[string]any{"id": id, "mac": mac.String, "ip_address": ip.String, "hostname": hostname.String, "product": product.String, "status": status.String}
		if parentID.Valid {
			result["parent_id"] = parentID.Int64
		}
		results = append(results, result)
	}
	if results == nil {
		results = []map[string]any{}
	}
	writeJSON(w, map[string]any{"results": results})
}

func (a *API) ListLogs(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if s := r.URL.Query().Get("limit"); s != "" {
		if v, _ := strconv.Atoi(s); v > 0 && v <= 500 {
			limit = v
		}
	}
	rows, err := a.DB.Query(`SELECT c.change_time, c.device_mac, c.change, u.username FROM changelog c LEFT JOIN users u ON c."user" = u.id ORDER BY c.change_time DESC LIMIT $1`, limit)
	if err != nil {
		logError("ListLogs query error: %v", err)
		http.Error(w, "logs query failed", 500)
		return
	}
	defer rows.Close()
	var logs []map[string]any
	for rows.Next() {
		var ctime time.Time
		var mac, change, user sql.NullString
		if err := rows.Scan(&ctime, &mac, &change, &user); err != nil {
			continue
		}
		logs = append(logs, map[string]any{"created_at": ctime, "device_mac": mac.String, "change": change.String, "user": user.String})
	}
	if logs == nil {
		logs = []map[string]any{}
	}
	writeJSON(w, logs)
}

func (a *API) ListUsers(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	rows, err := a.DB.Query(`SELECT u.id, u.username, u.status, COALESCE(array_agg(r.name) FILTER (WHERE r.name IS NOT NULL), '{}') FROM users u LEFT JOIN user_roles ur ON ur."user" = u.id LEFT JOIN roles r ON r.id = ur.role GROUP BY u.id ORDER BY u.username`)
	if err != nil {
		logError("ListUsers query error: %v", err)
		http.Error(w, "query failed", 500)
		return
	}
	defer rows.Close()
	var users []map[string]any
	for rows.Next() {
		var id int64
		var username string
		var status int
		var roles pq.StringArray
		if err := rows.Scan(&id, &username, &status, &roles); err != nil {
			continue
		}
		users = append(users, map[string]any{"id": id, "username": username, "status": status, "roles": []string(roles)})
	}
	if users == nil {
		users = []map[string]any{}
	}
	writeJSON(w, users)
}

func (a *API) CreateUser(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	var req struct {
		Username string   `json:"username"`
		Password string   `json:"password"`
		Roles    []string `json:"roles"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" || len(req.Username) > 64 {
		http.Error(w, "username must be between 1 and 64 characters", http.StatusBadRequest)
		return
	}
	passwordHash, err := hashPassword(req.Password)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	roles, err := validateRequestedRoles(req.Roles)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	tx, err := a.DB.BeginTx(r.Context(), nil)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	var id int64
	if err := tx.QueryRowContext(r.Context(), `
		INSERT INTO users (username, password, status, auth_version)
		VALUES ($1, $2, 1, 1) RETURNING id
	`, req.Username, passwordHash).Scan(&id); err != nil {
		logError("CreateUser insert error: %v", err)
		http.Error(w, "create user failed", http.StatusConflict)
		return
	}
	for _, role := range roles {
		result, err := tx.ExecContext(r.Context(), `
			INSERT INTO user_roles ("user", role)
			SELECT $1, id FROM roles WHERE name = $2
		`, id, role)
		if err != nil {
			http.Error(w, "create user failed", http.StatusInternalServerError)
			return
		}
		if rows, _ := result.RowsAffected(); rows != 1 {
			http.Error(w, "unknown role", http.StatusBadRequest)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "create user failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"id": id, "username": req.Username, "roles": roles})
}

func (a *API) UpdateUser(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid user id", http.StatusBadRequest)
		return
	}
	claims := getClaims(r)
	isSelf := claims != nil && claims.UserID == id
	isAdmin := a.isAdmin(r)
	if !isAdmin && !isSelf {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	var req struct {
		Password string   `json:"password"`
		Status   *int     `json:"status"`
		Roles    []string `json:"roles"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if !isAdmin && (req.Status != nil || req.Roles != nil) {
		http.Error(w, "only administrators may change status or roles", http.StatusForbidden)
		return
	}
	if isSelf && (req.Status != nil || req.Roles != nil) {
		http.Error(w, "cannot change your own status or roles", http.StatusBadRequest)
		return
	}
	if req.Status != nil && *req.Status != 0 && *req.Status != 1 {
		http.Error(w, "status must be 0 or 1", http.StatusBadRequest)
		return
	}

	var passwordHash string
	if req.Password != "" {
		passwordHash, err = hashPassword(req.Password)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	var roles []string
	if req.Roles != nil {
		roles, err = validateRequestedRoles(req.Roles)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	tx, err := a.DB.BeginTx(r.Context(), nil)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(r.Context(), `SELECT pg_advisory_xact_lock(924701)`); err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	var currentStatus int
	var currentAdmin bool
	if err := tx.QueryRowContext(r.Context(), `
		SELECT u.status,
		       EXISTS (
		           SELECT 1
		             FROM user_roles ur
		             JOIN roles role ON role.id = ur.role
		            WHERE ur."user" = u.id AND role.name = $2
		       )
		  FROM users u
		 WHERE u.id = $1
		 FOR UPDATE
	`, id, RoleAdmin).Scan(&currentStatus, &currentAdmin); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "user not found", http.StatusNotFound)
		} else {
			http.Error(w, "database error", http.StatusInternalServerError)
		}
		return
	}
	removesActiveAdministrator := currentStatus == 1 && currentAdmin &&
		((req.Status != nil && *req.Status == 0) || (req.Roles != nil && !slices.Contains(roles, RoleAdmin)))
	if removesActiveAdministrator {
		var otherActiveAdministrators int
		if err := tx.QueryRowContext(r.Context(), `
			SELECT COUNT(DISTINCT u.id)
			  FROM users u
			  JOIN user_roles ur ON ur."user" = u.id
			  JOIN roles role ON role.id = ur.role
			 WHERE u.status = 1 AND role.name = $1 AND u.id <> $2
		`, RoleAdmin, id).Scan(&otherActiveAdministrators); err != nil {
			http.Error(w, "database error", http.StatusInternalServerError)
			return
		}
		if otherActiveAdministrators == 0 {
			http.Error(w, "cannot disable or demote the last active administrator", http.StatusConflict)
			return
		}
	}

	changed := false
	if passwordHash != "" {
		if _, err := tx.ExecContext(r.Context(), `UPDATE users SET password=$1 WHERE id=$2`, passwordHash, id); err != nil {
			http.Error(w, "database error", http.StatusInternalServerError)
			return
		}
		changed = true
	}
	if req.Status != nil {
		if _, err := tx.ExecContext(r.Context(), `UPDATE users SET status=$1 WHERE id=$2`, *req.Status, id); err != nil {
			http.Error(w, "database error", http.StatusInternalServerError)
			return
		}
		changed = true
	}
	if req.Roles != nil {
		if _, err := tx.ExecContext(r.Context(), `DELETE FROM user_roles WHERE "user"=$1`, id); err != nil {
			http.Error(w, "database error", http.StatusInternalServerError)
			return
		}
		for _, role := range roles {
			result, err := tx.ExecContext(r.Context(), `
				INSERT INTO user_roles ("user", role)
				SELECT $1, id FROM roles WHERE name=$2
			`, id, role)
			if err != nil {
				http.Error(w, "database error", http.StatusInternalServerError)
				return
			}
			if rows, _ := result.RowsAffected(); rows != 1 {
				http.Error(w, "unknown role", http.StatusBadRequest)
				return
			}
		}
		changed = true
	}
	if changed {
		if _, err := tx.ExecContext(r.Context(), `UPDATE users SET auth_version=auth_version+1 WHERE id=$1`, id); err != nil {
			http.Error(w, "database error", http.StatusInternalServerError)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"status": "ok"})
}

func (a *API) DeleteUser(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid user id", http.StatusBadRequest)
		return
	}
	if claims := getClaims(r); claims != nil && claims.UserID == id {
		http.Error(w, "cannot delete yourself", http.StatusBadRequest)
		return
	}

	tx, err := a.DB.BeginTx(r.Context(), nil)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(r.Context(), `SELECT pg_advisory_xact_lock(924701)`); err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	var currentStatus int
	var currentAdmin bool
	if err := tx.QueryRowContext(r.Context(), `
		SELECT u.status,
		       EXISTS (
		           SELECT 1
		             FROM user_roles ur
		             JOIN roles role ON role.id = ur.role
		            WHERE ur."user" = u.id AND role.name = $2
		       )
		  FROM users u
		 WHERE u.id = $1
		 FOR UPDATE
	`, id, RoleAdmin).Scan(&currentStatus, &currentAdmin); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "user not found", http.StatusNotFound)
		} else {
			http.Error(w, "database error", http.StatusInternalServerError)
		}
		return
	}
	if currentStatus == 1 && currentAdmin {
		var otherActiveAdministrators int
		if err := tx.QueryRowContext(r.Context(), `
			SELECT COUNT(DISTINCT u.id)
			  FROM users u
			  JOIN user_roles ur ON ur."user" = u.id
			  JOIN roles role ON role.id = ur.role
			 WHERE u.status = 1 AND role.name = $1 AND u.id <> $2
		`, RoleAdmin, id).Scan(&otherActiveAdministrators); err != nil {
			http.Error(w, "database error", http.StatusInternalServerError)
			return
		}
		if otherActiveAdministrators == 0 {
			http.Error(w, "cannot delete the last active administrator", http.StatusConflict)
			return
		}
	}
	result, err := tx.ExecContext(r.Context(), `DELETE FROM users WHERE id=$1`, id)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"status": "ok", "deleted": id})
}

func (a *API) ListRoles(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	rows, err := a.DB.Query(`SELECT id, name FROM roles ORDER BY name`)
	if err != nil {
		logError("ListRoles query error: %v", err)
		http.Error(w, "query failed", 500)
		return
	}
	defer rows.Close()
	var roles []map[string]any
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			continue
		}
		roles = append(roles, map[string]any{"id": id, "name": name})
	}
	if roles == nil {
		roles = []map[string]any{}
	}
	writeJSON(w, roles)
}

func (a *API) ListSettings(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	rows, err := a.DB.QueryContext(r.Context(), `SELECT key, value FROM settings ORDER BY key`)
	if err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	settings := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}
		if secrets.IsSecretSetting(key) && value != "" {
			settings[key] = secrets.MaskedValue
		} else {
			settings[key] = value
		}
	}
	if err := rows.Err(); err != nil {
		http.Error(w, "query failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, settings)
}

type settingPatch struct {
	Value string
	Clear bool
}

var errUnknownSetting = errors.New("unknown setting")

func (a *API) UpdateSetting(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	key := strings.TrimSpace(chi.URLParam(r, "key"))
	if key == "" || len(key) > 64 {
		http.Error(w, "invalid setting key", http.StatusBadRequest)
		return
	}
	var req struct {
		Value string `json:"value"`
		Clear bool   `json:"clear,omitempty"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	restart, err := a.applySettingUpdates(r.Context(), map[string]settingPatch{key: {Value: req.Value, Clear: req.Clear}})
	if err != nil {
		writeSettingError(w, err)
		return
	}
	writeJSON(w, map[string]any{"status": "ok", "restart_required": restart})
}

// UpdateSettings applies a settings form atomically. Credential usernames and
// passwords are therefore never reloaded as mismatched half-updated pairs.
func (a *API) UpdateSettings(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	var req struct {
		Settings map[string]string `json:"settings"`
		Clear    []string          `json:"clear,omitempty"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if len(req.Settings)+len(req.Clear) == 0 || len(req.Settings)+len(req.Clear) > 100 {
		http.Error(w, "between 1 and 100 settings are required", http.StatusBadRequest)
		return
	}
	updates := make(map[string]settingPatch, len(req.Settings)+len(req.Clear))
	for key, value := range req.Settings {
		key = strings.TrimSpace(key)
		if key == "" || len(key) > 64 {
			http.Error(w, "invalid setting key", http.StatusBadRequest)
			return
		}
		updates[key] = settingPatch{Value: value}
	}
	for _, key := range req.Clear {
		key = strings.TrimSpace(key)
		if key == "" || len(key) > 64 {
			http.Error(w, "invalid setting key", http.StatusBadRequest)
			return
		}
		if _, exists := updates[key]; exists {
			http.Error(w, fmt.Sprintf("setting %q cannot be updated and cleared together", key), http.StatusBadRequest)
			return
		}
		updates[key] = settingPatch{Clear: true}
	}
	restart, err := a.applySettingUpdates(r.Context(), updates)
	if err != nil {
		writeSettingError(w, err)
		return
	}
	writeJSON(w, map[string]any{"status": "ok", "restart_required": restart})
}

func writeSettingError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, errUnknownSetting) {
		status = http.StatusNotFound
	} else if strings.Contains(err.Error(), "runtime reload") || strings.Contains(err.Error(), "database") || strings.Contains(err.Error(), "encrypt") {
		status = http.StatusInternalServerError
	}
	http.Error(w, err.Error(), status)
}

func (a *API) applySettingUpdates(ctx context.Context, updates map[string]settingPatch) ([]string, error) {
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("database error: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `SELECT key,value FROM settings FOR UPDATE`)
	if err != nil {
		return nil, fmt.Errorf("database error: %w", err)
	}
	current := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			rows.Close()
			return nil, fmt.Errorf("database error: %w", err)
		}
		current[key] = value
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("database error: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("database error: %w", err)
	}

	changed := make(map[string]string, len(updates))
	for key, update := range updates {
		if _, exists := current[key]; !exists {
			return nil, fmt.Errorf("%w: %s", errUnknownSetting, key)
		}
		value := update.Value
		if update.Clear {
			value = ""
		}
		if secrets.IsSecretSetting(key) {
			if !update.Clear && (value == "" || value == secrets.MaskedValue) {
				continue
			}
			if err := validateSettingValue(key, value); err != nil {
				return nil, fmt.Errorf("%s: %w", key, err)
			}
			if value != "" {
				if a.Secrets == nil {
					return nil, errors.New("encrypt setting: secret store is unavailable")
				}
				value, err = a.Secrets.Encrypt(value)
				if err != nil {
					return nil, fmt.Errorf("encrypt setting: %w", err)
				}
			}
		} else if err := validateSettingValue(key, value); err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		current[key] = value
		changed[key] = value
	}

	if err := validateSettingsSnapshot(current); err != nil {
		return nil, err
	}
	for key, value := range changed {
		if _, err := tx.ExecContext(ctx, `UPDATE settings SET value=$1,updated_at=NOW() WHERE key=$2`, value, key); err != nil {
			return nil, fmt.Errorf("database error: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("database error: %w", err)
	}

	if a.Firmware != nil {
		if err := a.Firmware.ReloadConfig(); err != nil {
			return nil, fmt.Errorf("setting saved but firmware runtime reload failed: %w", err)
		}
	}
	if a.Poller != nil {
		a.Poller.ReloadSettings()
	}
	if a.Alerts != nil {
		if err := a.Alerts.Reload(); err != nil {
			return nil, fmt.Errorf("setting saved but alert runtime reload failed: %w", err)
		}
	}

	restartSet := map[string]struct{}{}
	for key := range changed {
		switch key {
		case "listen_addr", "zabbix_enabled", "zabbix_listen", "zabbix_allowed_hosts", "cors_origins", "csp_img_sources", "csp_connect_sources":
			restartSet[key] = struct{}{}
		}
	}
	restart := make([]string, 0, len(restartSet))
	for key := range restartSet {
		restart = append(restart, key)
	}
	sort.Strings(restart)
	return restart, nil
}

func validateSettingValue(key, value string) error {
	if len(value) > 1<<20 || strings.ContainsRune(value, '\x00') {
		return errors.New("value is too large or contains a NUL byte")
	}
	trimmed := strings.TrimSpace(value)
	switch key {
	case "poll_interval":
		return validateSettingInt(trimmed, 5, 3600)
	case "poller_workers":
		return validateSettingInt(trimmed, 1, 500)
	case "scheduler_max_concurrent":
		return validateSettingInt(trimmed, 1, 100)
	case "scheduler_check_interval":
		return validateSettingInt(trimmed, 1, 3600)
	case "smtp_port", "sysmon_alerter_port":
		return validateSettingInt(trimmed, 1, 65535)
	case "chain_imbalance_threshold_db", "rx_mismatch_threshold_db":
		return validateSettingInt(trimmed, 1, 100)
	case "interference_warning_pct", "interference_critical_pct":
		v, err := strconv.ParseFloat(trimmed, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 100 {
			return errors.New("must be a finite number between 0 and 100")
		}
	case "zabbix_enabled", "scheduler_respect_maintenance", "wave_peer_fallback", "wave_mlo_multi_radio", "sysmon_alerter_enabled":
		if trimmed != "true" && trimmed != "false" {
			return errors.New("must be true or false")
		}
	case "listen_addr", "zabbix_listen":
		if err := validateListenAddress(trimmed); err != nil {
			return err
		}
	case "zabbix_server":
		if trimmed != "" {
			if strings.ContainsAny(trimmed, " \t\r\n") {
				return errors.New("must be a hostname or host:port")
			}
			if _, _, err := net.SplitHostPort(trimmed); err != nil && strings.Contains(trimmed, ":") && net.ParseIP(trimmed) == nil {
				return errors.New("must be a hostname, IP address, or host:port")
			}
		}
	case "sysmon_alerter_host":
		if trimmed != "" {
			if err := sysmonalerter.ValidateHost(trimmed); err != nil {
				return err
			}
		}
	case "sysmon_alerter_name":
		if trimmed != "" && !sysmonalerter.ValidProtocolName(trimmed) {
			return errors.New("must use only letters, digits, '-' or '_' and be 1-64 characters")
		}
	case "sysmon_alerter_application":
		if len([]rune(trimmed)) > 128 || strings.ContainsAny(value, "\r\n") {
			return errors.New("must be at most 128 characters and contain no newline")
		}
	case "sysmon_alerter_ca_pem":
		if len(value) > 256<<10 {
			return errors.New("certificate PEM must be at most 256 KiB")
		}
		if trimmed != "" {
			if err := sysmonalerter.ValidateCAPEM(value); err != nil {
				return err
			}
		}
	case "management_prefixes":
		var prefixes []string
		if err := json.Unmarshal([]byte(trimmed), &prefixes); err != nil {
			return errors.New("must be a JSON array of CIDR prefixes")
		}
		for _, prefix := range prefixes {
			if _, _, err := net.ParseCIDR(strings.TrimSpace(prefix)); err != nil {
				return fmt.Errorf("invalid CIDR %q", prefix)
			}
		}
	case "zabbix_allowed_hosts":
		for _, item := range strings.Split(trimmed, ",") {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			if net.ParseIP(item) == nil {
				if _, _, err := net.ParseCIDR(item); err != nil && strings.ContainsAny(item, " /\\\t\r\n") {
					return fmt.Errorf("invalid allowed host %q", item)
				}
			}
		}
	case "cors_origins":
		if trimmed == "" {
			break
		}
		if trimmed == "*" {
			return errors.New("wildcard origins are forbidden with cookie authentication")
		}
		origins := []string{trimmed}
		if strings.HasPrefix(trimmed, "[") {
			if err := json.Unmarshal([]byte(trimmed), &origins); err != nil {
				return errors.New("must be an origin or JSON array of origins")
			}
		}
		for _, origin := range origins {
			if _, err := normalizeOrigin(origin); err != nil {
				return err
			}
		}
	case "csp_img_sources", "csp_connect_sources":
		if strings.ContainsAny(value, ";\r\n\"'") {
			return errors.New("contains a forbidden CSP delimiter")
		}
		for _, source := range strings.Fields(value) {
			if !strings.HasPrefix(source, "https://") && !(key == "csp_connect_sources" && strings.HasPrefix(source, "wss://")) {
				return fmt.Errorf("unsupported CSP source %q", source)
			}
		}
	case "firmware_path", "backup_dir":
		if trimmed == "" || len(trimmed) > 4096 {
			return errors.New("path is required and must be at most 4096 bytes")
		}
	case "smtp_from":
		if trimmed != "" {
			address, err := mail.ParseAddress(trimmed)
			if err != nil || !strings.EqualFold(address.Address, trimmed) {
				return errors.New("must be a single email address")
			}
		}
	case "ap_cred1_user", "ap_cred2_user", "ap_cred3_user", "sta_cred1_user", "sta_cred2_user", "sta_cred3_user", "smtp_username":
		if len(trimmed) > 256 || strings.ContainsAny(value, "\r\n") {
			return errors.New("username is too long or contains a newline")
		}
	case "ap_cred1_pass", "ap_cred2_pass", "ap_cred3_pass", "sta_cred1_pass", "sta_cred2_pass", "sta_cred3_pass", "smtp_password":
		if len(value) > 65536 {
			return errors.New("secret is too long")
		}
	case "sysmon_alerter_token":
		if len(value) > 4096 || strings.ContainsAny(value, " \t\r\n") {
			return errors.New("token must be at most 4096 bytes and contain no whitespace")
		}
	}
	return nil
}

func validateSettingInt(value string, min, max int) error {
	v, err := strconv.Atoi(value)
	if err != nil || v < min || v > max {
		return fmt.Errorf("must be an integer between %d and %d", min, max)
	}
	return nil
}

func validateListenAddress(value string) error {
	_, port, err := net.SplitHostPort(value)
	if err != nil {
		return errors.New("must be in host:port form")
	}
	return validateSettingInt(port, 1, 65535)
}

func validateSettingsSnapshot(values map[string]string) error {
	for _, prefix := range []string{"ap", "sta"} {
		for i := 1; i <= 3; i++ {
			user := strings.TrimSpace(values[fmt.Sprintf("%s_cred%d_user", prefix, i)])
			pass := values[fmt.Sprintf("%s_cred%d_pass", prefix, i)]
			if (user == "") != (pass == "") {
				return fmt.Errorf("%s credential slot %d must contain both username and password or neither", strings.ToUpper(prefix), i)
			}
		}
	}
	if strings.EqualFold(strings.TrimSpace(values["sysmon_alerter_enabled"]), "true") {
		port, err := strconv.Atoi(strings.TrimSpace(values["sysmon_alerter_port"]))
		if err != nil {
			return errors.New("sysmon_alerter_port must be an integer")
		}
		token := values["sysmon_alerter_token"]
		// Snapshot validation runs after secret values have been encrypted for
		// storage. The plaintext was validated before encryption; use a safe
		// placeholder here so ciphertext length/format is not mistaken for the
		// on-wire bearer token. Startup/runtime reload decrypts and validates the
		// actual token again.
		if secrets.IsEncrypted(token) {
			token = "stored-encrypted-token"
		}
		cfg := sysmonalerter.Config{
			Enabled: true, Host: values["sysmon_alerter_host"], Port: port,
			Name: values["sysmon_alerter_name"], Token: token,
			Application: values["sysmon_alerter_application"], CAPEM: values["sysmon_alerter_ca_pem"],
		}
		if err := sysmonalerter.ValidateConfig(cfg); err != nil {
			return fmt.Errorf("sysmon-web alerter configuration: %w", err)
		}
	}

	warning, warnErr := strconv.ParseFloat(values["interference_warning_pct"], 64)
	critical, critErr := strconv.ParseFloat(values["interference_critical_pct"], 64)
	if warnErr == nil && critErr == nil && warning > critical {
		return errors.New("interference_warning_pct must not exceed interference_critical_pct")
	}
	return nil
}

func validateRequestedRoles(input []string) ([]string, error) {
	roles := recognizedRoles(input)
	if len(roles) == 0 {
		return nil, errors.New("at least one application role is required")
	}
	seenInput := map[string]struct{}{}
	for _, role := range input {
		role = strings.ToLower(strings.TrimSpace(role))
		if role == "" {
			continue
		}
		switch role {
		case RoleViewer, RoleCreator, RoleEditor, RoleAdmin:
			seenInput[role] = struct{}{}
		default:
			return nil, fmt.Errorf("unknown role %q", role)
		}
	}
	if len(seenInput) != len(roles) {
		return nil, errors.New("roles contain invalid or duplicate entries")
	}
	return roles, nil
}

func hashPassword(password string) (string, error) {
	if password == "" || len([]byte(password)) > 72 {
		return "", fmt.Errorf("password must be between 1 and 72 bytes")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return string(hash), nil
}

// verifyPassword checks a password against a bcrypt hash
func verifyPassword(password, stored string) bool {
	err := bcrypt.CompareHashAndPassword([]byte(stored), []byte(password))
	return err == nil
}

// WebSocket upgrades the connection to websocket
func (a *API) WebSocket(w http.ResponseWriter, r *http.Request) {
	claims := getClaims(r)
	if claims == nil {
		http.Error(w, "unauthorized", 401)
		return
	}
	a.WSHub.ServeWS(w, r, int(claims.UserID))
}

// ListJobs returns scheduled jobs
func (a *API) ListJobs(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	limitStr := r.URL.Query().Get("limit")
	limit := 50
	if l, err := strconv.Atoi(limitStr); err == nil && l > 0 && l <= 200 {
		limit = l
	}

	jobs, err := a.Scheduler.ListJobs(status, limit)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	writeJSON(w, jobs)
}

// CreateJob creates a new scheduled job
func (a *API) CreateJob(w http.ResponseWriter, r *http.Request) {
	if !a.requireEdit(w, r) {
		return
	}
	claims := getClaims(r)
	if claims == nil {
		http.Error(w, "unauthorized", 401)
		return
	}

	var req struct {
		JobType     string          `json:"job_type"`
		DeviceIDs   []int           `json:"device_ids"`
		Parameters  json.RawMessage `json:"parameters"`
		ScheduledAt string          `json:"scheduled_at"` // RFC3339
		RepeatCron  string          `json:"repeat_cron"`  // @daily, @hourly, @weekly, or duration like "24h"
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", 400)
		return
	}

	if req.JobType == "" || len(req.DeviceIDs) == 0 {
		http.Error(w, "job_type and device_ids required", 400)
		return
	}

	// Parse scheduled time
	scheduledAt := time.Now()
	if req.ScheduledAt != "" {
		var err error
		scheduledAt, err = time.Parse(time.RFC3339, req.ScheduledAt)
		if err != nil {
			http.Error(w, "invalid scheduled_at format, use RFC3339", 400)
			return
		}
	}

	// Parse parameters based on job type
	var params interface{}
	switch scheduler.JobType(req.JobType) {
	case scheduler.JobUpgrade:
		var upgradeParams scheduler.UpgradeParams
		if req.Parameters != nil {
			json.Unmarshal(req.Parameters, &upgradeParams)
		}
		params = upgradeParams
	case scheduler.JobReboot, scheduler.JobRefresh:
		params = map[string]interface{}{}
	default:
		http.Error(w, "invalid job_type: "+req.JobType, 400)
		return
	}

	jobID, err := a.Scheduler.CreateJob(
		scheduler.JobType(req.JobType),
		req.DeviceIDs,
		params,
		scheduledAt,
		req.RepeatCron,
		int(claims.UserID),
	)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	// Log
	a.logChangelog(
		fmt.Sprintf("Created %s job #%d for %d devices", req.JobType, jobID, len(req.DeviceIDs)),
		claims.UserID)

	writeJSON(w, map[string]any{
		"job_id":  jobID,
		"status":  "pending",
		"message": fmt.Sprintf("Job scheduled for %s", scheduledAt.Format(time.RFC3339)),
	})
}

// UpdateJob updates a scheduled job's next run time and/or repeat schedule.
// Only jobs in pending/blocked status can be edited.
func (a *API) UpdateJob(w http.ResponseWriter, r *http.Request) {
	if !a.requireEdit(w, r) {
		return
	}
	claims := getClaims(r)
	if claims == nil {
		http.Error(w, "unauthorized", 401)
		return
	}
	jobID, _ := strconv.Atoi(chi.URLParam(r, "id"))

	var req struct {
		ScheduledAt *string `json:"scheduled_at"` // RFC3339
		RepeatCron  *string `json:"repeat_cron"`  // "", @daily, @weekly, or duration like "24h"
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", 400)
		return
	}

	var scheduledAt *time.Time
	if req.ScheduledAt != nil {
		if *req.ScheduledAt == "" {
			http.Error(w, "scheduled_at cannot be empty", 400)
			return
		}
		t, err := time.Parse(time.RFC3339, *req.ScheduledAt)
		if err != nil {
			http.Error(w, "invalid scheduled_at format, use RFC3339", 400)
			return
		}
		scheduledAt = &t
	}

	updated, err := a.Scheduler.UpdateJobSchedule(jobID, scheduledAt, req.RepeatCron)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	// Log
	a.logChangelog(
		fmt.Sprintf("Updated scheduled job #%d (reschedule/repeat)", jobID),
		claims.UserID)

	writeJSON(w, updated)
}

// CancelJob cancels a pending scheduled job
func (a *API) CancelJob(w http.ResponseWriter, r *http.Request) {
	if !a.requireEdit(w, r) {
		return
	}
	claims := getClaims(r)
	if claims == nil {
		http.Error(w, "unauthorized", 401)
		return
	}
	jobID, _ := strconv.Atoi(chi.URLParam(r, "id"))

	if err := a.Scheduler.CancelJob(jobID); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	// Log
	a.logChangelog(
		fmt.Sprintf("Cancelled job #%d", jobID), claims.UserID)

	writeJSON(w, map[string]any{"status": "cancelled"})
}

// === Async Job Runs API ===

// StartJobRun starts an async job and returns immediately with job_id
func (a *API) StartJobRun(w http.ResponseWriter, r *http.Request) {
	if !a.requireEdit(w, r) {
		return
	}
	claims := getClaims(r)
	if claims == nil {
		http.Error(w, "unauthorized", 401)
		return
	}

	var req struct {
		JobType    string          `json:"job_type"`
		DeviceIDs  []int           `json:"device_ids"`
		Parameters json.RawMessage `json:"parameters"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}

	if req.JobType == "" {
		http.Error(w, "job_type required", 400)
		return
	}

	// Parse parameters based on job type
	var params interface{}
	switch jobs.JobType(req.JobType) {
	case jobs.JobUpgrade, jobs.JobBulkUpgrade, jobs.JobFanoutUpgrade:
		var upgradeParams jobs.UpgradeParams
		if req.Parameters != nil {
			json.Unmarshal(req.Parameters, &upgradeParams)
		}
		params = upgradeParams
	case jobs.JobBackup:
		var backupParams jobs.BackupParams
		if req.Parameters != nil {
			json.Unmarshal(req.Parameters, &backupParams)
		}
		params = backupParams
	case jobs.JobReboot, jobs.JobRefresh:
		params = map[string]interface{}{}
	default:
		http.Error(w, "invalid job_type: "+req.JobType, 400)
		return
	}

	jobID, err := a.Jobs.StartJob(r.Context(), jobs.JobType(req.JobType), req.DeviceIDs, params, int(claims.UserID))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	// Log
	a.logChangelog(
		fmt.Sprintf("Started %s job %s for %d devices", req.JobType, jobID, len(req.DeviceIDs)),
		claims.UserID)

	writeJSON(w, map[string]any{
		"job_id":  jobID,
		"status":  "pending",
		"message": "Job started",
	})
}

// GetJobRun returns the status of a job run
func (a *API) GetJobRun(w http.ResponseWriter, r *http.Request) {
	if !a.requireView(w, r) {
		return
	}
	jobID := chi.URLParam(r, "id")

	job, err := a.Jobs.GetJob(r.Context(), jobID)
	if err != nil {
		http.Error(w, "job not found", 404)
		return
	}

	writeJSON(w, job)
}

// GetJobRunEvents returns the event log for a job run
func (a *API) GetJobRunEvents(w http.ResponseWriter, r *http.Request) {
	if !a.requireView(w, r) {
		return
	}
	jobID := chi.URLParam(r, "id")

	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}

	events, err := a.Jobs.GetJobEvents(r.Context(), jobID, limit)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	if events == nil {
		events = []jobs.JobEvent{}
	}

	writeJSON(w, events)
}

// ListJobRuns returns recent job runs
func (a *API) ListJobRuns(w http.ResponseWriter, r *http.Request) {
	if !a.requireView(w, r) {
		return
	}

	status := r.URL.Query().Get("status")
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}

	jobRuns, err := a.Jobs.ListJobs(r.Context(), status, limit)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	if jobRuns == nil {
		jobRuns = []jobs.JobRun{}
	}

	writeJSON(w, jobRuns)
}

// CancelJobRun cancels a running or pending job run
func (a *API) CancelJobRun(w http.ResponseWriter, r *http.Request) {
	if !a.requireEdit(w, r) {
		return
	}
	claims := getClaims(r)
	if claims == nil {
		http.Error(w, "unauthorized", 401)
		return
	}
	jobID := chi.URLParam(r, "id")

	if err := a.Jobs.CancelJob(jobID); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	// Log
	a.logChangelog(
		fmt.Sprintf("Cancelled job run %s", jobID), claims.UserID)

	writeJSON(w, map[string]any{"status": "cancelled"})
}

// Maintenance Window APIs

// ListMaintenanceWindows returns all maintenance windows
func (a *API) ListMaintenanceWindows(w http.ResponseWriter, r *http.Request) {
	windows, err := a.Scheduler.ListMaintenanceWindows()
	if err != nil {
		logError("ListMaintenanceWindows error: %v", err)
		http.Error(w, "query failed", 500)
		return
	}
	writeJSON(w, windows)
}

// CreateMaintenanceWindow creates a new maintenance window
func (a *API) CreateMaintenanceWindow(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	claims := getClaims(r)
	if claims == nil {
		http.Error(w, "unauthorized", 401)
		return
	}

	var req struct {
		Name      string   `json:"name"`
		Scope     string   `json:"scope"`
		RegionID  *int     `json:"region_id"`
		SiteID    *int     `json:"site_id"`
		DayOfWeek []int    `json:"day_of_week"`
		StartTime string   `json:"start_time"`
		EndTime   string   `json:"end_time"`
		Timezone  string   `json:"timezone"`
		AllowJobs []string `json:"allow_jobs"`
		Enabled   bool     `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}

	if req.Name == "" || req.StartTime == "" || req.EndTime == "" {
		http.Error(w, "name, start_time, and end_time are required", 400)
		return
	}

	if req.Scope == "" {
		req.Scope = "global"
	}
	if req.Timezone == "" {
		req.Timezone = "UTC"
	}
	if len(req.AllowJobs) == 0 {
		req.AllowJobs = []string{"upgrade", "reboot"}
	}

	mw := scheduler.MaintenanceWindow{
		Name:      req.Name,
		Scope:     req.Scope,
		RegionID:  req.RegionID,
		SiteID:    req.SiteID,
		DayOfWeek: req.DayOfWeek,
		StartTime: req.StartTime,
		EndTime:   req.EndTime,
		Timezone:  req.Timezone,
		AllowJobs: req.AllowJobs,
		Enabled:   req.Enabled,
	}

	id, err := a.Scheduler.CreateMaintenanceWindow(mw, int(claims.UserID))
	if err != nil {
		logError("CreateMaintenanceWindow error: %v", err)
		http.Error(w, "create failed: "+err.Error(), 500)
		return
	}

	a.logChangelog(
		fmt.Sprintf("Created maintenance window: %s", req.Name), claims.UserID)

	writeJSON(w, map[string]any{"id": id})
}

// UpdateMaintenanceWindow updates an existing maintenance window
func (a *API) UpdateMaintenanceWindow(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	claims := getClaims(r)
	if claims == nil {
		http.Error(w, "unauthorized", 401)
		return
	}

	id, _ := strconv.Atoi(chi.URLParam(r, "id"))

	var req struct {
		Name      string   `json:"name"`
		Scope     string   `json:"scope"`
		RegionID  *int     `json:"region_id"`
		SiteID    *int     `json:"site_id"`
		DayOfWeek []int    `json:"day_of_week"`
		StartTime string   `json:"start_time"`
		EndTime   string   `json:"end_time"`
		Timezone  string   `json:"timezone"`
		AllowJobs []string `json:"allow_jobs"`
		Enabled   bool     `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}

	mw := scheduler.MaintenanceWindow{
		Name:      req.Name,
		Scope:     req.Scope,
		RegionID:  req.RegionID,
		SiteID:    req.SiteID,
		DayOfWeek: req.DayOfWeek,
		StartTime: req.StartTime,
		EndTime:   req.EndTime,
		Timezone:  req.Timezone,
		AllowJobs: req.AllowJobs,
		Enabled:   req.Enabled,
	}

	if err := a.Scheduler.UpdateMaintenanceWindow(id, mw); err != nil {
		logError("UpdateMaintenanceWindow error: %v", err)
		http.Error(w, "update failed: "+err.Error(), 500)
		return
	}

	a.logChangelog(
		fmt.Sprintf("Updated maintenance window %d: %s", id, req.Name), claims.UserID)

	writeJSON(w, map[string]any{"status": "ok"})
}

// DeleteMaintenanceWindow deletes a maintenance window
func (a *API) DeleteMaintenanceWindow(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	claims := getClaims(r)
	if claims == nil {
		http.Error(w, "unauthorized", 401)
		return
	}

	id, _ := strconv.Atoi(chi.URLParam(r, "id"))

	if err := a.Scheduler.DeleteMaintenanceWindow(id); err != nil {
		logError("DeleteMaintenanceWindow error: %v", err)
		http.Error(w, "delete failed: "+err.Error(), 500)
		return
	}

	a.logChangelog(
		fmt.Sprintf("Deleted maintenance window %d", id), claims.UserID)

	writeJSON(w, map[string]any{"status": "ok"})
}

// GetSchedulerSettings returns scheduler configuration
func (a *API) GetSchedulerSettings(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	writeJSON(w, a.Scheduler.GetConcurrencySettings())
}

// UpdateSchedulerSettings updates scheduler configuration
func (a *API) UpdateSchedulerSettings(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	claims := getClaims(r)
	if claims == nil {
		http.Error(w, "unauthorized", 401)
		return
	}

	var req struct {
		MaxConcurrent      *int  `json:"max_concurrent"`
		CheckIntervalSec   *int  `json:"check_interval_sec"`
		RespectMaintenance *bool `json:"respect_maintenance"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}

	// Update settings in database
	if req.MaxConcurrent != nil && *req.MaxConcurrent > 0 && *req.MaxConcurrent <= 50 {
		if _, err := a.DB.Exec(`INSERT INTO settings (key, value) VALUES ('scheduler_max_concurrent', $1)
			ON CONFLICT (key) DO UPDATE SET value = $1`, strconv.Itoa(*req.MaxConcurrent)); err != nil {
			log.Printf("Failed to save scheduler_max_concurrent: %v", err)
			http.Error(w, "database error", 500)
			return
		}
	}
	if req.CheckIntervalSec != nil && *req.CheckIntervalSec >= 5 && *req.CheckIntervalSec <= 300 {
		if _, err := a.DB.Exec(`INSERT INTO settings (key, value) VALUES ('scheduler_check_interval', $1)
			ON CONFLICT (key) DO UPDATE SET value = $1`, strconv.Itoa(*req.CheckIntervalSec)); err != nil {
			log.Printf("Failed to save scheduler_check_interval: %v", err)
			http.Error(w, "database error", 500)
			return
		}
	}
	if req.RespectMaintenance != nil {
		val := "false"
		if *req.RespectMaintenance {
			val = "true"
		}
		if _, err := a.DB.Exec(`INSERT INTO settings (key, value) VALUES ('scheduler_respect_maintenance', $1)
			ON CONFLICT (key) DO UPDATE SET value = $1`, val); err != nil {
			log.Printf("Failed to save scheduler_respect_maintenance: %v", err)
			http.Error(w, "database error", 500)
			return
		}
	}

	// Reload settings in scheduler
	a.Scheduler.ReloadSettings()

	// Changelog is non-critical
	a.logChangelog("Updated scheduler settings", claims.UserID)

	writeJSON(w, a.Scheduler.GetConcurrencySettings())
}

// BackupConfig backs up a device's configuration to filesystem
func (a *API) BackupConfig(w http.ResponseWriter, r *http.Request) {
	if !a.requireEdit(w, r) {
		return
	}
	claims := getClaims(r)
	if claims == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	deviceID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || deviceID <= 0 {
		http.Error(w, "invalid device id", http.StatusBadRequest)
		return
	}

	var ip, mac, hostname, platform string
	var username, password sql.NullString
	var parentID sql.NullInt64
	err = a.DB.QueryRowContext(r.Context(), `
		SELECT host(ip_address), mac, COALESCE(hostname, ''), parent_id, COALESCE(platform, ''),
		       username, password
		FROM devices WHERE id = $1
	`, deviceID).Scan(&ip, &mac, &hostname, &parentID, &platform, &username, &password)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "device not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}

	config, err := a.Poller.FetchDeviceBackup(deviceID, ip, platform, username.String, password.String)
	if err != nil {
		http.Error(w, "backup failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	backupRoot, err := a.configuredBackupRoot(r.Context())
	if err != nil {
		http.Error(w, "backup storage unavailable", http.StatusInternalServerError)
		return
	}
	dirPath, err := a.backupDirectory(r.Context(), backupRoot, ip, parentID)
	if err != nil {
		http.Error(w, "backup storage unavailable", http.StatusInternalServerError)
		return
	}
	name := hostname
	if strings.TrimSpace(name) == "" {
		name = sanitizeIPForPath(ip)
	}
	filename, err := configbackup.Write(dirPath, name, config)
	if err != nil {
		http.Error(w, "failed to write backup", http.StatusInternalServerError)
		return
	}

	deviceType := "AP"
	if parentID.Valid {
		deviceType = "STA"
	}
	a.logChangelogDevice(mac, fmt.Sprintf("Configuration backed up (%s)", deviceType), claims.UserID)
	writeJSON(w, map[string]any{
		"path":    filename,
		"size":    len(config),
		"message": "Backup complete",
	})
}

// RestoreConfig restores a device configuration from filesystem
func (a *API) RestoreConfig(w http.ResponseWriter, r *http.Request) {
	if !a.requireEdit(w, r) {
		return
	}
	claims := getClaims(r)
	if claims == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	deviceID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || deviceID <= 0 {
		http.Error(w, "invalid device id", http.StatusBadRequest)
		return
	}

	var req struct {
		Path string `json:"path"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	var ip, mac, username, password string
	err = a.DB.QueryRowContext(r.Context(), `
		SELECT host(ip_address), mac, COALESCE(username, ''), COALESCE(password, '')
		FROM devices WHERE id = $1
	`, deviceID).Scan(&ip, &mac, &username, &password)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "device not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}

	backupRoot, err := a.configuredBackupRoot(r.Context())
	if err != nil {
		http.Error(w, "backup storage unavailable", http.StatusInternalServerError)
		return
	}
	configData, realPath, _, err := configbackup.ReadExisting(backupRoot, req.Path)
	if err != nil {
		http.Error(w, "invalid or unavailable backup", http.StatusBadRequest)
		return
	}

	if err := a.Firmware.PushConfig(ip, username, password, configData); err != nil {
		http.Error(w, "restore failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	a.logChangelogDevice(mac, fmt.Sprintf("Configuration restored from %s", filepath.Base(realPath)), claims.UserID)
	writeJSON(w, map[string]any{"status": "restored"})
}

// ListConfigs lists config backups for a device from filesystem
func (a *API) ListConfigs(w http.ResponseWriter, r *http.Request) {
	if !a.requireView(w, r) {
		return
	}
	deviceID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || deviceID <= 0 {
		http.Error(w, "invalid device id", http.StatusBadRequest)
		return
	}

	var ip string
	var parentID sql.NullInt64
	err = a.DB.QueryRowContext(r.Context(), `SELECT host(ip_address), parent_id FROM devices WHERE id = $1`, deviceID).Scan(&ip, &parentID)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "device not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}

	backupRoot, err := a.configuredBackupRoot(r.Context())
	if err != nil {
		http.Error(w, "backup storage unavailable", http.StatusInternalServerError)
		return
	}
	dirPath, err := a.backupDirectory(r.Context(), backupRoot, ip, parentID)
	if err != nil {
		http.Error(w, "backup storage unavailable", http.StatusInternalServerError)
		return
	}

	entries, err := os.ReadDir(dirPath)
	if errors.Is(err, os.ErrNotExist) {
		writeJSON(w, []any{})
		return
	}
	if err != nil {
		http.Error(w, "backup storage unavailable", http.StatusInternalServerError)
		return
	}

	configs := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !strings.EqualFold(filepath.Ext(entry.Name()), ".cfg") {
			continue
		}
		candidate := filepath.Join(dirPath, entry.Name())
		realPath, info, err := configbackup.ResolveExisting(backupRoot, candidate)
		if err != nil {
			continue
		}
		relPath, err := filepath.Rel(backupRoot, realPath)
		if err != nil {
			continue
		}
		configs = append(configs, map[string]any{
			"name":       filepath.Base(realPath),
			"path":       filepath.ToSlash(relPath),
			"size":       info.Size(),
			"created_at": info.ModTime(),
		})
	}
	sort.Slice(configs, func(i, j int) bool {
		return configs[i]["created_at"].(time.Time).After(configs[j]["created_at"].(time.Time))
	})
	writeJSON(w, configs)
}

// ListAllConfigs lists all config backups across all devices.
func (a *API) ListAllConfigs(w http.ResponseWriter, r *http.Request) {
	if !a.requireView(w, r) {
		return
	}
	backupRoot, err := a.configuredBackupRoot(r.Context())
	if err != nil {
		http.Error(w, "backup storage unavailable", http.StatusInternalServerError)
		return
	}

	deviceByIP := make(map[string]map[string]any)
	rows, err := a.DB.QueryContext(r.Context(), `SELECT id, host(ip_address), hostname, product FROM devices`)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	for rows.Next() {
		var id int64
		var ip, hostname, product sql.NullString
		if err := rows.Scan(&id, &ip, &hostname, &product); err != nil {
			rows.Close()
			http.Error(w, "database error", http.StatusInternalServerError)
			return
		}
		if ip.Valid {
			deviceByIP[ip.String] = map[string]any{
				"id": id, "ip": ip.String, "hostname": hostname.String, "product": product.String,
			}
		}
	}
	if err := rows.Close(); err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	if err := rows.Err(); err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}

	configs := make([]map[string]any, 0)
	walkErr := filepath.WalkDir(backupRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 || !strings.EqualFold(filepath.Ext(d.Name()), ".cfg") {
			return nil
		}
		realPath, info, err := configbackup.ResolveExisting(backupRoot, path)
		if err != nil {
			return nil
		}
		relPath, err := filepath.Rel(backupRoot, realPath)
		if err != nil {
			return nil
		}
		parts := strings.Split(filepath.ToSlash(relPath), "/")
		deviceIP := ""
		if len(parts) >= 2 {
			deviceIP = strings.ReplaceAll(parts[len(parts)-2], "_", ".")
		}
		device := map[string]any{"ip": deviceIP}
		if known, ok := deviceByIP[deviceIP]; ok {
			device = known
		}
		configs = append(configs, map[string]any{
			"name":       filepath.Base(realPath),
			"path":       filepath.ToSlash(relPath),
			"size":       info.Size(),
			"created_at": info.ModTime(),
			"device":     device,
		})
		return nil
	})
	if walkErr != nil {
		http.Error(w, "backup storage unavailable", http.StatusInternalServerError)
		return
	}
	sort.Slice(configs, func(i, j int) bool {
		return configs[i]["created_at"].(time.Time).After(configs[j]["created_at"].(time.Time))
	})
	writeJSON(w, configs)
}

// DownloadConfig downloads a config backup file
func (a *API) DownloadConfig(w http.ResponseWriter, r *http.Request) {
	if !a.requireView(w, r) {
		return
	}
	requested := strings.TrimSpace(r.URL.Query().Get("path"))
	if requested == "" {
		http.Error(w, "path required", http.StatusBadRequest)
		return
	}
	backupRoot, err := a.configuredBackupRoot(r.Context())
	if err != nil {
		http.Error(w, "backup storage unavailable", http.StatusInternalServerError)
		return
	}
	data, realPath, info, err := configbackup.ReadExisting(backupRoot, requested)
	if err != nil {
		http.Error(w, "config not found", http.StatusNotFound)
		return
	}
	filename := filepath.Base(realPath)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	http.ServeContent(w, r, filename, info.ModTime(), bytes.NewReader(data))
}

// BulkBackup backs up multiple devices
func (a *API) BulkBackup(w http.ResponseWriter, r *http.Request) {
	if !a.requireEdit(w, r) {
		return
	}
	claims := getClaims(r)
	if claims == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req struct {
		DeviceIDs []int64 `json:"device_ids"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if len(req.DeviceIDs) == 0 || len(req.DeviceIDs) > 1000 {
		http.Error(w, "device_ids must contain 1 to 1000 entries", http.StatusBadRequest)
		return
	}

	backupRoot, err := a.configuredBackupRoot(r.Context())
	if err != nil {
		http.Error(w, "backup storage unavailable", http.StatusInternalServerError)
		return
	}
	results := make([]map[string]any, 0, len(req.DeviceIDs))
	for _, deviceID := range req.DeviceIDs {
		if deviceID <= 0 {
			results = append(results, map[string]any{"device_id": deviceID, "status": "failed", "error": "invalid device id"})
			continue
		}
		var ip, mac, hostname, platform string
		var username, password sql.NullString
		var parentID sql.NullInt64
		err := a.DB.QueryRowContext(r.Context(), `
			SELECT host(ip_address), mac, COALESCE(hostname, ''), parent_id, COALESCE(platform, ''),
			       username, password
			FROM devices WHERE id = $1
		`, deviceID).Scan(&ip, &mac, &hostname, &parentID, &platform, &username, &password)
		if err != nil {
			results = append(results, map[string]any{"device_id": deviceID, "status": "failed", "error": "not found"})
			continue
		}
		config, err := a.Poller.FetchDeviceBackup(deviceID, ip, platform, username.String, password.String)
		if err != nil {
			results = append(results, map[string]any{"device_id": deviceID, "status": "failed", "error": err.Error()})
			continue
		}
		dirPath, err := a.backupDirectory(r.Context(), backupRoot, ip, parentID)
		if err != nil {
			results = append(results, map[string]any{"device_id": deviceID, "status": "failed", "error": "backup storage unavailable"})
			continue
		}
		name := hostname
		if strings.TrimSpace(name) == "" {
			name = sanitizeIPForPath(ip)
		}
		filename, err := configbackup.Write(dirPath, name, config)
		if err != nil {
			results = append(results, map[string]any{"device_id": deviceID, "status": "failed", "error": "backup write failed"})
			continue
		}
		a.logChangelogDevice(mac, "Configuration backed up (bulk)", claims.UserID)
		results = append(results, map[string]any{"device_id": deviceID, "status": "success", "path": filename})
	}
	writeJSON(w, map[string]any{"results": results})
}

func (a *API) configuredBackupRoot(ctx context.Context) (string, error) {
	backupPath := "backups"
	err := a.DB.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'backup_dir'`).Scan(&backupPath)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	return configbackup.EnsureRoot(backupPath)
}

func (a *API) backupDirectory(ctx context.Context, root, ip string, parentID sql.NullInt64) (string, error) {
	parts := []string{}
	if parentID.Valid {
		var apIP string
		err := a.DB.QueryRowContext(ctx, `SELECT host(ip_address) FROM devices WHERE id = $1`, parentID.Int64).Scan(&apIP)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		if strings.TrimSpace(apIP) == "" {
			apIP = "unknown-ap"
		}
		parts = append(parts, sanitizeIPForPath(apIP))
	}
	parts = append(parts, sanitizeIPForPath(ip))
	return configbackup.EnsureDir(root, parts...)
}

// sanitizeIPForPath converts an IP address to a filesystem-safe string
// IPv6 addresses contain colons which are invalid in Windows paths
func sanitizeIPForPath(ip string) string {
	// Replace colons with dashes for IPv6
	return strings.ReplaceAll(ip, ":", "-")
}

// BatchConfig pushes configuration changes to multiple devices
func (a *API) BatchConfig(w http.ResponseWriter, r *http.Request) {
	if !a.requireEdit(w, r) {
		return
	}
	claims := getClaims(r)
	if claims == nil {
		http.Error(w, "unauthorized", 401)
		return
	}

	var req struct {
		DeviceIDs []int          `json:"device_ids"`
		Changes   map[string]any `json:"changes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", 400)
		return
	}

	var results []map[string]any
	for _, deviceID := range req.DeviceIDs {
		var ip, mac, username, password string
		err := a.DB.QueryRow(`
			SELECT host(ip_address), mac, COALESCE(username, ''), COALESCE(password, '')
			FROM devices WHERE id = $1
		`, deviceID).Scan(&ip, &mac, &username, &password)
		if err != nil {
			results = append(results, map[string]any{"device_id": deviceID, "status": "failed", "error": "not found"})
			continue
		}

		err = a.Firmware.ApplyConfig(ip, username, password, req.Changes)
		if err != nil {
			results = append(results, map[string]any{"device_id": deviceID, "status": "failed", "error": err.Error()})
			continue
		}

		// Log
		a.logChangelogDevice(mac, fmt.Sprintf("Batch config applied: %v", req.Changes), claims.UserID)

		results = append(results, map[string]any{"device_id": deviceID, "status": "success"})
	}

	writeJSON(w, map[string]any{"results": results})
}

// GenerateReport generates a network report
func (a *API) GenerateReport(w http.ResponseWriter, r *http.Request) {
	if !a.requireEdit(w, r) {
		return
	}
	claims := getClaims(r)
	if claims == nil {
		http.Error(w, "unauthorized", 401)
		return
	}

	var req struct {
		Type string `json:"type"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", 400)
		return
	}

	reportType, ok := normalizeReportType(req.Type)
	if !ok {
		http.Error(w, "unsupported report type", http.StatusBadRequest)
		return
	}

	var deviceCount int
	if err := a.DB.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM devices`).Scan(&deviceCount); err != nil {
		http.Error(w, "device count failed", http.StatusInternalServerError)
		return
	}

	reportData, err := a.generateReportData(r.Context(), reportType)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Store report
	var reportID int
	err = a.DB.QueryRowContext(r.Context(), `
		INSERT INTO reports (type, data, device_count, created_by)
		VALUES ($1, $2, $3, $4)
		RETURNING id
	`, reportType, reportData, deviceCount, claims.UserID).Scan(&reportID)
	if err != nil {
		http.Error(w, "save failed: "+err.Error(), 500)
		return
	}

	writeJSON(w, map[string]any{
		"report_id":    reportID,
		"type":         reportType,
		"device_count": deviceCount,
	})
}

func (a *API) generateReportData(ctx context.Context, reportType string) ([]byte, error) {
	var data map[string]any

	switch reportType {
	case "health":
		built, err := a.buildHealthReport(ctx)
		if err != nil {
			return nil, err
		}
		data = built
	case "inventory":
		built, err := a.buildInventoryReport(ctx)
		if err != nil {
			return nil, err
		}
		data = built
	case "performance":
		built, err := a.buildPerformanceReport(ctx)
		if err != nil {
			return nil, err
		}
		data = built

	case "chain":
		chainThreshold := a.getSettingIntDefault("chain_imbalance_threshold_db", 5)
		allStats := a.Stats.List()

		deviceInfoByMAC := make(map[string]map[string]any)
		rows, err := a.DB.QueryContext(ctx, `
		SELECT d.mac, host(d.ip_address), d.hostname, d.product, s.name
		FROM devices d
		LEFT JOIN sites s ON d.site_id = s.id
	`)
		if err != nil {
			return nil, fmt.Errorf("load chain report inventory: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var mac, ip, hostname, product, site sql.NullString
			if err := rows.Scan(&mac, &ip, &hostname, &product, &site); err != nil {
				return nil, fmt.Errorf("scan chain report inventory: %w", err)
			}
			if !mac.Valid || strings.TrimSpace(mac.String) == "" {
				continue
			}
			deviceInfoByMAC[strings.ToLower(mac.String)] = map[string]any{
				"ip":       ip.String,
				"hostname": hostname.String,
				"product":  product.String,
				"site":     site.String,
			}
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("read chain report inventory: %w", err)
		}

		issues := make([]map[string]any, 0)
		appendDeviceIssue := func(band, deviceHost, deviceIP, site, mac string, chains []int, noiseFloor int) {
			chains = sanitizeReportSignalChains(chains, noiseFloor)
			if len(chains) < 2 {
				return
			}
			spread := signalChainSpread(chains)
			if spread <= chainThreshold {
				return
			}
			minV, maxV := chains[0], chains[0]
			for _, v := range chains[1:] {
				if v < minV {
					minV = v
				}
				if v > maxV {
					maxV = v
				}
			}
			issues = append(issues, map[string]any{
				"scope":           "device",
				"band":            band,
				"hostname":        deviceHost,
				"ip":              deviceIP,
				"site":            site,
				"mac":             mac,
				"mismatch_side":   "device",
				"ap_chains":       chains,
				"ap_min_signal":   minV,
				"ap_max_signal":   maxV,
				"ap_spread_db":    spread,
				"sta_chains":      []int{},
				"sta_spread_db":   0,
				"spread_db":       spread,
				"parent_hostname": "",
				"parent_ip":       "",
				"affected_ip":     deviceIP,
			})
		}
		appendPeerIssue := func(band, staHost, staIP, site, parentHost, parentIP, mac string, apChains []int, apNoiseFloor int, staChains []int, staNoiseFloor int) {
			apChains = sanitizeReportSignalChains(apChains, apNoiseFloor)
			staChains = sanitizeReportSignalChains(staChains, staNoiseFloor)
			apSpread := signalChainSpread(apChains)
			staSpread := signalChainSpread(staChains)
			side := chainMismatchSide(apSpread, staSpread, chainThreshold)
			if side == "none" {
				return
			}
			apMin, apMax := 0, 0
			if len(apChains) > 0 {
				apMin, apMax = apChains[0], apChains[0]
				for _, v := range apChains[1:] {
					if v < apMin {
						apMin = v
					}
					if v > apMax {
						apMax = v
					}
				}
			}
			staMin, staMax := 0, 0
			if len(staChains) > 0 {
				staMin, staMax = staChains[0], staChains[0]
				for _, v := range staChains[1:] {
					if v < staMin {
						staMin = v
					}
					if v > staMax {
						staMax = v
					}
				}
			}
			maxSpread := apSpread
			if staSpread > maxSpread {
				maxSpread = staSpread
			}
			issues = append(issues, map[string]any{
				"scope":           "peer",
				"band":            band,
				"hostname":        staHost,
				"ip":              staIP,
				"site":            site,
				"mac":             mac,
				"mismatch_side":   side,
				"ap_chains":       apChains,
				"sta_chains":      staChains,
				"ap_min_signal":   apMin,
				"ap_max_signal":   apMax,
				"sta_min_signal":  staMin,
				"sta_max_signal":  staMax,
				"ap_spread_db":    apSpread,
				"sta_spread_db":   staSpread,
				"spread_db":       maxSpread,
				"parent_hostname": parentHost,
				"parent_ip":       parentIP,
				"affected_ip":     staIP,
			})
		}

		for _, s := range allStats {
			deviceHost := s.Hostname
			deviceIP := s.IP
			site := ""
			if info := deviceInfoByMAC[strings.ToLower(s.MAC)]; info != nil {
				if deviceHost == "" {
					if v, ok := info["hostname"].(string); ok {
						deviceHost = v
					}
				}
				if deviceIP == "" {
					if v, ok := info["ip"].(string); ok {
						deviceIP = v
					}
				}
				if v, ok := info["site"].(string); ok {
					site = v
				}
			}
			if deviceHost == "" {
				deviceHost = deviceIP
			}

			radioList := []struct {
				name string
				r    *stats.RadioStats
			}{
				{"60 GHz", s.Wireless.Radio60GHz},
				{"6 GHz", s.Wireless.Radio6GHz},
				{"5 GHz", s.Wireless.Radio5GHz},
				{"LTU", s.Wireless.RadioLTU},
			}
			for _, item := range radioList {
				if item.r == nil {
					continue
				}
				appendDeviceIssue(radioBandLabel(item.r, item.name), deviceHost, deviceIP, site, s.MAC, item.r.SignalPerChain, item.r.NoiseFloor)
			}

			for _, peer := range s.Peers {
				parentHost := deviceHost
				parentIP := deviceIP
				peerHost := peer.Hostname
				if peerHost == "" {
					peerHost = peer.IP
				}
				for _, item := range getPeerRadioEntries(peer) {
					if item.r == nil {
						continue
					}
					staChains := item.r.RemoteSignalPerChain
					staNoiseFloor := item.r.RemoteNoiseFloor
					if len(staChains) == 0 && item.r == peer.Radio5GHz && len(peer.RemoteSignalPerChain) > 0 {
						staChains = peer.RemoteSignalPerChain
						staNoiseFloor = peer.RemoteNoiseFloor
					}
					appendPeerIssue(item.name, peerHost, peer.IP, site, parentHost, parentIP, peer.MAC, item.r.SignalPerChain, item.r.NoiseFloor, staChains, staNoiseFloor)
				}
			}
		}

		sort.Slice(issues, func(i, j int) bool {
			idb, _ := issues[i]["spread_db"].(int)
			jdb, _ := issues[j]["spread_db"].(int)
			if idb == jdb {
				ih, _ := issues[i]["hostname"].(string)
				jh, _ := issues[j]["hostname"].(string)
				return ih < jh
			}
			return idb > jdb
		})

		deviceIssues, peerIssues := 0, 0
		bothCount, apOnlyCount, staOnlyCount := 0, 0, 0
		for _, issue := range issues {
			if issue["scope"] == "device" {
				deviceIssues++
				continue
			}
			peerIssues++
			switch issue["mismatch_side"] {
			case "both":
				bothCount++
			case "ap_only":
				apOnlyCount++
			case "sta_only":
				staOnlyCount++
			}
		}

		metricDevices := 0
		for _, snapshot := range allStats {
			if snapshot != nil && deviceInfoByMAC[strings.ToLower(snapshot.MAC)] != nil {
				metricDevices++
			}
		}
		data = baseReportData("chain", time.Now().UTC())
		data["summary"] = map[string]any{
			"threshold_db":    chainThreshold,
			"total_issues":    len(issues),
			"device_issues":   deviceIssues,
			"peer_issues":     peerIssues,
			"both_issues":     bothCount,
			"ap_only_issues":  apOnlyCount,
			"sta_only_issues": staOnlyCount,
		}
		data["coverage"] = reportCoverage(len(deviceInfoByMAC), metricDevices, 0)
		data["issues"] = issues

	case "rx_mismatch":
		rxMismatchThreshold := a.getSettingIntDefault("rx_mismatch_threshold_db", 8)
		allStats := a.Stats.List()

		deviceInfoByMAC := make(map[string]map[string]any)
		rows, err := a.DB.QueryContext(ctx, `
		SELECT d.mac, host(d.ip_address), d.hostname, s.name
		FROM devices d
		LEFT JOIN sites s ON d.site_id = s.id
	`)
		if err != nil {
			return nil, fmt.Errorf("load receive-mismatch report inventory: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var mac, ip, hostname, site sql.NullString
			if err := rows.Scan(&mac, &ip, &hostname, &site); err != nil {
				return nil, fmt.Errorf("scan receive-mismatch report inventory: %w", err)
			}
			if !mac.Valid || strings.TrimSpace(mac.String) == "" {
				continue
			}
			deviceInfoByMAC[strings.ToLower(mac.String)] = map[string]any{
				"ip":       ip.String,
				"hostname": hostname.String,
				"site":     site.String,
			}
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("read receive-mismatch report inventory: %w", err)
		}

		issues := make([]map[string]any, 0)
		apStronger, staStronger := 0, 0
		for _, s := range allStats {
			apHost := s.Hostname
			apIP := s.IP
			site := ""
			if info := deviceInfoByMAC[strings.ToLower(s.MAC)]; info != nil {
				if apHost == "" {
					if v, ok := info["hostname"].(string); ok {
						apHost = v
					}
				}
				if apIP == "" {
					if v, ok := info["ip"].(string); ok {
						apIP = v
					}
				}
				if v, ok := info["site"].(string); ok {
					site = v
				}
			}
			if apHost == "" {
				apHost = apIP
			}

			for _, peer := range s.Peers {
				staHost := peer.Hostname
				if staHost == "" {
					staHost = peer.IP
				}
				for _, item := range getPeerRadioEntries(peer) {
					if item.r == nil {
						continue
					}
					apRx := getPeerRadioSignalValue(item.r)
					staRx := getPeerRadioRemoteSignalValue(item.r)
					if apRx == 0 || staRx == 0 {
						continue
					}
					delta := apRx - staRx
					if delta < 0 {
						delta = -delta
					}
					if delta <= rxMismatchThreshold {
						continue
					}
					stronger := "equal"
					if apRx > staRx {
						stronger = "ap_rx"
						apStronger++
					} else if staRx > apRx {
						stronger = "sta_rx"
						staStronger++
					}
					issues = append(issues, map[string]any{
						"band":          item.name,
						"ap_hostname":   apHost,
						"ap_ip":         apIP,
						"sta_hostname":  staHost,
						"sta_ip":        peer.IP,
						"site":          site,
						"mac":           peer.MAC,
						"ap_rx":         apRx,
						"sta_rx":        staRx,
						"delta_db":      delta,
						"stronger_side": stronger,
						"affected_ip": func() string {
							if peer.IP != "" {
								return peer.IP
							}
							return apIP
						}(),
					})
				}
			}
		}

		sort.Slice(issues, func(i, j int) bool {
			idb, _ := issues[i]["delta_db"].(int)
			jdb, _ := issues[j]["delta_db"].(int)
			if idb == jdb {
				ih, _ := issues[i]["ap_hostname"].(string)
				jh, _ := issues[j]["ap_hostname"].(string)
				if ih == jh {
					is, _ := issues[i]["sta_hostname"].(string)
					js, _ := issues[j]["sta_hostname"].(string)
					return is < js
				}
				return ih < jh
			}
			return idb > jdb
		})

		metricDevices := 0
		for _, snapshot := range allStats {
			if snapshot != nil && deviceInfoByMAC[strings.ToLower(snapshot.MAC)] != nil {
				metricDevices++
			}
		}
		data = baseReportData("rx_mismatch", time.Now().UTC())
		data["summary"] = map[string]any{
			"threshold_db":        rxMismatchThreshold,
			"total_issues":        len(issues),
			"ap_stronger_issues":  apStronger,
			"sta_stronger_issues": staStronger,
		}
		data["coverage"] = reportCoverage(len(deviceInfoByMAC), metricDevices, 0)
		data["issues"] = issues

	default:
		return nil, fmt.Errorf("unsupported report type %q", reportType)
	}

	result, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("marshal report: %w", err)
	}
	return result, nil
}

// ListReports lists generated reports
func (a *API) ListReports(w http.ResponseWriter, r *http.Request) {
	if !a.requireView(w, r) {
		return
	}

	limit := 50
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			http.Error(w, "invalid limit", http.StatusBadRequest)
			return
		}
		if parsed > 200 {
			parsed = 200
		}
		limit = parsed
	}

	reportType := strings.TrimSpace(r.URL.Query().Get("type"))
	if reportType != "" {
		var ok bool
		reportType, ok = normalizeReportType(reportType)
		if !ok {
			http.Error(w, "unsupported report type", http.StatusBadRequest)
			return
		}
	}

	rows, err := a.DB.QueryContext(r.Context(), `
		SELECT r.id, r.type, r.device_count, r.created_at, r.created_by, COALESCE(u.username, '')
		FROM reports r
		LEFT JOIN users u ON u.id = r.created_by
		WHERE ($1 = '' OR r.type = $1)
		ORDER BY r.created_at DESC
		LIMIT $2
	`, reportType, limit)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	reports := make([]map[string]any, 0)
	for rows.Next() {
		var id, deviceCount int
		var reportTypeValue, createdByUsername string
		var createdAt time.Time
		var createdBy sql.NullInt64
		if err := rows.Scan(&id, &reportTypeValue, &deviceCount, &createdAt, &createdBy, &createdByUsername); err != nil {
			http.Error(w, "database row error", http.StatusInternalServerError)
			return
		}
		report := map[string]any{
			"id":                  id,
			"type":                reportTypeValue,
			"type_label":          reportTypeLabels[reportTypeValue],
			"device_count":        deviceCount,
			"created_at":          createdAt,
			"created_by_username": createdByUsername,
		}
		if createdBy.Valid {
			report["created_by"] = createdBy.Int64
		}
		reports = append(reports, report)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, "database iteration error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, reports)
}

// DeleteReport deletes a report
func (a *API) DeleteReport(w http.ResponseWriter, r *http.Request) {
	if !a.requireEdit(w, r) {
		return
	}
	reportID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || reportID <= 0 {
		http.Error(w, "invalid report id", http.StatusBadRequest)
		return
	}
	result, err := a.DB.ExecContext(r.Context(), `DELETE FROM reports WHERE id = $1`, reportID)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		http.Error(w, "report not found", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{"deleted": true})
}

// GetReport returns a single report with data for inline viewing
func (a *API) GetReport(w http.ResponseWriter, r *http.Request) {
	if !a.requireView(w, r) {
		return
	}
	reportID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || reportID <= 0 {
		http.Error(w, "invalid report id", http.StatusBadRequest)
		return
	}

	var reportType string
	var data []byte
	var deviceCount int
	var createdAt time.Time
	var createdBy sql.NullInt64
	var createdByUsername string
	err = a.DB.QueryRowContext(r.Context(), `
		SELECT r.type, r.data, r.device_count, r.created_at, r.created_by, COALESCE(u.username, '')
		FROM reports r
		LEFT JOIN users u ON u.id = r.created_by
		WHERE r.id = $1
	`, reportID).Scan(&reportType, &data, &deviceCount, &createdAt, &createdBy, &createdByUsername)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "report not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}

	response := map[string]any{
		"id":                  reportID,
		"type":                reportType,
		"type_label":          reportTypeLabels[reportType],
		"data":                json.RawMessage(data),
		"device_count":        deviceCount,
		"created_at":          createdAt,
		"created_by_username": createdByUsername,
	}
	if createdBy.Valid {
		response["created_by"] = createdBy.Int64
	}
	writeJSON(w, response)
}

// DownloadReport downloads a report as JSON or CSV.
func (a *API) DownloadReport(w http.ResponseWriter, r *http.Request) {
	if !a.requireView(w, r) {
		return
	}
	reportID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || reportID <= 0 {
		http.Error(w, "invalid report id", http.StatusBadRequest)
		return
	}
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if format == "" {
		format = "json"
	}
	if format != "json" && format != "csv" {
		http.Error(w, "format must be json or csv", http.StatusBadRequest)
		return
	}

	var reportType string
	var data []byte
	var createdAt time.Time
	err = a.DB.QueryRowContext(r.Context(), `SELECT type, data, created_at FROM reports WHERE id = $1`, reportID).
		Scan(&reportType, &data, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "report not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}

	dateStr := createdAt.Format("2006-01-02")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if format == "csv" {
		var reportData map[string]any
		if err := json.Unmarshal(data, &reportData); err != nil {
			http.Error(w, "report data is invalid", http.StatusInternalServerError)
			return
		}
		var buffer bytes.Buffer
		if err := writeReportCSV(&buffer, reportType, reportData); err != nil {
			http.Error(w, "CSV generation failed", http.StatusInternalServerError)
			return
		}
		filename := fmt.Sprintf("wavecontrol-%s-%s.csv", reportType, dateStr)
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
		_, _ = w.Write(buffer.Bytes())
		return
	}

	filename := fmt.Sprintf("wavecontrol-%s-%s.json", reportType, dateStr)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	_, _ = w.Write(data)
}

// CompareReports compares two same-type report snapshots and returns deltas.
func (a *API) CompareReports(w http.ResponseWriter, r *http.Request) {
	if !a.requireView(w, r) {
		return
	}
	var req struct {
		ReportID1 int64 `json:"report_id_1"`
		ReportID2 int64 `json:"report_id_2"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ReportID1 <= 0 || req.ReportID2 <= 0 {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	fetch := func(id int64) (string, []byte, time.Time, error) {
		var reportType string
		var data []byte
		var createdAt time.Time
		err := a.DB.QueryRowContext(r.Context(), `SELECT type, data, created_at FROM reports WHERE id = $1`, id).
			Scan(&reportType, &data, &createdAt)
		return reportType, data, createdAt, err
	}

	type1, data1, created1, err := fetch(req.ReportID1)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "report 1 not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	type2, data2, created2, err := fetch(req.ReportID2)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "report 2 not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	if type1 != type2 {
		http.Error(w, "reports must be the same type", http.StatusBadRequest)
		return
	}

	var report1, report2 map[string]any
	if err := json.Unmarshal(data1, &report1); err != nil {
		http.Error(w, "report 1 data is invalid", http.StatusInternalServerError)
		return
	}
	if err := json.Unmarshal(data2, &report2); err != nil {
		http.Error(w, "report 2 data is invalid", http.StatusInternalServerError)
		return
	}

	deltas := map[string]any{
		"report_type":   type1,
		"report_name":   reportTypeLabels[type1],
		"report_1_id":   req.ReportID1,
		"report_2_id":   req.ReportID2,
		"report_1_time": created1,
		"report_2_time": created2,
	}

	switch type1 {
	case "health":
		deltas["summary"] = compareSummary(getMap(report1, "summary"), getMap(report2, "summary"),
			[]string{"total", "online", "offline", "unknown", "uptime", "ap_count", "sta_count"})
		deltas["coverage"] = compareSummary(getMap(report1, "coverage"), getMap(report2, "coverage"),
			[]string{"inventory_devices", "metrics_devices", "missing_metrics", "signal_samples", "coverage_pct"})
		deltas["link_quality"] = compareSummary(getMap(report1, "link_quality"), getMap(report2, "link_quality"),
			[]string{"good", "fair", "poor", "no_signal"})
		deltas["system_health"] = compareSummary(getMap(report1, "system_health"), getMap(report2, "system_health"),
			[]string{"high_cpu", "high_mem", "high_temp"})
		deltas["stability"] = compareSummary(getMap(report1, "stability"), getMap(report2, "stability"),
			[]string{"flaps_1h", "flaps_24h", "reboots_1h", "reboots_24h"})

	case "performance":
		deltas["summary"] = compareSummary(getMap(report1, "summary"), getMap(report2, "summary"),
			[]string{"total_tx_rate", "total_rx_rate", "avg_signal", "device_count", "metrics_device_count", "ap_count", "sta_count"})
		deltas["coverage"] = compareSummary(getMap(report1, "coverage"), getMap(report2, "coverage"),
			[]string{"inventory_devices", "metrics_devices", "missing_metrics", "signal_samples", "coverage_pct"})
		deltas["signal_quality"] = compareSummary(getMap(report1, "signal_quality"), getMap(report2, "signal_quality"),
			[]string{"good", "fair", "poor", "no_signal"})

	case "inventory":
		oldSummary, newSummary := getMap(report1, "summary"), getMap(report2, "summary")
		if len(oldSummary) == 0 {
			oldSummary = map[string]any{"total": len(getSlice(report1, "devices"))}
		}
		if len(newSummary) == 0 {
			newSummary = map[string]any{"total": len(getSlice(report2, "devices"))}
		}
		deltas["summary"] = compareSummary(oldSummary, newSummary,
			[]string{"total", "online", "offline", "unknown", "ap_count", "sta_count", "site_count", "region_count", "unassigned_site", "firmware_versions"})

	case "chain":
		deltas["summary"] = compareSummary(getMap(report1, "summary"), getMap(report2, "summary"),
			[]string{"total_issues", "device_issues", "peer_issues", "both_issues", "ap_only_issues", "sta_only_issues"})
		deltas["coverage"] = compareSummary(getMap(report1, "coverage"), getMap(report2, "coverage"),
			[]string{"inventory_devices", "metrics_devices", "missing_metrics", "coverage_pct"})

	case "rx_mismatch":
		deltas["summary"] = compareSummary(getMap(report1, "summary"), getMap(report2, "summary"),
			[]string{"total_issues", "ap_stronger_issues", "sta_stronger_issues"})
		deltas["coverage"] = compareSummary(getMap(report1, "coverage"), getMap(report2, "coverage"),
			[]string{"inventory_devices", "metrics_devices", "missing_metrics", "coverage_pct"})

	default:
		http.Error(w, "unsupported report type", http.StatusBadRequest)
		return
	}

	writeJSON(w, deltas)
}

func getMap(m map[string]any, key string) map[string]any {
	if v, ok := m[key].(map[string]any); ok {
		return v
	}
	return map[string]any{}
}

func getSlice(m map[string]any, key string) []any {
	if v, ok := m[key].([]any); ok {
		return v
	}
	return []any{}
}

func compareSummary(old, new map[string]any, keys []string) map[string]any {
	result := make(map[string]any)
	for _, key := range keys {
		oldVal := getFloat64(old, key)
		newVal := getFloat64(new, key)
		result[key] = map[string]any{
			"report_1": oldVal,
			"report_2": newVal,
			"delta":    newVal - oldVal,
		}
	}
	return result
}

func csvEscape(s string) string {
	if strings.ContainsAny(s, ",\"\n") {
		return "\"" + strings.ReplaceAll(s, "\"", "\"\"") + "\""
	}
	return s
}

func getString(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func getInt64(m map[string]any, key string) int64 {
	switch v := m[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}

func getFloat64(m map[string]any, key string) float64 {
	switch v := m[key].(type) {
	case float64:
		return v
	case int64:
		return float64(v)
	case int:
		return float64(v)
	}
	return 0
}

// === Sites API ===

func (a *API) ListSites(w http.ResponseWriter, r *http.Request) {
	rows, err := a.DB.Query(`
		SELECT s.id, s.name, s.region_id, r.name as region_name, s.address, s.gps_lat, s.gps_lon, s.tower_h_m, s.notes,
		       (SELECT COUNT(*) FROM devices WHERE site_id = s.id) as device_count
		FROM sites s
		LEFT JOIN regions r ON s.region_id = r.id
		ORDER BY r.name NULLS LAST, s.name`)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()

	var sites []map[string]any
	for rows.Next() {
		var id int64
		var name string
		var regionID sql.NullInt64
		var regionName, address, notes sql.NullString
		var gpsLat, gpsLon, towerHM sql.NullFloat64
		var deviceCount int64
		if rows.Scan(&id, &name, &regionID, &regionName, &address, &gpsLat, &gpsLon, &towerHM, &notes, &deviceCount) != nil {
			continue
		}
		site := map[string]any{"id": id, "name": name, "device_count": deviceCount}
		if regionID.Valid {
			site["region_id"] = regionID.Int64
			site["region_name"] = regionName.String
		}
		if address.Valid {
			site["address"] = address.String
		}
		if gpsLat.Valid && gpsLon.Valid {
			site["gps_lat"] = gpsLat.Float64
			site["gps_lon"] = gpsLon.Float64
		}
		if towerHM.Valid {
			site["tower_h_m"] = towerHM.Float64
		}
		if notes.Valid {
			site["notes"] = notes.String
		}
		sites = append(sites, site)
	}
	if sites == nil {
		sites = []map[string]any{}
	}
	writeJSON(w, sites)
}

func (a *API) CreateSite(w http.ResponseWriter, r *http.Request) {
	if !a.requireEdit(w, r) {
		return
	}
	var req struct {
		Name     string   `json:"name"`
		RegionID *int64   `json:"region_id"`
		Address  string   `json:"address"`
		GpsLat   *float64 `json:"gps_lat"`
		GpsLon   *float64 `json:"gps_lon"`
		TowerHM  *float64 `json:"tower_h_m"`
		Notes    string   `json:"notes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}
	if req.Name == "" {
		http.Error(w, "name required", 400)
		return
	}

	var id int64
	err := a.DB.QueryRow(`INSERT INTO sites (name, region_id, address, gps_lat, gps_lon, tower_h_m, notes) 
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
		req.Name, req.RegionID, nilIfEmpty(req.Address), req.GpsLat, req.GpsLon, req.TowerHM, nilIfEmpty(req.Notes)).Scan(&id)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]any{"id": id, "name": req.Name})
}

func (a *API) UpdateSite(w http.ResponseWriter, r *http.Request) {
	if !a.requireEdit(w, r) {
		return
	}
	siteID, _ := strconv.Atoi(chi.URLParam(r, "id"))
	var req struct {
		Name     *string  `json:"name"`
		RegionID *int64   `json:"region_id"`
		Address  *string  `json:"address"`
		GpsLat   *float64 `json:"gps_lat"`
		GpsLon   *float64 `json:"gps_lon"`
		TowerHM  *float64 `json:"tower_h_m"`
		Notes    *string  `json:"notes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}

	// Build dynamic update query
	updates := []string{}
	args := []any{}
	i := 1
	if req.Name != nil {
		updates = append(updates, fmt.Sprintf("name = $%d", i))
		args = append(args, *req.Name)
		i++
	}
	if req.RegionID != nil {
		updates = append(updates, fmt.Sprintf("region_id = $%d", i))
		args = append(args, *req.RegionID)
		i++
	}
	if req.Address != nil {
		updates = append(updates, fmt.Sprintf("address = $%d", i))
		args = append(args, nilIfEmpty(*req.Address))
		i++
	}
	if req.GpsLat != nil {
		updates = append(updates, fmt.Sprintf("gps_lat = $%d", i))
		args = append(args, *req.GpsLat)
		i++
	}
	if req.GpsLon != nil {
		updates = append(updates, fmt.Sprintf("gps_lon = $%d", i))
		args = append(args, *req.GpsLon)
		i++
	}
	if req.TowerHM != nil {
		updates = append(updates, fmt.Sprintf("tower_h_m = $%d", i))
		args = append(args, *req.TowerHM)
		i++
	}
	if req.Notes != nil {
		updates = append(updates, fmt.Sprintf("notes = $%d", i))
		args = append(args, nilIfEmpty(*req.Notes))
		i++
	}

	if len(updates) == 0 {
		http.Error(w, "no updates", 400)
		return
	}

	args = append(args, siteID)
	query := fmt.Sprintf("UPDATE sites SET %s WHERE id = $%d", strings.Join(updates, ", "), i)
	_, err := a.DB.Exec(query, args...)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (a *API) DeleteSite(w http.ResponseWriter, r *http.Request) {
	if !a.requireEdit(w, r) {
		return
	}
	siteID, _ := strconv.Atoi(chi.URLParam(r, "id"))
	// Set devices to null site first
	if _, err := a.DB.Exec(`UPDATE devices SET site_id = NULL WHERE site_id = $1`, siteID); err != nil {
		log.Printf("Failed to unlink devices from site %d: %v", siteID, err)
		http.Error(w, "database error", 500)
		return
	}
	_, err := a.DB.Exec(`DELETE FROM sites WHERE id = $1`, siteID)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// === Regions API ===

func (a *API) ListRegions(w http.ResponseWriter, r *http.Request) {
	rows, err := a.DB.Query(`
		SELECT r.id, r.name, r.parent_id, p.name as parent_name,
		       (SELECT COUNT(*) FROM sites WHERE region_id = r.id) as site_count
		FROM regions r
		LEFT JOIN regions p ON r.parent_id = p.id
		ORDER BY p.name NULLS FIRST, r.name`)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()

	var regions []map[string]any
	for rows.Next() {
		var id int64
		var name string
		var parentID sql.NullInt64
		var parentName sql.NullString
		var siteCount int64
		if rows.Scan(&id, &name, &parentID, &parentName, &siteCount) != nil {
			continue
		}
		region := map[string]any{"id": id, "name": name, "site_count": siteCount}
		if parentID.Valid {
			region["parent_id"] = parentID.Int64
			region["parent_name"] = parentName.String
		}
		regions = append(regions, region)
	}
	if regions == nil {
		regions = []map[string]any{}
	}
	writeJSON(w, regions)
}

func (a *API) CreateRegion(w http.ResponseWriter, r *http.Request) {
	if !a.requireEdit(w, r) {
		return
	}
	var req struct {
		Name     string `json:"name"`
		ParentID *int64 `json:"parent_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}
	if req.Name == "" {
		http.Error(w, "name required", 400)
		return
	}

	var id int64
	err := a.DB.QueryRow(`INSERT INTO regions (name, parent_id) VALUES ($1, $2) RETURNING id`,
		req.Name, req.ParentID).Scan(&id)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]any{"id": id, "name": req.Name})
}

func (a *API) UpdateRegion(w http.ResponseWriter, r *http.Request) {
	if !a.requireEdit(w, r) {
		return
	}
	regionID, _ := strconv.Atoi(chi.URLParam(r, "id"))
	var req struct {
		Name     *string `json:"name"`
		ParentID *int64  `json:"parent_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}

	updates := []string{}
	args := []any{}
	i := 1
	if req.Name != nil {
		updates = append(updates, fmt.Sprintf("name = $%d", i))
		args = append(args, *req.Name)
		i++
	}
	if req.ParentID != nil {
		updates = append(updates, fmt.Sprintf("parent_id = $%d", i))
		args = append(args, *req.ParentID)
		i++
	}

	if len(updates) == 0 {
		http.Error(w, "no updates", 400)
		return
	}

	args = append(args, regionID)
	query := fmt.Sprintf("UPDATE regions SET %s WHERE id = $%d", strings.Join(updates, ", "), i)
	_, err := a.DB.Exec(query, args...)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (a *API) DeleteRegion(w http.ResponseWriter, r *http.Request) {
	if !a.requireEdit(w, r) {
		return
	}
	regionID, _ := strconv.Atoi(chi.URLParam(r, "id"))
	// Set sites to null region first
	if _, err := a.DB.Exec(`UPDATE sites SET region_id = NULL WHERE region_id = $1`, regionID); err != nil {
		log.Printf("Failed to unlink sites from region %d: %v", regionID, err)
		http.Error(w, "database error", 500)
		return
	}
	// Set child regions to null parent
	if _, err := a.DB.Exec(`UPDATE regions SET parent_id = NULL WHERE parent_id = $1`, regionID); err != nil {
		log.Printf("Failed to unlink child regions from region %d: %v", regionID, err)
		http.Error(w, "database error", 500)
		return
	}
	_, err := a.DB.Exec(`DELETE FROM regions WHERE id = $1`, regionID)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// BulkAssignSite assigns multiple devices to a site
func (a *API) BulkAssignSite(w http.ResponseWriter, r *http.Request) {
	if !a.requireEdit(w, r) {
		return
	}

	var req struct {
		DeviceIDs []int  `json:"device_ids"`
		SiteID    *int64 `json:"site_id"` // nil to unassign
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}

	if len(req.DeviceIDs) == 0 {
		http.Error(w, "device_ids required", 400)
		return
	}

	// Update all devices
	var siteID interface{} = nil
	if req.SiteID != nil {
		siteID = *req.SiteID
	}

	result, err := a.DB.Exec(`UPDATE devices SET site_id = $1 WHERE id = ANY($2)`,
		siteID, pq.Array(req.DeviceIDs))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	affected, _ := result.RowsAffected()
	writeJSON(w, map[string]any{
		"ok":       true,
		"affected": affected,
	})
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// sanitizeIPForPath converts an IP address to a filesystem-safe string
// IPv6 addresses contain colons which are invalid in Windows paths
// === TLS Certificate Management ===

func (a *API) GetTLSMode(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"mode": string(a.TLS.Mode()),
	})
}

func (a *API) SetTLSMode(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}

	var req struct {
		Mode string `json:"mode"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		http.Error(w, "invalid json", 400)
		return
	}

	mode := tlsutil.VerifyMode(req.Mode)
	if mode != tlsutil.ModeInsecure && mode != tlsutil.ModeTOFU && mode != tlsutil.ModeStrict {
		http.Error(w, "invalid mode: must be 'insecure', 'tofu', or 'strict'", 400)
		return
	}

	if err := a.TLS.SetMode(mode); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	writeJSON(w, map[string]any{"ok": true, "mode": string(mode)})
}

func (a *API) GetDeviceCert(w http.ResponseWriter, r *http.Request) {
	deviceID, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)

	info, err := a.TLS.GetCertInfo(deviceID)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	if info == nil {
		writeJSON(w, map[string]any{"pinned": false})
		return
	}

	result := map[string]any{
		"pinned":      true,
		"fingerprint": info.Fingerprint,
		"subject":     info.Subject,
		"issuer":      info.Issuer,
		"not_before":  info.NotBefore,
		"not_after":   info.NotAfter,
		"pinned_at":   info.PinnedAt,
		"verified":    info.Verified,
		"cert_valid":  info.CertValid,
	}
	if info.VerifiedAt != nil {
		result["verified_at"] = info.VerifiedAt
	}
	if info.VerifiedBy != 0 {
		result["verified_by"] = info.VerifiedBy
	}
	if info.PreviousFingerprint != "" {
		result["previous_fingerprint"] = info.PreviousFingerprint
	}
	if info.ChangedAt != nil {
		result["changed_at"] = info.ChangedAt
	}

	writeJSON(w, result)
}

func (a *API) PinDeviceCert(w http.ResponseWriter, r *http.Request) {
	if !a.requireEdit(w, r) {
		return
	}

	deviceID, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	claims := getClaims(r)

	// Get device IP
	var ip string
	if a.DB.QueryRow(`SELECT ip_address FROM devices WHERE id = $1`, deviceID).Scan(&ip) != nil {
		http.Error(w, "device not found", 404)
		return
	}

	info, err := a.TLS.PinCertFromDevice(deviceID, ip, int(claims.UserID), true)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	writeJSON(w, info)
}

func (a *API) UnpinDeviceCert(w http.ResponseWriter, r *http.Request) {
	if !a.requireEdit(w, r) {
		return
	}

	deviceID, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)

	if err := a.TLS.UnpinCert(deviceID); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	writeJSON(w, map[string]any{"ok": true})
}

func (a *API) UnpinSiteCerts(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}

	siteID, _ := strconv.Atoi(chi.URLParam(r, "id"))

	count, err := a.TLS.UnpinGroup(siteID)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	writeJSON(w, map[string]any{"ok": true, "unpinned": count})
}

func (a *API) GetCurrentDeviceCert(w http.ResponseWriter, r *http.Request) {
	deviceID, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)

	var ip string
	if a.DB.QueryRow(`SELECT ip_address FROM devices WHERE id = $1`, deviceID).Scan(&ip) != nil {
		http.Error(w, "device not found", 404)
		return
	}

	info, err := a.TLS.GetCurrentCert(ip)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	writeJSON(w, info)
}

// VerifyDeviceCert marks a device's certificate as verified by admin
func (a *API) VerifyDeviceCert(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}

	deviceID, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	claims := getClaims(r)

	if err := a.TLS.VerifyCert(deviceID, int(claims.UserID)); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	writeJSON(w, map[string]any{"ok": true})
}

// GetPendingCerts returns all certificates that need admin verification
func (a *API) GetPendingCerts(w http.ResponseWriter, r *http.Request) {
	certs, err := a.TLS.GetPendingCerts()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	writeJSON(w, map[string]any{"certs": certs})
}

// GetAllCerts returns all device certificates with their status
func (a *API) GetAllCerts(w http.ResponseWriter, r *http.Request) {
	certs, err := a.TLS.GetAllCerts()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	writeJSON(w, map[string]any{"certs": certs})
}

// BulkLearnCerts connects to all devices without pinned certs and learns them
func (a *API) BulkLearnCerts(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}

	claims := getClaims(r)

	var req struct {
		VerifyImmediately bool `json:"verify_immediately"`
	}
	json.NewDecoder(r.Body).Decode(&req) // Optional body

	learned, failed, errors := a.TLS.BulkLearnCerts(int(claims.UserID), req.VerifyImmediately)

	writeJSON(w, map[string]any{
		"ok":      true,
		"learned": learned,
		"failed":  failed,
		"errors":  errors,
	})
}

// BulkVerifyCerts verifies all pending certificates at once
func (a *API) BulkVerifyCerts(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}

	claims := getClaims(r)

	verified, err := a.TLS.BulkVerifyCerts(int(claims.UserID))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	writeJSON(w, map[string]any{
		"ok":       true,
		"verified": verified,
	})
}

// GetCertStats returns certificate statistics
func (a *API) GetCertStats(w http.ResponseWriter, r *http.Request) {
	total, verified, pending, changed, expired, noCert := a.TLS.Stats()

	writeJSON(w, map[string]any{
		"total":    total,
		"verified": verified,
		"pending":  pending,
		"changed":  changed,
		"expired":  expired,
		"no_cert":  noCert,
	})
}

// === Alert Rules ===

func (a *API) ListAlertRules(w http.ResponseWriter, r *http.Request) {
	rules, err := a.Alerts.ListRules()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if rules == nil {
		rules = []alerting.Rule{}
	}
	writeJSON(w, rules)
}

func (a *API) GetAlertChannelStatuses(w http.ResponseWriter, r *http.Request) {
	statuses, err := a.Alerts.NotificationChannelStatuses(r.Context())
	if err != nil {
		writeAlertManagerError(w, err)
		return
	}
	writeJSON(w, statuses)
}

func (a *API) TestSysmonAlerter(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	if err := a.Alerts.TestSysmon(ctx); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	statuses, err := a.Alerts.NotificationChannelStatuses(r.Context())
	if err != nil {
		writeAlertManagerError(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "channels": statuses})
}

func decodeAlertRulePatch(raw map[string]json.RawMessage, base alerting.Rule) (alerting.Rule, error) {
	allowed := map[string]bool{
		"name": true, "enabled": true, "scope": true, "scope_id": true,
		"target_role": true, "require_alertable": true, "metric": true,
		"operator": true, "threshold": true, "duration_seconds": true, "severity": true,
		"notify_channels": true, "notify_emails": true, "webhook_url": true,
		"notify_recovery": true, "cooldown_seconds": true,
	}
	baseJSON, err := json.Marshal(base)
	if err != nil {
		return base, err
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(baseJSON, &merged); err != nil {
		return base, err
	}
	for key, value := range raw {
		if !allowed[key] {
			return base, fmt.Errorf("unknown alert rule field %q", key)
		}
		merged[key] = value
	}
	body, err := json.Marshal(merged)
	if err != nil {
		return base, err
	}
	var rule alerting.Rule
	if err := json.Unmarshal(body, &rule); err != nil {
		return base, err
	}
	rule.ID = base.ID
	rule.CreatedAt = base.CreatedAt
	rule.CreatedBy = base.CreatedBy
	return rule, nil
}

func writeAlertManagerError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, alerting.ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case strings.Contains(err.Error(), "invalid") || strings.Contains(err.Error(), "must") ||
		strings.Contains(err.Error(), "required") || strings.Contains(err.Error(), "unsupported") ||
		strings.Contains(err.Error(), "scope") || strings.Contains(err.Error(), "threshold"):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (a *API) CreateAlertRule(w http.ResponseWriter, r *http.Request) {
	if !a.requireEdit(w, r) {
		return
	}
	claims := getClaims(r)
	var raw map[string]json.RawMessage
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&raw); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	base := alerting.Rule{Enabled: true, Scope: "all", TargetRole: "all", RequireAlertable: true, Severity: alerting.SeverityAuto, NotifyRecovery: true, CooldownSeconds: 300}
	rule, err := decodeAlertRulePatch(raw, base)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rule.CreatedBy = int(claims.UserID)
	id, err := a.Alerts.CreateRule(&rule)
	if err != nil {
		writeAlertManagerError(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "id": id})
}

func (a *API) UpdateAlertRule(w http.ResponseWriter, r *http.Request) {
	if !a.requireEdit(w, r) {
		return
	}
	ruleID, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil || ruleID <= 0 {
		http.Error(w, "invalid rule id", http.StatusBadRequest)
		return
	}
	existing, err := a.Alerts.GetRule(ruleID)
	if err != nil {
		writeAlertManagerError(w, err)
		return
	}
	var raw map[string]json.RawMessage
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&raw); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if len(raw) == 0 {
		http.Error(w, "empty patch", http.StatusBadRequest)
		return
	}
	rule, err := decodeAlertRulePatch(raw, existing)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := a.Alerts.UpdateRule(ruleID, &rule); err != nil {
		writeAlertManagerError(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (a *API) DeleteAlertRule(w http.ResponseWriter, r *http.Request) {
	if !a.requireEdit(w, r) {
		return
	}
	ruleID, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil || ruleID <= 0 {
		http.Error(w, "invalid rule id", http.StatusBadRequest)
		return
	}
	if err := a.Alerts.DeleteRule(ruleID); err != nil {
		writeAlertManagerError(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// === Alerts ===

func (a *API) ListAlerts(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			http.Error(w, "invalid limit", http.StatusBadRequest)
			return
		}
		limit = parsed
	}
	alerts, err := a.Alerts.ListAlerts(status, limit)
	if err != nil {
		writeAlertManagerError(w, err)
		return
	}
	if alerts == nil {
		alerts = []alerting.Alert{}
	}
	writeJSON(w, alerts)
}

func (a *API) AcknowledgeAlert(w http.ResponseWriter, r *http.Request) {
	if !a.requireEdit(w, r) {
		return
	}
	claims := getClaims(r)
	alertID, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil || alertID <= 0 {
		http.Error(w, "invalid alert id", http.StatusBadRequest)
		return
	}
	if err := a.Alerts.AcknowledgeAlert(alertID, int(claims.UserID)); err != nil {
		writeAlertManagerError(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (a *API) ResolveAlert(w http.ResponseWriter, r *http.Request) {
	if !a.requireEdit(w, r) {
		return
	}
	alertID, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil || alertID <= 0 {
		http.Error(w, "invalid alert id", http.StatusBadRequest)
		return
	}
	if err := a.Alerts.ResolveAlert(alertID); err != nil {
		writeAlertManagerError(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// === Bulk Operations Config ===

func (a *API) GetBulkOpsConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, a.BulkOps.GetConfig())
}

func (a *API) UpdateBulkOpsConfig(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}

	var cfg bulkops.Config
	if json.NewDecoder(r.Body).Decode(&cfg) != nil {
		http.Error(w, "invalid json", 400)
		return
	}

	// Validate
	if cfg.MaxGlobalConcurrent < 1 || cfg.MaxGlobalConcurrent > 100 {
		http.Error(w, "max_global_concurrent must be 1-100", 400)
		return
	}
	if cfg.MaxPerJob < 1 || cfg.MaxPerJob > 50 {
		http.Error(w, "max_per_job must be 1-50", 400)
		return
	}
	if cfg.MaxPerAP < 1 || cfg.MaxPerAP > 20 {
		http.Error(w, "max_per_ap must be 1-20", 400)
		return
	}

	if err := a.BulkOps.UpdateConfig(&cfg); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	writeJSON(w, map[string]any{"ok": true})
}

func (a *API) GetBulkOpsStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, a.BulkOps.Stats())
}

// === Poller Config ===

func (a *API) GetPollerConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, a.Poller.GetRuntimeConfig())
}

func (a *API) UpdatePollerConfig(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}

	var cfg poller.RuntimeConfig
	if json.NewDecoder(r.Body).Decode(&cfg) != nil {
		http.Error(w, "invalid json", 400)
		return
	}

	if err := a.Poller.UpdateRuntimeConfig(cfg); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	writeJSON(w, map[string]any{"ok": true})
}

// === Dry Run ===

func (a *API) DryRunOperation(w http.ResponseWriter, r *http.Request) {
	if !a.requireEdit(w, r) {
		return
	}

	var req struct {
		Operation  string `json:"operation"` // "upgrade", "config"
		DeviceIDs  []int  `json:"device_ids"`
		Parameters struct {
			FirmwareFile string `json:"firmware_file,omitempty"`
			Force        bool   `json:"force,omitempty"`
		} `json:"parameters"`
	}

	if json.NewDecoder(r.Body).Decode(&req) != nil {
		http.Error(w, "invalid json", 400)
		return
	}

	if len(req.DeviceIDs) == 0 {
		http.Error(w, "device_ids required", 400)
		return
	}

	// Build compatibility check results
	results := make([]bulkops.DryRunResult, 0, len(req.DeviceIDs))

	for _, deviceID := range req.DeviceIDs {
		result := bulkops.DryRunResult{
			DeviceID:   deviceID,
			Compatible: true,
		}

		// Get device info from database
		var ip, hostname, product, firmware, flavor, status string
		err := a.DB.QueryRow(`
			SELECT host(ip_address), COALESCE(hostname, ''), COALESCE(product, ''), 
			       COALESCE(firmware, ''), COALESCE(flavor, ''), COALESCE(status, 'unknown')
			FROM devices WHERE id = $1
		`, deviceID).Scan(&ip, &hostname, &product, &firmware, &flavor, &status)

		if err != nil {
			result.Compatible = false
			result.Issues = append(result.Issues, "Device not found")
			results = append(results, result)
			continue
		}

		result.DeviceIP = ip
		result.Hostname = hostname
		result.CurrentVer = firmware
		result.Flavor = flavor

		// Check device is online using stats store (real-time) or database status (fallback)
		online := false
		if stats := a.Stats.Get(ip); stats != nil {
			online = stats.Online
		} else {
			online = status == "online"
		}

		if !online {
			result.Compatible = false
			result.Issues = append(result.Issues, "Device is offline")
		}

		// For upgrades, check firmware compatibility
		if req.Operation == "upgrade" {
			targetFirmware := req.Parameters.FirmwareFile

			// Auto-select firmware if not specified
			if targetFirmware == "" {
				fw, err := a.Firmware.SelectFirmwareForDevice(flavor)
				if err != nil || fw == nil {
					result.Compatible = false
					result.Issues = append(result.Issues, "No compatible firmware found for flavor: "+flavor)
				} else {
					targetFirmware = fw.Name
					result.TargetVer = fw.Version
				}
			} else {
				// Verify specified firmware exists and is compatible
				fwInfo, err := a.Firmware.GetFirmwareInfo(targetFirmware)
				if err != nil {
					result.Compatible = false
					result.Issues = append(result.Issues, "Firmware file not found: "+targetFirmware)
				} else {
					result.TargetVer = fwInfo.Version
					// Check flavor compatibility
					if fwInfo.Flavor != "" && fwInfo.Flavor != flavor {
						result.Compatible = false
						result.Issues = append(result.Issues, fmt.Sprintf("Firmware flavor mismatch: %s vs %s", fwInfo.Flavor, flavor))
					}
				}
			}

			// Check if already at target version
			if result.TargetVer != "" && result.CurrentVer == result.TargetVer && !req.Parameters.Force {
				result.Warnings = append(result.Warnings, "Already at target version")
			}
		}

		// Check TLS certificate status if TOFU is enabled
		if a.TLS.Mode() == tlsutil.ModeTOFU && !a.TLS.IsPinned(int64(deviceID)) {
			result.Warnings = append(result.Warnings, "Certificate not pinned - will be pinned on first connection")
		}

		results = append(results, result)
	}

	// Summary stats
	compatible := 0
	incompatible := 0
	for _, r := range results {
		if r.Compatible {
			compatible++
		} else {
			incompatible++
		}
	}

	writeJSON(w, map[string]any{
		"results":      results,
		"compatible":   compatible,
		"incompatible": incompatible,
		"total":        len(results),
	})
}

// === Drilldown Lists API ===

// ListDrilldownLists returns all drilldown lists
func (a *API) ListDrilldownLists(w http.ResponseWriter, r *http.Request) {
	rows, err := a.DB.Query(`
		SELECT l.id, l.name, l.description, l.enabled, l.poll_interval, l.created_at,
		       (SELECT COUNT(*) FROM drilldown_hosts WHERE list_id = l.id) as host_count
		FROM drilldown_lists l
		ORDER BY l.name
	`)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()

	var lists []map[string]any
	for rows.Next() {
		var id, pollInterval, hostCount int
		var name string
		var description sql.NullString
		var enabled bool
		var createdAt time.Time
		if rows.Scan(&id, &name, &description, &enabled, &pollInterval, &createdAt, &hostCount) != nil {
			continue
		}
		lists = append(lists, map[string]any{
			"id":            id,
			"name":          name,
			"description":   description.String,
			"enabled":       enabled,
			"poll_interval": pollInterval,
			"host_count":    hostCount,
			"created_at":    createdAt,
		})
	}
	if lists == nil {
		lists = []map[string]any{}
	}
	writeJSON(w, lists)
}

// CreateDrilldownList creates a new drilldown list
func (a *API) CreateDrilldownList(w http.ResponseWriter, r *http.Request) {
	if !a.requireEdit(w, r) {
		return
	}
	var req struct {
		Name         string `json:"name"`
		Description  string `json:"description"`
		PollInterval int    `json:"poll_interval"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil || req.Name == "" {
		http.Error(w, "name required", 400)
		return
	}
	if req.PollInterval <= 0 {
		req.PollInterval = 30
	}

	var id int
	err := a.DB.QueryRow(`
		INSERT INTO drilldown_lists (name, description, poll_interval)
		VALUES ($1, $2, $3) RETURNING id
	`, req.Name, req.Description, req.PollInterval).Scan(&id)
	if err != nil {
		http.Error(w, "failed to create list: "+err.Error(), 500)
		return
	}

	writeJSON(w, map[string]any{"id": id, "name": req.Name})
}

// UpdateDrilldownList updates a drilldown list
func (a *API) UpdateDrilldownList(w http.ResponseWriter, r *http.Request) {
	if !a.requireEdit(w, r) {
		return
	}
	listID, _ := strconv.Atoi(chi.URLParam(r, "id"))

	var req struct {
		Name         string `json:"name"`
		Description  string `json:"description"`
		Enabled      *bool  `json:"enabled"`
		PollInterval int    `json:"poll_interval"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		http.Error(w, "invalid request", 400)
		return
	}

	// Build dynamic update
	updates := []string{"updated_at = NOW()"}
	args := []any{}
	argIdx := 1

	if req.Name != "" {
		updates = append(updates, fmt.Sprintf("name = $%d", argIdx))
		args = append(args, req.Name)
		argIdx++
	}
	if req.Description != "" {
		updates = append(updates, fmt.Sprintf("description = $%d", argIdx))
		args = append(args, req.Description)
		argIdx++
	}
	if req.Enabled != nil {
		updates = append(updates, fmt.Sprintf("enabled = $%d", argIdx))
		args = append(args, *req.Enabled)
		argIdx++
	}
	if req.PollInterval > 0 {
		updates = append(updates, fmt.Sprintf("poll_interval = $%d", argIdx))
		args = append(args, req.PollInterval)
		argIdx++
	}

	args = append(args, listID)
	query := fmt.Sprintf("UPDATE drilldown_lists SET %s WHERE id = $%d", strings.Join(updates, ", "), argIdx)
	if _, err := a.DB.Exec(query, args...); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	writeJSON(w, map[string]any{"success": true})
}

// DeleteDrilldownList deletes a drilldown list
func (a *API) DeleteDrilldownList(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	listID, _ := strconv.Atoi(chi.URLParam(r, "id"))

	if _, err := a.DB.Exec(`DELETE FROM drilldown_lists WHERE id = $1`, listID); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	writeJSON(w, map[string]any{"success": true})
}

// GetDrilldownHosts returns hosts for a drilldown list
func (a *API) GetDrilldownHosts(w http.ResponseWriter, r *http.Request) {
	listID, _ := strconv.Atoi(chi.URLParam(r, "id"))

	rows, err := a.DB.Query(`
		SELECT h.id, h.host, h.device_id, h.last_poll, h.last_error,
		       d.hostname, d.model, d.product
		FROM drilldown_hosts h
		LEFT JOIN devices d ON h.device_id = d.id
		WHERE h.list_id = $1
		ORDER BY h.host
	`, listID)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()

	var hosts []map[string]any
	for rows.Next() {
		var id int
		var host string
		var hostname, model, product, lastError sql.NullString
		var deviceID sql.NullInt64
		var lastPoll sql.NullTime
		if rows.Scan(&id, &host, &deviceID, &lastPoll, &lastError, &hostname, &model, &product) != nil {
			continue
		}
		h := map[string]any{
			"id":   id,
			"host": host,
		}
		if deviceID.Valid {
			h["device_id"] = deviceID.Int64
			h["hostname"] = hostname.String
			h["model"] = model.String
			h["product"] = product.String
		}
		if lastPoll.Valid {
			h["last_poll"] = lastPoll.Time
		}
		if lastError.Valid && lastError.String != "" {
			h["last_error"] = lastError.String
		}
		hosts = append(hosts, h)
	}
	if hosts == nil {
		hosts = []map[string]any{}
	}
	writeJSON(w, hosts)
}

// AddDrilldownHost adds a host to a drilldown list
func (a *API) AddDrilldownHost(w http.ResponseWriter, r *http.Request) {
	if !a.requireEdit(w, r) {
		return
	}
	listID, _ := strconv.Atoi(chi.URLParam(r, "id"))

	var req struct {
		Host string `json:"host"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil || req.Host == "" {
		http.Error(w, "host required", 400)
		return
	}

	// Try to find matching device
	var deviceID sql.NullInt64
	a.DB.QueryRow(`SELECT id FROM devices WHERE host(ip_address) = $1 OR hostname = $1`, req.Host).Scan(&deviceID)

	var id int
	err := a.DB.QueryRow(`
		INSERT INTO drilldown_hosts (list_id, host, device_id)
		VALUES ($1, $2, $3) RETURNING id
	`, listID, req.Host, deviceID).Scan(&id)
	if err != nil {
		http.Error(w, "failed to add host: "+err.Error(), 500)
		return
	}

	writeJSON(w, map[string]any{"id": id, "host": req.Host, "device_id": deviceID.Int64})
}

// RemoveDrilldownHost removes a host from a drilldown list
func (a *API) RemoveDrilldownHost(w http.ResponseWriter, r *http.Request) {
	if !a.requireEdit(w, r) {
		return
	}
	hostID, _ := strconv.Atoi(chi.URLParam(r, "hostId"))

	if _, err := a.DB.Exec(`DELETE FROM drilldown_hosts WHERE id = $1`, hostID); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	writeJSON(w, map[string]any{"success": true})
}

// --- Ultra Debug (per-device request/response ring buffers) ---

type ultraDebugToggleRequest struct {
	Enabled bool `json:"enabled"`
}

// SetUltraDebug enables or disables the per-device ultra debug ring buffer.
// Requires administrator permissions.
func (a *API) SetUltraDebug(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	if a.UltraDebug == nil {
		http.Error(w, "ultra debug not configured", http.StatusNotImplemented)
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid device id", http.StatusBadRequest)
		return
	}
	// Validate device exists
	var exists bool
	if err := a.DB.QueryRow("SELECT EXISTS(SELECT 1 FROM devices WHERE id=$1)", id).Scan(&exists); err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	if !exists {
		http.Error(w, "device not found", http.StatusNotFound)
		return
	}
	var req ultraDebugToggleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	if req.Enabled {
		a.UltraDebug.Enable(id)
	} else {
		a.UltraDebug.Disable(id)
	}
	if snap, ok := a.UltraDebug.Snapshot(id, 0); ok {
		writeJSON(w, snap)
		return
	}
	writeJSON(w, map[string]any{
		"device_id": id,
		"enabled":   false,
	})
}

// ListUltraDebug returns info about all currently-enabled ultra debug buffers.
func (a *API) ListUltraDebug(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	if a.UltraDebug == nil {
		writeJSON(w, map[string]any{
			"max_bytes": 0,
			"buffers":   []any{},
		})
		return
	}
	writeJSON(w, map[string]any{
		"max_bytes": a.UltraDebug.MaxBytes(),
		"buffers":   a.UltraDebug.List(),
	})
}

// GetUltraDebug returns the contents of a single device's ultra debug buffer.
// Optional query param: tail=<n> to return only the last n entries.
func (a *API) GetUltraDebug(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	if a.UltraDebug == nil {
		http.Error(w, "ultra debug not configured", http.StatusNotImplemented)
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid device id", http.StatusBadRequest)
		return
	}
	tail := 0
	if v := r.URL.Query().Get("tail"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			tail = n
		}
	}
	snap, ok := a.UltraDebug.Snapshot(id, tail)
	if !ok {
		http.Error(w, "ultra debug not enabled for device", http.StatusNotFound)
		return
	}
	writeJSON(w, snap)
}

// DownloadUltraDebug downloads the raw JSON array for a device's ultra debug buffer.
func (a *API) DownloadUltraDebug(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	if a.UltraDebug == nil {
		http.Error(w, "ultra debug not configured", http.StatusNotImplemented)
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid device id", http.StatusBadRequest)
		return
	}
	b, ok, err := a.UltraDebug.Download(id)
	if err != nil {
		http.Error(w, "failed to export ultra debug", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "ultra debug not enabled for device", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"wavecontrol-ultra-%d.json\"", id))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

// ClearUltraDebug empties a device's ultra debug buffer without disabling it.
// Requires administrator permissions.
func (a *API) ClearUltraDebug(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	if a.UltraDebug == nil {
		http.Error(w, "ultra debug not configured", http.StatusNotImplemented)
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid device id", http.StatusBadRequest)
		return
	}
	if ok := a.UltraDebug.Clear(id); !ok {
		http.Error(w, "ultra debug not enabled for device", http.StatusNotFound)
		return
	}
	if snap, ok := a.UltraDebug.Snapshot(id, 0); ok {
		writeJSON(w, snap)
		return
	}
	writeJSON(w, map[string]any{"device_id": id, "cleared": true})
}

// --- Ultra Debug (host-scoped; non-deviceID flows) ---

type ultraDebugHostToggleRequest struct {
	Host    string `json:"host"`
	Enabled bool   `json:"enabled"`
}

// SetUltraDebugHost enables or disables a host-scoped ultra debug ring buffer.
// This is intended for flows where a device ID is not available (e.g. drilldown polling).
// Requires administrator permissions.
func (a *API) SetUltraDebugHost(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	if a.UltraDebug == nil {
		http.Error(w, "ultra debug not configured", http.StatusNotImplemented)
		return
	}
	var req ultraDebugHostToggleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	req.Host = strings.TrimSpace(req.Host)
	if req.Host == "" {
		http.Error(w, "host required", http.StatusBadRequest)
		return
	}
	if req.Enabled {
		_ = a.UltraDebug.EnableHost(req.Host)
	} else {
		a.UltraDebug.DisableHost(req.Host)
	}
	if snap, ok := a.UltraDebug.SnapshotHost(req.Host, 0); ok {
		writeJSON(w, snap)
		return
	}
	writeJSON(w, map[string]any{
		"host":    req.Host,
		"enabled": false,
	})
}

// GetUltraDebugHost returns the contents of a single host's ultra debug buffer.
// Optional query param: tail=<n> to return only the last n entries.
func (a *API) GetUltraDebugHost(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	if a.UltraDebug == nil {
		http.Error(w, "ultra debug not configured", http.StatusNotImplemented)
		return
	}
	host := chi.URLParam(r, "host")
	if strings.TrimSpace(host) == "" {
		http.Error(w, "host required", http.StatusBadRequest)
		return
	}
	tail := 0
	if v := r.URL.Query().Get("tail"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			tail = n
		}
	}
	snap, ok := a.UltraDebug.SnapshotHost(host, tail)
	if !ok {
		http.Error(w, "ultra debug not enabled for host", http.StatusNotFound)
		return
	}
	writeJSON(w, snap)
}

// DownloadUltraDebugHost downloads the raw JSON array for a host's ultra debug buffer.
func (a *API) DownloadUltraDebugHost(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	if a.UltraDebug == nil {
		http.Error(w, "ultra debug not configured", http.StatusNotImplemented)
		return
	}
	host := chi.URLParam(r, "host")
	if strings.TrimSpace(host) == "" {
		http.Error(w, "host required", http.StatusBadRequest)
		return
	}
	b, ok, err := a.UltraDebug.DownloadHost(host)
	if err != nil {
		http.Error(w, "failed to export ultra debug", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "ultra debug not enabled for host", http.StatusNotFound)
		return
	}
	// Make a safe filename token
	safe := strings.NewReplacer(":", "_", "/", "_", "\\", "_", " ", "_").Replace(host)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"wavecontrol-ultra-host-%s.json\"", safe))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

// ClearUltraDebugHost empties a host's ultra debug buffer without disabling it.
// Requires administrator permissions.
func (a *API) ClearUltraDebugHost(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	if a.UltraDebug == nil {
		http.Error(w, "ultra debug not configured", http.StatusNotImplemented)
		return
	}
	host := chi.URLParam(r, "host")
	host = strings.TrimSpace(host)
	if host == "" {
		http.Error(w, "host required", http.StatusBadRequest)
		return
	}
	if ok := a.UltraDebug.ClearHost(host); !ok {
		http.Error(w, "ultra debug not enabled for host", http.StatusNotFound)
		return
	}
	if snap, ok := a.UltraDebug.SnapshotHost(host, 0); ok {
		writeJSON(w, snap)
		return
	}
	writeJSON(w, map[string]any{"host": host, "cleared": true})
}

// GetWaveParseCounters exposes in-process Wave parsing telemetry counters.
//
// This endpoint is intended for debugging / rollout verification. It does not
// persist data and will reset on process restart.
func (a *API) GetWaveParseCounters(w http.ResponseWriter, r *http.Request) {
	// Read-only endpoint; any authenticated user can access.
	if !a.requireAuth(w, r) {
		return
	}
	writeJSON(w, poller.GetWaveParseCountersSnapshot())
}
