BEGIN;

ALTER TABLE alert_rules
    ADD COLUMN IF NOT EXISTS severity VARCHAR(20) NOT NULL DEFAULT 'auto',
    ADD COLUMN IF NOT EXISTS notify_recovery BOOLEAN NOT NULL DEFAULT TRUE;

UPDATE alert_rules
SET severity = 'auto'
WHERE severity IS NULL OR severity NOT IN ('auto', 'info', 'warning', 'critical');

ALTER TABLE alert_rules
    DROP CONSTRAINT IF EXISTS alert_rules_severity_check;
ALTER TABLE alert_rules
    ADD CONSTRAINT alert_rules_severity_check
        CHECK (severity IN ('auto', 'info', 'warning', 'critical'));

ALTER TABLE alerts
    ADD COLUMN IF NOT EXISTS recovery_notified_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS recovery_notify_error TEXT;

ALTER TABLE alert_notification_outbox
    ADD COLUMN IF NOT EXISTS event VARCHAR(16) NOT NULL DEFAULT 'triggered';

ALTER TABLE alert_notification_outbox
    DROP CONSTRAINT IF EXISTS alert_notification_outbox_alert_id_channel_key,
    DROP CONSTRAINT IF EXISTS alert_notification_outbox_channel_check,
    DROP CONSTRAINT IF EXISTS alert_notification_outbox_event_check;

ALTER TABLE alert_notification_outbox
    ADD CONSTRAINT alert_notification_outbox_channel_check
        CHECK (channel IN ('email', 'webhook', 'zabbix', 'sysmon')),
    ADD CONSTRAINT alert_notification_outbox_event_check
        CHECK (event IN ('triggered', 'resolved'));

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname IN ('alert_notification_outbox_alert_channel_event_key', 'alert_notification_outbox_alert_id_channel_event_key')
    ) THEN
        ALTER TABLE alert_notification_outbox
            ADD CONSTRAINT alert_notification_outbox_alert_channel_event_key
            UNIQUE (alert_id, channel, event);
    END IF;
END $$;

COMMIT;
