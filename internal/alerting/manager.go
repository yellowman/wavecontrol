package alerting

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"
	"github.com/yellowman/wavecontrol/internal/secrets"
	"github.com/yellowman/wavecontrol/internal/stats"
	"github.com/yellowman/wavecontrol/internal/sysmonalerter"
)

const (
	MetricSignal60GHz     = "signal_60ghz"
	MetricSignal5GHz      = "signal_5ghz"
	MetricSignal6GHz      = "signal_6ghz"
	MetricSignalLTU       = "signal_ltu"
	MetricCPU             = "cpu"
	MetricTemperature     = "temperature"
	MetricRAM             = "ram"
	MetricOfflineDuration = "offline_duration"
	MetricCapacity        = "capacity"
	MetricPeerCount       = "peer_count"
	MetricLinkScore       = "link_score"
	MetricInterference    = "interference"
	MetricChainImbalance  = "chain_imbalance"
	MetricGPSSync         = "gps_sync"

	OpLT  = "lt"
	OpLTE = "lte"
	OpGT  = "gt"
	OpGTE = "gte"
	OpEQ  = "eq"
	OpNE  = "ne"

	SeverityAuto     = "auto"
	SeverityInfo     = "info"
	SeverityWarning  = "warning"
	SeverityCritical = "critical"
)

var ErrNotFound = errors.New("not found")

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
	Severity         string    `json:"severity"`
	NotifyChannels   []string  `json:"notify_channels"`
	NotifyEmails     []string  `json:"notify_emails,omitempty"`
	WebhookURL       string    `json:"webhook_url,omitempty"`
	NotifyRecovery   bool      `json:"notify_recovery"`
	CooldownSeconds  int       `json:"cooldown_seconds"`
	CreatedAt        time.Time `json:"created_at"`
	CreatedBy        int       `json:"created_by,omitempty"`
}

type Alert struct {
	ID                     int                    `json:"id"`
	RuleID                 *int                   `json:"rule_id,omitempty"`
	DeviceID               int                    `json:"device_id"`
	Metric                 string                 `json:"metric"`
	Value                  float64                `json:"value"`
	Threshold              float64                `json:"threshold"`
	Message                string                 `json:"message"`
	Severity               string                 `json:"severity"`
	Status                 string                 `json:"status"`
	TriggeredAt            time.Time              `json:"triggered_at"`
	AcknowledgedAt         *time.Time             `json:"acknowledged_at,omitempty"`
	AcknowledgedBy         *int                   `json:"acknowledged_by,omitempty"`
	ResolvedAt             *time.Time             `json:"resolved_at,omitempty"`
	NotifiedAt             *time.Time             `json:"notified_at,omitempty"`
	NotifyError            string                 `json:"notify_error,omitempty"`
	RecoveryNotifiedAt     *time.Time             `json:"recovery_notified_at,omitempty"`
	RecoveryNotifyError    string                 `json:"recovery_notify_error,omitempty"`
	NotifyChannels         []string               `json:"notify_channels"`
	DeliveryStatus         string                 `json:"delivery_status"`
	RecoveryChannels       []string               `json:"recovery_channels"`
	RecoveryDeliveryStatus string                 `json:"recovery_delivery_status"`
	Deliveries             []NotificationDelivery `json:"deliveries"`
}

type AlertState struct {
	RuleID           int
	DeviceID         int
	FirstTriggeredAt time.Time
	LastValue        float64
	Notified         bool // an alert row/outbox has been durably created for this occurrence
}

type deviceAlertPolicy struct {
	DeviceID           int
	Role               string
	Alertable          bool
	AlertSilencedUntil *time.Time
	CreatedAt          time.Time
}

type SMTPConfig struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
}

type Manager struct {
	db         *sql.DB
	statsStore *stats.Store
	secrets    *secrets.Manager

	mu             sync.RWMutex
	rules          []Rule
	states         map[string]*AlertState
	lastNotified   map[string]time.Time
	smtpConfig     *SMTPConfig
	sysmonClient   *sysmonalerter.Client
	notificationCh chan struct{}
}

func NewManager(db *sql.DB, statsStore *stats.Store, secretStore *secrets.Manager) (*Manager, error) {
	sysmonClient, err := sysmonalerter.NewClient(sysmonalerter.Config{})
	if err != nil {
		return nil, err
	}
	m := &Manager{
		db:             db,
		statsStore:     statsStore,
		secrets:        secretStore,
		states:         make(map[string]*AlertState),
		lastNotified:   make(map[string]time.Time),
		notificationCh: make(chan struct{}, generalNotificationWorkers+2),
		sysmonClient:   sysmonClient,
	}
	if err := m.Reload(); err != nil {
		return nil, err
	}
	return m, nil
}

const generalNotificationWorkers = 3

func (m *Manager) Start(ctx context.Context) {
	for i := 0; i < generalNotificationWorkers; i++ {
		go m.runNotificationWorker(ctx, false)
	}
	// sysmon-web uses one ordered, long-lived request/reply stream. Keep its
	// indefinite retry path isolated so an unavailable sysmon server cannot
	// delay email, webhook, or Zabbix notifications.
	go m.runNotificationWorker(ctx, true)
	go m.sysmonClient.Run(ctx)
}

func (m *Manager) loadRules() ([]Rule, error) {
	rows, err := m.db.Query(`
		SELECT id, name, enabled, scope, scope_id, target_role, require_alertable, metric, operator, threshold,
		       duration_seconds, severity, notify_channels, notify_emails, webhook_url, notify_recovery, cooldown_seconds
		FROM alert_rules WHERE enabled = true
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
		if err := rows.Scan(&r.ID, &r.Name, &r.Enabled, &r.Scope, &scopeID, &r.TargetRole, &r.RequireAlertable,
			&r.Metric, &r.Operator, &r.Threshold, &r.DurationSeconds, &r.Severity, &channels, &emails, &webhookURL,
			&r.NotifyRecovery, &r.CooldownSeconds); err != nil {
			return nil, err
		}
		if scopeID.Valid {
			id := int(scopeID.Int64)
			r.ScopeID = &id
		}
		r.NotifyChannels = append([]string(nil), channels...)
		r.NotifyEmails = append([]string(nil), emails...)
		if webhookURL.Valid {
			r.WebhookURL = webhookURL.String
		}
		normalizeRule(&r)
		if err := ValidateRule(&r); err != nil {
			return nil, fmt.Errorf("enabled alert rule %d is invalid: %w", r.ID, err)
		}
		rules = append(rules, r)
	}
	return rules, rows.Err()
}

func (m *Manager) loadStates() (map[string]*AlertState, error) {
	rows, err := m.db.Query(`
		SELECT s.rule_id, s.device_id, s.first_triggered_at, s.last_value, s.notified
		FROM alert_states s
		JOIN alert_rules r ON r.id = s.rule_id
		WHERE r.enabled = true
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	states := make(map[string]*AlertState)
	for rows.Next() {
		var s AlertState
		if err := rows.Scan(&s.RuleID, &s.DeviceID, &s.FirstTriggeredAt, &s.LastValue, &s.Notified); err != nil {
			return nil, err
		}
		states[stateKey(s.RuleID, s.DeviceID)] = &s
	}
	return states, rows.Err()
}

func (m *Manager) loadLastNotified() (map[string]time.Time, error) {
	rows, err := m.db.Query(`
		SELECT a.rule_id, a.device_id, MAX(a.triggered_at)
		FROM alerts a
		JOIN alert_rules r ON r.id=a.rule_id
		WHERE a.rule_id IS NOT NULL AND a.triggered_at >= r.updated_at
		GROUP BY a.rule_id, a.device_id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]time.Time)
	for rows.Next() {
		var ruleID, deviceID int
		var t time.Time
		if err := rows.Scan(&ruleID, &deviceID, &t); err != nil {
			return nil, err
		}
		out[stateKey(ruleID, deviceID)] = t
	}
	return out, rows.Err()
}

func (m *Manager) loadSMTPConfig() (*SMTPConfig, error) {
	values := map[string]string{}
	for _, key := range []string{"smtp_host", "smtp_port", "smtp_username", "smtp_password", "smtp_from"} {
		var value string
		err := m.db.QueryRow(`SELECT value FROM settings WHERE key = $1`, key).Scan(&value)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if key == "smtp_password" && value != "" && m.secrets != nil {
			plain, err := m.secrets.Decrypt(value)
			if err != nil {
				return nil, fmt.Errorf("decrypt smtp_password: %w", err)
			}
			value = plain
		}
		values[key] = value
	}
	if strings.TrimSpace(values["smtp_host"]) == "" {
		return nil, nil
	}
	port := 25
	if _, err := fmt.Sscanf(values["smtp_port"], "%d", &port); err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("invalid smtp_port %q", values["smtp_port"])
	}
	return &SMTPConfig{
		Host: strings.TrimSpace(values["smtp_host"]), Port: port,
		Username: values["smtp_username"], Password: values["smtp_password"], From: strings.TrimSpace(values["smtp_from"]),
	}, nil
}

func (m *Manager) loadSysmonConfig() (sysmonalerter.Config, error) {
	keys := []string{
		"sysmon_alerter_enabled", "sysmon_alerter_host", "sysmon_alerter_port",
		"sysmon_alerter_name", "sysmon_alerter_token", "sysmon_alerter_application",
		"sysmon_alerter_ca_pem",
	}
	values := make(map[string]string, len(keys))
	for _, key := range keys {
		var value string
		err := m.db.QueryRow(`SELECT value FROM settings WHERE key=$1`, key).Scan(&value)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return sysmonalerter.Config{}, err
		}
		if key == "sysmon_alerter_token" && value != "" && m.secrets != nil {
			plain, err := m.secrets.Decrypt(value)
			if err != nil {
				return sysmonalerter.Config{}, fmt.Errorf("decrypt sysmon_alerter_token: %w", err)
			}
			value = plain
		}
		values[key] = value
	}
	port := sysmonalerter.DefaultPort
	if raw := strings.TrimSpace(values["sysmon_alerter_port"]); raw != "" {
		if _, err := fmt.Sscanf(raw, "%d", &port); err != nil || port < 1 || port > 65535 {
			return sysmonalerter.Config{}, fmt.Errorf("invalid sysmon_alerter_port %q", raw)
		}
	}
	return sysmonalerter.Config{
		Enabled: strings.EqualFold(strings.TrimSpace(values["sysmon_alerter_enabled"]), "true"),
		Host:    values["sysmon_alerter_host"], Port: port,
		Name: values["sysmon_alerter_name"], Token: values["sysmon_alerter_token"],
		Application: values["sysmon_alerter_application"], CAPEM: values["sysmon_alerter_ca_pem"],
	}, nil
}

func (m *Manager) Reload() error {
	rules, err := m.loadRules()
	if err != nil {
		return fmt.Errorf("load rules: %w", err)
	}
	states, err := m.loadStates()
	if err != nil {
		return fmt.Errorf("load states: %w", err)
	}
	last, err := m.loadLastNotified()
	if err != nil {
		return fmt.Errorf("load cooldown state: %w", err)
	}
	smtpCfg, err := m.loadSMTPConfig()
	if err != nil {
		return fmt.Errorf("load SMTP config: %w", err)
	}
	sysmonCfg, err := m.loadSysmonConfig()
	if err != nil {
		return fmt.Errorf("load sysmon-web alerter config: %w", err)
	}
	if err := m.sysmonClient.UpdateConfig(sysmonCfg); err != nil {
		return fmt.Errorf("load sysmon-web alerter config: %w", err)
	}
	m.mu.Lock()
	m.rules = rules
	m.states = states
	m.lastNotified = last
	m.smtpConfig = smtpCfg
	m.mu.Unlock()
	log.Printf("Loaded %d alert rules", len(rules))
	return nil
}

func (m *Manager) Evaluate(ctx context.Context) {
	m.mu.RLock()
	rules := append([]Rule(nil), m.rules...)
	m.mu.RUnlock()
	if len(rules) == 0 {
		return
	}

	policies, err := m.loadDeviceAlertPolicies(ctx)
	if err != nil {
		// Alert policy is an authorization/silence boundary. Fail closed rather
		// than treating a database outage as permission to page every device.
		log.Printf("alert evaluation skipped: load device policies: %v", err)
		return
	}

	allStats := m.statsStore.List()
	now := time.Now()
	activeRuleIDs := make(map[int]struct{}, len(rules))
	rulesByID := make(map[int]Rule, len(rules))
	for _, rule := range rules {
		activeRuleIDs[rule.ID] = struct{}{}
		rulesByID[rule.ID] = rule
		for _, ds := range allStats {
			if ds == nil || ds.DeviceID <= 0 {
				continue
			}
			key := stateKey(rule.ID, ds.DeviceID)
			policy, ok := policies[ds.DeviceID]
			if !ok || !m.ruleApplies(rule, ds, policy, now) {
				m.handleResolved(ctx, key, rule, ds.DeviceID, "device no longer matches the rule scope or alert policy")
				continue
			}
			value, ok := m.getMetricValue(rule.Metric, ds, policy, now)
			if !ok {
				m.handleResolved(ctx, key, rule, ds.DeviceID, "metric is no longer available")
				continue
			}
			if evaluateCondition(rule.Operator, value, rule.Threshold) {
				m.handleTriggered(rule, ds, value, now)
			} else {
				m.handleResolved(ctx, key, rule, ds.DeviceID, fmt.Sprintf("condition returned to normal at %.2f", value))
			}
		}
	}

	// Prune in-memory state for devices/rules that no longer exist. Do not
	// resolve merely because a live stats row is temporarily absent.
	m.mu.RLock()
	var stale []*AlertState
	for _, s := range m.states {
		if _, ok := activeRuleIDs[s.RuleID]; !ok {
			stale = append(stale, s)
			continue
		}
		if _, ok := policies[s.DeviceID]; !ok {
			stale = append(stale, s)
		}
	}
	m.mu.RUnlock()
	for _, s := range stale {
		rule, ok := rulesByID[s.RuleID]
		if !ok {
			continue
		}
		m.handleResolved(ctx, stateKey(s.RuleID, s.DeviceID), rule, s.DeviceID, "device is no longer present in the monitored inventory")
	}
}

func (m *Manager) ruleApplies(rule Rule, ds *stats.DeviceStats, policy deviceAlertPolicy, now time.Time) bool {
	if ds == nil || ds.DeviceID <= 0 || policy.DeviceID != ds.DeviceID {
		return false
	}
	if !scopeMatches(rule, ds) || !roleMatches(rule.TargetRole, policy.Role) {
		return false
	}
	if rule.RequireAlertable && !policy.Alertable {
		return false
	}
	return policy.AlertSilencedUntil == nil || !now.Before(*policy.AlertSilencedUntil)
}

func scopeMatches(rule Rule, ds *stats.DeviceStats) bool {
	switch rule.Scope {
	case "all":
		return true
	case "site":
		return rule.ScopeID != nil && ds.SiteID == *rule.ScopeID
	case "device":
		return rule.ScopeID != nil && ds.DeviceID == *rule.ScopeID
	default:
		return false
	}
}

func roleMatches(targetRole, deviceRole string) bool {
	return targetRole == "all" || targetRole == deviceRole
}

func (m *Manager) loadDeviceAlertPolicies(ctx context.Context) (map[int]deviceAlertPolicy, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT id, COALESCE(role, ''), parent_id, COALESCE(managed, false),
		       COALESCE(alertable, false), alert_silenced_until, created_at
		FROM devices
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	policies := make(map[int]deviceAlertPolicy)
	for rows.Next() {
		var id int
		var role string
		var parentID sql.NullInt64
		var managed, alertable bool
		var silenced sql.NullTime
		var createdAt time.Time
		if err := rows.Scan(&id, &role, &parentID, &managed, &alertable, &silenced, &createdAt); err != nil {
			return nil, err
		}
		role = strings.ToLower(strings.TrimSpace(role))
		if role != "ap" && role != "sta" {
			if parentID.Valid && !managed {
				role = "sta"
			} else {
				role = "ap"
			}
		}
		p := deviceAlertPolicy{DeviceID: id, Role: role, Alertable: alertable, CreatedAt: createdAt}
		if silenced.Valid {
			t := silenced.Time
			p.AlertSilencedUntil = &t
		}
		policies[id] = p
	}
	return policies, rows.Err()
}

func (m *Manager) getMetricValue(metric string, ds *stats.DeviceStats, policy deviceAlertPolicy, now time.Time) (float64, bool) {
	switch metric {
	case MetricSignal60GHz:
		if ds.Wireless.Radio60GHz != nil {
			return float64(ds.Wireless.Radio60GHz.Signal), true
		}
	case MetricSignal5GHz:
		if ds.Wireless.Radio5GHz != nil {
			return float64(ds.Wireless.Radio5GHz.Signal), true
		}
	case MetricSignal6GHz:
		// Radio6GHz is also used as the secondary slot for a second 5 GHz
		// interface on Wave MLO5. Frequency prevents that compatibility slot
		// from being mislabeled as a 6 GHz measurement.
		if radio := ds.Wireless.Radio6GHz; radio != nil && radio.Frequency >= 5925 {
			return float64(radio.Signal), true
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
		if ds.Status != stats.StatusOffline {
			return 0, true
		}
		start := ds.LastSeen
		if start.IsZero() {
			start = policy.CreatedAt
		}
		if start.IsZero() || start.After(now) {
			return 0, true
		}
		return now.Sub(start).Seconds(), true
	case MetricPeerCount:
		return float64(ds.PeerCount), true
	case MetricCapacity:
		if ds.Wireless.Radio60GHz != nil && ds.Wireless.Radio60GHz.Capacity != nil {
			return float64(ds.Wireless.Radio60GHz.Capacity.Combined) / 1e6, true
		}
	case MetricLinkScore:
		if ds.Wireless.LinkScore != nil {
			return float64(ds.Wireless.LinkScore.DL), true
		}
	case MetricInterference:
		value, found := maxRadioInterference(ds)
		return value, found
	case MetricChainImbalance:
		value, found := maxRadioChainImbalance(ds)
		return value, found
	case MetricGPSSync:
		return gpsSyncMetric(ds)
	}
	return 0, false
}

func maxRadioInterference(ds *stats.DeviceStats) (float64, bool) {
	if ds == nil {
		return 0, false
	}
	maxValue := 0.0
	found := false
	check := func(radio *stats.RadioStats) {
		if radio == nil || radio.Utilization == nil || !finite(radio.Utilization.Interference) {
			return
		}
		if !found || radio.Utilization.Interference > maxValue {
			maxValue = radio.Utilization.Interference
		}
		found = true
	}
	check(ds.Wireless.Radio60GHz)
	check(ds.Wireless.Radio5GHz)
	check(ds.Wireless.Radio6GHz)
	check(ds.Wireless.RadioLTU)
	for i := range ds.Wireless.Radios {
		check(&ds.Wireless.Radios[i])
	}
	return maxValue, found
}

func maxRadioChainImbalance(ds *stats.DeviceStats) (float64, bool) {
	if ds == nil {
		return 0, false
	}
	maxSpread := 0.0
	found := false
	check := func(radio *stats.RadioStats) {
		if radio == nil || len(radio.SignalPerChain) < 2 {
			return
		}
		minSignal, maxSignal, count := 0, 0, 0
		for _, signal := range radio.SignalPerChain {
			// Chain RSSI is reported in negative dBm. Zero is the parser's
			// missing-value default and must not create a false imbalance.
			if signal >= 0 {
				continue
			}
			if count == 0 || signal < minSignal {
				minSignal = signal
			}
			if count == 0 || signal > maxSignal {
				maxSignal = signal
			}
			count++
		}
		if count < 2 {
			return
		}
		spread := float64(maxSignal - minSignal)
		if !found || spread > maxSpread {
			maxSpread = spread
		}
		found = true
	}
	check(ds.Wireless.Radio60GHz)
	check(ds.Wireless.Radio5GHz)
	check(ds.Wireless.Radio6GHz)
	check(ds.Wireless.RadioLTU)
	for i := range ds.Wireless.Radios {
		check(&ds.Wireless.Radios[i])
	}
	return maxSpread, found
}

func gpsSyncMetric(ds *stats.DeviceStats) (float64, bool) {
	if ds == nil {
		return 0, false
	}
	seen := false
	check := func(radio *stats.RadioStats) bool {
		if radio == nil || radio.GPSSyncState == 0 {
			return false
		}
		seen = true
		return radio.GPSSyncState == 2
	}
	if check(ds.Wireless.Radio60GHz) || check(ds.Wireless.Radio5GHz) ||
		check(ds.Wireless.Radio6GHz) || check(ds.Wireless.RadioLTU) {
		return 1, true
	}
	for i := range ds.Wireless.Radios {
		if check(&ds.Wireless.Radios[i]) {
			return 1, true
		}
	}
	if seen {
		return 0, true
	}
	// A true parsed configuration value is still useful when the platform
	// omits the live radio state. A false value is not enough to distinguish
	// "not configured" from "configured but unsynchronized", so it remains
	// unavailable rather than generating a false alert.
	if ds.Config != nil && ds.Config.GPSSync {
		return 1, true
	}
	return 0, false
}

func evaluateCondition(op string, value, threshold float64) bool {
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
	default:
		return false
	}
}

func stateKey(ruleID, deviceID int) string { return fmt.Sprintf("%d:%d", ruleID, deviceID) }

func (m *Manager) handleTriggered(rule Rule, ds *stats.DeviceStats, value float64, now time.Time) {
	key := stateKey(rule.ID, ds.DeviceID)
	m.mu.Lock()
	state, exists := m.states[key]
	if !exists {
		state = &AlertState{RuleID: rule.ID, DeviceID: ds.DeviceID, FirstTriggeredAt: now, LastValue: value}
		m.states[key] = state
	}
	state.LastValue = value
	firstTriggered := state.FirstTriggeredAt
	alreadyAlerted := state.Notified
	lastNotify, hasLast := m.lastNotified[key]
	m.mu.Unlock()

	if !exists {
		_, err := m.db.Exec(`
			INSERT INTO alert_states (rule_id, device_id, first_triggered_at, last_value, last_checked_at, notified)
			VALUES ($1, $2, $3, $4, NOW(), false)
			ON CONFLICT (rule_id, device_id) DO UPDATE SET
			  first_triggered_at = EXCLUDED.first_triggered_at,
			  last_value = EXCLUDED.last_value,
			  last_checked_at = NOW(), notified = false
		`, rule.ID, ds.DeviceID, now, value)
		if err != nil {
			log.Printf("persist alert state failed: %v", err)
			m.mu.Lock()
			if cur := m.states[key]; cur == state {
				delete(m.states, key)
			}
			m.mu.Unlock()
			return
		}
	} else if _, err := m.db.Exec(`UPDATE alert_states SET last_value=$1, last_checked_at=NOW() WHERE rule_id=$2 AND device_id=$3`, value, rule.ID, ds.DeviceID); err != nil {
		log.Printf("update alert state failed: %v", err)
		return
	}

	if now.Sub(firstTriggered) < time.Duration(rule.DurationSeconds)*time.Second || alreadyAlerted {
		return
	}
	if hasLast && now.Sub(lastNotify) < time.Duration(rule.CooldownSeconds)*time.Second {
		return
	}

	message := m.formatAlertMessage(rule, ds, value)
	severity := m.determineSeverity(rule, value)
	alert := Alert{RuleID: &rule.ID, DeviceID: ds.DeviceID, Metric: rule.Metric, Value: value,
		Threshold: rule.Threshold, Message: message, Severity: severity, Status: "active", TriggeredAt: now}
	payload := notificationPayload{Event: "triggered", Rule: rule, Alert: alert, Device: notificationDevice{
		ID: ds.DeviceID, SiteID: ds.SiteID, IP: ds.IP, MAC: ds.MAC, Hostname: ds.Hostname,
	}}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		log.Printf("marshal alert notification payload: %v", err)
		return
	}

	tx, err := m.db.Begin()
	if err != nil {
		log.Printf("begin alert transaction: %v", err)
		return
	}
	defer tx.Rollback()
	var alertID int
	if err := tx.QueryRow(`
		INSERT INTO alerts (rule_id, device_id, metric, value, threshold, message, severity, notified_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7, CASE WHEN cardinality($8::text[]) = 0 THEN NOW() ELSE NULL END)
		RETURNING id
	`, rule.ID, ds.DeviceID, rule.Metric, value, rule.Threshold, message, severity, pq.Array(rule.NotifyChannels)).Scan(&alertID); err != nil {
		log.Printf("create alert: %v", err)
		return
	}
	alert.ID = alertID
	payload.Alert.ID = alertID
	payloadJSON, err = json.Marshal(payload)
	if err != nil {
		return
	}
	for _, channel := range uniqueChannels(rule.NotifyChannels) {
		if _, err := tx.Exec(`
			INSERT INTO alert_notification_outbox (alert_id, channel, event, payload)
			VALUES ($1,$2,'triggered',$3::jsonb)
			ON CONFLICT (alert_id, channel, event) DO NOTHING
		`, alertID, channel, payloadJSON); err != nil {
			log.Printf("enqueue %s notification: %v", channel, err)
			return
		}
	}
	if _, err := tx.Exec(`UPDATE alert_states SET notified=true,last_value=$1,last_checked_at=NOW() WHERE rule_id=$2 AND device_id=$3`, value, rule.ID, ds.DeviceID); err != nil {
		return
	}
	if err := tx.Commit(); err != nil {
		log.Printf("commit alert transaction: %v", err)
		return
	}

	m.mu.Lock()
	if cur := m.states[key]; cur != nil {
		cur.Notified = true
	}
	m.lastNotified[key] = now
	m.mu.Unlock()
	m.wakeNotificationWorker()
}

func (m *Manager) handleResolved(ctx context.Context, key string, rule Rule, deviceID int, reason string) {
	m.mu.RLock()
	state, exists := m.states[key]
	if exists {
		copyState := *state
		state = &copyState
	}
	m.mu.RUnlock()
	if !exists {
		return
	}

	tx, err := m.db.Begin()
	if err != nil {
		return
	}
	defer tx.Rollback()
	enqueued := false
	if state.Notified {
		var err error
		enqueued, err = m.resolveRuleDeviceAlertsTx(ctx, tx, rule, deviceID, reason)
		if err != nil {
			log.Printf("resolve alerts failed: %v", err)
			return
		}
	}
	if _, err := tx.Exec(`DELETE FROM alert_states WHERE rule_id=$1 AND device_id=$2`, rule.ID, deviceID); err != nil {
		log.Printf("delete alert state failed: %v", err)
		return
	}
	if err := tx.Commit(); err != nil {
		log.Printf("commit alert resolution failed: %v", err)
		return
	}
	m.mu.Lock()
	delete(m.states, key)
	m.mu.Unlock()
	if enqueued {
		m.wakeNotificationWorker()
	}
}

func (m *Manager) formatAlertMessage(rule Rule, ds *stats.DeviceStats, value float64) string {
	hostname := ds.Hostname
	if hostname == "" {
		hostname = ds.IP
	}
	if rule.Metric == MetricGPSSync {
		state := "not synchronized"
		if value >= 1 {
			state = "synchronized"
		}
		return fmt.Sprintf("%s: GPS synchronization is %s on %s", rule.Name, state, hostname)
	}
	opStr := map[string]string{OpLT: "below", OpLTE: "at or below", OpGT: "above", OpGTE: "at or above", OpEQ: "equal to", OpNE: "not equal to"}[rule.Operator]
	return fmt.Sprintf("%s: %s is %s threshold (%.2f %s %.2f) on %s", rule.Name, rule.Metric, opStr, value, rule.Operator, rule.Threshold, hostname)
}

func (m *Manager) determineSeverity(rule Rule, value float64) string {
	if rule.Severity == SeverityInfo || rule.Severity == SeverityWarning || rule.Severity == SeverityCritical {
		return rule.Severity
	}
	switch rule.Metric {
	case MetricSignal60GHz, MetricSignal5GHz, MetricSignal6GHz, MetricSignalLTU:
		if value < -80 {
			return SeverityCritical
		}
		if value < -70 {
			return SeverityWarning
		}
	case MetricCPU, MetricRAM:
		if value > 95 {
			return SeverityCritical
		}
		if value > 85 {
			return SeverityWarning
		}
	case MetricTemperature:
		if value > 85 {
			return SeverityCritical
		}
		if value > 75 {
			return SeverityWarning
		}
	case MetricOfflineDuration:
		if value > 3600 {
			return SeverityCritical
		}
		if value > 300 {
			return SeverityWarning
		}
	case MetricCapacity:
		if value < 25 {
			return SeverityCritical
		}
		if value < 100 {
			return SeverityWarning
		}
	case MetricLinkScore:
		if value < 25 {
			return SeverityCritical
		}
		if value < 50 {
			return SeverityWarning
		}
	case MetricInterference:
		if value >= 50 {
			return SeverityCritical
		}
		if value >= 25 {
			return SeverityWarning
		}
	case MetricChainImbalance:
		if value >= 12 {
			return SeverityCritical
		}
		if value >= 6 {
			return SeverityWarning
		}
	case MetricGPSSync:
		if value < 1 {
			return SeverityCritical
		}
	}
	return SeverityWarning
}

func normalizeRule(rule *Rule) {
	if rule == nil {
		return
	}
	rule.Name = strings.TrimSpace(rule.Name)
	rule.Scope = strings.ToLower(strings.TrimSpace(rule.Scope))
	if rule.Scope == "" {
		rule.Scope = "all"
	}
	rule.TargetRole = strings.ToLower(strings.TrimSpace(rule.TargetRole))
	if rule.TargetRole == "" {
		rule.TargetRole = "all"
	}
	rule.Metric = strings.ToLower(strings.TrimSpace(rule.Metric))
	rule.Operator = strings.ToLower(strings.TrimSpace(rule.Operator))
	rule.Severity = strings.ToLower(strings.TrimSpace(rule.Severity))
	if rule.Severity == "" {
		rule.Severity = SeverityAuto
	}
	rule.WebhookURL = strings.TrimSpace(rule.WebhookURL)
	for i := range rule.NotifyChannels {
		rule.NotifyChannels[i] = strings.ToLower(strings.TrimSpace(rule.NotifyChannels[i]))
	}
	rule.NotifyChannels = uniqueChannels(rule.NotifyChannels)
	for i := range rule.NotifyEmails {
		rule.NotifyEmails[i] = strings.TrimSpace(rule.NotifyEmails[i])
	}
}

func uniqueChannels(channels []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(channels))
	for _, ch := range channels {
		ch = strings.ToLower(strings.TrimSpace(ch))
		if ch == "" {
			continue
		}
		if _, ok := seen[ch]; ok {
			continue
		}
		seen[ch] = struct{}{}
		out = append(out, ch)
	}
	return out
}

func (m *Manager) resetRuleStateTx(ctx context.Context, tx *sql.Tx, rule Rule, reason string) (bool, error) {
	enqueued, err := m.resolveRuleAlertsTx(ctx, tx, rule, reason)
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM alert_states WHERE rule_id=$1`, rule.ID); err != nil {
		return false, err
	}
	return enqueued, nil
}

func (m *Manager) clearRuleStateMemory(ruleID int) {
	prefix := fmt.Sprintf("%d:", ruleID)
	m.mu.Lock()
	for key := range m.states {
		if strings.HasPrefix(key, prefix) {
			delete(m.states, key)
		}
	}
	for key := range m.lastNotified {
		if strings.HasPrefix(key, prefix) {
			delete(m.lastNotified, key)
		}
	}
	m.mu.Unlock()
}

// ClearDeviceState drops in-memory duration and cooldown state after a device
// policy change has durably resolved its active alerts and deleted alert_states.
func (m *Manager) ClearDeviceState(deviceID int) {
	suffix := fmt.Sprintf(":%d", deviceID)
	m.mu.Lock()
	for key, state := range m.states {
		if state.DeviceID == deviceID {
			delete(m.states, key)
		}
	}
	for key := range m.lastNotified {
		if strings.HasSuffix(key, suffix) {
			delete(m.lastNotified, key)
		}
	}
	m.mu.Unlock()
}

func (m *Manager) CreateRule(rule *Rule) (int, error) {
	normalizeRule(rule)
	if err := ValidateRule(rule); err != nil {
		return 0, err
	}
	var id int
	err := m.db.QueryRow(`
		INSERT INTO alert_rules (name,enabled,scope,scope_id,target_role,require_alertable,metric,operator,threshold,
		 duration_seconds,severity,notify_channels,notify_emails,webhook_url,notify_recovery,cooldown_seconds,created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,NULLIF($14,''),$15,$16,$17)
		RETURNING id
	`, rule.Name, rule.Enabled, rule.Scope, rule.ScopeID, rule.TargetRole, rule.RequireAlertable, rule.Metric, rule.Operator,
		rule.Threshold, rule.DurationSeconds, rule.Severity, pq.Array(rule.NotifyChannels), pq.Array(rule.NotifyEmails), rule.WebhookURL,
		rule.NotifyRecovery, rule.CooldownSeconds, rule.CreatedBy).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, m.Reload()
}

func (m *Manager) GetRule(id int) (Rule, error) {
	var r Rule
	var scopeID sql.NullInt64
	var channels, emails pq.StringArray
	var webhook sql.NullString
	err := m.db.QueryRow(`
		SELECT id,name,enabled,scope,scope_id,target_role,require_alertable,metric,operator,threshold,
		 duration_seconds,severity,notify_channels,notify_emails,webhook_url,notify_recovery,cooldown_seconds,created_at,COALESCE(created_by,0)
		FROM alert_rules WHERE id=$1
	`, id).Scan(&r.ID, &r.Name, &r.Enabled, &r.Scope, &scopeID, &r.TargetRole, &r.RequireAlertable, &r.Metric,
		&r.Operator, &r.Threshold, &r.DurationSeconds, &r.Severity, &channels, &emails, &webhook, &r.NotifyRecovery, &r.CooldownSeconds, &r.CreatedAt, &r.CreatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	if err != nil {
		return r, err
	}
	if scopeID.Valid {
		v := int(scopeID.Int64)
		r.ScopeID = &v
	}
	r.NotifyChannels = append([]string(nil), channels...)
	r.NotifyEmails = append([]string(nil), emails...)
	if webhook.Valid {
		r.WebhookURL = webhook.String
	}
	normalizeRule(&r)
	return r, nil
}

func (m *Manager) UpdateRule(id int, rule *Rule) error {
	normalizeRule(rule)
	if err := ValidateRule(rule); err != nil {
		return err
	}
	tx, err := m.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var lockedID int
	if err := tx.QueryRow(`SELECT id FROM alert_rules WHERE id=$1 FOR UPDATE`, id).Scan(&lockedID); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	oldRule, err := getRuleTx(context.Background(), tx, id)
	if err != nil {
		return err
	}
	enqueued, err := m.resetRuleStateTx(context.Background(), tx, oldRule, "alert rule was changed or disabled")
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`
		UPDATE alert_rules SET name=$1,enabled=$2,scope=$3,scope_id=$4,target_role=$5,require_alertable=$6,
		 metric=$7,operator=$8,threshold=$9,duration_seconds=$10,severity=$11,notify_channels=$12,notify_emails=$13,
		 webhook_url=NULLIF($14,''),notify_recovery=$15,cooldown_seconds=$16,updated_at=NOW() WHERE id=$17
	`, rule.Name, rule.Enabled, rule.Scope, rule.ScopeID, rule.TargetRole, rule.RequireAlertable, rule.Metric, rule.Operator,
		rule.Threshold, rule.DurationSeconds, rule.Severity, pq.Array(rule.NotifyChannels), pq.Array(rule.NotifyEmails), rule.WebhookURL,
		rule.NotifyRecovery, rule.CooldownSeconds, id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	m.clearRuleStateMemory(id)
	if enqueued {
		m.wakeNotificationWorker()
	}
	return m.Reload()
}

func (m *Manager) DeleteRule(id int) error {
	tx, err := m.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var lockedID int
	if err := tx.QueryRow(`SELECT id FROM alert_rules WHERE id=$1 FOR UPDATE`, id).Scan(&lockedID); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	oldRule, err := getRuleTx(context.Background(), tx, id)
	if err != nil {
		return err
	}
	enqueued, err := m.resetRuleStateTx(context.Background(), tx, oldRule, "alert rule was deleted")
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM alert_rules WHERE id=$1`, id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	m.clearRuleStateMemory(id)
	if enqueued {
		m.wakeNotificationWorker()
	}
	return m.Reload()
}

func (m *Manager) ListRules() ([]Rule, error) {
	rows, err := m.db.Query(`
		SELECT id,name,enabled,scope,scope_id,target_role,require_alertable,metric,operator,threshold,
		 duration_seconds,severity,notify_channels,notify_emails,webhook_url,notify_recovery,cooldown_seconds,created_at
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
		var webhook sql.NullString
		if err := rows.Scan(&r.ID, &r.Name, &r.Enabled, &r.Scope, &scopeID, &r.TargetRole, &r.RequireAlertable,
			&r.Metric, &r.Operator, &r.Threshold, &r.DurationSeconds, &r.Severity, &channels, &emails, &webhook,
			&r.NotifyRecovery, &r.CooldownSeconds, &r.CreatedAt); err != nil {
			return nil, err
		}
		if scopeID.Valid {
			v := int(scopeID.Int64)
			r.ScopeID = &v
		}
		r.NotifyChannels = append([]string(nil), channels...)
		r.NotifyEmails = append([]string(nil), emails...)
		if webhook.Valid {
			r.WebhookURL = webhook.String
		}
		normalizeRule(&r)
		rules = append(rules, r)
	}
	return rules, rows.Err()
}

type alertScanner interface{ Scan(dest ...any) error }

func scanAlert(row alertScanner) (Alert, error) {
	var a Alert
	var ruleID, ackBy sql.NullInt64
	var ackAt, resAt, notifiedAt, recoveryNotifiedAt sql.NullTime
	var value, threshold sql.NullFloat64
	var notifyError, recoveryNotifyError sql.NullString
	err := row.Scan(&a.ID, &ruleID, &a.DeviceID, &a.Metric, &value, &threshold, &a.Message, &a.Severity,
		&a.Status, &a.TriggeredAt, &ackAt, &ackBy, &resAt, &notifiedAt, &notifyError, &recoveryNotifiedAt, &recoveryNotifyError)
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
	if notifiedAt.Valid {
		a.NotifiedAt = &notifiedAt.Time
	}
	if notifyError.Valid {
		a.NotifyError = notifyError.String
	}
	if recoveryNotifiedAt.Valid {
		a.RecoveryNotifiedAt = &recoveryNotifiedAt.Time
	}
	if recoveryNotifyError.Valid {
		a.RecoveryNotifyError = recoveryNotifyError.String
	}
	a.NotifyChannels = []string{}
	a.RecoveryChannels = []string{}
	a.Deliveries = []NotificationDelivery{}
	return a, nil
}

func (m *Manager) ListAlerts(status string, limit int) ([]Alert, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	status = strings.ToLower(strings.TrimSpace(status))
	if status != "" && status != "open" && status != "active" && status != "acknowledged" && status != "resolved" {
		return nil, fmt.Errorf("invalid alert status")
	}
	query := `SELECT id,rule_id,device_id,metric,value,threshold,message,severity,status,triggered_at,
	                 acknowledged_at,acknowledged_by,resolved_at,notified_at,notify_error,
	                 recovery_notified_at,recovery_notify_error FROM alerts`
	args := []any{}
	switch status {
	case "open":
		query += ` WHERE status IN ('active','acknowledged')`
	case "":
	default:
		query += ` WHERE status=$1`
		args = append(args, status)
	}
	args = append(args, limit)
	query += fmt.Sprintf(" ORDER BY triggered_at DESC LIMIT $%d", len(args))
	rows, err := m.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	var alerts []Alert
	alertIndexes := map[int]int{}
	for rows.Next() {
		a, err := scanAlert(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		alertIndexes[a.ID] = len(alerts)
		alerts = append(alerts, a)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(alerts) == 0 {
		return alerts, nil
	}
	ids := make([]int, 0, len(alerts))
	for _, alert := range alerts {
		ids = append(ids, alert.ID)
	}
	deliveryRows, err := m.db.Query(`
		SELECT alert_id,channel,event,status,attempts,COALESCE(last_error,''),next_attempt_at,sent_at
		FROM alert_notification_outbox
		WHERE alert_id=ANY($1)
		ORDER BY alert_id,event,channel
	`, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	for deliveryRows.Next() {
		var alertID int
		var delivery NotificationDelivery
		var nextAttempt sql.NullTime
		var sentAt sql.NullTime
		if err := deliveryRows.Scan(&alertID, &delivery.Channel, &delivery.Event, &delivery.Status,
			&delivery.Attempts, &delivery.LastError, &nextAttempt, &sentAt); err != nil {
			deliveryRows.Close()
			return nil, err
		}
		if nextAttempt.Valid {
			t := nextAttempt.Time
			delivery.NextAttemptAt = &t
		}
		if sentAt.Valid {
			t := sentAt.Time
			delivery.SentAt = &t
		}
		if index, ok := alertIndexes[alertID]; ok {
			alerts[index].Deliveries = append(alerts[index].Deliveries, delivery)
		}
	}
	if err := deliveryRows.Close(); err != nil {
		return nil, err
	}
	if err := deliveryRows.Err(); err != nil {
		return nil, err
	}
	for i := range alerts {
		alerts[i].NotifyChannels, alerts[i].DeliveryStatus = summarizeDeliveries(alerts[i].Deliveries, "triggered", "in_app")
		alerts[i].RecoveryChannels, alerts[i].RecoveryDeliveryStatus = summarizeDeliveries(alerts[i].Deliveries, "resolved", "not_requested")
	}
	return alerts, nil
}

func summarizeDeliveries(deliveries []NotificationDelivery, event, emptyStatus string) ([]string, string) {
	channels := []string{}
	counts := map[string]int{}
	for _, delivery := range deliveries {
		if delivery.Event != event {
			continue
		}
		channels = append(channels, delivery.Channel)
		counts[delivery.Status]++
	}
	if len(channels) == 0 {
		return channels, emptyStatus
	}
	if counts["sending"] > 0 {
		return channels, "sending"
	}
	if counts["pending"] > 0 {
		return channels, "pending"
	}
	if counts["failed"] > 0 {
		return channels, "retrying"
	}
	if counts["sent"] == len(channels) {
		return channels, "sent"
	}
	if counts["sent"] > 0 && counts["dead"] > 0 {
		return channels, "partial"
	}
	return channels, "failed"
}

func (m *Manager) AcknowledgeAlert(alertID, userID int) error {
	res, err := m.db.Exec(`
		UPDATE alerts SET status='acknowledged',acknowledged_at=NOW(),acknowledged_by=$1
		WHERE id=$2 AND status='active'
	`, userID, alertID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (m *Manager) ResolveAlert(alertID int) error {
	tx, err := m.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var ruleID sql.NullInt64
	var deviceID int
	var status string
	if err := tx.QueryRow(`SELECT rule_id,device_id,status FROM alerts WHERE id=$1 FOR UPDATE`, alertID).Scan(&ruleID, &deviceID, &status); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if status != "active" && status != "acknowledged" {
		return ErrNotFound
	}
	var rule *Rule
	if ruleID.Valid {
		loaded, err := getRuleTx(context.Background(), tx, int(ruleID.Int64))
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if err == nil {
			rule = &loaded
		}
	}
	enqueued, err := m.resolveAlertTx(context.Background(), tx, rule, alertID, "resolved manually by an operator")
	if err != nil {
		return err
	}
	if ruleID.Valid {
		if _, err := tx.Exec(`DELETE FROM alert_states WHERE rule_id=$1 AND device_id=$2`, ruleID.Int64, deviceID); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if ruleID.Valid {
		key := stateKey(int(ruleID.Int64), deviceID)
		m.mu.Lock()
		delete(m.states, key)
		// Keep lastNotified so manual resolution does not bypass the rule's
		// post-trigger cooldown. If the condition remains present, evaluation
		// starts a fresh persistence window and may open a new occurrence only
		// after both persistence and cooldown have elapsed.
		m.mu.Unlock()
	}
	if enqueued {
		m.wakeNotificationWorker()
	}
	return nil
}

// ResolveDeviceAlerts closes all active occurrences for a device after its
// alert policy is disabled or silenced. Recovery delivery uses each alert's
// original rule payload so rule-specific email and webhook targets are kept.
func (m *Manager) ResolveDeviceAlerts(ctx context.Context, deviceID int, reason string) error {
	if deviceID <= 0 {
		return fmt.Errorf("invalid device id")
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
		SELECT id FROM alerts
		WHERE device_id=$1 AND status IN ('active','acknowledged')
		ORDER BY id
		FOR UPDATE
	`, deviceID)
	if err != nil {
		return err
	}
	var ids []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	enqueued := false
	for _, id := range ids {
		queued, err := m.resolveAlertTx(ctx, tx, nil, id, reason)
		if err != nil {
			return err
		}
		enqueued = enqueued || queued
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM alert_states WHERE device_id=$1`, deviceID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	m.ClearDeviceState(deviceID)
	if enqueued {
		m.wakeNotificationWorker()
	}
	return nil
}

func (m *Manager) GetActiveAlertCount() int {
	var count int
	_ = m.db.QueryRow(`SELECT COUNT(*) FROM alerts WHERE status IN ('active','acknowledged')`).Scan(&count)
	return count
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
