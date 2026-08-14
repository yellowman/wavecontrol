-- Persisted device identity mismatches for explicit AP replacement adoption.
-- A MAC mismatch means the expected inventory row responded at the expected IP,
-- but the device there reported a different physical MAC.

CREATE TABLE IF NOT EXISTS device_identity_mismatches (
    device_id INTEGER PRIMARY KEY REFERENCES devices(id) ON DELETE CASCADE,
    expected_mac VARCHAR(17) NOT NULL,
    observed_macs TEXT[] NOT NULL,
    observed_ip INET NOT NULL,
    source VARCHAR(32),
    observed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_error TEXT
);

CREATE INDEX IF NOT EXISTS idx_device_identity_mismatches_observed_ip ON device_identity_mismatches(observed_ip);
CREATE INDEX IF NOT EXISTS idx_device_identity_mismatches_observed_at ON device_identity_mismatches(observed_at DESC);
