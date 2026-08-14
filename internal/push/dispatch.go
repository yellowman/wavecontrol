package push

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"
)

type outboxRow struct {
	ID             string
	UserID         int64
	MobileDeviceID string
	Platform       string
	Provider       string
	TokenEnc       string
	DeviceEnabled  bool
	EventType      string
	Severity       string
	Payload        []byte
	Attempts       int
}

// Start runs the retrying push dispatcher until ctx is cancelled.
func (s *Service) Start(ctx context.Context) {
	interval := 5 * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		s.dispatchOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) dispatchOnce(ctx context.Context) {
	rows, err := s.claimDue(ctx, 50)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("mobile push claim failed: %v", err)
		}
		return
	}
	for _, row := range rows {
		s.dispatchRow(ctx, row)
	}
}

func (s *Service) claimDue(ctx context.Context, limit int) ([]outboxRow, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		WITH picked AS (
			SELECT id
			FROM notification_outbox
			WHERE status IN ('pending', 'failed')
			  AND next_attempt_at <= NOW()
			  AND attempts < 8
			ORDER BY created_at ASC
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		UPDATE notification_outbox n
		SET status = 'sending', attempts = attempts + 1, updated_at = NOW()
		FROM picked, mobile_devices md
		WHERE n.id = picked.id AND md.id = n.mobile_device_id
		RETURNING n.id::text, n.user_id, n.mobile_device_id::text,
		          md.platform, md.provider, md.token_encrypted, md.enabled,
		          n.event_type, n.severity, n.payload, n.attempts
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []outboxRow
	for rows.Next() {
		var r outboxRow
		if err := rows.Scan(&r.ID, &r.UserID, &r.MobileDeviceID, &r.Platform, &r.Provider, &r.TokenEnc, &r.DeviceEnabled, &r.EventType, &r.Severity, &r.Payload, &r.Attempts); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Service) dispatchRow(ctx context.Context, row outboxRow) {
	if !row.DeviceEnabled {
		s.finishRow(ctx, row.ID, "dead", "mobile device disabled", "")
		return
	}
	var msg Message
	if err := json.Unmarshal(row.Payload, &msg); err != nil {
		s.finishRow(ctx, row.ID, "dead", "invalid payload: "+err.Error(), "")
		return
	}
	token, err := s.decryptToken(row.TokenEnc)
	if err != nil {
		s.finishRow(ctx, row.ID, "dead", "token decrypt failed: "+err.Error(), "")
		return
	}
	providerName := strings.ToLower(row.Provider)
	provider := s.providers[providerName]
	if provider == nil {
		s.finishRow(ctx, row.ID, "dead", "unknown push provider: "+providerName, "")
		return
	}
	res := provider.Send(ctx, MobileDevice{ID: row.MobileDeviceID, UserID: row.UserID, Platform: row.Platform, Provider: row.Provider, Token: token}, msg)
	if res.Err == nil {
		s.finishRow(ctx, row.ID, "sent", "", res.ProviderMessageID)
		_, _ = s.db.ExecContext(ctx, `UPDATE mobile_devices SET last_seen_at = NOW(), last_error = NULL, updated_at = NOW() WHERE id = $1::uuid`, row.MobileDeviceID)
		return
	}
	if res.Terminal {
		s.finishRow(ctx, row.ID, "dead", res.Err.Error(), res.ProviderMessageID)
		_, _ = s.db.ExecContext(ctx, `UPDATE mobile_devices SET enabled = false, last_error = $1, updated_at = NOW() WHERE id = $2::uuid`, res.Err.Error(), row.MobileDeviceID)
		return
	}
	nextDelay := retryDelay(row.Attempts)
	_, err = s.db.ExecContext(ctx, `
		UPDATE notification_outbox
		SET status = 'failed', error = $1, provider_message_id = NULL,
		    next_attempt_at = NOW() + ($2::text)::interval,
		    updated_at = NOW()
		WHERE id = $3::uuid
	`, res.Err.Error(), fmt.Sprintf("%d seconds", int(nextDelay.Seconds())), row.ID)
	if err != nil {
		log.Printf("mobile push retry update failed: %v", err)
	}
}

func retryDelay(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	seconds := 30 * (1 << min(attempts-1, 6))
	if seconds > 30*60 {
		seconds = 30 * 60
	}
	return time.Duration(seconds) * time.Second
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (s *Service) finishRow(ctx context.Context, id string, status string, errText string, providerMessageID string) {
	var sentExpr string
	if status == "sent" {
		sentExpr = ", sent_at = NOW()"
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE notification_outbox
		SET status = $1,
		    error = NULLIF($2, ''),
		    provider_message_id = NULLIF($3, ''),
		    updated_at = NOW()`+sentExpr+`
		WHERE id = $4::uuid
	`, status, errText, providerMessageID, id)
	if err != nil {
		log.Printf("mobile push outbox update failed: %v", err)
	}
}
