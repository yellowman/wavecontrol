-- Migration: Convert legacy single-username + password array to credential pairs
-- Run this on existing waveControl databases to migrate to the new credential format

-- Step 1: Create a function to extract values from JSON arrays
-- (PostgreSQL doesn't have easy array iteration in plain SQL)

-- Step 2: Migrate AP credentials
-- Get the existing ap_username and first 3 passwords from ap_passwords JSON array
DO $$
DECLARE
    ap_user TEXT;
    ap_pass_json TEXT;
    passes TEXT[];
    pass_count INT := 0;
    i INT;
BEGIN
    -- Get legacy ap_username (or default_username fallback)
    SELECT value INTO ap_user FROM settings WHERE key = 'ap_username';
    IF ap_user IS NULL THEN
        SELECT value INTO ap_user FROM settings WHERE key = 'default_username';
    END IF;
    IF ap_user IS NULL THEN
        ap_user := '';
    END IF;
    
    -- Get legacy ap_passwords JSON array
    SELECT value INTO ap_pass_json FROM settings WHERE key = 'ap_passwords';
    IF ap_pass_json IS NULL THEN
        SELECT value INTO ap_pass_json FROM settings WHERE key = 'default_passwords';
    END IF;
    
    -- Parse JSON array to text array
    IF ap_pass_json IS NOT NULL AND ap_pass_json != '' AND ap_pass_json != '[]' THEN
        SELECT ARRAY(SELECT json_array_elements_text(ap_pass_json::json)) INTO passes;
    ELSE
        passes := ARRAY[]::TEXT[];
    END IF;
    
    pass_count := LEAST(3, COALESCE(array_length(passes, 1), 0));
    -- PostgreSQL integer FOR loops count downward when the lower bound is
    -- greater than the upper bound, so guard empty ranges explicitly.
    IF pass_count > 0 THEN
        FOR i IN 1..pass_count LOOP
            INSERT INTO settings (key, value) VALUES
                ('ap_cred' || i || '_user', ap_user),
                ('ap_cred' || i || '_pass', passes[i])
            ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value;
        END LOOP;
    END IF;

    -- Ensure empty slots exist
    IF pass_count < 3 THEN
        FOR i IN (pass_count + 1)..3 LOOP
            INSERT INTO settings (key, value) VALUES
                ('ap_cred' || i || '_user', ''),
                ('ap_cred' || i || '_pass', '')
            ON CONFLICT (key) DO NOTHING;
        END LOOP;
    END IF;
END $$;

-- Step 3: Migrate STA credentials
DO $$
DECLARE
    sta_user TEXT;
    sta_pass_json TEXT;
    ap_user TEXT;
    passes TEXT[];
    pass_count INT := 0;
    i INT;
BEGIN
    -- Get legacy sta_username (or fall back to ap_username)
    SELECT value INTO sta_user FROM settings WHERE key = 'sta_username';
    IF sta_user IS NULL OR sta_user = '' THEN
        SELECT value INTO sta_user FROM settings WHERE key = 'ap_username';
    END IF;
    IF sta_user IS NULL OR sta_user = '' THEN
        SELECT value INTO sta_user FROM settings WHERE key = 'default_username';
    END IF;
    IF sta_user IS NULL THEN
        sta_user := '';
    END IF;
    
    -- Get legacy sta_passwords JSON array
    SELECT value INTO sta_pass_json FROM settings WHERE key = 'sta_passwords';
    IF sta_pass_json IS NULL OR sta_pass_json = '' OR sta_pass_json = '[]' THEN
        -- Fall back to ap_passwords
        SELECT value INTO sta_pass_json FROM settings WHERE key = 'ap_passwords';
    END IF;
    IF sta_pass_json IS NULL OR sta_pass_json = '' OR sta_pass_json = '[]' THEN
        SELECT value INTO sta_pass_json FROM settings WHERE key = 'default_passwords';
    END IF;
    
    -- Parse JSON array to text array
    IF sta_pass_json IS NOT NULL AND sta_pass_json != '' AND sta_pass_json != '[]' THEN
        SELECT ARRAY(SELECT json_array_elements_text(sta_pass_json::json)) INTO passes;
    ELSE
        passes := ARRAY[]::TEXT[];
    END IF;
    
    pass_count := LEAST(3, COALESCE(array_length(passes, 1), 0));
    IF pass_count > 0 THEN
        FOR i IN 1..pass_count LOOP
            INSERT INTO settings (key, value) VALUES
                ('sta_cred' || i || '_user', sta_user),
                ('sta_cred' || i || '_pass', passes[i])
            ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value;
        END LOOP;
    END IF;

    -- Ensure empty slots exist
    IF pass_count < 3 THEN
        FOR i IN (pass_count + 1)..3 LOOP
            INSERT INTO settings (key, value) VALUES
                ('sta_cred' || i || '_user', ''),
                ('sta_cred' || i || '_pass', '')
            ON CONFLICT (key) DO NOTHING;
        END LOOP;
    END IF;
END $$;

-- Step 4: Verify migration
SELECT key, value FROM settings 
WHERE key LIKE 'ap_cred%' OR key LIKE 'sta_cred%'
ORDER BY key;

-- Optional: Clean up legacy settings (uncomment after verifying migration)
-- DELETE FROM settings WHERE key IN (
--     'ap_username', 'ap_passwords',
--     'sta_username', 'sta_passwords', 
--     'default_username', 'default_passwords', 'default_usernames'
-- );
