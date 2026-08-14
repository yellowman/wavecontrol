-- waveControl Database Schema
-- PostgreSQL 14+

-- Enable required extensions
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- Roles table (fixed set)
CREATE TABLE IF NOT EXISTS roles (
    id SERIAL PRIMARY KEY,
    name VARCHAR(32) UNIQUE NOT NULL
);

INSERT INTO roles (name) VALUES 
    ('administrator'), ('creator'), ('editor'), ('viewer')
ON CONFLICT (name) DO NOTHING;

-- Users table
CREATE TABLE IF NOT EXISTS users (
    id SERIAL PRIMARY KEY,
    username VARCHAR(64) UNIQUE NOT NULL,
    password VARCHAR(256) NOT NULL,  -- bcrypt hash
    status INTEGER DEFAULT 1,  -- 1=active, 0=disabled
    created_at TIMESTAMP DEFAULT NOW()
);

-- User-Role mapping
CREATE TABLE IF NOT EXISTS user_roles (
    "user" INTEGER REFERENCES users(id) ON DELETE CASCADE,
    role INTEGER REFERENCES roles(id) ON DELETE CASCADE,
    PRIMARY KEY ("user", role)
);

-- Settings table (key-value store for configuration)
CREATE TABLE IF NOT EXISTS settings (
    key VARCHAR(64) PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at TIMESTAMP DEFAULT NOW()
);

-- Default settings
INSERT INTO settings (key, value) VALUES 
    ('poll_interval', '30'),
    ('ap_cred1_user', 'ubnt'),
    ('ap_cred1_pass', 'ubnt'),
    ('ap_cred2_user', ''),
    ('ap_cred2_pass', ''),
    ('ap_cred3_user', ''),
    ('ap_cred3_pass', ''),
    ('sta_cred1_user', 'ubnt'),
    ('sta_cred1_pass', 'ubnt'),
    ('sta_cred2_user', ''),
    ('sta_cred2_pass', ''),
    ('sta_cred3_user', ''),
    ('sta_cred3_pass', ''),
    ('firmware_path', 'firmware'),
    ('listen_addr', '127.0.0.1:8080'),
    ('zabbix_enabled', 'false'),
    ('zabbix_listen', '127.0.0.1:10050'),
    ('zabbix_allowed_hosts', ''),
    ('management_prefixes', '[]'),
    -- Quality thresholds (percent)
    ('interference_warning_pct', '10'),
    ('interference_critical_pct', '25')
ON CONFLICT (key) DO NOTHING;

-- Regions table (for grouping sites)
CREATE TABLE IF NOT EXISTS regions (
    id SERIAL PRIMARY KEY,
    name VARCHAR(128) NOT NULL,
    parent_id INTEGER REFERENCES regions(id) ON DELETE SET NULL, -- for city -> state -> country hierarchy
    created_at TIMESTAMP DEFAULT NOW()
);

-- Sites table (tower sites, building locations, etc.)
CREATE TABLE IF NOT EXISTS sites (
    id SERIAL PRIMARY KEY,
    name VARCHAR(128) NOT NULL,
    region_id INTEGER REFERENCES regions(id) ON DELETE SET NULL,
    address TEXT,
    gps_lat DOUBLE PRECISION,
    gps_lon DOUBLE PRECISION,
    tower_h_m DOUBLE PRECISION,
    notes TEXT,
    created_at TIMESTAMP DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_sites_region ON sites(region_id);

-- Devices table (inventory - static data only)
-- Real-time stats are kept in memory, not in DB
CREATE TABLE IF NOT EXISTS devices (
    id SERIAL PRIMARY KEY,
    mac VARCHAR(17) UNIQUE NOT NULL,
    ip_address INET NOT NULL,
    hostname VARCHAR(128),
    
    -- Device identification
    product VARCHAR(64),      -- "Wave AP", "Wave Long-Range", "LTU-Rocket", "Rocket 5AC"
    model VARCHAR(32),        -- "Wave-AP", "Wave-LR", "LTU-Rocket"
    platform VARCHAR(16),     -- "wave", "ltu", "airmax"
    flavor VARCHAR(16),       -- "GMC", "GMP", "MGMP", "AFLTUROCKET", "Rocket"
    role VARCHAR(8),          -- "ap", "sta"
	    managed BOOLEAN NOT NULL DEFAULT FALSE, -- true when device was explicitly added via Add IP / Add Bulk

    -- Alert policy
    alertable BOOLEAN NOT NULL DEFAULT TRUE,
    alert_silenced_until TIMESTAMPTZ,
    alert_notes TEXT,
    
    -- Site grouping
    site_id INTEGER REFERENCES sites(id) ON DELETE SET NULL,
    
    -- Wireless config
    ssid VARCHAR(64),
    frequency INTEGER,        -- MHz
    channel_width INTEGER,    -- MHz
    
    -- GPS location
    gps_lat DOUBLE PRECISION,
    gps_lon DOUBLE PRECISION,

    -- Antenna modeling / planning (optional)
    -- These are used for future RF modeling and can be configured from the web UI.
    antenna_model VARCHAR(64),
    antenna_override BOOLEAN DEFAULT FALSE,
    antenna_azimuth_deg DOUBLE PRECISION,
    antenna_downtilt_deg DOUBLE PRECISION,
    antenna_electrical_downtilt_deg DOUBLE PRECISION DEFAULT 0,
    antenna_beamwidth_h_deg DOUBLE PRECISION,
    antenna_beamwidth_v_deg DOUBLE PRECISION,

    -- Sector planning / export (optional)
    -- radius_m: expected reach of this AP/sector in meters
    radius_m DOUBLE PRECISION,
    -- tech: optional planning code for external tools (leave NULL to derive from frequency)
    tech INTEGER,
    -- throughput/latency targets for planning/export (units: Mbps, ms)
    down_mbps DOUBLE PRECISION,
    up_mbps DOUBLE PRECISION,
    latency_ms DOUBLE PRECISION,
    -- business/residential marker for planning (free-form 1-char code, e.g. B/R/X)
    bizres VARCHAR(1) DEFAULT 'X',
    
    -- Firmware info (updated when firmware changes)
    firmware VARCHAR(128),         -- Full firmware string
    firmware_version VARCHAR(32),  -- Version number (may include suffix like -beta)
    
    -- Hierarchy (AP -> STA relationship)
    parent_id INTEGER REFERENCES devices(id) ON DELETE SET NULL,
    parent_mac VARCHAR(17),        -- AP MAC for STA reassociation after restart
    
    -- Credentials (for this device, overrides defaults)
    username VARCHAR(64),
    password VARCHAR(128),
    
    -- Status tracking (basic, real-time stats in memory)
    status VARCHAR(16) DEFAULT 'unknown',  -- online, offline, upgrading, unknown
    status_reason VARCHAR(128),            -- short reason for unknown/offline (optional)
    last_seen TIMESTAMP,
    
    created_at TIMESTAMP DEFAULT NOW(),
    updated_at TIMESTAMP DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_devices_parent ON devices(parent_id);
CREATE INDEX IF NOT EXISTS idx_devices_status ON devices(status);
CREATE INDEX IF NOT EXISTS idx_devices_ip ON devices(ip_address);
CREATE INDEX IF NOT EXISTS idx_devices_platform ON devices(platform);
CREATE INDEX IF NOT EXISTS idx_devices_alertable ON devices(alertable);
CREATE INDEX IF NOT EXISTS idx_devices_role_alertable ON devices(role, alertable);
CREATE INDEX IF NOT EXISTS idx_devices_alert_silenced_until ON devices(alert_silenced_until);

-- Device identity mismatch records
-- Created when a polled row responds at the expected IP with a different physical MAC.
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

-- Firmware upgrade jobs
CREATE TABLE IF NOT EXISTS firmware_jobs (
    id SERIAL PRIMARY KEY,
    device_id INTEGER REFERENCES devices(id) ON DELETE CASCADE,
    firmware_file VARCHAR(256) NOT NULL,
    target_version VARCHAR(64),
    status VARCHAR(16) DEFAULT 'pending',  -- pending, uploading, rebooting, verifying, success, failed, skipped
    error_message TEXT,
    started_at TIMESTAMP DEFAULT NOW(),
    completed_at TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_firmware_jobs_device ON firmware_jobs(device_id);
CREATE INDEX IF NOT EXISTS idx_firmware_jobs_status ON firmware_jobs(status);

-- Scheduled jobs (for scheduled upgrades, reboots, etc.)
CREATE TABLE IF NOT EXISTS scheduled_jobs (
    id SERIAL PRIMARY KEY,
    job_type VARCHAR(32) NOT NULL,       -- 'upgrade', 'reboot', 'poll'
    device_ids INTEGER[],                 -- Target devices
    parameters JSONB,                     -- Job-specific params
    scheduled_at TIMESTAMP NOT NULL,
    repeat_cron VARCHAR(64),              -- NULL = one-time, else cron expression
    last_run TIMESTAMP,
    next_run TIMESTAMP,
    status VARCHAR(16) DEFAULT 'pending', -- pending, running, completed, failed, cancelled
    created_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMP DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_scheduled_jobs_next ON scheduled_jobs(next_run) WHERE status = 'pending';

-- Changelog (audit log)
CREATE TABLE IF NOT EXISTS changelog (
    id SERIAL PRIMARY KEY,
    change_time TIMESTAMP DEFAULT NOW(),
    device_mac VARCHAR(17),
    change TEXT NOT NULL,
    "user" INTEGER REFERENCES users(id) ON DELETE SET NULL
);

CREATE INDEX IF NOT EXISTS idx_changelog_time ON changelog(change_time DESC);
CREATE INDEX IF NOT EXISTS idx_changelog_mac ON changelog(device_mac);

-- Create default admin user (password: admin)
-- bcrypt hash with cost 10
INSERT INTO users (username, password, status) 
VALUES ('admin', '$2b$10$Q0ReFRsQ9Pvbt.7oYvBAieKwTekyDR.lhzluAPT3amPLsP4ob1rlG', 1)
ON CONFLICT (username) DO NOTHING;

-- Assign admin role to admin user
INSERT INTO user_roles ("user", role)
SELECT u.id, r.id FROM users u, roles r 
WHERE u.username = 'admin' AND r.name = 'administrator'
ON CONFLICT DO NOTHING;

-- Helper function to update updated_at timestamp
CREATE OR REPLACE FUNCTION update_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- Trigger for devices
DROP TRIGGER IF EXISTS devices_updated_at ON devices;
CREATE TRIGGER devices_updated_at
    BEFORE UPDATE ON devices
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

-- Trigger for settings
DROP TRIGGER IF EXISTS settings_updated_at ON settings;
CREATE TRIGGER settings_updated_at
    BEFORE UPDATE ON settings
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();

-- Device configuration backups (DEPRECATED - now using filesystem storage)
-- This table is no longer used but kept for backward compatibility
-- Old data can be exported using: SELECT config_data FROM device_configs WHERE device_id = X
-- CREATE TABLE IF NOT EXISTS device_configs (
--     id SERIAL PRIMARY KEY,
--     device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
--     config_data BYTEA NOT NULL,
--     created_at TIMESTAMPTZ DEFAULT NOW(),
--     created_by INTEGER REFERENCES users(id) ON DELETE SET NULL
-- );

-- CREATE INDEX idx_device_configs_device ON device_configs(device_id);

-- Device TLS certificates (for trust-on-first-use)
CREATE TABLE IF NOT EXISTS device_certs (
    id SERIAL PRIMARY KEY,
    device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    fingerprint VARCHAR(64) NOT NULL,  -- SHA-256 fingerprint (hex)
    subject TEXT,                       -- Certificate subject
    issuer TEXT,                        -- Certificate issuer  
    not_before TIMESTAMPTZ,
    not_after TIMESTAMPTZ,
    pinned_at TIMESTAMPTZ DEFAULT NOW(),
    pinned_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
    verified BOOLEAN DEFAULT false,     -- Admin has verified this cert
    verified_at TIMESTAMPTZ,
    verified_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
    previous_fingerprint VARCHAR(64),   -- Previous cert fingerprint (if changed)
    changed_at TIMESTAMPTZ,             -- When cert changed
    UNIQUE(device_id)
);

CREATE INDEX idx_device_certs_device ON device_certs(device_id);
CREATE INDEX idx_device_certs_fingerprint ON device_certs(fingerprint);

-- Alert rules (thresholds)
CREATE TABLE IF NOT EXISTS alert_rules (
    id SERIAL PRIMARY KEY,
    name VARCHAR(100) NOT NULL,
    enabled BOOLEAN DEFAULT true,
    
    -- Target scope
    scope VARCHAR(20) DEFAULT 'all',     -- 'all', 'site', 'group', 'device'
    scope_id INTEGER,                    -- site_id, group_id, or device_id
    target_role VARCHAR(16) NOT NULL DEFAULT 'all', -- 'all', 'ap', 'sta'
    require_alertable BOOLEAN NOT NULL DEFAULT TRUE, -- honor devices.alertable/silence gates
    
    -- Condition
    metric VARCHAR(50) NOT NULL,         -- 'signal_60ghz', 'signal_5ghz', 'cpu', 'temperature', 'offline_duration', 'capacity', 'peer_count'
    operator VARCHAR(10) NOT NULL,       -- 'lt', 'gt', 'eq', 'ne', 'lte', 'gte'
    threshold NUMERIC NOT NULL,
    duration_seconds INTEGER DEFAULT 0,  -- How long condition must persist (0 = immediate)
    
    -- Notification
    notify_channels TEXT[],              -- ['email', 'webhook', 'zabbix', 'mobile']
    notify_emails TEXT[],                -- Email addresses
    webhook_url TEXT,                    -- Webhook URL
    cooldown_seconds INTEGER DEFAULT 300, -- Minimum time between repeat alerts
    
    -- Metadata
    created_at TIMESTAMPTZ DEFAULT NOW(),
    created_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX idx_alert_rules_enabled ON alert_rules(enabled);
CREATE INDEX idx_alert_rules_metric ON alert_rules(metric);
CREATE INDEX IF NOT EXISTS idx_alert_rules_target_role ON alert_rules(target_role);
CREATE INDEX IF NOT EXISTS idx_alert_rules_require_alertable ON alert_rules(require_alertable);

-- Alert history
CREATE TABLE IF NOT EXISTS alerts (
    id SERIAL PRIMARY KEY,
    rule_id INTEGER REFERENCES alert_rules(id) ON DELETE SET NULL,
    device_id INTEGER REFERENCES devices(id) ON DELETE CASCADE,
    
    -- Alert details
    metric VARCHAR(50) NOT NULL,
    value NUMERIC,
    threshold NUMERIC,
    message TEXT NOT NULL,
    severity VARCHAR(20) DEFAULT 'warning',  -- 'info', 'warning', 'critical'
    
    -- State
    status VARCHAR(20) DEFAULT 'active',     -- 'active', 'acknowledged', 'resolved'
    triggered_at TIMESTAMPTZ DEFAULT NOW(),
    acknowledged_at TIMESTAMPTZ,
    acknowledged_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
    resolved_at TIMESTAMPTZ,
    
    -- Notification tracking
    notified_at TIMESTAMPTZ,
    notify_error TEXT
);

CREATE INDEX idx_alerts_status ON alerts(status);
CREATE INDEX idx_alerts_device ON alerts(device_id);
CREATE INDEX idx_alerts_triggered ON alerts(triggered_at DESC);
CREATE INDEX idx_alerts_rule ON alerts(rule_id);

-- Alert state tracking (for duration-based alerts)
CREATE TABLE IF NOT EXISTS alert_states (
    id SERIAL PRIMARY KEY,
    rule_id INTEGER NOT NULL REFERENCES alert_rules(id) ON DELETE CASCADE,
    device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    first_triggered_at TIMESTAMPTZ DEFAULT NOW(),
    last_value NUMERIC,
    last_checked_at TIMESTAMPTZ DEFAULT NOW(),
    notified BOOLEAN DEFAULT false,
    UNIQUE(rule_id, device_id)
);


-- Native mobile clients and durable push outbox
CREATE TABLE IF NOT EXISTS mobile_devices (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    platform VARCHAR(16) NOT NULL CHECK (platform IN ('android', 'ios')),
    provider VARCHAR(16) NOT NULL CHECK (provider IN ('fcm', 'apns')),
    token_hash CHAR(64) NOT NULL,
    token_encrypted TEXT NOT NULL,
    device_name VARCHAR(128),
    app_version VARCHAR(64),
    os_version VARCHAR(64),
    enabled BOOLEAN NOT NULL DEFAULT true,
    last_seen_at TIMESTAMPTZ DEFAULT NOW(),
    last_error TEXT,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE(user_id, platform, provider, token_hash)
);

CREATE TABLE IF NOT EXISTS mobile_push_preferences (
    user_id INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    push_enabled BOOLEAN NOT NULL DEFAULT true,
    notify_critical BOOLEAN NOT NULL DEFAULT true,
    notify_warning BOOLEAN NOT NULL DEFAULT true,
    notify_info BOOLEAN NOT NULL DEFAULT false,
    quiet_hours_start TIME,
    quiet_hours_end TIME,
    timezone VARCHAR(64) NOT NULL DEFAULT 'UTC',
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS notification_outbox (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    seq BIGSERIAL UNIQUE,
    alert_id INTEGER REFERENCES alerts(id) ON DELETE CASCADE,
    user_id INTEGER REFERENCES users(id) ON DELETE CASCADE,
    mobile_device_id UUID REFERENCES mobile_devices(id) ON DELETE CASCADE,
    event_type VARCHAR(64) NOT NULL,
    severity VARCHAR(20) NOT NULL,
    dedupe_key TEXT NOT NULL,
    collapse_key TEXT,
    payload JSONB NOT NULL,
    status VARCHAR(16) NOT NULL DEFAULT 'pending',
    attempts INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ DEFAULT NOW(),
    provider_message_id TEXT,
    error TEXT,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    sent_at TIMESTAMPTZ,
    UNIQUE(dedupe_key, mobile_device_id)
);

CREATE INDEX IF NOT EXISTS idx_mobile_devices_user ON mobile_devices(user_id);
CREATE INDEX IF NOT EXISTS idx_mobile_devices_enabled ON mobile_devices(enabled);
CREATE INDEX IF NOT EXISTS idx_notification_outbox_due ON notification_outbox(status, next_attempt_at, created_at);
CREATE INDEX IF NOT EXISTS idx_notification_outbox_user ON notification_outbox(user_id, seq DESC);

-- Reports
CREATE TABLE IF NOT EXISTS reports (
    id SERIAL PRIMARY KEY,
    type VARCHAR(50) NOT NULL,
    data JSONB NOT NULL,
    device_count INTEGER DEFAULT 0,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    created_by INTEGER REFERENCES users(id) ON DELETE SET NULL
);

CREATE INDEX idx_reports_type ON reports(type);
CREATE INDEX idx_reports_created ON reports(created_at DESC);

-- Job runs (execution instances of any job type)
-- This unifies firmware_jobs, scheduled_jobs, and ad-hoc operations
CREATE TABLE IF NOT EXISTS job_runs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    job_type VARCHAR(32) NOT NULL,         -- 'upgrade', 'backup', 'restore', 'reboot', 'bulk_upgrade', 'fanout_upgrade'
    status VARCHAR(16) DEFAULT 'pending',  -- pending, running, completed, failed, cancelled
    progress INTEGER DEFAULT 0,            -- 0-100 percent
    total_steps INTEGER DEFAULT 1,         -- Total steps for progress calculation
    completed_steps INTEGER DEFAULT 0,     -- Steps completed so far
    
    -- Target specification
    device_ids INTEGER[],                  -- Target devices (can be NULL for non-device jobs)
    parameters JSONB,                      -- Job-specific params
    
    -- Results
    result JSONB,                          -- Final result data
    error_message TEXT,                    -- Error if failed
    
    -- Timing
    created_at TIMESTAMPTZ DEFAULT NOW(),
    started_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    
    -- Ownership
    created_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
    scheduled_job_id INTEGER REFERENCES scheduled_jobs(id) ON DELETE SET NULL  -- Link to scheduler if triggered by schedule
);

CREATE INDEX idx_job_runs_status ON job_runs(status);
CREATE INDEX idx_job_runs_type ON job_runs(job_type);
CREATE INDEX idx_job_runs_created ON job_runs(created_at DESC);
CREATE INDEX idx_job_runs_user ON job_runs(created_by);

-- Job events (progress log for a job run)
CREATE TABLE IF NOT EXISTS job_events (
    id SERIAL PRIMARY KEY,
    job_id UUID NOT NULL REFERENCES job_runs(id) ON DELETE CASCADE,
    event_time TIMESTAMPTZ DEFAULT NOW(),
    event_type VARCHAR(32) NOT NULL,       -- 'started', 'progress', 'step_complete', 'warning', 'error', 'completed'
    device_id INTEGER,                     -- Which device this event relates to (optional)
    message TEXT NOT NULL,
    data JSONB                             -- Additional event data
);

CREATE INDEX idx_job_events_job ON job_events(job_id);
CREATE INDEX idx_job_events_time ON job_events(event_time DESC);

-- Maintenance windows (per region/site)
CREATE TABLE IF NOT EXISTS maintenance_windows (
    id SERIAL PRIMARY KEY,
    name VARCHAR(128) NOT NULL,
    
    -- Scope: can be global, region, or site
    scope VARCHAR(20) DEFAULT 'global',   -- 'global', 'region', 'site'
    region_id INTEGER REFERENCES regions(id) ON DELETE CASCADE,
    site_id INTEGER REFERENCES sites(id) ON DELETE CASCADE,
    
    -- Schedule: day of week + time window
    -- dow: 0=Sunday, 1=Monday, ..., 6=Saturday, NULL=any day
    day_of_week INTEGER[],                -- Array of days, e.g., {2,4} for Tue/Thu
    start_time TIME NOT NULL,             -- Start time (in UTC or local TZ)
    end_time TIME NOT NULL,               -- End time
    timezone VARCHAR(64) DEFAULT 'UTC',   -- Timezone for interpreting times
    
    -- Options
    allow_jobs VARCHAR(32)[] DEFAULT ARRAY['upgrade', 'reboot'],  -- Job types allowed during window
    enabled BOOLEAN DEFAULT true,
    
    -- Metadata
    created_at TIMESTAMPTZ DEFAULT NOW(),
    created_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX idx_maintenance_windows_enabled ON maintenance_windows(enabled);
CREATE INDEX idx_maintenance_windows_region ON maintenance_windows(region_id);
CREATE INDEX idx_maintenance_windows_site ON maintenance_windows(site_id);

-- Add scheduler settings
INSERT INTO settings (key, value) VALUES 
    ('scheduler_max_concurrent', '5'),
    ('scheduler_check_interval', '10'),
    ('scheduler_respect_maintenance', 'true')
ON CONFLICT (key) DO NOTHING;

-- Add progress columns to scheduled_jobs if not exist
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns 
                   WHERE table_name = 'scheduled_jobs' AND column_name = 'progress') THEN
        ALTER TABLE scheduled_jobs ADD COLUMN progress INTEGER DEFAULT 0;
        ALTER TABLE scheduled_jobs ADD COLUMN total_devices INTEGER DEFAULT 0;
        ALTER TABLE scheduled_jobs ADD COLUMN completed_devices INTEGER DEFAULT 0;
        ALTER TABLE scheduled_jobs ADD COLUMN error_message TEXT;
    END IF;
END $$;

-- Custom drilldown lists for targeted device polling
CREATE TABLE IF NOT EXISTS drilldown_lists (
    id SERIAL PRIMARY KEY,
    name VARCHAR(64) NOT NULL UNIQUE,
    description TEXT,
    enabled BOOLEAN DEFAULT true,
    poll_interval INTEGER DEFAULT 30,    -- seconds between polls
    created_at TIMESTAMP DEFAULT NOW(),
    updated_at TIMESTAMP DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS drilldown_hosts (
    id SERIAL PRIMARY KEY,
    list_id INTEGER REFERENCES drilldown_lists(id) ON DELETE CASCADE,
    host VARCHAR(64) NOT NULL,           -- IP address or hostname
    username VARCHAR(64),                -- Override credentials
    password VARCHAR(128),
    device_id INTEGER REFERENCES devices(id) ON DELETE SET NULL,  -- Linked device if known
    last_poll TIMESTAMP,
    last_error TEXT,
    created_at TIMESTAMP DEFAULT NOW(),
    UNIQUE(list_id, host)
);

INSERT INTO settings (key, value) VALUES ('chain_imbalance_threshold_db', '5') ON CONFLICT (key) DO NOTHING;
INSERT INTO settings (key, value) VALUES ('rx_mismatch_threshold_db', '8') ON CONFLICT (key) DO NOTHING;

INSERT INTO settings (key, value) VALUES
    ('mobile_push_enabled', 'true'),
    ('fcm_enabled', 'false'),
    ('fcm_project_id', ''),
    ('fcm_service_account_json', ''),
    ('apns_enabled', 'false'),
    ('apns_team_id', ''),
    ('apns_key_id', ''),
    ('apns_bundle_id', ''),
    ('apns_private_key_p8', ''),
    ('apns_production', 'false')
ON CONFLICT (key) DO NOTHING;
