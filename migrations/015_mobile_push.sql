-- Migration number retained for upgrade-sequence compatibility.
--
-- Native APNs/FCM push delivery was removed before release and is outside
-- waveControl's scope.  Do not create provider credentials, device tokens, or
-- mobile delivery tables here.  Migration 018 removes any objects created by
-- development builds that briefly carried the abandoned implementation.
SELECT 1;
