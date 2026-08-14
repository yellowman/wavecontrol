-- Migration: Add credential pairs and zabbix_allowed_hosts
-- Run this on existing waveControl databases

-- Add new settings (won't overwrite if already exist)
INSERT INTO settings (key, value) VALUES 
    ('ap_cred1_user', ''),
    ('ap_cred1_pass', ''),
    ('ap_cred2_user', ''),
    ('ap_cred2_pass', ''),
    ('ap_cred3_user', ''),
    ('ap_cred3_pass', ''),
    ('sta_cred1_user', ''),
    ('sta_cred1_pass', ''),
    ('sta_cred2_user', ''),
    ('sta_cred2_pass', ''),
    ('sta_cred3_user', ''),
    ('sta_cred3_pass', ''),
    ('zabbix_allowed_hosts', '')
ON CONFLICT (key) DO NOTHING;

-- Now manually set your credentials:
-- UPDATE settings SET value = 'root' WHERE key = 'ap_cred1_user';
-- UPDATE settings SET value = 'yourpassword' WHERE key = 'ap_cred1_pass';
-- etc.
