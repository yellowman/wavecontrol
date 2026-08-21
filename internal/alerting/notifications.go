package alerting

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/smtp"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"
	"github.com/yellowman/wavecontrol/internal/sysmonalerter"
)

type notificationDevice struct {
	ID       int    `json:"id"`
	SiteID   int    `json:"site_id,omitempty"`
	IP       string `json:"ip,omitempty"`
	MAC      string `json:"mac,omitempty"`
	Hostname string `json:"hostname,omitempty"`
}

type notificationPayload struct {
	Event       string             `json:"event"`
	Rule        Rule               `json:"rule"`
	Alert       Alert              `json:"alert"`
	Device      notificationDevice `json:"device"`
	ClearReason string             `json:"clear_reason,omitempty"`
}

type outboxItem struct {
	ID       int64
	AlertID  int
	Channel  string
	Event    string
	Payload  notificationPayload
	Attempts int
}

// NotificationDelivery is the safe per-channel delivery state returned with
// alert history. It intentionally contains no channel credentials or payload.
type NotificationDelivery struct {
	Channel       string     `json:"channel"`
	Event         string     `json:"event"`
	Status        string     `json:"status"`
	Attempts      int        `json:"attempts"`
	LastError     string     `json:"last_error,omitempty"`
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"`
	SentAt        *time.Time `json:"sent_at,omitempty"`
}

// NotificationChannelStatus describes whether a rule may use a delivery
// channel. Webhooks are configured per rule; the other channels use admin
// settings. SysmonStatus never exposes the bearer token or certificate body.
type NotificationChannelStatus struct {
	Channel      string                `json:"channel"`
	Label        string                `json:"label"`
	Configured   bool                  `json:"configured"`
	Enabled      bool                  `json:"enabled"`
	Description  string                `json:"description"`
	SysmonStatus *sysmonalerter.Status `json:"sysmon_status,omitempty"`
}

func (m *Manager) wakeNotificationWorker() {
	// A channel receive is not a broadcast. Queue enough wake tokens for the
	// three general workers and the isolated sysmon worker; periodic scans are
	// still the safety net for coalesced or missed signals.
	for i := 0; i < generalNotificationWorkers+1; i++ {
		select {
		case m.notificationCh <- struct{}{}:
		default:
			return
		}
	}
}

func (m *Manager) runNotificationWorker(ctx context.Context, sysmonOnly bool) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		// One worker performs stale-claim recovery once per scan. Keeping this
		// out of claimNotification avoids an UPDATE before every outbox row.
		if sysmonOnly {
			if err := m.recoverStaleNotificationClaims(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("recover stale alert notification claims: %v", err)
			}
		}
		for i := 0; i < 32; i++ {
			item, ok, err := m.claimNotification(ctx, sysmonOnly)
			if err != nil {
				if !errors.Is(err, context.Canceled) {
					log.Printf("alert notification claim failed: %v", err)
				}
				break
			}
			if !ok {
				break
			}
			err = m.deliverNotification(ctx, item)
			if err == nil {
				if err := m.markNotificationSent(ctx, item); err != nil && !errors.Is(err, context.Canceled) {
					log.Printf("mark alert notification %d sent: %v", item.ID, err)
				}
			} else if markErr := m.markNotificationFailed(ctx, item, err); markErr != nil && !errors.Is(markErr, context.Canceled) {
				log.Printf("record alert notification %d failure: delivery=%v persistence=%v", item.ID, err, markErr)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-m.notificationCh:
		}
	}
}

func (m *Manager) recoverStaleNotificationClaims(ctx context.Context) error {
	// A process may die after claiming a row. Recover abandoned claims rather
	// than leaving them in "sending" forever. A resolved trigger is terminal;
	// every other stale claim returns to the durable retry path.
	_, err := m.db.ExecContext(ctx, `
		UPDATE alert_notification_outbox o
		SET status=CASE WHEN o.event='triggered' AND a.status='resolved' THEN 'dead' ELSE 'failed' END,
		    last_error=CASE WHEN o.event='triggered' AND a.status='resolved'
		      THEN 'delivery claim expired after the alert closed'
		      ELSE 'recovered stale sending claim' END,
		    next_attempt_at=NOW(), updated_at=NOW()
		FROM alerts a
		WHERE o.alert_id=a.id AND o.status='sending' AND o.updated_at < NOW() - INTERVAL '5 minutes'
	`)
	return err
}

func (m *Manager) claimNotification(ctx context.Context, sysmonOnly bool) (outboxItem, bool, error) {
	var item outboxItem
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return item, false, err
	}
	defer tx.Rollback()

	var raw []byte
	var attempts int
	channelFilter := "o.channel <> 'sysmon'"
	if sysmonOnly {
		channelFilter = "o.channel = 'sysmon'"
	}
	claimQuery := fmt.Sprintf(`
		SELECT o.id, o.alert_id, o.channel, o.event, o.payload, o.attempts
		FROM alert_notification_outbox o
		JOIN alerts a ON a.id=o.alert_id
		WHERE o.status IN ('pending','failed') AND o.next_attempt_at <= NOW()
		  AND %s
		  AND (o.channel='sysmon' OR o.attempts < 8)
		  AND (o.event='resolved' OR a.status IN ('active','acknowledged'))
		  AND NOT (
		    o.event='resolved' AND EXISTS (
		      SELECT 1 FROM alert_notification_outbox t
		      WHERE t.alert_id=o.alert_id AND t.channel=o.channel
		        AND t.event='triggered' AND t.status='sending'
		    )
		  )
		ORDER BY o.next_attempt_at, o.id
		FOR UPDATE SKIP LOCKED
		LIMIT 1
	`, channelFilter)
	err = tx.QueryRowContext(ctx, claimQuery).Scan(&item.ID, &item.AlertID, &item.Channel, &item.Event, &raw, &attempts)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return item, false, nil
		}
		return item, false, err
	}
	if err := json.Unmarshal(raw, &item.Payload); err != nil {
		if _, updateErr := tx.ExecContext(ctx, `
			UPDATE alert_notification_outbox SET status='dead', attempts=8, last_error=$1, updated_at=NOW() WHERE id=$2
		`, "invalid notification payload: "+err.Error(), item.ID); updateErr != nil {
			return item, false, updateErr
		}
		if err := tx.Commit(); err != nil {
			return item, false, err
		}
		return item, false, nil
	}
	if item.Payload.Event == "" {
		item.Payload.Event = item.Event
	}
	item.Attempts = attempts + 1
	if _, err := tx.ExecContext(ctx, `
		UPDATE alert_notification_outbox SET status='sending', attempts=$1, updated_at=NOW() WHERE id=$2
	`, item.Attempts, item.ID); err != nil {
		return item, false, err
	}
	if err := tx.Commit(); err != nil {
		return item, false, err
	}
	return item, true, nil
}

func (m *Manager) deliverNotification(ctx context.Context, item outboxItem) error {
	switch item.Channel {
	case "email":
		return m.sendEmail(item.Payload)
	case "webhook":
		return sendWebhook(ctx, item.Payload)
	case "zabbix":
		return m.sendZabbix(ctx, item.Payload)
	case "sysmon":
		return m.sendSysmon(ctx, item.Payload)
	default:
		return fmt.Errorf("unsupported notification channel %q", item.Channel)
	}
}

func (m *Manager) markNotificationSent(ctx context.Context, item outboxItem) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
		UPDATE alert_notification_outbox
		SET status='sent', sent_at=NOW(), last_error=NULL, updated_at=NOW()
		WHERE id=$1 AND status='sending' AND attempts=$2
	`, item.ID, item.Attempts)
	if err != nil {
		return err
	}
	if rows, err := result.RowsAffected(); err != nil {
		return err
	} else if rows != 1 {
		// The claim was recovered, canceled, or superseded while this worker was
		// delivering. Do not overwrite the newer durable state.
		return tx.Commit()
	}
	columnTime := "notified_at"
	columnError := "notify_error"
	if item.Event == "resolved" {
		columnTime = "recovery_notified_at"
		columnError = "recovery_notify_error"
	}
	query := fmt.Sprintf(`
		UPDATE alerts a SET %s=NOW(), %s=NULL
		WHERE a.id=$1 AND NOT EXISTS (
		  SELECT 1 FROM alert_notification_outbox o
		  WHERE o.alert_id=a.id AND o.event=$2 AND o.status <> 'sent'
		)
	`, columnTime, columnError)
	if _, err := tx.ExecContext(ctx, query, item.AlertID, item.Event); err != nil {
		return err
	}
	return tx.Commit()
}

func (m *Manager) markNotificationFailed(ctx context.Context, item outboxItem, deliveryErr error) error {
	status, backoff := notificationFailurePolicy(item.Channel, item.Attempts)
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var alertStatus string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM alerts WHERE id=$1 FOR UPDATE`, item.AlertID).Scan(&alertStatus); err != nil {
		return err
	}
	message := truncateError(deliveryErr.Error(), 2000)
	if item.Event == "triggered" && alertStatus == "resolved" {
		status = "dead"
		message = truncateError("alert closed before delivery completed: "+message, 2000)
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE alert_notification_outbox
		SET status=$1, last_error=$2, next_attempt_at=$3, updated_at=NOW()
		WHERE id=$4 AND status='sending' AND attempts=$5
	`, status, message, time.Now().Add(backoff), item.ID, item.Attempts)
	if err != nil {
		return err
	}
	if rows, err := result.RowsAffected(); err != nil {
		return err
	} else if rows != 1 {
		return tx.Commit()
	}
	column := "notify_error"
	if item.Event == "resolved" {
		column = "recovery_notify_error"
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE alerts SET %s=$1 WHERE id=$2`, column), message, item.AlertID); err != nil {
		return err
	}
	return tx.Commit()
}

func notificationFailurePolicy(channel string, attempts int) (status string, backoff time.Duration) {
	if attempts < 1 {
		attempts = 1
	}
	if channel == "sysmon" {
		// The alerter protocol calls for reconnecting after a few seconds,
		// doubling to one minute. Keep retrying while the occurrence remains
		// relevant; resolution cancels a stale trigger and recovery remains
		// durable until sysmon-web accepts the matching OK.
		backoff = time.Duration(1<<min(attempts-1, 4)) * 5 * time.Second
		if backoff > time.Minute {
			backoff = time.Minute
		}
		return "failed", backoff
	}
	status = "failed"
	if attempts >= 8 {
		status = "dead"
	}
	backoff = time.Duration(1<<min(attempts-1, 8)) * 30 * time.Second
	if backoff > 30*time.Minute {
		backoff = 30 * time.Minute
	}
	return status, backoff
}

// resolveRuleDeviceAlertsTx closes every active alert for one rule/device
// occurrence. The alert row, trigger cancellation, and recovery enqueue happen
// atomically so a delayed trigger can never be delivered after an OK without a
// corresponding durable recovery record.
func (m *Manager) resolveRuleDeviceAlertsTx(ctx context.Context, tx *sql.Tx, rule Rule, deviceID int, reason string) (bool, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id FROM alerts
		WHERE rule_id=$1 AND device_id=$2 AND status IN ('active','acknowledged')
		ORDER BY id
		FOR UPDATE
	`, rule.ID, deviceID)
	if err != nil {
		return false, err
	}
	var ids []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return false, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	if err := rows.Err(); err != nil {
		return false, err
	}

	enqueued := false
	for _, id := range ids {
		queued, err := m.resolveAlertTx(ctx, tx, &rule, id, reason)
		if err != nil {
			return false, err
		}
		enqueued = enqueued || queued
	}
	return enqueued, nil
}

func (m *Manager) resolveRuleAlertsTx(ctx context.Context, tx *sql.Tx, rule Rule, reason string) (bool, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id FROM alerts
		WHERE rule_id=$1 AND status IN ('active','acknowledged')
		ORDER BY id
		FOR UPDATE
	`, rule.ID)
	if err != nil {
		return false, err
	}
	var ids []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return false, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	if err := rows.Err(); err != nil {
		return false, err
	}

	enqueued := false
	for _, id := range ids {
		queued, err := m.resolveAlertTx(ctx, tx, &rule, id, reason)
		if err != nil {
			return false, err
		}
		enqueued = enqueued || queued
	}
	return enqueued, nil
}

// resolveAlertTx is the single durable close path used by automatic recovery,
// manual resolution, rule changes, and per-device alert policy changes.
func (m *Manager) resolveAlertTx(ctx context.Context, tx *sql.Tx, rule *Rule, alertID int, reason string) (bool, error) {
	var alert Alert
	var ruleID, acknowledgedBy sql.NullInt64
	var value, threshold sql.NullFloat64
	var acknowledgedAt, resolvedAt sql.NullTime
	var notifiedAt, recoveryNotifiedAt sql.NullTime
	var notifyError, recoveryNotifyError sql.NullString
	var device notificationDevice
	err := tx.QueryRowContext(ctx, `
		SELECT a.id,a.rule_id,a.device_id,a.metric,a.value,a.threshold,a.message,a.severity,a.status,
		       a.triggered_at,a.acknowledged_at,a.acknowledged_by,a.resolved_at,
		       a.notified_at,a.notify_error,a.recovery_notified_at,a.recovery_notify_error,
		       COALESCE(d.site_id,0),host(d.ip_address),d.mac,COALESCE(d.hostname,'')
		FROM alerts a
		JOIN devices d ON d.id=a.device_id
		WHERE a.id=$1
		FOR UPDATE OF a
	`, alertID).Scan(
		&alert.ID, &ruleID, &alert.DeviceID, &alert.Metric, &value, &threshold, &alert.Message, &alert.Severity, &alert.Status,
		&alert.TriggeredAt, &acknowledgedAt, &acknowledgedBy, &resolvedAt,
		&notifiedAt, &notifyError, &recoveryNotifiedAt, &recoveryNotifyError,
		&device.SiteID, &device.IP, &device.MAC, &device.Hostname,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	if alert.Status == "resolved" {
		return false, nil
	}
	if ruleID.Valid {
		v := int(ruleID.Int64)
		alert.RuleID = &v
	}
	if value.Valid {
		alert.Value = value.Float64
	}
	if threshold.Valid {
		alert.Threshold = threshold.Float64
	}
	if acknowledgedAt.Valid {
		alert.AcknowledgedAt = &acknowledgedAt.Time
	}
	if acknowledgedBy.Valid {
		v := int(acknowledgedBy.Int64)
		alert.AcknowledgedBy = &v
	}
	if notifiedAt.Valid {
		alert.NotifiedAt = &notifiedAt.Time
	}
	if notifyError.Valid {
		alert.NotifyError = notifyError.String
	}
	if recoveryNotifiedAt.Valid {
		alert.RecoveryNotifiedAt = &recoveryNotifiedAt.Time
	}
	if recoveryNotifyError.Valid {
		alert.RecoveryNotifyError = recoveryNotifyError.String
	}
	device.ID = alert.DeviceID

	now := time.Now()
	alert.Status = "resolved"
	alert.ResolvedAt = &now
	if _, err := tx.ExecContext(ctx, `
		UPDATE alerts
		SET status='resolved',resolved_at=$1,recovery_notify_error=NULL
		WHERE id=$2
	`, now, alertID); err != nil {
		return false, err
	}

	type triggerDelivery struct {
		Channel string
		Status  string
		Raw     []byte
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT channel,status,payload
		FROM alert_notification_outbox
		WHERE alert_id=$1 AND event='triggered'
		ORDER BY id
		FOR UPDATE
	`, alertID)
	if err != nil {
		return false, err
	}
	var triggers []triggerDelivery
	for rows.Next() {
		var delivery triggerDelivery
		if err := rows.Scan(&delivery.Channel, &delivery.Status, &delivery.Raw); err != nil {
			rows.Close()
			return false, err
		}
		triggers = append(triggers, delivery)
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	if err := rows.Err(); err != nil {
		return false, err
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE alert_notification_outbox
		SET status='dead',last_error='alert closed before trigger delivery',updated_at=NOW()
		WHERE alert_id=$1 AND event='triggered' AND status IN ('pending','failed')
	`, alertID); err != nil {
		return false, err
	}

	if rule == nil && ruleID.Valid {
		loaded, loadErr := getRuleTx(ctx, tx, int(ruleID.Int64))
		if loadErr == nil {
			rule = &loaded
		} else if !errors.Is(loadErr, ErrNotFound) {
			return false, loadErr
		}
	}

	enqueued := false
	for _, trigger := range triggers {
		if trigger.Status != "sent" && trigger.Status != "sending" {
			continue
		}
		var payload notificationPayload
		if err := json.Unmarshal(trigger.Raw, &payload); err != nil {
			if rule == nil {
				return false, fmt.Errorf("rebuild recovery payload for alert %d: %w", alertID, err)
			}
			payload = notificationPayload{Rule: *rule, Device: device}
		}
		if rule != nil {
			payload.Rule = *rule
		}
		if !payload.Rule.NotifyRecovery {
			continue
		}
		payload.Event = "resolved"
		payload.Alert = alert
		payload.Device = device
		payload.ClearReason = truncateError(strings.TrimSpace(reason), 512)
		raw, err := json.Marshal(payload)
		if err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO alert_notification_outbox (alert_id,channel,event,payload)
			VALUES ($1,$2,'resolved',$3::jsonb)
			ON CONFLICT (alert_id,channel,event) DO UPDATE SET
			  payload=EXCLUDED.payload,
			  status=CASE WHEN alert_notification_outbox.status='sent' THEN 'sent' ELSE 'pending' END,
			  attempts=CASE WHEN alert_notification_outbox.status='sent' THEN alert_notification_outbox.attempts ELSE 0 END,
			  next_attempt_at=CASE WHEN alert_notification_outbox.status='sent' THEN alert_notification_outbox.next_attempt_at ELSE NOW() END,
			  last_error=CASE WHEN alert_notification_outbox.status='sent' THEN alert_notification_outbox.last_error ELSE NULL END,
			  updated_at=NOW()
		`, alertID, trigger.Channel, raw); err != nil {
			return false, err
		}
		enqueued = true
	}
	return enqueued, nil
}

func getRuleTx(ctx context.Context, tx *sql.Tx, id int) (Rule, error) {
	var r Rule
	var scopeID sql.NullInt64
	var channels, emails pq.StringArray
	var webhook sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT id,name,enabled,scope,scope_id,target_role,require_alertable,metric,operator,threshold,
		       duration_seconds,severity,notify_channels,notify_emails,webhook_url,notify_recovery,
		       cooldown_seconds,created_at,COALESCE(created_by,0)
		FROM alert_rules WHERE id=$1
	`, id).Scan(&r.ID, &r.Name, &r.Enabled, &r.Scope, &scopeID, &r.TargetRole, &r.RequireAlertable,
		&r.Metric, &r.Operator, &r.Threshold, &r.DurationSeconds, &r.Severity, &channels, &emails,
		&webhook, &r.NotifyRecovery, &r.CooldownSeconds, &r.CreatedAt, &r.CreatedBy)
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

func truncateError(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (m *Manager) sendEmail(payload notificationPayload) error {
	m.mu.RLock()
	cfg := m.smtpConfig
	if cfg != nil {
		copyCfg := *cfg
		cfg = &copyCfg
	}
	m.mu.RUnlock()
	if cfg == nil || len(payload.Rule.NotifyEmails) == 0 {
		return fmt.Errorf("email is not configured")
	}
	if cfg.From == "" {
		return fmt.Errorf("smtp_from is not configured")
	}
	host := payload.Device.Hostname
	if host == "" {
		host = payload.Device.IP
	}
	eventLabel := strings.ToUpper(normalizeNotificationEvent(payload.Event))
	severity := strings.ToUpper(payload.Alert.Severity)
	if payload.Event == "resolved" {
		severity = "OK"
	}
	subject := strings.NewReplacer("\r", " ", "\n", " ").Replace(fmt.Sprintf("[%s] %s", severity, payload.Rule.Name))
	body := fmt.Sprintf("Event: %s\r\nAlert: %s\r\n\r\nDevice: %s (%s)\r\nMetric: %s\r\nValue: %.2f\r\nThreshold: %.2f\r\nTriggered: %s\r\n",
		eventLabel, payload.Rule.Name, host, payload.Device.IP, payload.Alert.Metric, payload.Alert.Value,
		payload.Alert.Threshold, payload.Alert.TriggeredAt.Format(time.RFC3339))
	if payload.Alert.ResolvedAt != nil {
		body += fmt.Sprintf("Cleared: %s\r\n", payload.Alert.ResolvedAt.Format(time.RFC3339))
	}
	body += fmt.Sprintf("\r\nMessage: %s\r\n", notificationText(payload))
	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n%s",
		cfg.From, strings.Join(payload.Rule.NotifyEmails, ", "), subject, body)
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	var auth smtp.Auth
	if cfg.Username != "" {
		auth = smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)
	}
	return smtp.SendMail(addr, auth, cfg.From, payload.Rule.NotifyEmails, []byte(msg))
}

func validateWebhookURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("scheme must be http or https")
	}
	if u.Hostname() == "" || u.User != nil {
		return fmt.Errorf("host is required and userinfo is forbidden")
	}
	// Rule validation must not depend on DNS availability. Literal addresses are
	// rejected here; hostnames are resolved and re-validated by the protected
	// transport at delivery time, including after redirects.
	if ip := net.ParseIP(u.Hostname()); ip != nil && unsafeWebhookIP(ip) {
		return fmt.Errorf("private or special-use addresses are forbidden")
	}
	return nil
}

func resolvePublicHost(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		if unsafeWebhookIP(ip) {
			return nil, fmt.Errorf("private or special-use addresses are forbidden")
		}
		return []net.IP{ip}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("resolve host: %w", err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("host has no addresses")
	}
	for _, ip := range addrs {
		if unsafeWebhookIP(ip) {
			return nil, fmt.Errorf("host resolves to a private or special-use address")
		}
	}
	return addrs, nil
}

func unsafeWebhookIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	addr = addr.Unmap()
	if !addr.IsValid() || !addr.IsGlobalUnicast() {
		return true
	}
	for _, prefix := range forbiddenWebhookPrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

var forbiddenWebhookPrefixes = func() []netip.Prefix {
	raw := []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
		"169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24",
		"192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24",
		"240.0.0.0/4", "::/128", "::1/128", "100::/64", "2001:db8::/32",
		"fc00::/7", "fe80::/10",
	}
	out := make([]netip.Prefix, 0, len(raw))
	for _, value := range raw {
		out = append(out, netip.MustParsePrefix(value))
	}
	return out
}()

func webhookClient() *http.Client {
	dialer := &net.Dialer{Timeout: 8 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			ips, err := resolvePublicHost(ctx, host)
			if err != nil {
				return nil, err
			}
			var lastErr error
			for _, ip := range ips {
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			return nil, lastErr
		},
		TLSHandshakeTimeout: 8 * time.Second,
	}
	client := &http.Client{Transport: transport, Timeout: 12 * time.Second}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return fmt.Errorf("too many redirects")
		}
		return validateWebhookURL(req.URL.String())
	}
	return client
}

func sendWebhook(ctx context.Context, payload notificationPayload) error {
	if err := validateWebhookURL(payload.Rule.WebhookURL); err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{
		"event":    normalizeNotificationEvent(payload.Event),
		"alert_id": payload.Alert.ID, "rule_name": payload.Rule.Name,
		"device_id": payload.Device.ID, "device_ip": payload.Device.IP,
		"hostname": payload.Device.Hostname, "metric": payload.Alert.Metric,
		"value": payload.Alert.Value, "threshold": payload.Alert.Threshold,
		"severity": payload.Alert.Severity, "message": notificationText(payload),
		"triggered_at": payload.Alert.TriggeredAt.Format(time.RFC3339),
		"resolved_at":  payload.Alert.ResolvedAt,
		"clear_reason": payload.ClearReason,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, payload.Rule.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := webhookClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned HTTP %d", resp.StatusCode)
	}
	return nil
}

func (m *Manager) sendZabbix(ctx context.Context, payload notificationPayload) error {
	var server, senderHost string
	if err := m.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='zabbix_server'`).Scan(&server); err != nil || strings.TrimSpace(server) == "" {
		return fmt.Errorf("zabbix_server is not configured")
	}
	_ = m.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='zabbix_sender_host'`).Scan(&senderHost)
	if strings.TrimSpace(senderHost) == "" {
		senderHost = "wavecontrol"
	}
	if _, _, err := net.SplitHostPort(server); err != nil {
		server = net.JoinHostPort(server, "10051")
	}
	value, _ := json.Marshal(map[string]any{
		"event":    normalizeNotificationEvent(payload.Event),
		"alert_id": payload.Alert.ID, "severity": payload.Alert.Severity,
		"device_id": payload.Device.ID, "device_ip": payload.Device.IP,
		"metric": payload.Alert.Metric, "value": payload.Alert.Value,
		"message": notificationText(payload), "resolved_at": payload.Alert.ResolvedAt,
	})
	request, _ := json.Marshal(map[string]any{
		"request": "sender data",
		"data":    []map[string]string{{"host": senderHost, "key": "wavecontrol.alert", "value": string(value)}},
	})
	packet := make([]byte, 13+len(request))
	copy(packet[:5], []byte{'Z', 'B', 'X', 'D', 1})
	binary.LittleEndian.PutUint64(packet[5:13], uint64(len(request)))
	copy(packet[13:], request)

	d := net.Dialer{Timeout: 8 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", server)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write(packet); err != nil {
		return err
	}
	header := make([]byte, 13)
	if _, err := io.ReadFull(conn, header); err != nil {
		return err
	}
	if string(header[:4]) != "ZBXD" || header[4] != 1 {
		return fmt.Errorf("invalid Zabbix response header")
	}
	length := binary.LittleEndian.Uint64(header[5:13])
	if length > 1<<20 {
		return fmt.Errorf("oversized Zabbix response")
	}
	response := make([]byte, int(length))
	if _, err := io.ReadFull(conn, response); err != nil {
		return err
	}
	var parsed struct {
		Response string `json:"response"`
		Info     string `json:"info"`
	}
	if err := json.Unmarshal(response, &parsed); err != nil {
		return err
	}
	if parsed.Response != "success" {
		return fmt.Errorf("Zabbix rejected alert: %s", parsed.Info)
	}
	return nil
}

func normalizeNotificationEvent(event string) string {
	if strings.EqualFold(strings.TrimSpace(event), "resolved") {
		return "resolved"
	}
	return "triggered"
}

func notificationText(payload notificationPayload) string {
	if normalizeNotificationEvent(payload.Event) != "resolved" {
		return payload.Alert.Message
	}
	host := strings.TrimSpace(payload.Device.Hostname)
	if host == "" {
		host = strings.TrimSpace(payload.Device.IP)
	}
	if host == "" {
		host = fmt.Sprintf("device %d", payload.Device.ID)
	}
	reason := strings.TrimSpace(payload.ClearReason)
	if reason == "" {
		reason = "condition returned to normal"
	}
	return fmt.Sprintf("%s cleared on %s: %s", payload.Rule.Name, host, reason)
}

func sysmonProtocolStatus(payload notificationPayload) string {
	if normalizeNotificationEvent(payload.Event) == "resolved" {
		return "OK"
	}
	if strings.EqualFold(payload.Alert.Severity, SeverityCritical) {
		return "CRITICAL"
	}
	// sysmon-web has no INFO verb. WARNING and OK are both quiet; mapping
	// WaveControl info/warning to WARNING preserves quiet delivery semantics.
	return "WARNING"
}

func sysmonObject(payload notificationPayload) string {
	ruleID := 0
	if payload.Alert.RuleID != nil {
		ruleID = *payload.Alert.RuleID
	} else if payload.Rule.ID > 0 {
		ruleID = payload.Rule.ID
	}
	object := fmt.Sprintf("device-%d-rule-%d", payload.Device.ID, ruleID)
	if !sysmonalerter.ValidProtocolName(object) {
		return fmt.Sprintf("alert-%d", payload.Alert.ID)
	}
	return object
}

func (m *Manager) sendSysmon(ctx context.Context, payload notificationPayload) error {
	if m.sysmonClient == nil {
		return errors.New("sysmon-web alerter client is unavailable")
	}
	return m.sysmonClient.Send(ctx, sysmonProtocolStatus(payload), sysmonObject(payload), notificationText(payload))
}

// NotificationChannelStatuses returns safe runtime readiness information for
// the alert editor. It never returns SMTP passwords, sysmon tokens, or PEM.
func (m *Manager) NotificationChannelStatuses(ctx context.Context) ([]NotificationChannelStatus, error) {
	m.mu.RLock()
	smtpCfg := m.smtpConfig
	if smtpCfg != nil {
		copyCfg := *smtpCfg
		smtpCfg = &copyCfg
	}
	m.mu.RUnlock()

	statuses := []NotificationChannelStatus{
		{
			Channel: "webhook", Label: "Webhook", Configured: true, Enabled: true,
			Description: "Destination and HTTPS URL are configured on each alert rule.",
		},
		{
			Channel: "email", Label: "Email", Configured: smtpCfg != nil && strings.TrimSpace(smtpCfg.From) != "", Enabled: true,
			Description: "Uses the SMTP server and sender configured by an administrator.",
		},
	}

	var zabbixServer string
	err := m.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='zabbix_server'`).Scan(&zabbixServer)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	statuses = append(statuses, NotificationChannelStatus{
		Channel: "zabbix", Label: "Zabbix sender", Configured: strings.TrimSpace(zabbixServer) != "", Enabled: true,
		Description: "Sends wavecontrol.alert through the configured Zabbix trapper endpoint.",
	})

	if m.sysmonClient != nil {
		status := m.sysmonClient.Status()
		statuses = append(statuses, NotificationChannelStatus{
			Channel: "sysmon", Label: "sysmon-web", Configured: status.Configured, Enabled: status.Enabled,
			Description:  "Forwards CRITICAL/WARNING/OK events through sysmon-web's TLS alerter protocol.",
			SysmonStatus: &status,
		})
	} else {
		statuses = append(statuses, NotificationChannelStatus{
			Channel: "sysmon", Label: "sysmon-web", Configured: false, Enabled: false,
			Description: "The sysmon-web alerter client is unavailable.",
		})
	}
	return statuses, nil
}

func (m *Manager) TestSysmon(ctx context.Context) error {
	if m.sysmonClient == nil {
		return errors.New("sysmon-web alerter client is unavailable")
	}
	return m.sysmonClient.Test(ctx)
}
