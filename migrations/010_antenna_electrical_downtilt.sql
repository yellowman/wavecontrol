-- 010_antenna_electrical_downtilt.sql
-- Adds electrical downtilt for antennas (many sectors include built-in electrical tilt).
-- Mechanical downtilt remains in antenna_downtilt_deg.

ALTER TABLE devices
  ADD COLUMN IF NOT EXISTS antenna_electrical_downtilt_deg DOUBLE PRECISION DEFAULT 0;
