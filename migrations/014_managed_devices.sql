-- 014_managed_devices.sql
--
-- Adds a "managed" flag to devices.
--
-- managed=true means the device was explicitly added via Add IP / Add Bulk and should:
--  - Always be eligible for direct polling (even if it is a STA and has a parent_id)
--  - Be rendered at the root level in the dashboard hierarchy (UI behavior)
--
ALTER TABLE devices
    ADD COLUMN IF NOT EXISTS managed BOOLEAN NOT NULL DEFAULT FALSE;

-- Optional helper index (tiny, but useful when filtering poll targets)
CREATE INDEX IF NOT EXISTS idx_devices_managed ON devices(managed);
