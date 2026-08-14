-- Add optional tower height (meters) to sites for planning/export.

ALTER TABLE sites
  ADD COLUMN IF NOT EXISTS tower_h_m DOUBLE PRECISION;
