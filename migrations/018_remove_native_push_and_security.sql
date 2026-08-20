-- Remove the abandoned native APNs/FCM subsystem and add the durable alert
-- delivery/authentication columns required by the remediated server.
BEGIN;

ALTER TABLE users
  ADD COLUMN IF NOT EXISTS auth_version BIGINT NOT NULL DEFAULT 1;

ALTER TABLE devices ALTER COLUMN password TYPE TEXT;
ALTER TABLE drilldown_hosts
  ADD COLUMN IF NOT EXISTS username VARCHAR(64),
  ADD COLUMN IF NOT EXISTS password TEXT;
ALTER TABLE drilldown_hosts ALTER COLUMN password TYPE TEXT;

ALTER TABLE alerts
  ADD COLUMN IF NOT EXISTS notified_at TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS notify_error TEXT;

CREATE TABLE IF NOT EXISTS alert_notification_outbox (
  id BIGSERIAL PRIMARY KEY,
  alert_id INTEGER NOT NULL REFERENCES alerts(id) ON DELETE CASCADE,
  channel VARCHAR(16) NOT NULL CHECK (channel IN ('email', 'webhook', 'zabbix')),
  payload JSONB NOT NULL,
  status VARCHAR(16) NOT NULL DEFAULT 'pending'
    CHECK (status IN ('pending', 'sending', 'failed', 'sent', 'dead')),
  attempts INTEGER NOT NULL DEFAULT 0,
  next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  last_error TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  sent_at TIMESTAMPTZ,
  UNIQUE(alert_id, channel)
);

CREATE INDEX IF NOT EXISTS idx_alert_notification_outbox_due
  ON alert_notification_outbox(status, next_attempt_at, id);

DROP TABLE IF EXISTS notification_outbox;
DROP TABLE IF EXISTS mobile_push_preferences;
DROP TABLE IF EXISTS mobile_devices;

DELETE FROM settings WHERE key IN (
  'mobile_push_enabled', 'fcm_enabled', 'fcm_project_id',
  'fcm_service_account_json', 'apns_enabled', 'apns_team_id',
  'apns_key_id', 'apns_bundle_id', 'apns_private_key_p8',
  'apns_production'
);

UPDATE alert_rules
SET notify_channels = array_remove(COALESCE(notify_channels, ARRAY[]::TEXT[]), 'mobile')
WHERE 'mobile' = ANY(COALESCE(notify_channels, ARRAY[]::TEXT[]));

COMMIT;
