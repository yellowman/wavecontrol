-- Migration 005: Add verification tracking to device_certs
-- Adds columns to track admin verification of certificates and cert changes

-- Add verified column if it doesn't exist
ALTER TABLE device_certs ADD COLUMN IF NOT EXISTS verified BOOLEAN DEFAULT false;

-- Add verified_at timestamp
ALTER TABLE device_certs ADD COLUMN IF NOT EXISTS verified_at TIMESTAMPTZ;

-- Add verified_by user reference
ALTER TABLE device_certs ADD COLUMN IF NOT EXISTS verified_by INTEGER REFERENCES users(id) ON DELETE SET NULL;

-- Add previous_fingerprint to track cert changes
ALTER TABLE device_certs ADD COLUMN IF NOT EXISTS previous_fingerprint VARCHAR(64);

-- Add changed_at timestamp to track when cert changed
ALTER TABLE device_certs ADD COLUMN IF NOT EXISTS changed_at TIMESTAMPTZ;

-- Set existing pinned certs as verified (backward compatibility)
UPDATE device_certs SET verified = true WHERE verified IS NULL OR verified = false;
