-- Ensure bizres has a sane default and normalize existing rows.
-- Biz/Res codes:
--   B = Business
--   R = Residential
--   X = Both (default)

ALTER TABLE devices
  ALTER COLUMN bizres SET DEFAULT 'X';

UPDATE devices
  SET bizres = 'X'
  WHERE bizres IS NULL OR bizres = '';
