package main

import (
	"database/sql"
	"fmt"
	"strings"
)

// ensureRuntimeSchema brings older installations to the minimum schema the
// current binary requires. These migrations are idempotent, transactional, and
// fail startup on error; running with a partially upgraded schema is unsafe.
func ensureRuntimeSchema(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	statements := []string{
		`INSERT INTO roles (name) VALUES ('administrator'), ('creator'), ('editor'), ('viewer') ON CONFLICT (name) DO NOTHING`,
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS auth_version BIGINT NOT NULL DEFAULT 1`,
		`ALTER TABLE devices
		  ADD COLUMN IF NOT EXISTS managed BOOLEAN NOT NULL DEFAULT FALSE,
		  ADD COLUMN IF NOT EXISTS status_reason VARCHAR(128),
		  ADD COLUMN IF NOT EXISTS antenna_model VARCHAR(64),
		  ADD COLUMN IF NOT EXISTS antenna_override BOOLEAN DEFAULT FALSE,
		  ADD COLUMN IF NOT EXISTS antenna_azimuth_deg DOUBLE PRECISION,
		  ADD COLUMN IF NOT EXISTS antenna_downtilt_deg DOUBLE PRECISION,
		  ADD COLUMN IF NOT EXISTS antenna_electrical_downtilt_deg DOUBLE PRECISION DEFAULT 0,
		  ADD COLUMN IF NOT EXISTS antenna_beamwidth_h_deg DOUBLE PRECISION,
		  ADD COLUMN IF NOT EXISTS antenna_beamwidth_v_deg DOUBLE PRECISION,
		  ADD COLUMN IF NOT EXISTS radius_m DOUBLE PRECISION,
		  ADD COLUMN IF NOT EXISTS tech INTEGER,
		  ADD COLUMN IF NOT EXISTS down_mbps DOUBLE PRECISION,
		  ADD COLUMN IF NOT EXISTS up_mbps DOUBLE PRECISION,
		  ADD COLUMN IF NOT EXISTS latency_ms DOUBLE PRECISION,
		  ADD COLUMN IF NOT EXISTS bizres VARCHAR(1),
		  ADD COLUMN IF NOT EXISTS alertable BOOLEAN NOT NULL DEFAULT TRUE,
		  ADD COLUMN IF NOT EXISTS alert_silenced_until TIMESTAMPTZ,
		  ADD COLUMN IF NOT EXISTS alert_notes TEXT`,
		`ALTER TABLE devices ALTER COLUMN password TYPE TEXT`,
		`ALTER TABLE sites ADD COLUMN IF NOT EXISTS tower_h_m DOUBLE PRECISION`,
		`ALTER TABLE alert_rules
		  ADD COLUMN IF NOT EXISTS target_role VARCHAR(16) NOT NULL DEFAULT 'all',
		  ADD COLUMN IF NOT EXISTS require_alertable BOOLEAN NOT NULL DEFAULT TRUE,
		  ADD COLUMN IF NOT EXISTS severity VARCHAR(20) NOT NULL DEFAULT 'auto',
		  ADD COLUMN IF NOT EXISTS notify_recovery BOOLEAN NOT NULL DEFAULT TRUE`,
		`ALTER TABLE alerts
		  ADD COLUMN IF NOT EXISTS notified_at TIMESTAMPTZ,
		  ADD COLUMN IF NOT EXISTS notify_error TEXT,
		  ADD COLUMN IF NOT EXISTS recovery_notified_at TIMESTAMPTZ,
		  ADD COLUMN IF NOT EXISTS recovery_notify_error TEXT`,
		`UPDATE alert_rules SET target_role = 'all' WHERE target_role IS NULL OR btrim(target_role) = ''`,
		`UPDATE alert_rules SET severity = 'auto' WHERE severity IS NULL OR severity NOT IN ('auto','info','warning','critical')`,
		`DO $$ BEGIN
		  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname='alert_rules_severity_check') THEN
		    ALTER TABLE alert_rules ADD CONSTRAINT alert_rules_severity_check
		      CHECK (severity IN ('auto','info','warning','critical'));
		  END IF;
		END $$`,
		`CREATE INDEX IF NOT EXISTS idx_devices_managed ON devices(managed)`,
		`CREATE INDEX IF NOT EXISTS idx_devices_alertable ON devices(alertable)`,
		`CREATE INDEX IF NOT EXISTS idx_devices_role_alertable ON devices(role, alertable)`,
		`CREATE INDEX IF NOT EXISTS idx_devices_alert_silenced_until ON devices(alert_silenced_until)`,
		`CREATE INDEX IF NOT EXISTS idx_alert_rules_target_role ON alert_rules(target_role)`,
		`CREATE INDEX IF NOT EXISTS idx_alert_rules_require_alertable ON alert_rules(require_alertable)`,
		`ALTER TABLE scheduled_jobs
		  ADD COLUMN IF NOT EXISTS progress INTEGER DEFAULT 0,
		  ADD COLUMN IF NOT EXISTS total_devices INTEGER DEFAULT 0,
		  ADD COLUMN IF NOT EXISTS completed_devices INTEGER DEFAULT 0,
		  ADD COLUMN IF NOT EXISTS error_message TEXT`,
		`ALTER TABLE drilldown_hosts
		  ADD COLUMN IF NOT EXISTS username VARCHAR(64),
		  ADD COLUMN IF NOT EXISTS password TEXT`,
		`ALTER TABLE drilldown_hosts ALTER COLUMN password TYPE TEXT`,
		`CREATE TABLE IF NOT EXISTS device_identity_mismatches (
		    device_id INTEGER PRIMARY KEY REFERENCES devices(id) ON DELETE CASCADE,
		    expected_mac VARCHAR(17) NOT NULL,
		    observed_macs TEXT[] NOT NULL,
		    observed_ip INET NOT NULL,
		    source VARCHAR(32),
		    observed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		    last_error TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_device_identity_mismatches_observed_ip ON device_identity_mismatches(observed_ip)`,
		`CREATE INDEX IF NOT EXISTS idx_device_identity_mismatches_observed_at ON device_identity_mismatches(observed_at DESC)`,
		`CREATE TABLE IF NOT EXISTS alert_notification_outbox (
		    id BIGSERIAL PRIMARY KEY,
		    alert_id INTEGER NOT NULL REFERENCES alerts(id) ON DELETE CASCADE,
		    channel VARCHAR(16) NOT NULL CHECK (channel IN ('email', 'webhook', 'zabbix', 'sysmon')),
		    event VARCHAR(16) NOT NULL DEFAULT 'triggered' CHECK (event IN ('triggered', 'resolved')),
		    payload JSONB NOT NULL,
		    status VARCHAR(16) NOT NULL DEFAULT 'pending'
		      CHECK (status IN ('pending', 'sending', 'failed', 'sent', 'dead')),
		    attempts INTEGER NOT NULL DEFAULT 0,
		    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		    last_error TEXT,
		    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		    sent_at TIMESTAMPTZ,
		    CONSTRAINT alert_notification_outbox_alert_channel_event_key UNIQUE(alert_id, channel, event)
		)`,
		`ALTER TABLE alert_notification_outbox ADD COLUMN IF NOT EXISTS event VARCHAR(16) NOT NULL DEFAULT 'triggered'`,
		`ALTER TABLE alert_notification_outbox DROP CONSTRAINT IF EXISTS alert_notification_outbox_alert_id_channel_key`,
		`ALTER TABLE alert_notification_outbox DROP CONSTRAINT IF EXISTS alert_notification_outbox_channel_check`,
		`ALTER TABLE alert_notification_outbox DROP CONSTRAINT IF EXISTS alert_notification_outbox_event_check`,
		`DO $$ BEGIN
		  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname='alert_notification_outbox_channel_check') THEN
		    ALTER TABLE alert_notification_outbox ADD CONSTRAINT alert_notification_outbox_channel_check
		      CHECK (channel IN ('email','webhook','zabbix','sysmon'));
		  END IF;
		END $$`,
		`DO $$ BEGIN
		  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname='alert_notification_outbox_event_check') THEN
		    ALTER TABLE alert_notification_outbox ADD CONSTRAINT alert_notification_outbox_event_check
		      CHECK (event IN ('triggered','resolved'));
		  END IF;
		END $$`,
		`DO $$ BEGIN
		  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname IN ('alert_notification_outbox_alert_channel_event_key','alert_notification_outbox_alert_id_channel_event_key')) THEN
		    ALTER TABLE alert_notification_outbox ADD CONSTRAINT alert_notification_outbox_alert_channel_event_key
		      UNIQUE (alert_id, channel, event);
		  END IF;
		END $$`,
		`CREATE INDEX IF NOT EXISTS idx_alert_notification_outbox_due
		  ON alert_notification_outbox(status, next_attempt_at, id)`,
		// Native APNs/FCM delivery is intentionally out of scope. Remove the
		// abandoned schema and provider credentials from upgraded installations.
		`DROP TABLE IF EXISTS notification_outbox`,
		`DROP TABLE IF EXISTS mobile_push_preferences`,
		`DROP TABLE IF EXISTS mobile_devices`,
		`DELETE FROM settings WHERE key IN (
		  'mobile_push_enabled', 'fcm_enabled', 'fcm_project_id',
		  'fcm_service_account_json', 'apns_enabled', 'apns_team_id',
		  'apns_key_id', 'apns_bundle_id', 'apns_private_key_p8',
		  'apns_production'
		)`,
		`UPDATE alert_rules
		 SET notify_channels = array_remove(COALESCE(notify_channels, ARRAY[]::TEXT[]), 'mobile')
		 WHERE 'mobile' = ANY(COALESCE(notify_channels, ARRAY[]::TEXT[]))`,
		`UPDATE alerts a SET status='resolved', resolved_at=COALESCE(resolved_at, NOW())
		 FROM alert_rules r
		 WHERE a.rule_id=r.id AND r.enabled=false AND a.status IN ('active','acknowledged')`,
		`DELETE FROM alert_states s USING alert_rules r
		 WHERE s.rule_id=r.id AND r.enabled=false`,
	}

	for i, stmt := range statements {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("runtime schema step %d failed: %w", i+1, err)
		}
	}
	return tx.Commit()
}

// validateRuntimeSchema verifies every table and column the current binary
// relies on after migrations have completed. This prevents a superficially
// healthy process from serving handlers that can only fail at runtime.
func validateRuntimeSchema(db *sql.DB) error {
	required := map[string][]string{
		"roles":                      {"id", "name"},
		"users":                      {"id", "username", "password", "status", "auth_version"},
		"user_roles":                 {"user", "role"},
		"settings":                   {"key", "value"},
		"sites":                      {"id", "name", "tower_h_m"},
		"devices":                    {"id", "mac", "ip_address", "role", "managed", "alertable", "alert_silenced_until", "username", "password", "status"},
		"scheduled_jobs":             {"id", "status", "progress", "total_devices", "completed_devices", "error_message"},
		"alert_rules":                {"id", "enabled", "scope", "scope_id", "target_role", "require_alertable", "metric", "operator", "threshold", "severity", "notify_channels", "notify_recovery"},
		"alerts":                     {"id", "rule_id", "device_id", "status", "triggered_at", "resolved_at", "notified_at", "notify_error", "recovery_notified_at", "recovery_notify_error"},
		"alert_states":               {"rule_id", "device_id", "first_triggered_at", "last_value", "last_checked_at", "notified"},
		"alert_notification_outbox":  {"id", "alert_id", "channel", "event", "payload", "status", "attempts", "next_attempt_at", "last_error", "updated_at", "sent_at"},
		"device_identity_mismatches": {"device_id", "expected_mac", "observed_macs", "observed_ip"},
		"reports":                    {"id", "type", "data"},
		"job_runs":                   {"id", "job_type", "status"},
		"job_events":                 {"id", "job_id", "event_type"},
		"maintenance_windows":        {"id", "scope", "start_time", "end_time"},
		"drilldown_lists":            {"id", "name", "enabled"},
		"drilldown_hosts":            {"id", "list_id", "host", "password"},
		"device_certs":               {"device_id"},
	}
	missing := make([]string, 0)
	for table, columns := range required {
		for _, column := range columns {
			var exists bool
			if err := db.QueryRow(`
				SELECT EXISTS (
					SELECT 1 FROM information_schema.columns
					WHERE table_schema='public' AND table_name=$1 AND column_name=$2
				)
			`, table, column).Scan(&exists); err != nil {
				return fmt.Errorf("validate %s.%s: %w", table, column, err)
			}
			if !exists {
				missing = append(missing, table+"."+column)
			}
		}
	}
	if len(missing) != 0 {
		return fmt.Errorf("database schema is out of date (missing: %s)", strings.Join(missing, ", "))
	}
	return nil
}
