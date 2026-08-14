package push

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// RegisterRequest is sent by Android/iOS clients after OS push registration.
type RegisterRequest struct {
	Platform  string `json:"platform"` // android, ios
	Provider  string `json:"provider"` // fcm, apns
	Token     string `json:"token"`    // FCM registration token or APNs device token
	Name      string `json:"device_name"`
	AppVer    string `json:"app_version"`
	OSVersion string `json:"os_version"`
}

// MobileDevice is a registered handset/tablet push target.
type MobileDevice struct {
	ID        string    `json:"id"`
	UserID    int64     `json:"user_id,omitempty"`
	Platform  string    `json:"platform"`
	Provider  string    `json:"provider"`
	Name      string    `json:"device_name,omitempty"`
	AppVer    string    `json:"app_version,omitempty"`
	OSVersion string    `json:"os_version,omitempty"`
	Enabled   bool      `json:"enabled"`
	LastSeen  time.Time `json:"last_seen_at"`
	LastError *string   `json:"last_error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Token     string    `json:"-"`
	TokenEnc  string    `json:"-"`
}

// Preferences controls which mobile alerts a user receives.
type Preferences struct {
	UserID         int64   `json:"user_id,omitempty"`
	PushEnabled    bool    `json:"push_enabled"`
	NotifyCritical bool    `json:"notify_critical"`
	NotifyWarning  bool    `json:"notify_warning"`
	NotifyInfo     bool    `json:"notify_info"`
	QuietStart     *string `json:"quiet_hours_start,omitempty"`
	QuietEnd       *string `json:"quiet_hours_end,omitempty"`
	Timezone       string  `json:"timezone"`
	UpdatedAt      string  `json:"updated_at,omitempty"`
}

// AlertNotification is the alert-manager to push-service handoff.
type AlertNotification struct {
	EventType   string    `json:"event_type"` // alert.created, alert.resolved
	AlertID     int       `json:"alert_id"`
	RuleID      int       `json:"rule_id"`
	RuleName    string    `json:"rule_name"`
	DeviceID    int       `json:"device_id"`
	DeviceIP    string    `json:"device_ip"`
	Hostname    string    `json:"hostname"`
	SiteID      int       `json:"site_id,omitempty"`
	Metric      string    `json:"metric"`
	Value       float64   `json:"value"`
	Threshold   float64   `json:"threshold"`
	Severity    string    `json:"severity"`
	Message     string    `json:"message"`
	TriggeredAt time.Time `json:"triggered_at"`
	ResolvedAt  time.Time `json:"resolved_at,omitempty"`
}

// Message is the provider-neutral push payload stored in the outbox.
type Message struct {
	EventType string            `json:"event_type"`
	Title     string            `json:"title"`
	Body      string            `json:"body"`
	Severity  string            `json:"severity"`
	Collapse  string            `json:"collapse_key,omitempty"`
	DeepLink  string            `json:"deep_link,omitempty"`
	Data      map[string]string `json:"data,omitempty"`
}

// ProviderResult reports delivery status for one push provider attempt.
type ProviderResult struct {
	ProviderMessageID string
	Terminal          bool
	Err               error
}

// Provider sends a Message to one mobile device.
type Provider interface {
	Send(ctx context.Context, device MobileDevice, msg Message) ProviderResult
}

// Service owns mobile-device registration, durable outbox enqueue, and dispatch.
type Service struct {
	db        *sql.DB
	gcm       cipher.AEAD
	providers map[string]Provider
}

// NewService creates a push service. encSecret should be stable across restarts; the JWT secret is acceptable.
func NewService(db *sql.DB, encSecret []byte) (*Service, error) {
	if len(encSecret) == 0 {
		return nil, errors.New("push encryption secret is empty")
	}
	key := sha256.Sum256(encSecret)
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	s := &Service{db: db, gcm: gcm}
	s.providers = map[string]Provider{
		"fcm":  NewFCMProvider(db),
		"apns": NewAPNSProvider(db),
	}
	return s, nil
}

func normalizePlatform(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "android", "ios":
		return s
	default:
		return ""
	}
}

func normalizeProvider(s string, platform string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		if platform == "ios" {
			return "apns"
		}
		return "fcm"
	}
	switch s {
	case "fcm", "apns":
		return s
	default:
		return ""
	}
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *Service) encryptToken(token string) (string, error) {
	nonce := make([]byte, s.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := s.gcm.Seal(nonce, nonce, []byte(token), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

func (s *Service) decryptToken(encoded string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	if len(raw) < s.gcm.NonceSize() {
		return "", errors.New("encrypted token is too short")
	}
	nonce := raw[:s.gcm.NonceSize()]
	ciphertext := raw[s.gcm.NonceSize():]
	plain, err := s.gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// RegisterDevice creates or updates one mobile device token for a user.
func (s *Service) RegisterDevice(ctx context.Context, userID int64, req RegisterRequest) (*MobileDevice, error) {
	platform := normalizePlatform(req.Platform)
	if platform == "" {
		return nil, errors.New("platform must be android or ios")
	}
	provider := normalizeProvider(req.Provider, platform)
	if provider == "" {
		return nil, errors.New("provider must be fcm or apns")
	}
	token := strings.TrimSpace(req.Token)
	if token == "" || len(token) > 4096 {
		return nil, errors.New("invalid push token")
	}
	tokHash := tokenHash(token)
	tokEnc, err := s.encryptToken(token)
	if err != nil {
		return nil, err
	}

	var dev MobileDevice
	var lastErr sql.NullString
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO mobile_devices (user_id, platform, provider, token_hash, token_encrypted,
			device_name, app_version, os_version, enabled, last_seen_at, last_error, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, true, NOW(), NULL, NOW())
		ON CONFLICT (user_id, platform, provider, token_hash) DO UPDATE SET
			token_encrypted = EXCLUDED.token_encrypted,
			device_name = EXCLUDED.device_name,
			app_version = EXCLUDED.app_version,
			os_version = EXCLUDED.os_version,
			enabled = true,
			last_seen_at = NOW(),
			last_error = NULL,
			updated_at = NOW()
		RETURNING id::text, user_id, platform, provider, device_name, app_version, os_version,
			enabled, last_seen_at, last_error, created_at, updated_at
	`, userID, platform, provider, tokHash, tokEnc, req.Name, req.AppVer, req.OSVersion).Scan(
		&dev.ID, &dev.UserID, &dev.Platform, &dev.Provider, &dev.Name, &dev.AppVer, &dev.OSVersion,
		&dev.Enabled, &dev.LastSeen, &lastErr, &dev.CreatedAt, &dev.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	if lastErr.Valid {
		dev.LastError = &lastErr.String
	}
	return &dev, nil
}

// UnregisterDevice disables a token for a user. If token is omitted all user's devices on platform/provider are disabled.
func (s *Service) UnregisterDevice(ctx context.Context, userID int64, platform, provider, token string) error {
	platform = normalizePlatform(platform)
	provider = normalizeProvider(provider, platform)
	if platform == "" {
		return errors.New("platform must be android or ios")
	}
	if provider == "" {
		return errors.New("provider must be fcm or apns")
	}
	if strings.TrimSpace(token) == "" {
		_, err := s.db.ExecContext(ctx, `
			UPDATE mobile_devices SET enabled = false, updated_at = NOW()
			WHERE user_id = $1 AND platform = $2 AND provider = $3
		`, userID, platform, provider)
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE mobile_devices SET enabled = false, updated_at = NOW()
		WHERE user_id = $1 AND platform = $2 AND provider = $3 AND token_hash = $4
	`, userID, platform, provider, tokenHash(token))
	return err
}

// ListDevices returns registered mobile devices for one user.
func (s *Service) ListDevices(ctx context.Context, userID int64) ([]MobileDevice, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id::text, user_id, platform, provider, device_name, app_version, os_version,
		       enabled, last_seen_at, last_error, created_at, updated_at
		FROM mobile_devices
		WHERE user_id = $1
		ORDER BY updated_at DESC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []MobileDevice
	for rows.Next() {
		var d MobileDevice
		var lastErr sql.NullString
		if err := rows.Scan(&d.ID, &d.UserID, &d.Platform, &d.Provider, &d.Name, &d.AppVer, &d.OSVersion, &d.Enabled, &d.LastSeen, &lastErr, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, err
		}
		if lastErr.Valid {
			d.LastError = &lastErr.String
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// GetPreferences returns a user's push preferences, creating defaults when missing.
func (s *Service) GetPreferences(ctx context.Context, userID int64) (Preferences, error) {
	var pref Preferences
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO mobile_push_preferences (user_id)
		VALUES ($1)
		ON CONFLICT (user_id) DO NOTHING
	`, userID); err != nil {
		return pref, err
	}

	var quietStart, quietEnd sql.NullString
	var updatedAt time.Time
	err := s.db.QueryRowContext(ctx, `
		SELECT user_id, push_enabled, notify_critical, notify_warning, notify_info,
		       quiet_hours_start::text, quiet_hours_end::text, timezone, updated_at
		FROM mobile_push_preferences WHERE user_id = $1
	`, userID).Scan(&pref.UserID, &pref.PushEnabled, &pref.NotifyCritical, &pref.NotifyWarning, &pref.NotifyInfo, &quietStart, &quietEnd, &pref.Timezone, &updatedAt)
	if err != nil {
		return pref, err
	}
	if quietStart.Valid {
		pref.QuietStart = &quietStart.String
	}
	if quietEnd.Valid {
		pref.QuietEnd = &quietEnd.String
	}
	pref.UpdatedAt = updatedAt.Format(time.RFC3339)
	return pref, nil
}

// UpdatePreferences replaces a user's push preferences.
func (s *Service) UpdatePreferences(ctx context.Context, userID int64, pref Preferences) (Preferences, error) {
	if pref.Timezone == "" {
		pref.Timezone = "UTC"
	}
	var quietStart, quietEnd any
	if pref.QuietStart != nil && strings.TrimSpace(*pref.QuietStart) != "" {
		quietStart = strings.TrimSpace(*pref.QuietStart)
	}
	if pref.QuietEnd != nil && strings.TrimSpace(*pref.QuietEnd) != "" {
		quietEnd = strings.TrimSpace(*pref.QuietEnd)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO mobile_push_preferences
			(user_id, push_enabled, notify_critical, notify_warning, notify_info,
			 quiet_hours_start, quiet_hours_end, timezone, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6::time, $7::time, $8, NOW())
		ON CONFLICT (user_id) DO UPDATE SET
			push_enabled = EXCLUDED.push_enabled,
			notify_critical = EXCLUDED.notify_critical,
			notify_warning = EXCLUDED.notify_warning,
			notify_info = EXCLUDED.notify_info,
			quiet_hours_start = EXCLUDED.quiet_hours_start,
			quiet_hours_end = EXCLUDED.quiet_hours_end,
			timezone = EXCLUDED.timezone,
			updated_at = NOW()
	`, userID, pref.PushEnabled, pref.NotifyCritical, pref.NotifyWarning, pref.NotifyInfo, quietStart, quietEnd, pref.Timezone)
	if err != nil {
		return pref, err
	}
	return s.GetPreferences(ctx, userID)
}

// EnqueueAlert queues a mobile push for all eligible registered mobile devices.
func (s *Service) EnqueueAlert(ctx context.Context, n AlertNotification) error {
	if strings.TrimSpace(n.EventType) == "" {
		n.EventType = "alert.created"
	}
	title := n.Hostname
	if title == "" {
		title = n.DeviceIP
	}
	if title == "" {
		title = fmt.Sprintf("Device %d", n.DeviceID)
	}
	if n.EventType == "alert.resolved" {
		title = "Resolved: " + title
	} else {
		title = strings.ToUpper(n.Severity) + ": " + title
	}
	body := n.Message
	if n.EventType == "alert.resolved" {
		body = fmt.Sprintf("%s recovered from %s", title, n.Metric)
	}
	collapse := fmt.Sprintf("device:%d:alert", n.DeviceID)
	deepLink := fmt.Sprintf("wavecontrol://alerts/%d", n.AlertID)
	data := map[string]string{
		"event_type": n.EventType,
		"alert_id":   fmt.Sprint(n.AlertID),
		"rule_id":    fmt.Sprint(n.RuleID),
		"device_id":  fmt.Sprint(n.DeviceID),
		"site_id":    fmt.Sprint(n.SiteID),
		"severity":   n.Severity,
		"metric":     n.Metric,
		"deep_link":  deepLink,
	}
	msg := Message{EventType: n.EventType, Title: title, Body: body, Severity: n.Severity, Collapse: collapse, DeepLink: deepLink, Data: data}
	payload, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	dedupeKey := fmt.Sprintf("%s:alert:%d:device:%d", n.EventType, n.AlertID, n.DeviceID)
	return s.enqueueForEligibleDevices(ctx, n.Severity, dedupeKey, collapse, payload, n.AlertID)
}

// EnqueueTest queues a test mobile push for the current user's enabled mobile devices.
func (s *Service) EnqueueTest(ctx context.Context, userID int64) error {
	msg := Message{
		EventType: "mobile.test",
		Title:     "waveControl test notification",
		Body:      "Mobile push delivery is configured for this device.",
		Severity:  "info",
		Collapse:  fmt.Sprintf("mobile-test:%d", userID),
		DeepLink:  "wavecontrol://alerts",
		Data: map[string]string{
			"event_type": "mobile.test",
			"severity":   "info",
		},
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT md.id::text
		FROM mobile_devices md
		WHERE md.user_id = $1 AND md.enabled = true
	`, userID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		_, err = s.db.ExecContext(ctx, `
			INSERT INTO notification_outbox
				(user_id, mobile_device_id, event_type, severity, dedupe_key, collapse_key, payload, status, next_attempt_at)
			VALUES ($1, $2::uuid, 'mobile.test', 'info', $3, $4, $5, 'pending', NOW())
			ON CONFLICT (dedupe_key, mobile_device_id) DO UPDATE SET
				payload = EXCLUDED.payload,
				status = 'pending',
				next_attempt_at = NOW(),
				error = NULL,
				updated_at = NOW()
		`, userID, id, fmt.Sprintf("mobile.test:%d:%d", userID, time.Now().UnixNano()), msg.Collapse, payload)
		if err != nil {
			return err
		}
	}
	return rows.Err()
}

func (s *Service) enqueueForEligibleDevices(ctx context.Context, severity, dedupeKey, collapse string, payload []byte, alertID int) error {
	severity = strings.ToLower(strings.TrimSpace(severity))
	if severity == "" {
		severity = "warning"
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT md.id::text, md.user_id
		FROM mobile_devices md
		JOIN users u ON u.id = md.user_id AND u.status = 1
		LEFT JOIN mobile_push_preferences pref ON pref.user_id = md.user_id
		WHERE md.enabled = true
		  AND COALESCE(pref.push_enabled, true) = true
		  AND (
		    ($1 = 'critical' AND COALESCE(pref.notify_critical, true) = true) OR
		    ($1 = 'warning'  AND COALESCE(pref.notify_warning, true) = true) OR
		    ($1 = 'info'     AND COALESCE(pref.notify_info, false) = true) OR
		    ($1 NOT IN ('critical', 'warning', 'info') AND COALESCE(pref.notify_warning, true) = true)
		  )
		  AND (
		    $1 = 'critical' OR
		    pref.quiet_hours_start IS NULL OR
		    pref.quiet_hours_end IS NULL OR
		    CASE
		      WHEN pref.quiet_hours_start < pref.quiet_hours_end THEN
		        ((NOW() AT TIME ZONE COALESCE(NULLIF(pref.timezone, ''), 'UTC'))::time NOT BETWEEN pref.quiet_hours_start AND pref.quiet_hours_end)
		      ELSE
		        NOT (
		          (NOW() AT TIME ZONE COALESCE(NULLIF(pref.timezone, ''), 'UTC'))::time >= pref.quiet_hours_start OR
		          (NOW() AT TIME ZONE COALESCE(NULLIF(pref.timezone, ''), 'UTC'))::time < pref.quiet_hours_end
		        )
		    END
		  )
	`, severity)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var deviceID string
		var userID int64
		if err := rows.Scan(&deviceID, &userID); err != nil {
			return err
		}
		_, err = s.db.ExecContext(ctx, `
			INSERT INTO notification_outbox
				(user_id, mobile_device_id, alert_id, event_type, severity, dedupe_key, collapse_key, payload, status, next_attempt_at)
			VALUES ($1, $2::uuid, NULLIF($7, 0), COALESCE(($3::jsonb)->>'event_type', 'alert'), $4, $5, $6, $3, 'pending', NOW())
			ON CONFLICT (dedupe_key, mobile_device_id) DO UPDATE SET
				payload = EXCLUDED.payload,
				severity = EXCLUDED.severity,
				collapse_key = EXCLUDED.collapse_key,
				status = 'pending',
				next_attempt_at = NOW(),
				error = NULL,
				updated_at = NOW()
		`, userID, deviceID, payload, severity, dedupeKey, collapse, alertID)
		if err != nil {
			return err
		}
	}
	return rows.Err()
}
