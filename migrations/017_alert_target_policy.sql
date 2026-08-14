-- Alert target policy: per-device alertability plus rule-level role filters.

ALTER TABLE devices
  ADD COLUMN IF NOT EXISTS alertable BOOLEAN NOT NULL DEFAULT TRUE,
  ADD COLUMN IF NOT EXISTS alert_silenced_until TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS alert_notes TEXT;

-- Auto-discovered STAs are noisy by default. Directly managed devices remain alertable.
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
