package alerting

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/smtp"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"
	"github.com/yellowman/wavecontrol/internal/stats"
)

// Metric names
const (
	MetricSignal60GHz     = "signal_60ghz"
	MetricSignal5GHz      = "signal_5ghz"
	MetricSignalLTU       = "signal_ltu"
	MetricCPU             = "cpu"
	MetricTemperature     = "temperature"
	MetricRAM             = "ram"
	MetricOfflineDuration = "offline_duration"
	MetricCapacity        = "capacity"
	MetricPeerCount       = "peer_count"
	MetricLinkScore       = "link_score"
)

// Operator names
const (
	OpLT  = "lt"
	OpLTE = "lte"
	OpGT  = "gt"
	OpGTE = "gte"
	OpEQ  = "eq"
	OpNE  = "ne"
)

// Severity levels
const (
	SeverityInfo     = "info"
	SeverityWarning  = "warning"
	SeverityCritical = "critical"
)

// dbExecIgnore executes a query, logs errors but doesn't return them (fire-and-forget)
func dbExecIgnore(db *sql.DB, query string, args ...any) {
	if _, err := db.Exec(query, args...); err != nil {
		log.Printf("DB exec error: %v", err)
	}
}

// Rule represents an alert rule
type Rule struct {
	ID               int       `json:"id"`
	Name             string    `json:"name"`
	Enabled          bool      `json:"enabled"`
	Scope            string    `json:"scope"`
	ScopeID          *int      `json:"scope_id,omitempty"`
	TargetRole       string    `json:"target_role"`
	RequireAlertable bool      `json:"require_alertable"`
	Metric           string    `json:"metric"`
	Operator         string    `json:"operator"`
	Threshold        float64   `json:"threshold"`
	DurationSeconds  int       `json:"duration_seconds"`
	NotifyChannels   []string  `json:"notify_channels"`
	NotifyEmails     []string  `json:"notify_emails,omitempty"`
	WebhookURL       string    `json:"webhook_url,omitempty"`
	CooldownSeconds  int       `json:"cooldown_seconds"`
	CreatedAt        time.Time `json:"created_at"`
	CreatedBy        int       `json:"created_by,omitempty"`
}

// Alert represents a triggered alert
type Alert struct {
	ID             int        `json:"id"`
	RuleID         *int       `json:"rule_id,omitempty"`
	DeviceID       int        `json:"device_id"`
	Metric         string     `json:"metric"`
	Value          float64    `json:"value"`
	Threshold      float64    `json:"threshold"`
	Message        string     `json:"message"`
	Severity       string     `json:"severity"`
	Status         string     `json:"status"`
	TriggeredAt    time.Time  `json:"triggered_at"`
	AcknowledgedAt *time.Time `json:"acknowledged_at,omitempty"`
	AcknowledgedBy *int       `json:"acknowledged_by,omitempty"`
	ResolvedAt     *time.Time `json:"resolved_at,omitempty"`
}

// AlertState tracks duration-based alert conditions
type AlertState struct {
	RuleID           int
	DeviceID         int
	FirstTriggeredAt time.Time
	LastValue        float64
	Notified         bool
}

type deviceAlertPolicy struct {
	DeviceID           int
	Role               string
	Alertable          bool
	AlertSilencedUntil *time.Time
}

// SMTPConfig for email notifications
type SMTPConfig struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
}

// MobileAlertNotification is the push-service handoff for native Android/iOS clients.
type MobileAlertNotification struct {
	EventType   string
	AlertID     int
	RuleID      int
	RuleName    string
	DeviceID    int
	DeviceIP    string
	Hostname    string
	SiteID      int
	Metric      string
	Value       float64
	Threshold   float64
	Severity    string
	Message     string
	TriggeredAt time.Time
	ResolvedAt  time.Time
}

// MobileNotifier is implemented by the durable mobile push outbox.
type MobileNotifier interface {
	EnqueueAlert(ctx context.Context, n MobileAlertNotification) error
}

// Manager handles alert rule evaluation and notifications
type Manager struct {
	db             *sql.DB
	statsStore     *stats.Store
	smtpConfig     *SMTPConfig
	httpClient     *http.Client
	mobileNotifier MobileNotifier

	mu     sync.RWMutex
	rules  []Rule
	states map[string]*AlertState // key: "rule_id:device_id"

	// Cooldown tracking
	lastNotified map[string]time.Time // key: "rule_id:device_id"
}

// NewManager creates a new alert manager
func NewManager(db *sql.DB, statsStore *stats.Store) *Manager {
	m := &Manager{
		db:           db,
		statsStore:   statsStore,
		states:       make(map[string]*AlertState),
		lastNotified: make(map[string]time.Time),
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
	m.loadRules()
	m.loadSMTPConfig()
	m.loadStates()
	return m
}

// SetMobileNotifier attaches a durable native-mobile push outbox.
func (m *Manager) SetMobileNotifier(n MobileNotifier) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mobileNotifier = n
}

func (m *Manager) loadRules() {
	rows, err := m.db.Query(`
		SELECT id, name, enabled, scope, scope_id, target_role, require_alertable, metric, operator, threshold,
		       duration_seconds, notify_channels, notify_emails, webhook_url, cooldown_seconds
		FROM alert_rules WHERE enabled = true
	`)
	if err != nil {
		log.Printf("Failed to load alert rules: %v", err)
		return
	}
	defer rows.Close()

	m.mu.Lock()
	defer m.mu.Unlock()

	m.rules = nil
	for rows.Next() {
		var r Rule
		var scopeID sql.NullInt64
		var channels, emails pq.StringArray
		var webhookURL sql.NullString

		err := rows.Scan(&r.ID, &r.Name, &r.Enabled, &r.Scope, &scopeID, &r.TargetRole, &r.RequireAlertable, &r.Metric, &r.Operator,
			&r.Threshold, &r.DurationSeconds, &channels, &emails, &webhookURL, &r.CooldownSeconds)
		if err != nil {
			continue
		}

		if scopeID.Valid {
			id := int(scopeID.Int64)
			r.ScopeID = &id
		}
		r.NotifyChannels = channels
		r.NotifyEmails = emails
		if webhookURL.Valid {
			r.WebhookURL = webhookURL.String
		}
		normalizeRuleDefaults(&r)

		m.rules = append(m.rules, r)
	}
	log.Printf("Loaded %d alert rules", len(m.rules))
}

func (m *Manager) loadSMTPConfig() {
	var host, username, password, from string
	var port int

	m.db.QueryRow(`SELECT value FROM settings WHERE key = 'smtp_host'`).Scan(&host)
	m.db.QueryRow(`SELECT value FROM settings WHERE key = 'smtp_port'`).Scan(&port)
	m.db.QueryRow(`SELECT value FROM settings WHERE key = 'smtp_username'`).Scan(&username)
	m.db.QueryRow(`SELECT value FROM settings WHERE key = 'smtp_password'`).Scan(&password)
	m.db.QueryRow(`SELECT value FROM settings WHERE key = 'smtp_from'`).Scan(&from)

	if host != "" {
		m.smtpConfig = &SMTPConfig{
			Host:     host,
			Port:     port,
			Username: username,
			Password: password,
			From:     from,
		}
	}
}

func (m *Manager) loadStates() {
	rows, err := m.db.Query(`
		SELECT rule_id, device_id, first_triggered_at, last_value, notified
		FROM alert_states
	`)
	if err != nil {
		return
	}
	defer rows.Close()

	m.mu.Lock()
	defer m.mu.Unlock()

	for rows.Next() {
		var s AlertState
		if rows.Scan(&s.RuleID, &s.DeviceID, &s.FirstTriggeredAt, &s.LastValue, &s.Notified) == nil {
			key := fmt.Sprintf("%d:%d", s.RuleID, s.DeviceID)
			m.states[key] = &s
		}
	}
}

// Evaluate checks all rules against current device stats
func (m *Manager) Evaluate(ctx context.Context) {
	m.mu.RLock()
	rules := make([]Rule, len(m.rules))
	copy(rules, m.rules)
	m.mu.RUnlock()

	if len(rules) == 0 {
		return
	}

	// Get all device stats
	allStats := m.statsStore.List()
	if len(allStats) == 0 {
		return
	}

	now := time.Now()
	policies := m.loadDeviceAlertPolicies(ctx)

	for _, rule := range rules {
		for _, ds := range allStats {
			// Check if rule applies to this device
			if !m.ruleApplies(rule, ds, policyForStats(ds, policies), now) {
				continue
			}

			// Get metric value
			value, ok := m.getMetricValue(rule.Metric, ds)
			if !ok {
				continue
			}

			// Evaluate condition
			triggered := m.evaluateCondition(rule.Operator, value, rule.Threshold)

			key := fmt.Sprintf("%d:%d", rule.ID, ds.DeviceID)

			if triggered {
				m.handleTriggered(key, rule, ds, value, now)
			} else {
				m.handleResolved(key, rule, ds)
			}
		}
	}
}

func (m *Manager) ruleApplies(rule Rule, ds *stats.DeviceStats, policy deviceAlertPolicy, now time.Time) bool {
	// Alerts are inventory events. In-memory peer/placeholder rows without a
	// database device id cannot be acknowledged, resolved, deduplicated, or
	// filtered by device policy, so never create alert state for device_id=0.
	if ds == nil || ds.DeviceID <= 0 {
		return false
	}
	if !scopeMatches(rule, ds) {
		return false
	}
	if !roleMatches(rule.TargetRole, policy.Role) {
		return false
	}
	if rule.RequireAlertable && !policy.Alertable {
		return false
	}
	if policy.AlertSilencedUntil != nil && now.Before(*policy.AlertSilencedUntil) {
		return false
	}
	return true
}

func scopeMatches(rule Rule, ds *stats.DeviceStats) bool {
	switch strings.ToLower(strings.TrimSpace(rule.Scope)) {
	case "all", "":
		return true
	case "site":
		return rule.ScopeID != nil && ds.SiteID == *rule.ScopeID
	case "device":
		return rule.ScopeID != nil && ds.DeviceID == *rule.ScopeID
	default:
		return true
	}
}

func roleMatches(targetRole, deviceRole string) bool {
	targetRole = normalizeTargetRole(targetRole)
	if targetRole == "all" {
		return true
	}
	return targetRole == normalizeTargetRole(deviceRole)
}

func normalizeTargetRole(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "ap", "sta":
		return strings.ToLower(strings.TrimSpace(role))
	default:
		return "all"
	}
}

func normalizeDeviceRole(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "ap", "sta":
		return strings.ToLower(strings.TrimSpace(role))
	default:
		return "ap"
	}
}

func normalizeRuleDefaults(rule *Rule) {
	if rule == nil {
		return
	}
	rule.TargetRole = normalizeTargetRole(rule.TargetRole)
}

func policyForStats(ds *stats.DeviceStats, policies map[int]deviceAlertPolicy) deviceAlertPolicy {
	if ds == nil {
		return deviceAlertPolicy{Alertable: true, Role: "ap"}
	}
	if p, ok := policies[ds.DeviceID]; ok {
		return p
	}
	role := "ap"
	if strings.TrimSpace(ds.ParentMAC) != "" || strings.TrimSpace(ds.ParentIP) != "" {
		role = "sta"
	}
	return deviceAlertPolicy{DeviceID: ds.DeviceID, Role: role, Alertable: true}
}

func (m *Manager) loadDeviceAlertPolicies(ctx context.Context) map[int]deviceAlertPolicy {
	rows, err := m.db.QueryContext(ctx, `
		SELECT id, COALESCE(role, ''), parent_id, COALESCE(managed, false),
		       COALESCE(alertable, true), alert_silenced_until
		FROM devices
	`)
	if err != nil {
		log.Printf("Failed to load device alert policies: %v", err)
		return map[int]deviceAlertPolicy{}
	}
	defer rows.Close()

	policies := make(map[int]deviceAlertPolicy)
	for rows.Next() {
		var id int
		var role string
		var parentID sql.NullInt64
		var managed bool
		var alertable bool
		var silenced sql.NullTime
		if err := rows.Scan(&id, &role, &parentID, &managed, &alertable, &silenced); err != nil {
			continue
		}
		role = strings.ToLower(strings.TrimSpace(role))
		if role != "ap" && role != "sta" {
			if parentID.Valid && !managed {
				role = "sta"
			} else {
				role = "ap"
			}
		}
		policy := deviceAlertPolicy{DeviceID: id, Role: role, Alertable: alertable}
		if silenced.Valid {
			t := silenced.Time
			policy.AlertSilencedUntil = &t
		}
		policies[id] = policy
	}
	return policies
}

func (m *Manager) getMetricValue(metric string, ds *stats.DeviceStats) (float64, bool) {
	switch metric {
	case MetricSignal60GHz:
		if ds.Wireless.Radio60GHz != nil {
			return float64(ds.Wireless.Radio60GHz.Signal), true
		}
	case MetricSignal5GHz:
		if ds.Wireless.Radio5GHz != nil {
			return float64(ds.Wireless.Radio5GHz.Signal), true
		}
	case MetricSignalLTU:
		if ds.Wireless.RadioLTU != nil {
			return float64(ds.Wireless.RadioLTU.Signal), true
		}
	case MetricCPU:
		return ds.CPUUsage, true
	case MetricTemperature:
		return ds.Temperature.CPU, true
	case MetricRAM:
		return ds.MemUsage, true
	case MetricOfflineDuration:
		// Only count offline duration when device is truly offline (unreachable).
		// Devices in an unknown state (reachable-but-unpollable) should not trigger offline-duration alerts.
		if ds.Status == stats.StatusOffline && !ds.LastSeen.IsZero() {
			return time.Since(ds.LastSeen).Seconds(), true
		}
		return 0, true
	case MetricPeerCount:
		return float64(ds.PeerCount), true
	case MetricCapacity:
		if ds.Wireless.Radio60GHz != nil && ds.Wireless.Radio60GHz.Capacity != nil {
			return float64(ds.Wireless.Radio60GHz.Capacity.Combined) / 1e6, true // Mbps
		}
	case MetricLinkScore:
		if ds.Wireless.LinkScore != nil {
			return float64(ds.Wireless.LinkScore.DL), true
		}
	}
	return 0, false
}

func (m *Manager) evaluateCondition(op string, value, threshold float64) bool {
	switch op {
	case OpLT:
		return value < threshold
	case OpLTE:
		return value <= threshold
	case OpGT:
		return value > threshold
	case OpGTE:
		return value >= threshold
	case OpEQ:
		return value == threshold
	case OpNE:
		return value != threshold
	}
	return false
}

func (m *Manager) handleTriggered(key string, rule Rule, ds *stats.DeviceStats, value float64, now time.Time) {
	m.mu.Lock()
	state, exists := m.states[key]
	if !exists {
		state = &AlertState{
			RuleID:           rule.ID,
			DeviceID:         ds.DeviceID,
			FirstTriggeredAt: now,
		}
		m.states[key] = state

		// Persist state
		dbExecIgnore(m.db, `
			INSERT INTO alert_states (rule_id, device_id, first_triggered_at, last_value)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (rule_id, device_id) DO UPDATE SET
				first_triggered_at = $3, last_value = $4, last_checked_at = NOW()
		`, rule.ID, ds.DeviceID, now, value)
	}
	state.LastValue = value
	m.mu.Unlock()

	// Check duration requirement
	duration := now.Sub(state.FirstTriggeredAt)
	if duration < time.Duration(rule.DurationSeconds)*time.Second {
		return
	}

	// Check cooldown
	lastNotify, hasLast := m.lastNotified[key]
	if hasLast && now.Sub(lastNotify) < time.Duration(rule.CooldownSeconds)*time.Second {
		return
	}

	// Check if already notified for this occurrence
	if state.Notified {
		return
	}

	// Create alert
	message := m.formatAlertMessage(rule, ds, value)
	severity := m.determineSeverity(rule, value)

	var alertID int
	err := m.db.QueryRow(`
		INSERT INTO alerts (rule_id, device_id, metric, value, threshold, message, severity)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id
	`, rule.ID, ds.DeviceID, rule.Metric, value, rule.Threshold, message, severity).Scan(&alertID)

	if err != nil {
		log.Printf("Failed to create alert: %v", err)
		return
	}

	// Send notifications
	alert := Alert{
		ID:          alertID,
		RuleID:      &rule.ID,
		DeviceID:    ds.DeviceID,
		Metric:      rule.Metric,
		Value:       value,
		Threshold:   rule.Threshold,
		Message:     message,
		Severity:    severity,
		Status:      "active",
		TriggeredAt: now,
	}

	go m.sendNotifications(rule, alert, ds)

	// Update state
	m.mu.Lock()
	state.Notified = true
	m.lastNotified[key] = now
	m.mu.Unlock()

	dbExecIgnore(m.db, `UPDATE alert_states SET notified = true WHERE rule_id = $1 AND device_id = $2`,
		rule.ID, ds.DeviceID)
	dbExecIgnore(m.db, `UPDATE alerts SET notified_at = NOW() WHERE id = $1`, alertID)
}

func (m *Manager) handleResolved(key string, rule Rule, ds *stats.DeviceStats) {
	m.mu.Lock()
	state, exists := m.states[key]
	if !exists {
		m.mu.Unlock()
		return
	}
	delete(m.states, key)
	m.mu.Unlock()

	if !state.Notified {
		// Never notified, just clean up
		dbExecIgnore(m.db, `DELETE FROM alert_states WHERE rule_id = $1 AND device_id = $2`,
			rule.ID, ds.DeviceID)
		return
	}

	rows, err := m.db.Query(`
		UPDATE alerts SET status = 'resolved', resolved_at = NOW()
		WHERE rule_id = $1 AND device_id = $2 AND status = 'active'
		RETURNING id, rule_id, device_id, metric, value, threshold, message, severity, status,
		          triggered_at, acknowledged_at, acknowledged_by, resolved_at
	`, rule.ID, ds.DeviceID)
	if err != nil {
		log.Printf("Failed to resolve alerts: %v", err)
	} else {
		defer rows.Close()
		for rows.Next() {
			alert, scanErr := scanAlert(rows)
			if scanErr != nil {
				log.Printf("Failed to scan resolved alert: %v", scanErr)
				continue
			}
			m.sendMobile("alert.resolved", rule, alert, ds)
		}
	}

	dbExecIgnore(m.db, `DELETE FROM alert_states WHERE rule_id = $1 AND device_id = $2`,
		rule.ID, ds.DeviceID)
}

func (m *Manager) formatAlertMessage(rule Rule, ds *stats.DeviceStats, value float64) string {
	hostname := ds.Hostname
	if hostname == "" {
		hostname = ds.IP
	}

	opStr := map[string]string{
		OpLT: "below", OpLTE: "at or below",
		OpGT: "above", OpGTE: "at or above",
		OpEQ: "equal to", OpNE: "not equal to",
	}[rule.Operator]

	return fmt.Sprintf("%s: %s is %s threshold (%.2f %s %.2f) on %s",
		rule.Name, rule.Metric, opStr, value, rule.Operator, rule.Threshold, hostname)
}

func (m *Manager) determineSeverity(rule Rule, value float64) string {
	// Simple severity based on how far over threshold
	switch rule.Metric {
	case MetricSignal60GHz, MetricSignal5GHz, MetricSignalLTU:
		if value < -80 {
			return SeverityCritical
		} else if value < -70 {
			return SeverityWarning
		}
	case MetricCPU, MetricRAM:
		if value > 95 {
			return SeverityCritical
		} else if value > 85 {
			return SeverityWarning
		}
	case MetricTemperature:
		if value > 85 {
			return SeverityCritical
		} else if value > 75 {
			return SeverityWarning
		}
	case MetricOfflineDuration:
		if value > 3600 { // 1 hour
			return SeverityCritical
		} else if value > 300 { // 5 min
			return SeverityWarning
		}
	}
	return SeverityWarning
}

func (m *Manager) sendNotifications(rule Rule, alert Alert, ds *stats.DeviceStats) {
	for _, channel := range rule.NotifyChannels {
		switch channel {
		case "email":
			if err := m.sendEmail(rule, alert, ds); err != nil {
				log.Printf("Email notification failed: %v", err)
				dbExecIgnore(m.db, `UPDATE alerts SET notify_error = $1 WHERE id = $2`, err.Error(), alert.ID)
			}
		case "webhook":
			if err := m.sendWebhook(rule, alert, ds); err != nil {
				log.Printf("Webhook notification failed: %v", err)
				dbExecIgnore(m.db, `UPDATE alerts SET notify_error = $1 WHERE id = $2`, err.Error(), alert.ID)
			}
		case "zabbix":
			// Zabbix integration via zabbix_sender or HTTP API
			log.Printf("Zabbix alert: %s", alert.Message)
		case "mobile":
			m.sendMobile("alert.created", rule, alert, ds)
		}
	}
}

func (m *Manager) sendMobile(eventType string, rule Rule, alert Alert, ds *stats.DeviceStats) {
	m.mu.RLock()
	notifier := m.mobileNotifier
	m.mu.RUnlock()
	if notifier == nil || !ruleHasChannel(rule, "mobile") {
		return
	}
	var ruleID int
	if alert.RuleID != nil {
		ruleID = *alert.RuleID
	}
	var resolvedAt time.Time
	if alert.ResolvedAt != nil {
		resolvedAt = *alert.ResolvedAt
	}
	n := MobileAlertNotification{
		EventType:   eventType,
		AlertID:     alert.ID,
		RuleID:      ruleID,
		RuleName:    rule.Name,
		DeviceID:    ds.DeviceID,
		DeviceIP:    ds.IP,
		Hostname:    ds.Hostname,
		SiteID:      ds.SiteID,
		Metric:      alert.Metric,
		Value:       alert.Value,
		Threshold:   alert.Threshold,
		Severity:    alert.Severity,
		Message:     alert.Message,
		TriggeredAt: alert.TriggeredAt,
		ResolvedAt:  resolvedAt,
	}
	if err := notifier.EnqueueAlert(context.Background(), n); err != nil {
		log.Printf("Mobile push enqueue failed: %v", err)
		dbExecIgnore(m.db, `UPDATE alerts SET notify_error = $1 WHERE id = $2`, err.Error(), alert.ID)
	}
}

func ruleHasChannel(rule Rule, want string) bool {
	for _, ch := range rule.NotifyChannels {
		if strings.EqualFold(strings.TrimSpace(ch), want) {
			return true
		}
	}
	return false
}

func (m *Manager) sendEmail(rule Rule, alert Alert, ds *stats.DeviceStats) error {
	if m.smtpConfig == nil || len(rule.NotifyEmails) == 0 {
		return fmt.Errorf("email not configured")
	}

	subject := fmt.Sprintf("[%s] %s", alert.Severity, rule.Name)
	body := fmt.Sprintf(`Alert: %s
	
Device: %s (%s)
Metric: %s
Value: %.2f
Threshold: %.2f
Time: %s

Message: %s
`,
		rule.Name,
		ds.Hostname, ds.IP,
		alert.Metric,
		alert.Value,
		alert.Threshold,
		alert.TriggeredAt.Format(time.RFC3339),
		alert.Message,
	)

	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\n\r\n%s",
		m.smtpConfig.From,
		rule.NotifyEmails[0],
		subject,
		body,
	)

	addr := fmt.Sprintf("%s:%d", m.smtpConfig.Host, m.smtpConfig.Port)

	var auth smtp.Auth
	if m.smtpConfig.Username != "" {
		auth = smtp.PlainAuth("", m.smtpConfig.Username, m.smtpConfig.Password, m.smtpConfig.Host)
	}

	return smtp.SendMail(addr, auth, m.smtpConfig.From, rule.NotifyEmails, []byte(msg))
}

func (m *Manager) sendWebhook(rule Rule, alert Alert, ds *stats.DeviceStats) error {
	if rule.WebhookURL == "" {
		return nil
	}

	payload := map[string]interface{}{
		"alert_id":     alert.ID,
		"rule_name":    rule.Name,
		"device_id":    ds.DeviceID,
		"device_ip":    ds.IP,
		"hostname":     ds.Hostname,
		"metric":       alert.Metric,
		"value":        alert.Value,
		"threshold":    alert.Threshold,
		"severity":     alert.Severity,
		"message":      alert.Message,
		"triggered_at": alert.TriggeredAt.Format(time.RFC3339),
	}

	body, _ := json.Marshal(payload)

	resp, err := m.httpClient.Post(rule.WebhookURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("webhook returned %d", resp.StatusCode)
	}

	return nil
}

// Reload refreshes rules from database
func (m *Manager) Reload() {
	m.loadRules()
	m.loadSMTPConfig()
}

// === CRUD operations ===

func (m *Manager) CreateRule(rule *Rule) (int, error) {
	normalizeRuleDefaults(rule)
	var id int
	err := m.db.QueryRow(`
		INSERT INTO alert_rules (name, enabled, scope, scope_id, target_role, require_alertable, metric, operator, threshold,
			duration_seconds, notify_channels, notify_emails, webhook_url, cooldown_seconds, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		RETURNING id
	`, rule.Name, rule.Enabled, rule.Scope, rule.ScopeID, rule.TargetRole, rule.RequireAlertable, rule.Metric, rule.Operator, rule.Threshold,
		rule.DurationSeconds, pq.Array(rule.NotifyChannels), pq.Array(rule.NotifyEmails),
		rule.WebhookURL, rule.CooldownSeconds, rule.CreatedBy).Scan(&id)

	if err == nil {
		m.Reload()
	}
	return id, err
}

func (m *Manager) UpdateRule(id int, rule *Rule) error {
	normalizeRuleDefaults(rule)
	_, err := m.db.Exec(`
		UPDATE alert_rules SET
			name = $1, enabled = $2, scope = $3, scope_id = $4, target_role = $5, require_alertable = $6, metric = $7, operator = $8,
			threshold = $9, duration_seconds = $10, notify_channels = $11, notify_emails = $12,
			webhook_url = $13, cooldown_seconds = $14, updated_at = NOW()
		WHERE id = $15
	`, rule.Name, rule.Enabled, rule.Scope, rule.ScopeID, rule.TargetRole, rule.RequireAlertable, rule.Metric, rule.Operator, rule.Threshold,
		rule.DurationSeconds, pq.Array(rule.NotifyChannels), pq.Array(rule.NotifyEmails),
		rule.WebhookURL, rule.CooldownSeconds, id)

	if err == nil {
		m.Reload()
	}
	return err
}

func (m *Manager) DeleteRule(id int) error {
	_, err := m.db.Exec(`DELETE FROM alert_rules WHERE id = $1`, id)
	if err == nil {
		m.Reload()
	}
	return err
}

func (m *Manager) ListRules() ([]Rule, error) {
	rows, err := m.db.Query(`
		SELECT id, name, enabled, scope, scope_id, target_role, require_alertable, metric, operator, threshold,
		       duration_seconds, notify_channels, notify_emails, webhook_url, cooldown_seconds, created_at
		FROM alert_rules ORDER BY name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rules []Rule
	for rows.Next() {
		var r Rule
		var scopeID sql.NullInt64
		var channels, emails pq.StringArray
		var webhookURL sql.NullString

		err := rows.Scan(&r.ID, &r.Name, &r.Enabled, &r.Scope, &scopeID, &r.TargetRole, &r.RequireAlertable, &r.Metric, &r.Operator,
			&r.Threshold, &r.DurationSeconds, &channels, &emails, &webhookURL, &r.CooldownSeconds, &r.CreatedAt)
		if err != nil {
			continue
		}

		if scopeID.Valid {
			id := int(scopeID.Int64)
			r.ScopeID = &id
		}
		r.NotifyChannels = channels
		r.NotifyEmails = emails
		if webhookURL.Valid {
			r.WebhookURL = webhookURL.String
		}
		normalizeRuleDefaults(&r)

		rules = append(rules, r)
	}
	return rules, nil
}

type alertScanner interface {
	Scan(dest ...any) error
}

func scanAlert(row alertScanner) (Alert, error) {
	var a Alert
	var ruleID, ackBy sql.NullInt64
	var ackAt, resAt sql.NullTime
	var value, threshold sql.NullFloat64
	err := row.Scan(&a.ID, &ruleID, &a.DeviceID, &a.Metric, &value, &threshold, &a.Message, &a.Severity, &a.Status,
		&a.TriggeredAt, &ackAt, &ackBy, &resAt)
	if err != nil {
		return a, err
	}
	if ruleID.Valid {
		v := int(ruleID.Int64)
		a.RuleID = &v
	}
	if value.Valid {
		a.Value = value.Float64
	}
	if threshold.Valid {
		a.Threshold = threshold.Float64
	}
	if ackAt.Valid {
		a.AcknowledgedAt = &ackAt.Time
	}
	if ackBy.Valid {
		v := int(ackBy.Int64)
		a.AcknowledgedBy = &v
	}
	if resAt.Valid {
		a.ResolvedAt = &resAt.Time
	}
	return a, nil
}

func (m *Manager) ListAlerts(status string, limit int) ([]Alert, error) {
	query := `
		SELECT id, rule_id, device_id, metric, value, threshold, message, severity, status,
		       triggered_at, acknowledged_at, acknowledged_by, resolved_at
		FROM alerts
	`
	args := []interface{}{}

	if status != "" {
		query += " WHERE status = $1"
		args = append(args, status)
	}
	query += " ORDER BY triggered_at DESC LIMIT " + fmt.Sprintf("%d", limit)

	rows, err := m.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var alerts []Alert
	for rows.Next() {
		a, err := scanAlert(rows)
		if err != nil {
			continue
		}
		alerts = append(alerts, a)
	}
	return alerts, rows.Err()
}

// ListAlertsSince returns alerts newer than an integer alert id for mobile reconciliation.
func (m *Manager) ListAlertsSince(status string, sinceID int, limit int) ([]Alert, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := `
		SELECT id, rule_id, device_id, metric, value, threshold, message, severity, status,
		       triggered_at, acknowledged_at, acknowledged_by, resolved_at
		FROM alerts
		WHERE id > $1
	`
	args := []interface{}{sinceID}
	if status != "" {
		query += ` AND status = $2`
		args = append(args, status)
	}
	query += ` ORDER BY id ASC LIMIT $` + fmt.Sprint(len(args)+1)
	args = append(args, limit)
	rows, err := m.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var alerts []Alert
	for rows.Next() {
		a, err := scanAlert(rows)
		if err != nil {
			continue
		}
		alerts = append(alerts, a)
	}
	return alerts, rows.Err()
}

func (m *Manager) AcknowledgeAlert(alertID, userID int) error {
	_, err := m.db.Exec(`
		UPDATE alerts SET status = 'acknowledged', acknowledged_at = NOW(), acknowledged_by = $1
		WHERE id = $2
	`, userID, alertID)
	return err
}

func (m *Manager) ResolveAlert(alertID int) error {
	_, err := m.db.Exec(`
		UPDATE alerts SET status = 'resolved', resolved_at = NOW()
		WHERE id = $1
	`, alertID)
	return err
}

// GetActiveAlertCount returns count of active alerts
func (m *Manager) GetActiveAlertCount() int {
	var count int
	m.db.QueryRow(`SELECT COUNT(*) FROM alerts WHERE status = 'active'`).Scan(&count)
	return count
}
