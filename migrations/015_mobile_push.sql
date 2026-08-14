-- Native mobile client push notifications.
-- Server remains alert authority; Android/iOS clients register push tokens and reconcile with REST/WebSocket.

CREATE EXTENSION IF NOT EXISTS pgcrypto;

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
