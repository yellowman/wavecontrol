-- Remove redundant username/password from drilldown_hosts
-- Credentials should come from devices table or global STA credentials

ALTER TABLE drilldown_hosts DROP COLUMN IF EXISTS username;
ALTER TABLE drilldown_hosts DROP COLUMN IF EXISTS password;
