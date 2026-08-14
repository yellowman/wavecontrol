-- 009_antenna_params.sql
-- Optional antenna modeling fields for APs (future RF planning)

ALTER TABLE devices
  ADD COLUMN IF NOT EXISTS antenna_model VARCHAR(64),
  ADD COLUMN IF NOT EXISTS antenna_override BOOLEAN DEFAULT FALSE,
  ADD COLUMN IF NOT EXISTS antenna_azimuth_deg DOUBLE PRECISION,
  ADD COLUMN IF NOT EXISTS antenna_downtilt_deg DOUBLE PRECISION,
  ADD COLUMN IF NOT EXISTS antenna_beamwidth_h_deg DOUBLE PRECISION,
  ADD COLUMN IF NOT EXISTS antenna_beamwidth_v_deg DOUBLE PRECISION;
