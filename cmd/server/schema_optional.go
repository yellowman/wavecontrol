package main

import (
	"database/sql"
	"log"
)

// ensureDeviceStatusReasonColumn adds devices.status_reason if it does not exist.
//
// This keeps the daemon backward-compatible with databases created from older schema
// without requiring an immediate manual migration.
func ensureDeviceStatusReasonColumn(db *sql.DB) {
	var exists bool
	err := db.QueryRow(`
		SELECT EXISTS (
			SELECT 1
			FROM information_schema.columns
			WHERE table_schema = 'public'
				AND table_name = 'devices'
				AND column_name = 'status_reason'
		)
	`).Scan(&exists)
	if err != nil {
		log.Printf("WARN: schema check for devices.status_reason failed: %v", err)
		return
	}
	if exists {
		return
	}

	if _, err := db.Exec(`ALTER TABLE devices ADD COLUMN IF NOT EXISTS status_reason VARCHAR(128)`); err != nil {
		log.Printf("WARN: failed adding devices.status_reason: %v", err)
		return
	}
	log.Printf("WARN: added missing devices.status_reason column")
}

// ensureDeviceAntennaColumns adds optional antenna modeling fields to devices if they do not exist.
//
// This keeps the daemon backward-compatible with databases created from older schema
// without requiring an immediate manual migration.
func ensureDeviceAntennaColumns(db *sql.DB) {
	// Run a single idempotent ALTER TABLE with IF NOT EXISTS so partial/mixed schema
	// states are corrected safely.
	if _, err := db.Exec(`
		ALTER TABLE devices
		  ADD COLUMN IF NOT EXISTS antenna_model VARCHAR(64),
		  ADD COLUMN IF NOT EXISTS antenna_override BOOLEAN DEFAULT FALSE,
		  ADD COLUMN IF NOT EXISTS antenna_azimuth_deg DOUBLE PRECISION,
		  ADD COLUMN IF NOT EXISTS antenna_downtilt_deg DOUBLE PRECISION,
		  ADD COLUMN IF NOT EXISTS antenna_electrical_downtilt_deg DOUBLE PRECISION DEFAULT 0,
		  ADD COLUMN IF NOT EXISTS antenna_beamwidth_h_deg DOUBLE PRECISION,
		  ADD COLUMN IF NOT EXISTS antenna_beamwidth_v_deg DOUBLE PRECISION
	`); err != nil {
		log.Printf("WARN: failed ensuring devices antenna columns: %v", err)
	}
}

// ensureDeviceSectorPlanningColumns adds optional sector planning/export fields to devices.
//
// These are used for CSV export and future RF modeling/planning. They are optional
// and are safe to add to existing deployments.
func ensureDeviceSectorPlanningColumns(db *sql.DB) {
	if _, err := db.Exec(`
		ALTER TABLE devices
		  ADD COLUMN IF NOT EXISTS radius_m DOUBLE PRECISION,
		  ADD COLUMN IF NOT EXISTS tech INTEGER,
		  ADD COLUMN IF NOT EXISTS down_mbps DOUBLE PRECISION,
		  ADD COLUMN IF NOT EXISTS up_mbps DOUBLE PRECISION,
		  ADD COLUMN IF NOT EXISTS latency_ms DOUBLE PRECISION,
		  ADD COLUMN IF NOT EXISTS bizres VARCHAR(1)
	`); err != nil {
		log.Printf("WARN: failed ensuring devices sector planning columns: %v", err)
	}
}

// ensureSiteTowerHeightColumn adds optional sites.tower_h_m (tower height meters).
func ensureSiteTowerHeightColumn(db *sql.DB) {
	if _, err := db.Exec(`ALTER TABLE sites ADD COLUMN IF NOT EXISTS tower_h_m DOUBLE PRECISION`); err != nil {
		log.Printf("WARN: failed ensuring sites.tower_h_m: %v", err)
	}
}

// ensureMobilePushSchema adds native mobile push registration and durable outbox tables.
func ensureMobilePushSchema(db *sql.DB) {
	if _, err := db.Exec(`CREATE EXTENSION IF NOT EXISTS pgcrypto`); err != nil {
		log.Printf("WARN: could not ensure pgcrypto extension for mobile push UUIDs: %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS mobile_devices (
		    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		    platform VARCHAR(16) NOT NULL CHECK (platform IN ('android', 'ios')),
		    provider VARCHAR(16) NOT NULL CHECK (provider IN ('fcm', 'apns')),
		    token_hash CHAR(64) NOT NULL,
		    token_encrypted TEXT NOT NULL,
		    device_name VARCHAR(128),
		    app_version VARCHAR(64),
		    os_version VARCHAR(64),
		    enabled BOOLEAN NOT NULL DEFAULT true,
		    last_seen_at TIMESTAMPTZ DEFAULT NOW(),
		    last_error TEXT,
		    created_at TIMESTAMPTZ DEFAULT NOW(),
		    updated_at TIMESTAMPTZ DEFAULT NOW(),
		    UNIQUE(user_id, platform, provider, token_hash)
		);

		CREATE TABLE IF NOT EXISTS mobile_push_preferences (
		    user_id INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
		    push_enabled BOOLEAN NOT NULL DEFAULT true,
		    notify_critical BOOLEAN NOT NULL DEFAULT true,
		    notify_warning BOOLEAN NOT NULL DEFAULT true,
		    notify_info BOOLEAN NOT NULL DEFAULT false,
		    quiet_hours_start TIME,
		    quiet_hours_end TIME,
		    timezone VARCHAR(64) NOT NULL DEFAULT 'UTC',
		    created_at TIMESTAMPTZ DEFAULT NOW(),
		    updated_at TIMESTAMPTZ DEFAULT NOW()
		);

		CREATE TABLE IF NOT EXISTS notification_outbox (
		    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		    seq BIGSERIAL UNIQUE,
		    alert_id INTEGER REFERENCES alerts(id) ON DELETE CASCADE,
		    user_id INTEGER REFERENCES users(id) ON DELETE CASCADE,
		    mobile_device_id UUID REFERENCES mobile_devices(id) ON DELETE CASCADE,
		    event_type VARCHAR(64) NOT NULL,
		    severity VARCHAR(20) NOT NULL,
		    dedupe_key TEXT NOT NULL,
		    collapse_key TEXT,
		    payload JSONB NOT NULL,
		    status VARCHAR(16) NOT NULL DEFAULT 'pending',
		    attempts INTEGER NOT NULL DEFAULT 0,
		    next_attempt_at TIMESTAMPTZ DEFAULT NOW(),
		    provider_message_id TEXT,
		    error TEXT,
		    created_at TIMESTAMPTZ DEFAULT NOW(),
		    updated_at TIMESTAMPTZ DEFAULT NOW(),
		    sent_at TIMESTAMPTZ,
		    UNIQUE(dedupe_key, mobile_device_id)
		);

		CREATE INDEX IF NOT EXISTS idx_mobile_devices_user ON mobile_devices(user_id);
		CREATE INDEX IF NOT EXISTS idx_mobile_devices_enabled ON mobile_devices(enabled);
		CREATE INDEX IF NOT EXISTS idx_notification_outbox_due ON notification_outbox(status, next_attempt_at, created_at);
		CREATE INDEX IF NOT EXISTS idx_notification_outbox_user ON notification_outbox(user_id, seq DESC);

		INSERT INTO settings (key, value) VALUES
		    ('mobile_push_enabled', 'true'),
		    ('fcm_enabled', 'false'),
		    ('fcm_project_id', ''),
		    ('fcm_service_account_json', ''),
		    ('apns_enabled', 'false'),
		    ('apns_team_id', ''),
		    ('apns_key_id', ''),
		    ('apns_bundle_id', ''),
		    ('apns_private_key_p8', ''),
		    ('apns_production', 'false')
		ON CONFLICT (key) DO NOTHING;
	`); err != nil {
		log.Printf("WARN: failed ensuring mobile push schema: %v", err)
	}
}

// ensureAlertTargetPolicySchema adds per-device alertability and rule-level target filters.
func ensureAlertTargetPolicySchema(db *sql.DB) {
	if _, err := db.Exec(`
		ALTER TABLE devices
		  ADD COLUMN IF NOT EXISTS alertable BOOLEAN NOT NULL DEFAULT TRUE,
		  ADD COLUMN IF NOT EXISTS alert_silenced_until TIMESTAMPTZ,
		  ADD COLUMN IF NOT EXISTS alert_notes TEXT;

		-- Auto-discovered STAs should not start paging operators unless explicitly promoted.
		UPDATE devices
		SET alertable = FALSE
		WHERE COALESCE(managed, FALSE) = FALSE
		  AND parent_id IS NOT NULL
		  AND alertable = TRUE;

		CREATE INDEX IF NOT EXISTS idx_devices_alertable ON devices(alertable);
		CREATE INDEX IF NOT EXISTS idx_devices_role_alertable ON devices(role, alertable);
		CREATE INDEX IF NOT EXISTS idx_devices_alert_silenced_until ON devices(alert_silenced_until);

		ALTER TABLE alert_rules
		  ADD COLUMN IF NOT EXISTS target_role VARCHAR(16) NOT NULL DEFAULT 'all',
		  ADD COLUMN IF NOT EXISTS require_alertable BOOLEAN NOT NULL DEFAULT TRUE;

		UPDATE alert_rules
		SET target_role = 'all'
		WHERE target_role IS NULL OR target_role = '';

		CREATE INDEX IF NOT EXISTS idx_alert_rules_target_role ON alert_rules(target_role);
		CREATE INDEX IF NOT EXISTS idx_alert_rules_require_alertable ON alert_rules(require_alertable);
	`); err != nil {
		log.Printf("WARN: failed ensuring alert target policy schema: %v", err)
	}
}

// ensureDeviceIdentityMismatchSchema adds persisted MAC mismatch records used by
// the explicit replacement-MAC adoption workflow.
func ensureDeviceIdentityMismatchSchema(db *sql.DB) {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS device_identity_mismatches (
		    device_id INTEGER PRIMARY KEY REFERENCES devices(id) ON DELETE CASCADE,
		    expected_mac VARCHAR(17) NOT NULL,
		    observed_macs TEXT[] NOT NULL,
		    observed_ip INET NOT NULL,
		    source VARCHAR(32),
		    observed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		    last_error TEXT
		);
		CREATE INDEX IF NOT EXISTS idx_device_identity_mismatches_observed_ip ON device_identity_mismatches(observed_ip);
		CREATE INDEX IF NOT EXISTS idx_device_identity_mismatches_observed_at ON device_identity_mismatches(observed_at DESC);
	`); err != nil {
		log.Printf("WARN: failed ensuring device identity mismatch schema: %v", err)
	}
}
