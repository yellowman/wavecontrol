-- Custom drilldown lists for targeted device polling
CREATE TABLE IF NOT EXISTS drilldown_lists (
    id SERIAL PRIMARY KEY,
    name VARCHAR(64) NOT NULL UNIQUE,
    description TEXT,
    enabled BOOLEAN DEFAULT true,
    poll_interval INTEGER DEFAULT 30,
    created_at TIMESTAMP DEFAULT NOW(),
    updated_at TIMESTAMP DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS drilldown_hosts (
    id SERIAL PRIMARY KEY,
    list_id INTEGER REFERENCES drilldown_lists(id) ON DELETE CASCADE,
    host VARCHAR(64) NOT NULL,
    username VARCHAR(64),
    password VARCHAR(128),
    device_id INTEGER REFERENCES devices(id) ON DELETE SET NULL,
    last_poll TIMESTAMP,
    last_error TEXT,
    created_at TIMESTAMP DEFAULT NOW(),
    UNIQUE(list_id, host)
);

-- Grant permissions to app user (run as superuser, replace 'wavecontrol' with your app user)
-- GRANT ALL ON drilldown_lists TO wavecontrol;
-- GRANT ALL ON drilldown_hosts TO wavecontrol;
-- GRANT USAGE, SELECT ON SEQUENCE drilldown_lists_id_seq TO wavecontrol;
-- GRANT USAGE, SELECT ON SEQUENCE drilldown_hosts_id_seq TO wavecontrol;
