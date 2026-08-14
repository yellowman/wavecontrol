-- 008_status_reason.sql
-- Add optional status_reason field for unknown/offline state context

ALTER TABLE devices
    ADD COLUMN IF NOT EXISTS status_reason VARCHAR(128);
