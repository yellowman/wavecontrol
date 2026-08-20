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
)

type notificationDevice struct {
	ID       int    `json:"id"`
	SiteID   int    `json:"site_id,omitempty"`
	IP       string `json:"ip,omitempty"`
	MAC      string `json:"mac,omitempty"`
	Hostname string `json:"hostname,omitempty"`
}

type notificationPayload struct {
	Rule   Rule               `json:"rule"`
	Alert  Alert              `json:"alert"`
	Device notificationDevice `json:"device"`
}

type outboxItem struct {
	ID       int64
	AlertID  int
	Channel  string
	Payload  notificationPayload
	Attempts int
}

func (m *Manager) wakeNotificationWorker() {
	select {
	case m.notificationCh <- struct{}{}:
	default:
	}
}

func (m *Manager) runNotificationWorker(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	m.wakeNotificationWorker()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-m.notificationCh:
		}
		for i := 0; i < 32; i++ {
			item, ok, err := m.claimNotification(ctx)
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
	}
}

func (m *Manager) claimNotification(ctx context.Context) (outboxItem, bool, error) {
	var item outboxItem
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return item, false, err
	}
	defer tx.Rollback()

	// A process may die after claiming a row. Recover abandoned claims rather
	// than leaving them in "sending" forever.
	if _, err := tx.ExecContext(ctx, `
		UPDATE alert_notification_outbox
		SET status='failed', last_error='recovered stale sending claim', next_attempt_at=NOW(), updated_at=NOW()
		WHERE status='sending' AND updated_at < NOW() - INTERVAL '5 minutes'
	`); err != nil {
		return item, false, err
	}

	var raw []byte
	var attempts int
	err = tx.QueryRowContext(ctx, `
		SELECT id, alert_id, channel, payload, attempts
		FROM alert_notification_outbox
		WHERE status IN ('pending','failed') AND next_attempt_at <= NOW() AND attempts < 8
		ORDER BY next_attempt_at, id
		FOR UPDATE SKIP LOCKED
		LIMIT 1
	`).Scan(&item.ID, &item.AlertID, &item.Channel, &raw, &attempts)
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
	if _, err := tx.ExecContext(ctx, `
		UPDATE alerts a SET notified_at=NOW(), notify_error=NULL
		WHERE a.id=$1 AND NOT EXISTS (
		  SELECT 1 FROM alert_notification_outbox o
		  WHERE o.alert_id=a.id AND o.status <> 'sent'
		)
	`, item.AlertID); err != nil {
		return err
	}
	return tx.Commit()
}

func (m *Manager) markNotificationFailed(ctx context.Context, item outboxItem, deliveryErr error) error {
	status := "failed"
	if item.Attempts >= 8 {
		status = "dead"
	}
	backoff := time.Duration(1<<min(item.Attempts-1, 8)) * 30 * time.Second
	if backoff > 30*time.Minute {
		backoff = 30 * time.Minute
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	message := truncateError(deliveryErr.Error(), 2000)
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
	if _, err := tx.ExecContext(ctx, `UPDATE alerts SET notify_error=$1 WHERE id=$2`, message, item.AlertID); err != nil {
		return err
	}
	return tx.Commit()
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
	subject := strings.NewReplacer("\r", " ", "\n", " ").Replace(fmt.Sprintf("[%s] %s", payload.Alert.Severity, payload.Rule.Name))
	body := fmt.Sprintf("Alert: %s\r\n\r\nDevice: %s (%s)\r\nMetric: %s\r\nValue: %.2f\r\nThreshold: %.2f\r\nTime: %s\r\n\r\nMessage: %s\r\n",
		payload.Rule.Name, host, payload.Device.IP, payload.Alert.Metric, payload.Alert.Value,
		payload.Alert.Threshold, payload.Alert.TriggeredAt.Format(time.RFC3339), payload.Alert.Message)
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
		"alert_id": payload.Alert.ID, "rule_name": payload.Rule.Name,
		"device_id": payload.Device.ID, "device_ip": payload.Device.IP,
		"hostname": payload.Device.Hostname, "metric": payload.Alert.Metric,
		"value": payload.Alert.Value, "threshold": payload.Alert.Threshold,
		"severity": payload.Alert.Severity, "message": payload.Alert.Message,
		"triggered_at": payload.Alert.TriggeredAt.Format(time.RFC3339),
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
		"alert_id": payload.Alert.ID, "severity": payload.Alert.Severity,
		"device_id": payload.Device.ID, "device_ip": payload.Device.IP,
		"metric": payload.Alert.Metric, "value": payload.Alert.Value,
		"message": payload.Alert.Message,
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
