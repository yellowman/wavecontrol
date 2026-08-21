# waveControl Architecture Specification

## Overview

waveControl is a device management and monitoring system for Ubiquiti Wave (60GHz, 5GHz/6GHz MLO), LTU, and airMAX (M/AC) wireless devices. It provides real-time visibility into device stats that are only available through the Ubiquiti HTTP API, with a focus on radio-specific metrics like per-chain signal levels. It also provides a bridge to Zabbix to retrieve these statistics since they are not available from the Ubiquiti device SNMP server.

### Design Goals

1. **Minimal configuration** - Only database DSN and JWT secret in environment; everything else configurable via web UI
2. **Real-time stats in memory** - Frequently changing data (signal levels, rates, counts) never hits the database
3. **Static data in database** - Device inventory, firmware versions, credentials, user accounts
4. **Auto-discovery** - Add an AP IP, automatically discover all connected STAs
5. **Continuous polling** - Background refresh of all device stats every 30 seconds
6. **Integration-ready** - Zabbix bridge connector for external monitoring systems
7. **Secure by default** - Daemonizes, drops privileges, uses pledge on OpenBSD

---

## Dashboard Performance (Large Deployments)

The web UI is optimized for deployments with 5000+ devices using several techniques:

### Virtual Scrolling (500+ devices)

When device count exceeds 500, the table automatically switches to virtual scrolling:

- **Only visible rows rendered**: ~50 rows in viewport + 100 row buffer above/below
- **Memory efficient**: 5000 devices = ~250 DOM nodes instead of 5000
- **Smooth scrolling**: Full data stays in JS memory for instant search/filter/sort

```
+-----------------------------------------+
|  JS Memory: 5000 device objects         |  <- Always in memory
+-----------------------------------------+
|  DOM Buffer: rows 450-650 (200 total)   |  <- Rendered in DOM
+-----------------------------------------+
|  Viewport: rows 500-550 (50 visible)    |  <- What user sees
+-----------------------------------------+
```

#### Trade-offs and Design Rationale

Virtual scrolling is a trade-off: it introduces constant latency from JavaScript row rendering on every scroll event, but prevents severely degraded browser performance at high device counts. Without virtualization, browsers struggle with 1000+ DOM rows (slow reflows, high memory, laggy interactions).

**Why 500 threshold?**
- Below 500: Native DOM scrolling is snappy, no JS overhead
- Above 500: DOM performance degrades noticeably, virtualization pays off
- The threshold balances "no JS tax" for small deployments vs "usable UI" for large ones

**Performance-critical code paths:**
Since virtual scrolling runs JS on every scroll, these paths must be optimized:
- `VirtualTable.handleScroll()` - Called on every scroll event
- `VirtualTable.renderRows()` - Generates visible row HTML
- `renderDeviceRow()` - Single row HTML generation
- `getSignalClass*()` / `getDeviceSignal()` - Called per-row for styling

Guidelines for these hot paths:
- Avoid object allocation in loops (reuse arrays, use primitives)
- Minimize DOM queries (cache element references)
- Use string concatenation over template literals in tight loops
- Prefer `===` over type-coercing `==`
- Avoid function calls where inline code suffices

### WebSocket Update Batching

Instead of updating DOM for each of 5000 `stats_update` messages:

1. Updates are collected in a batch window (100ms)
2. Single DOM update pass after batch window closes
3. Reduces browser reflows from 5000 to 1 per poll cycle

### Performance Optimizations

Key optimizations in the virtual scrolling implementation:

**Scroll Handler (`_onScroll`):**
- Uses `requestAnimationFrame` to coalesce scroll events
- Prevents multiple renders within a single frame
- `{ passive: true }` on scroll listener avoids scroll blocking

**Viewport Rendering (`_renderViewport`):**
- Early-exit when buffer range unchanged (small scrolls within buffer)
- Avoids Set allocation - iterates cache directly for removal check
- Updates row positions with string concatenation (faster than template literals)

**Row Creation (`_createRow`):**
- Single `cssText` assignment instead of multiple `style.*` properties
- String concatenation for transform values

**Signal Bars (`getSignalBars`):**
- Pre-computed bar indices constant
- Loop-based string building instead of `Array.map().join()`

**Data Filtering (`getSortedFilteredDevices`):**
- Uses `indexOf` instead of `includes` (marginally faster)
- Set-based AP lookup for O(1) parent checks
- In-place sort to avoid array copy

### Tree Sidebar Optimization

For large datasets (500+ devices):
- Tree nodes default to **collapsed** (must click to expand)
- Only expanded AP's STAs are rendered
- Reduces initial DOM node count significantly

### Server-Side Data Processing (Go > JS Philosophy)

**Core principle: Go is fast, browsers are slow.** JavaScript in browsers is inherently inefficient - garbage collection pauses, single-threaded execution, JIT compilation overhead, and DOM manipulation costs make client-side computation expensive. Go, by contrast, compiles to native code, has efficient garbage collection, and runs on the server where resources are plentiful.

**Therefore:** Any parsing, extraction, aggregation, or classification that can be done once during data ingest should be done in Go, not repeatedly in JavaScript during rendering.

**Computed server-side (Go):**
| Data | Where | Rationale |
|------|-------|-----------|
| `firmware_version` | Poller, during device update | Extracted once, used everywhere |
| `signal_combined` | Poller, when per-chain data arrives | MRC formula `10*log10(sum10^(dBm/10))` computed once |
| `signal_quality` | Poller, after signal computed | String classification done once |
| Platform/flavor | Poller, device discovery | Parsed once from firmware string |
| Online/offline | Stats store | State machine in Go |
| Device grouping (AP/STA hierarchy) | API response building | Tree structure built once per request |

**Computed client-side (unavoidable):**
| Data | Why Client-Side |
|------|-----------------|
| Virtual scroll row rendering | DOM manipulation must happen in browser |
| HTML escaping | Security - must happen at render time |
| CSS class selection | Conditional styling based on data |
| Event handlers | User interaction |

**NOT computed client-side (moved to Go):**
| Previously JS | Now Go |
|---------------|--------|
| `combineSignals()` - power addition | `stats.CombineSignals()` |
| `getSignalClass*()` - quality strings | `stats.SignalQuality5GHz/60GHz()` |
| `extractFirmwareVersion()` | `airmax.ExtractVersion()` |
| RSSI-to-dBm conversion | `airmax.rssiToDbm()` |

**API Design Principle:**

APIs should return **pre-computed, display-ready data**. The client should be a thin renderer, not a data processor. Every computation saved in JavaScript is:
1. Faster page load (less parsing)
2. Smoother scrolling (less work per frame)
3. Better battery life (less CPU)
4. More consistent (single implementation in Go)

**Pre-computed fields sent via API/WebSocket:**
```json
{
  "firmware": "GMC.ipq5018.v4.1.0.abc123.251220.bin",
  "firmware_version": "v4.1.0",
  "radio_5ghz": {
    "signal_per_chain": [-63, -65],
    "signal_combined": -61,
    "signal_quality": "good"
  }
}
```

JavaScript just renders `signal_combined` and applies CSS class from `signal_quality` - no computation needed.

### CSS Performance Optimizations

Virtual table rows use CSS containment for smoother scrolling:

```css
.virtual-body-table tbody tr {
  contain: layout style;  /* Isolate layout/style recalculations */
}
.virtual-body-table tbody {
  will-change: contents;  /* Hint GPU acceleration for transforms */
}
```

This tells the browser that changes inside a row won't affect other rows, enabling optimized paint/composite operations.

### Performance Files

| File | Purpose |
|------|---------|
| `web/js/virtual-table.js` | VirtualTable class with scroll handling |
| `web/js/virtual-integration.js` | Exports, WS batcher, data helpers |

### Configuration

Threshold in `virtual-integration.js`:

```javascript
export const VIRTUAL_THRESHOLD = 500  // Switch to virtual above this count
```

---

## Security Architecture

### XSS Prevention

All user-provided and device-provided strings are escaped before DOM insertion using dedicated helper functions:

```javascript
// web/js/components.js

function escapeHTML(str) {
  if (!str) return '';
  return String(str)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#039;');
}

function escapeAttr(str) {
  if (!str) return '';
  return String(str)
    .replace(/&/g, '&amp;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#039;');
}
```

**Usage pattern:** All innerHTML assignments use template literals with `${escapeHTML(value)}` for text content and `${escapeAttr(value)}` for attribute values:

```javascript
// Example: Device table row
`<td>${escapeHTML(device.hostname)}</td>
 <td class="${escapeAttr(device.status)}">${escapeHTML(device.ip_address)}</td>`
```

**Escaped sources include:**
- Device hostnames, IPs, MACs
- Firmware versions and product names
- User names and messages
- Job status/error messages
- Search results
- Alert messages
- Any data from API responses

### Role-Based Access Control (RBAC)

Four permission levels enforced at API layer:

| Role | Permissions |
|------|-------------|
| `viewer` | Read-only: list devices, stats, reports, logs |
| `creator` | viewer + add devices |
| `editor` | creator + delete, upgrade, jobs, backup/restore, sites/regions, alerts |
| `administrator` | editor + user management, settings, TLS config, bulk-ops config |

**Enforcement helpers:**

```go
func (a *API) hasRole(r *http.Request, role string) bool
func (a *API) requireView(w, r) bool   // 403 if not viewer+
func (a *API) requireCreate(w, r) bool // 403 if not creator+
func (a *API) requireEdit(w, r) bool   // 403 if not editor+
func (a *API) requireAdmin(w, r) bool  // 403 if not administrator
```

**Protected endpoints:**

| Level | Endpoints |
|-------|-----------|
| `creator` | AddDevice, BulkAddDevices |
| `editor` | DeleteDevice, Upgrade*, Backup*, Job*, Site*, Region*, Alert* |
| `admin` | User*, Setting*, TLS mode, BulkOps config |

**Sensitive settings:** The settings API is administrator-only. Secret setting values are returned as a fixed mask and remain unchanged when that mask is submitted; clearing a secret requires an explicit `clear` request.

### Request Timeout Architecture

Timeouts applied selectively by route group to prevent DoS while allowing long operations:

| Route Group | Timeout | Rationale |
|-------------|---------|-----------|
| WebSocket | None | Persistent connections |
| Long operations | 5 min | Firmware uploads, bulk operations |
| Standard API | 60 sec | Normal CRUD operations |
| Public | 60 sec | Login, health check |

**Long operation endpoints (5 min):**
- `POST /firmware` (upload, max 1GB)
- `POST /devices/{id}/upgrade`
- `POST /devices/{id}/upgrade-fanout`
- `POST /devices/bulk-upgrade`
- `POST /devices/retry-upgrade` (retry with alternate credentials)
- `POST /devices/bulk-add`
- `POST /devices/{id}/backup`
- `POST /devices/{id}/restore`
- `POST /devices/bulk-backup`
- `POST /devices/batch-config`
- `POST /reports/generate`

**Async job architecture:** Long operations can also be submitted as async jobs via `POST /job-runs`, which return immediately with a job ID. Progress is streamed via WebSocket, and results are stored in `job_runs`/`job_events` tables. This prevents HTTP timeout issues entirely.

### Static File Path Traversal Prevention

The SPA file server uses `os.DirFS` to sandbox all file access within the web root:

```go
// In buildRouter():
webFS := os.DirFS(webRoot)
fsHandler := http.FileServerFS(webFS)

r.NotFound(func(w http.ResponseWriter, r *http.Request) {
    // Clean and force relative path using path (not filepath) for URL handling
    urlPath := path.Clean("/" + r.URL.Path)
    relPath := strings.TrimPrefix(urlPath, "/")
    
    // If path is empty or tries to escape, serve index.html
    if relPath == "" || relPath == "." {
        http.ServeFile(w, r, filepath.Join(webRoot, "index.html"))
        return
    }
    
    // Check if file exists in webFS (safe - cannot escape webRoot)
    if f, err := fs.Stat(webFS, relPath); err == nil && !f.IsDir() {
        // Create modified request with cleaned path for FileServer
        r2 := new(http.Request)
        *r2 = *r
        r2.URL = new(url.URL)
        *r2.URL = *r.URL
        r2.URL.Path = "/" + relPath
        fsHandler.ServeHTTP(w, r2)
        return
    }
    
    // Fall back to index.html for SPA routing
    http.ServeFile(w, r, filepath.Join(webRoot, "index.html"))
})
```

**Security properties:**
- `os.DirFS()` creates a filesystem rooted at webRoot that cannot be escaped
- `path.Clean()` normalizes URL paths (removes `..`, double slashes)
- `fs.Stat()` only checks within the sandboxed filesystem
- Request path is sanitized before passing to FileServer

**Attack mitigation:**
- `/etc/passwd` -> relPath="etc/passwd" -> fs.Stat fails (not in webFS) -> serves index.html
- `/../../../etc/passwd` -> path.Clean normalizes to `/etc/passwd` -> same result
- `/%2e%2e/etc/passwd` -> URL decoded by Go -> same result

### Additional Security Measures

1. **JWT algorithm verification** - Prevents algorithm substitution attacks
2. **Bcrypt password hashing** - Uses `bcrypt.DefaultCost` (10)
3. **Constant-time password comparison** - bcrypt.CompareHashAndPassword is constant-time
4. **Generic login errors** - Returns same error for invalid user/password/disabled to prevent enumeration
5. **CORS with origin validation** - Configurable allowed origins, `AllowCredentials: false`
6. **WebSocket origin validation** - Matches CORS policy, same-origin by default
7. **Rate limiting** - 300 req/min per IP, 10k IP cap
8. **Security headers** - CSP, X-Frame-Options, X-Content-Type-Options
9. **Privilege dropping** - Unix daemon drops to unprivileged user
10. **OpenBSD pledge** - Restricts syscalls to minimum required
11. **Firmware path validation** - `ResolveFirmwarePath()` prevents directory traversal

### Content Security Policy (CSP)

CSP is configurable via settings to allow different map tile providers:

| Setting | Default | Description |
|---------|---------|-------------|
| `csp_img_sources` | (empty) | Additional img-src domains (space-separated) |
| `csp_connect_sources` | (empty) | Additional connect-src domains (space-separated) |

**Base CSP (always applied):**
- `default-src 'self'`
- `script-src 'self' https://unpkg.com`
- `script-src-attr 'none'`
- `style-src 'self' 'unsafe-inline' https://unpkg.com`
- `font-src 'self' data:`
- `img-src 'self' data: https://*.tile.openstreetmap.org https://*.basemaps.cartocdn.com` + extra
- `connect-src 'self' wss:` + extra

**Example - adding Mapbox tiles:**
```sql
UPDATE settings SET value = 'https://*.tiles.mapbox.com https://api.mapbox.com' 
WHERE key = 'csp_img_sources';
```

Note: CSP changes require server restart to take effect.

### Password Hashing

Passwords are hashed using bcrypt:

```go
func hashPassword(password string) string {
    hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
    if err != nil {
        return ""
    }
    return string(hash)
}

func verifyPassword(password, stored string) bool {
    err := bcrypt.CompareHashAndPassword([]byte(stored), []byte(password))
    return err == nil
}
```

### Firmware Path Security

Firmware operations validate paths to prevent directory traversal:

```go
// ResolveFirmwarePath safely resolves a firmware name to a validated path
func (s *Service) ResolveFirmwarePath(firmwareName string) (string, error) {
    // Reject path separators and traversal attempts
    if strings.ContainsAny(firmwareName, "/\\") || strings.Contains(firmwareName, "..") {
        return "", fmt.Errorf("invalid firmware name: path separators not allowed")
    }
    
    // Build absolute path
    firmwarePath := filepath.Join(s.firmwarePath, firmwareName)
    absPath, err := filepath.Abs(firmwarePath)
    if err != nil {
        return "", fmt.Errorf("invalid path: %w", err)
    }
    
    // Verify path is within firmware directory
    absFirmwareDir, _ := filepath.Abs(s.firmwarePath)
    if !strings.HasPrefix(absPath, absFirmwareDir+string(filepath.Separator)) {
        return "", fmt.Errorf("path escapes firmware directory")
    }
    
    // Verify file exists
    if _, err := os.Stat(absPath); err != nil {
        return "", fmt.Errorf("firmware not found: %s", firmwareName)
    }
    
    return absPath, nil
}

// ListFirmwarePublic returns firmware files with paths stripped for API responses
func (s *Service) ListFirmwarePublic() ([]FirmwareFile, error) {
    files, err := s.ListFirmware()
    if err != nil {
        return nil, err
    }
    // Strip paths - return only name, size, version, etc.
    for i := range files {
        files[i].Path = ""
    }
    return files, nil
}
```

**API changes:**
- `GET /firmware` returns only filename, not full path
- `POST /devices/{id}/upgrade` accepts firmware name, not path
- Server resolves name to validated path internally

### Security Audit Summary (December 2024)

Complete security remediation performed. See `AUDIT.md` for full details.

**CRITICAL Fixes:**
- [FIXED] Arbitrary file read via SPA path traversal -> Fixed with `os.DirFS` sandboxing
- [FIXED] Stored XSS across UI -> Fixed with comprehensive HTML/attribute escaping

**HIGH Fixes:**
- [FIXED] CORS/WebSocket origin validation -> `AllowCredentials: false`, matching policies
- [FIXED] Firmware upgrade RBAC -> `requireEdit()` on all upgrade endpoints
- [FIXED] Firmware path traversal -> `ResolveFirmwarePath()` validates names, strips paths from API
- [FIXED] Config backup/restore panics -> Nil checks on file operations
- [FIXED] Scheduler SQL injection -> Parameterized queries, `pq.Array()` for integer arrays

**MEDIUM Fixes:**
- [FIXED] Settings information leakage -> Expanded sensitive key filtering for non-admins
- [FIXED] Weak password hashing -> Using bcrypt with cost 10

**Build Quality:**
- [FIXED] Added `go.sum` for reproducible builds and dependency verification

---

## Data Model

### What Goes in the Database (Static/Inventory)

```sql
-- Device inventory (changes rarely)
devices:
  - id (PK)
  - mac (unique identifier, ALWAYS LOWERCASE)
  - ip_address (management IP)
  - hostname
  - product (Wave-LR, LTU-LR, etc.)
  - model
  - platform (wave, ltu, airmax, airfiber)
  - flavor (GMC, GMP, MGMP, GP, MW, AFLTU, AFLTUROCKET, AF5XHD, XC, 2XC, WA, 2WA, XM, XW)
  - firmware (full firmware string, e.g., "GMC.ipq5018.v4.1.0.abc123.251220.0747.bin")
  - firmware_version (extracted version with suffixes preserved, e.g., "v4.1.0-beta")
  - parent_id (FK to parent AP, NULL for APs)
  - role ("ap" or "sta"; best-effort classification from polling)
  - managed (boolean; true when explicitly added via Add IP/Add Bulk; forces direct polling even if parent_id is set)
  - username (device credentials)
  - password (device credentials, encrypted)
  - first_seen
  - created_by (user who added it)
```

**Topology loop guardrail**

A legitimate `STA` should always have an `AP` parent. In the field we sometimes see a topology loop when a directly-polled
station mis-identifies its AP as a "peer" device, which can result in the AP being learned/updated in the DB as a `STA` and
parented by another `STA`. Once that happens, WC would stop directly polling the AP (because it now has `parent_id` set).

To self-heal:

- Before each polling cycle, WC checks for obviously-invalid parent relationships:
  - `role=sta` whose parent is also `role=sta`
  - self-parent loops (`parent_id == id`)
  - `role=ap` that somehow has a non-NULL `parent_id`
- WC clears `parent_id` (and `parent_mac`) for those devices, causing them to be polled directly again.
- On the next successful poll, platform-specific role detection promotes true APs back to `role=ap` (and clears parent fields).
  STAs that were temporarily un-parented will be re-attached when their AP is next polled.

**MAC Address Normalization:**

MAC addresses are **always stored and compared in lowercase**. Ubiquiti devices inconsistently report MACs in either case (sometimes `AA:BB:CC:DD:EE:FF`, sometimes `aa:bb:cc:dd:ee:ff`), even varying between API calls on the same device.

To prevent duplicate records and failed lookups:
- All MAC addresses are normalized to lowercase at ingestion time
- Discovery functions (`discoverDevice`, `discoverAirMAXDevice`) normalize before returning
- Poller functions normalize when reading from API responses
- Stats store normalizes on `Update()` and `UpdatePeers()`
- Database queries use lowercase MACs

```go
// NormalizeMAC is defined in internal/stats/store.go
func NormalizeMAC(mac string) string {
    return strings.ToLower(mac)
}
```

```sql
-- Configuration
settings:
  - key (PK)
  - value
  
-- Example settings:
--   poll_interval: 30
--   ap_cred1_user: ""       -- credential slots are explicitly configured
--   ap_cred1_pass: ""       -- secret value is encrypted at rest
--   sta_cred1_user: ""
--   sta_cred1_pass: ""
--   firmware_path: firmware
--   backup_dir: backups
--   zabbix_enabled: true
--   zabbix_listen: 127.0.0.1:10050

-- AP groups (optional organization)
device_groups:
  - id
  - name
  - description

device_group_members:
  - device_id
  - group_id

-- Users, roles, changelog (same as before)
```

### What Stays in Memory (Real-time Stats)

```go
// In-memory device state (refreshed every poll cycle)
type DeviceStats struct {
    // Identity (for correlation)
    MAC       string
    IP        string
    
    // Status
    Online    bool
    LastSeen  time.Time
    LastError string
    
    // General stats
    Uptime    int64   // seconds
    CPUUsage  float64 // percent
    MemUsage  float64 // percent
    Temperature float64 // celsius
    
    // Wireless stats (varies by device type)
    Wireless  WirelessStats
    
    // Connected peers (for APs)
    Peers     []PeerStats
    PeerCount int
}

type WirelessStats struct {
    // 60 GHz radio
    Radio60GHz *RadioStats
    
    // 5 GHz radio (backup/failover on some models)
    Radio5GHz  *RadioStats
}

type RadioStats struct {
    Frequency   int     // MHz
    ChannelWidth int    // MHz
    TxPower     int     // dBm
    NoiseFloor  int     // dBm
    
    // Per-chain signal (critical for troubleshooting)
    Chains      []ChainStats
    
    // Aggregate rates
    TxRate      int64   // bps
    RxRate      int64   // bps
    TxBytes     int64
    RxBytes     int64
    TxPackets   int64
    RxPackets   int64
    TxErrors    int64
    RxErrors    int64
    
    // Quality metrics
    LinkScore   int     // 0-100
    Airtime     float64 // percent utilization
}

type ChainStats struct {
    Index     int
    Signal    int     // dBm
    NoiseFloor int    // dBm
    // SNR is computed as Signal - NoiseFloor.
    // NOTE (Wave): NoiseFloor is a long-term average, so SNR should be treated as an estimate.
    SNR       int     // dB
}

type PeerStats struct {
    // Identity
    MAC       string
    IP        string
    Hostname  string
    
    // Connection
    SSID      string
    ConnectedTime int64 // seconds
    Distance  float64   // meters
    
    // Per-radio signal stats (populated based on platform)
    Radio60GHz *PeerRadioStats  // Wave 60GHz main link
    Radio5GHz  *PeerRadioStats  // Wave 5GHz backup, airMAX main
    RadioLTU   *PeerRadioStats  // LTU main link
    
    // Legacy remote signal (airMAX/LTU backward compatibility)
    RemoteSignal         int
    RemoteNoiseFloor     int
    RemoteSignalPerChain []int
}

type PeerRadioStats struct {
    ID        string
    Active    bool
    Connected bool
    
    // AP RX (what AP receives from STA) - from "local" section
    Signal         int       // dBm (aggregate)
    SignalPerChain []int     // per-chain dBm
    NoiseFloor     int       // dBm
    SNR            int       // signal - noise_floor
    RemoteSNR       int       // remote_signal - remote_noise_floor
    
    // STA RX (what STA receives from AP) - from "remote" section
    RemoteSignal         int
    RemoteSignalPerChain []int
    RemoteNoiseFloor     int
    
    // Modulation/capacity
    MCS       *MCSStats
    Capacity  *CapacityStats  // dl/ul/combined in bps
    // CINR (Carrier to Interference & Noise Ratio), in dB.
    // dl: AP -> STA (TX on AP; RX on STA)
    // ul: STA -> AP (TX on STA; RX on AP)
    CINR      *CINRStats

    // EVM (Error Vector Magnitude), in dB.
    // IMPORTANT: For AirMAX, the raw EVM values we collect are normalized so that "higher is better"
    // in this UI (see internal/stats/store.go for details).
    // dl: AP -> STA (TX on AP; RX on STA)
    // ul: STA -> AP (TX on STA; RX on AP)
    EVM      *EVMStats
    LinkScore *LinkScore      // dl/ul percentages
    
    // Airtime
    AirtimeDL float64
    AirtimeUL float64
}

type CINRStats struct {
    DL float64 // dB (AP -> STA)
    UL float64 // dB (STA -> AP)
}

type EVMStats struct {
    DL float64 // dB (AP -> STA)
    UL float64 // dB (STA -> AP)

    // NOTE: AirMAX EVM is re-mapped into a "higher is better" representation in the poller.
    // See internal/stats/store.go comments for details.
}

type CapacityStats struct {
    DL            int64  // bps
    UL            int64  // bps
    Combined      int64  // bps
    DLIdeal       int64  // bps (max achievable)
    ULIdeal       int64  // bps
    CombinedIdeal int64  // bps
}
```

---

## Architecture

### Component Overview

```
+-------------------------------------------------------------+
|                      waveControl Server                      |
+-------------------------------------------------------------+
|                                                             |
|  +-------------+    +-------------+    +-------------+     |
|  |   HTTP API  |    |  Poller     |    |  Zabbix     |     |
|  |  (chi/REST) |    |  (30s loop) |    |  Bridge     |     |
|  +------+------+    +------+------+    +------+------+     |
|         |                  |                  |             |
|         +--------+---------+---------+--------+             |
|                  |                   |                      |
|         +--------v--------+ +--------v--------+            |
|         |  Stats Store    | |  Device Store   |            |
|         |  (in-memory)    | |  (PostgreSQL)   |            |
|         +-----------------+ +-----------------+            |
|                                                             |
+-------------------------------------------------------------+
                              |
                              v
              +-------------------------------+
              |     Ubiquiti Devices          |
              |  +-----+  +-----+  +-----+   |
              |  | AP1 |  | AP2 |  | AP3 |   |
              |  +--+--+  +--+--+  +--+--+   |
              |     |        |        |       |
              |  +--+--+  +--+--+  +--+--+   |
              |  |STAs |  |STAs |  |STAs |   |
              |  +-----+  +-----+  +-----+   |
              +-------------------------------+
```

### Stats Store (In-Memory)

```go
type StatsStore struct {
    mu      sync.RWMutex
    devices map[string]*DeviceStats  // keyed by MAC
    
    // Index by IP for fast lookup
    byIP    map[string]string        // IP -> MAC
}

func (s *StatsStore) Get(mac string) *DeviceStats
func (s *StatsStore) GetByIP(ip string) *DeviceStats
func (s *StatsStore) Update(mac string, stats *DeviceStats)
func (s *StatsStore) List() []*DeviceStats
func (s *StatsStore) ListAPs() []*DeviceStats
func (s *StatsStore) GetPeers(apMAC string) []*DeviceStats
```

### Poller

```go
type Poller struct {
    db          *sql.DB
    stats       *StatsStore
    interval    time.Duration
    
    // Credential cache (from DB)
    creds       map[string]Credentials  // MAC -> creds
    defaultUser string
    defaultPass []string
}

func (p *Poller) Start(ctx context.Context)
func (p *Poller) pollDevice(ip string, creds Credentials) (*DeviceStats, error)
func (p *Poller) discoverSTAs(apIP string, creds Credentials) ([]STAInfo, error)

// Poll cycle:
// 1. Load AP list from database
// 2. For each AP:
//    a. Connect and authenticate
//    b. Fetch /device for static info (update DB if changed)
//    c. Fetch /statistics for real-time stats (update memory)
//    d. Extract connected STAs
//    e. For each STA: update memory stats
// 3. Mark devices not seen as offline
// 4. Sleep until next interval
```

### State-Transition Database Pattern

**Principle**: Database writes only occur on state changes, not on every poll cycle.

This pattern dramatically reduces database I/O - with 5000 devices at 30-second polls, this eliminates ~600,000 unnecessary writes per hour.

#### What Triggers a Database Write

| Event | Database Action |
|-------|-----------------|
| Device comes online (was offline/unknown) | `UPDATE status='online', last_seen=NOW()` |
| Device goes offline (was online) | `UPDATE status='offline', last_seen=NOW()` |
| Hostname changes | `UPDATE hostname=?` |
| Firmware changes | `UPDATE firmware=?, firmware_version=?` |
| Other static info changes | Update via `IS DISTINCT FROM` check |

#### What Does NOT Trigger a Database Write

| Event | Where Data Lives |
|-------|------------------|
| Successful poll (device already online) | Memory store only |
| Stats refresh (CPU, memory, signal, rates) | Memory store only |
| Peer list update | Memory store only |
| Real-time counters | Memory store only |

#### Implementation Pattern

```go
// Update memory store - returns true if state changed (offline->online)
becameOnline := store.Update(ip, deviceStats)

// Only write to DB on state transition
if becameOnline {
    db.Exec(`UPDATE devices SET status = 'online', last_seen = NOW() WHERE id = $1`, deviceID)
}

// For failures - SetOffline returns true if state changed (online->offline)
becameOffline := store.SetOffline(ip, errorMessage)
if becameOffline {
    db.Exec(`UPDATE devices SET status = 'offline', last_seen = NOW() WHERE id = $1`, deviceID)
}
```

#### Periodic Batch Sync

For crash recovery, a periodic batch sync runs every ~10 minutes:
- Syncs `last_seen` timestamps to database
- Ensures `status` column matches memory state
- Single bulk query instead of per-device writes

```go
// Every 20 poll cycles (~10 min at 30s interval)
if cleanupCounter%20 == 0 {
    p.batchSyncToDB()
}
```

This pattern applies uniformly to all device types: Wave, LTU, airMAX AC/M, AirFiber.

### Device Identification

**MAC address is the authoritative unique identifier for all devices.**

#### Why MAC, Not IP?

- **IP addresses are unstable**: Devices can get new DHCP leases, change subnets, or roam between networks
- **airMAX IP is especially unreliable**: airMAX reports the "last seen" IP from wireless frames, not the configured management IP
- **MAC is burned in**: Every device has a unique, permanent MAC address

#### MAC Mismatch Protection

When wavecontrol connects to an IP, it may observe one or more MAC addresses from the device APIs. In a real network, **IPs get reused** (DHCP, repurposed radios, swap-outs), which can lead to a dangerous failure mode: applying new radio stats to an old device record.

To prevent this, the poller enforces a **canonical MAC** per poll job:

- The database/job MAC (`devices.mac`) is the authoritative identity.
- For each poll, the poller collects a set of observed MAC candidates from the API response.
  - **airMAX/LTU**: the **Ethernet/management (eth0) MAC is the canonical identity**. `wireless.apmac` may be observed and logged for context, but is **not preferred** when `eth0` is present.
  - **Wave**: `device.mac` from the Wave API stats response.
- If a preferred/canonical MAC candidate is present (e.g. `eth0`), the expected/job MAC must match it.
  - Other observed MACs are informational and are included in logs/context.
- If the device returns a MAC candidate set and **none match the expected/job MAC**, wavecontrol treats this as a data quality issue:
  - Do **not** apply the stats/peers update to the expected device
  - Mark the expected device `status=unknown`, `status_reason=mac_mismatch`
  - Do **not** advance `last_seen` for the expected device (we did not see it)
  - Persist `device_identity_mismatches` with expected MAC, observed MAC candidates, observed IP, source, timestamp, and last error
  - Broadcast a WebSocket patch containing `identity_mismatch` context for the selected detail pane
  - Log a warning once with IP, expected MAC, and observed MAC candidates

#### Replacement MAC Adoption

A MAC mismatch is never auto-healed. AP or directly managed STA replacement must be an explicit operator action because the same symptom can also mean DHCP reuse, inventory duplication, or a wrong IP assignment.

The operator may resolve a confirmed AP or managed STA swap through `POST /api/wavecontrol/devices/{id}/learn-mac`. The server must:

1. Require editor/administrator permission.
2. Verify the device is still in `status_reason=mac_mismatch`.
3. Verify a persisted identity-mismatch row exists for the device.
4. Verify the requested new MAC is one of the observed MAC candidates.
5. Verify the observed IP still matches the device row IP.
6. Reject the request if the observed MAC already exists on another device row.
7. Update the device MAC, clear mismatch state, remove stale in-memory stats, write changelog, broadcast a WebSocket patch, and queue a refresh. For AP rows only, rewrite child `parent_mac` references from the old AP MAC to the new AP MAC; for STA rows, keep the AP association unchanged.

#### Identification Flow

```
AP Poll Response -> Peer List
                      |
                      v
              peer.MAC available?
                  |       |
                  Yes     No
                  |       |
                  v       v
        Match by MAC    Skip peer
              |         (insufficient data)
              v
    Found in database?
        |       |
        Yes     No
        |       |
        v       v
    Update    Insert new
    device    device record
```

#### Database Key

```sql
-- MAC is the lookup key for device identity
SELECT * FROM devices WHERE mac = $1;

-- IP is stored but not used for identity matching
UPDATE devices SET ip_address = $2 WHERE mac = $1;
```

#### In-Memory Stats Store

```go
// Store is keyed by MAC (lowercase). IP-only entries use the key "ip:<addr>".
type Store struct {
    devices map[string]*DeviceStats  // key: MAC or "ip:<addr>"
    byMAC   map[string]string        // MAC -> current IP
    byIP    map[string]string        // IP -> MAC
}

// Lookups by IP do not treat IP as identity.
func (s *Store) Get(ip string) *DeviceStats {
    if mac := s.byIP[ip]; mac != "" {
        return s.devices[mac]
    }
    return s.devices["ip:"+ip]
}
```

#### API Peer Matching

```go
// Prefer MAC match (authoritative).
// If MAC is missing (rare), fall back to management IP match (filtered).
for _, peer := range parentStats.Peers {
    if peer.MAC != "" && peer.MAC == deviceMAC {
        // Found - populate live stats
    }
}
```

#### STA Parent Changes (Roaming)

When a STA moves from one AP to another:

1. New AP reports STA in peer list (with MAC)
2. `updateSTAsInDB()` finds existing record by MAC
3. `parent_id` is updated to new AP
4. Site may be cleared if SSID changed (device repurposed)

```go
// Check for role or SSID change
wasStandalone := !existingParentID.Valid
if wasStandalone {
    // Clear site - was AP, now STA
    shouldClearSite = true
}
if existingSSID.String != newSSID {
    // Clear site - moved to different network
    shouldClearSite = true
}
```

### Management IP Prefix Filter

airMAX devices report unreliable IP addresses - they often report the "last seen" IP from wireless frames rather than the configured management IP. This includes customer traffic IPs, DHCP addresses from connected clients, and other transient addresses that pass through the link. These IPs cannot be used for device management.

The **Management IP Prefix** feature filters which IPs are stored in the database, ensuring only reachable management IPs are recorded.

#### Configuration

In Settings > Management IP Prefixes, add CIDR prefixes (one per line):
```
172.24.0.0/16
10.0.0.0/8
```

#### Behavior

- **No prefixes configured**: All IPs are accepted (default, backward compatible)
- **Prefixes configured**: Only IPs matching a prefix are ever stored; non-matching IPs are completely ignored

#### The Golden Rule

When management prefixes are configured:
- **Management IP presented** → **ALWAYS update** the database (if different from current)
- **Non-management IP presented** → **NEVER touch** the IP field (ignore completely)

This ensures that spurious non-management IPs (customer traffic, DHCP leases, etc.) can never overwrite or clear a valid management IP, even if they are reported more recently.

#### IP Learning Rules

**For new devices** (auto-discovered STAs):
- Device is created if it has a MAC address (MAC is the authoritative identifier)
- If reported IP matches a prefix → store that IP
- If reported IP doesn't match → store NULL for IP (device is tracked, IP will be captured later)

**For existing devices**:
- If new IP matches a prefix → update IP in database
- If new IP doesn't match → **do nothing** (leave existing IP completely unchanged)

#### Why This Matters

airMAX radios (especially older models) report whatever IP they see in wireless frames:
1. Customer CPE behind the radio sends traffic with source 192.168.1.x
2. airMAX radio reports the STA as having IP 192.168.1.x
3. This IP is useless for management - the radio's actual management IP might be 172.24.50.100

By configuring management prefixes:
- Only 172.24.x.x IPs are stored
- Device appears in database with NULL IP initially
- When radio reports 172.24.50.100, it gets stored
- Future spurious IPs (192.168.x.x) are ignored

#### Implementation

The management IP prefix filter is enforced in two coordinated layers with restart-safe seeding:

**Memory Store (stats.Store)**:
```go
// Store has an IP filter function and MAC->IP tracking
type Store struct {
    devices  map[string]*DeviceStats  // keyed by MAC (lowercase); IP-only devices are keyed by "ip:<addr>"
    byMAC    map[string]string        // MAC -> IP lookup (effective management IP)
    byIP     map[string]string        // IP -> MAC lookup (for lookups without treating IP as identity)
    ipFilter func(string) bool        // Set by poller from management prefixes
}

// SetIPFilter sets the filter function (called when prefixes change)
func (s *Store) SetIPFilter(fn func(string) bool)

// GetIPByMAC returns current effective IP for a MAC (empty if not found)
func (s *Store) GetIPByMAC(mac string) string

// SetIPForMAC seeds the store from DB (used on restart)
// Only sets if IP passes filter; returns true if changed
func (s *Store) SetIPForMAC(mac, ip string) bool

// UpdatePeers updates STA stats, returns map of MAC->newIP for changed IPs
func (s *Store) UpdatePeers(apIP string, peers []*PeerStats) map[string]string {
    ipChanges := make(map[string]string)
    for _, peer := range peers {
        ipAllowed := s.ipFilter == nil || s.ipFilter(peer.IP)
        currentIP := s.byMAC[peer.MAC]
        
        if ipAllowed && peer.IP != "" && currentIP != peer.IP {
            // Management IP changed - update store and track for DB sync
            sta.IP = peer.IP
            ipChanges[peer.MAC] = peer.IP
        }
        // Non-management IP: don't touch store IP, don't add to ipChanges
        // Other stats (hostname, signal, etc.) always updated
    }
    return ipChanges
}
```

**Database Layer (poller.updateSTAsInDB)**:
```go
func (p *Poller) updateSTAsInDB(apID int64, peers []*PeerStats, ipChanges map[string]string) {
    for _, peer := range peers {
        // Query existing device from DB
        var existingIP sql.NullString
        db.QueryRow(`SELECT host(ip_address) FROM devices WHERE mac = $1`, peer.MAC).Scan(&existingIP)
        
        // RESTART SEEDING: If DB has valid IP but store doesn't, seed store from DB
        storeIP := p.store.GetIPByMAC(peer.MAC)
        if existingIP.Valid && existingIP.String != "" && storeIP == "" {
            if p.isAllowedIP(existingIP.String) {
                p.store.SetIPForMAC(peer.MAC, existingIP.String)
                // Now store has the IP from DB
            }
        }
        
        // Check if store updated the IP this cycle
        newIP, ipChanged := ipChanges[peer.MAC]
        
        if ipChanged {
            // Store updated IP - sync to database
            db.Exec(`UPDATE devices SET ip_address = $1, ... WHERE mac = $2`, newIP, peer.MAC)
        } else {
            // Store didn't change IP - don't touch ip_address in DB
            db.Exec(`UPDATE devices SET hostname = $1, ... WHERE mac = $2`, peer.Hostname, peer.MAC)
        }
    }
}
```

**The Golden Rule**:
- Management IP presented → Update memory store → Sync to DB
- Non-management IP presented → Ignore completely (no store change, no DB write)
- On restart: Seed memory store from DB (preserves valid IPs across restarts)

**Data Flow**:
```
┌─────────────────────────────────────────────────────────────────┐
│                        NORMAL OPERATION                         │
├─────────────────────────────────────────────────────────────────┤
│  AP reports STA                                                 │
│       ↓                                                         │
│  store.UpdatePeers()                                            │
│       ↓                                                         │
│  IP in management prefix?                                       │
│       ├─ YES → Update store.IP, add to ipChanges                │
│       └─ NO  → Keep existing store.IP, ipChanges empty          │
│       ↓                                                         │
│  updateSTAsInDB(ipChanges)                                      │
│       ↓                                                         │
│  MAC in ipChanges?                                              │
│       ├─ YES → UPDATE devices SET ip_address = newIP ...        │
│       └─ NO  → UPDATE devices SET hostname = ... (no IP change) │
└─────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────┐
│                      AFTER RESTART                              │
├─────────────────────────────────────────────────────────────────┤
│  Memory store is empty                                          │
│       ↓                                                         │
│  AP reports STA with non-management IP                          │
│       ↓                                                         │
│  store.UpdatePeers() → rejects IP, store stays empty            │
│       ↓                                                         │
│  updateSTAsInDB() queries DB                                    │
│       ↓                                                         │
│  DB has valid management IP, store is empty                     │
│       ↓                                                         │
│  SEED: store.SetIPForMAC(mac, dbIP)                             │
│       ↓                                                         │
│  Store now has correct IP from DB                               │
│       ↓                                                         │
│  Future non-mgmt IPs ignored, valid IP preserved                │
└─────────────────────────────────────────────────────────────────┘
```

This approach ensures:
1. Memory store is authoritative for "current effective IP"
2. Database only updated when memory store changes
3. Non-management IPs never touch either memory or database
4. Redundant DB writes avoided (only write when something changed)
5. Valid management IPs survive restarts (seeded from DB)

#### Example Scenarios

With prefix `172.24.0.0/16` configured:

**Scenario 1: New STA with management IP**
| Reported | Action |
|----------|--------|
| MAC: AA:BB:CC:DD:EE:FF, IP: 172.24.34.24 | Create device, store IP 172.24.34.24 |

**Scenario 2: New STA with non-management IP**
| Reported | Action |
|----------|--------|
| MAC: AA:BB:CC:DD:EE:FF, IP: 192.168.1.50 | Create device, store IP as NULL |

**Scenario 3: Existing STA (NULL IP) reports management IP**
| Before | Reported | After |
|--------|----------|-------|
| IP: NULL | 172.24.34.24 | IP: 172.24.34.24 |

**Scenario 4: Existing STA (has management IP) reports non-management IP**
| Before | Reported | After |
|--------|----------|-------|
| IP: 172.24.34.24 | 192.168.1.50 | IP: 172.24.34.24 (unchanged - non-mgmt IP ignored) |

**Scenario 5: Existing STA (has non-management IP from before config) reports management IP**
| Before | Reported | After |
|--------|----------|-------|
| IP: 192.168.1.50 | 172.24.80.100 | IP: 172.24.80.100 (updated - captured management IP) |

**Scenario 6: Existing STA (has non-management IP) reports another non-management IP**
| Before | Reported | After |
|--------|----------|-------|
| IP: 192.168.1.50 | 10.0.0.100 | IP: 192.168.1.50 (unchanged - non-mgmt IP ignored) |

**Scenario 7: Existing STA (NULL IP) reports non-management IP**
| Before | Reported | After |
|--------|----------|-------|
| IP: NULL | 192.168.1.50 | IP: NULL (unchanged - non-mgmt IP ignored) |

**Scenario 8: After restart - DB has valid IP, store empty, non-mgmt IP reported**
| DB State | Store State | Reported | After |
|----------|-------------|----------|-------|
| IP: 172.24.34.24 | (empty) | 192.168.1.50 | Store seeded with 172.24.34.24 from DB, non-mgmt IP ignored |

**Scenario 9: After restart - DB has valid IP, store empty, mgmt IP reported**
| DB State | Store State | Reported | After |
|----------|-------------|----------|-------|
| IP: 172.24.34.24 | (empty) | 172.24.50.100 | Store updated to 172.24.50.100, DB synced to new IP |

#### Manual vs Automatic Device Addition

**Automatic (STA auto-discovery)**: Management prefix filtering is **always applied**. When an AP reports its connected STAs, only management-prefix IPs are stored. This prevents storing spurious IPs that airMAX devices report.

**Manual (Add Device / Bulk Add)**: Management prefix filtering is **NOT applied**. When you manually add a device by IP address, you're explicitly telling the system "this is the correct management IP" - it's stored as provided. Use manual addition if:
- The device's management IP is outside your configured prefixes
- You need to override a NULL or incorrect IP
- The device never reports a valid management IP automatically

#### Troubleshooting

**Device shows NULL IP**: 
- The device has never reported an IP within the management prefix
- Check if the device's management IP is actually in one of the configured prefixes
- The device may be misconfigured or not reporting its management interface
- **Fix**: Manually add/edit the device with the correct IP

**Device shows old non-management IP**:
- This IP was stored before management prefixes were configured
- The IP will be updated when the device reports a valid management IP
- Non-management IPs are never cleared automatically (to preserve any working IP)
- **Fix**: Manually edit the device's IP in the UI, or wait for valid management IP

**Devices not appearing**:
- Devices without a MAC address cannot be tracked
- Ensure the AP is reporting peer information correctly

**Debug logs to watch for**:
- `"STA XX:XX: syncing IP X.X.X.X to database (store updated)"` - store changed IP, syncing to DB
- `"STA XX:XX: seeded memory store with DB IP X.X.X.X"` - after restart, restored valid IP from DB
- `"Auto-discovered new STA: XX:XX (IP X.X.X.X not in management prefixes, storing NULL)"` - new device with bad IP

---

## Security

### Process Security

waveControl implements defense-in-depth security measures:

1. **Daemonization** - By default, forks to background. Use `-d` flag for foreground/debug mode.

2. **Privilege Dropping** - When started as root:
   - Looks for user `_wavecontrol`, then `www`, then `nobody` (or use `-u` flag)
   - Chroots to user's home directory (use `-U` to skip chroot)
   - Drops to that user after binding ports
   - Sets supplementary group to `www` if available

3. **OpenBSD Pledge** - Uses pledge(2) to restrict syscalls:
   ```
   promises: stdio rpath wpath cpath inet dns
   ```

4. **PID File** - Writes PID to `/var/run/wavecontrol.pid` (configurable with `-pidfile`)

### Logging

Two logging modes based on `-d` flag:

**Debug mode (`-d`):**
- All log output to stderr with timestamps
- Verbose logging of all operations
- Useful for development and troubleshooting

**Daemon mode (default):**
- Errors only, sent to syslog (LOG_DAEMON facility)
- No debug output
- Only critical errors: database connection failures, bind failures, etc.

```go
// Logging functions
logDebug(format, v...)  // Only in debug mode
logError(format, v...)  // Always logged (syslog in daemon)
logFatal(format, v...)  // Logs and exits
```

### Ultra Debug (Per-target HTTP Capture)

Wavecontrol includes an **Ultra Debug** facility that captures full HTTP request/response details (headers + bodies) for debugging device interactions **without flooding syslog**.

Key properties:
- **Opt-in per target** (device ID or host) — nothing is captured unless Ultra Debug is enabled.
- **In-memory ring buffer** — up to **32 MB per target**; old entries are overwritten round-robin.
- **Scopes**:
  - **Device scope**: for a specific device ID that wavecontrol talks to (e.g., polling or firmware upgrade).
  - **Host scope**: for non-device flows where there is no device ID (e.g., discovery or other host-only HTTP interactions).
- **Captured content**: method, URL, headers, request body, response status, response headers, response body, error (if any), and timing.

#### API

Ultra Debug is controlled via HTTP API endpoints:

- **Toggle per-device capture**
  - `POST /api/wavecontrol/devices/{id}/ultra-debug`
  - Body: `{ "enabled": true }` or `{ "enabled": false }`

- **List enabled targets**
  - `GET /api/wavecontrol/ultra-debug`
  - Returns enabled device IDs and enabled hosts (used by the UI badge + target selector).

- **Fetch device buffer**
  - `GET /api/wavecontrol/ultra-debug/{id}?tail=200`
  - Returns a snapshot (`enabled`, `max_bytes`, `bytes_used`, and `log` entries).

- **Download device buffer**
  - `GET /api/wavecontrol/ultra-debug/{id}/download`
  - Returns the raw JSON entry array.

- **Clear device buffer**
  - `POST /api/wavecontrol/ultra-debug/{id}/clear`
  - Clears the in-memory log entries for that device (capture remains enabled).

- **Toggle host capture**
  - `POST /api/wavecontrol/ultra-debug/host`
  - Body: `{ "host": "172.20.1.10", "enabled": true }`

- **Fetch host buffer**
  - `GET /api/wavecontrol/ultra-debug/host/{host}?tail=200`

- **Download host buffer**
  - `GET /api/wavecontrol/ultra-debug/host/{host}/download`

- **Clear host buffer**
  - `POST /api/wavecontrol/ultra-debug/host/{host}/clear`
  - Clears the in-memory log entries for that host (capture remains enabled).

#### Entry format

Each captured log entry is a JSON object with fields similar to:

- `ts`: RFC3339 timestamp
- `scope`: `"device"` or `"host"`
- `device_id` (device scope) or `host` (host scope)
- `label`: caller-provided label (e.g., `"wave/login"`, `"airmax/stations"`, `"firmware/trigger"`)
- `method`, `url`
- `request_headers`, `request_body`
- `response_status`, `response_headers`, `response_body`
- `error` (if the request failed)
- `duration_ms` (timing)

#### UI

- A top-bar **ULTRA** badge appears when any Ultra Debug target is enabled.
- Clicking the badge opens the Ultra Debug modal.
- The modal supports:
  - Target selection (device / host)
  - Tail size selection (how many entries to fetch)
  - Auto-refresh
  - **Clear** (empties the selected buffer without disabling capture)
  - **Disable** (turns off capture for the selected target)
  - **Pretty** view (human-friendly, collapsible entries)
  - **Raw JSON** view (full export)
  - **Copy** and **Download** actions (raw JSON)

### Command Line Flags

```
-d              Debug mode (foreground, verbose logging to stderr)
-web            Standalone HTTP server mode (implies -d)
-addr string    Listen address (override settings)
-webroot string Path to web directory
-pidfile string PID file path (daemon mode)
-U              Unchrooted mode (skip chroot, just chdir)
-u string       User to run as
```

### Chroot (Default Behavior)

When running as root, wavecontrol chroots to the user's home directory by default. This provides strong isolation through filesystem restriction.

**Chroot Semantics - Order of Operations:**

1. **Pre-chroot initialization (as root):**
   - Parse command-line flags and load settings from database
   - Open database connection (TCP to PostgreSQL)
   - Bind HTTP listen socket
   - Load TLS certificates (if configured)
   - Initialize logging

2. **Chroot transition:**
   - `chroot(user_home_dir)` - Changes filesystem root
   - `chdir("/")` - Change to new root
   - All paths are now relative to the jail

3. **Privilege drop:**
   - `setgid(user_gid)` - Drop group privileges
   - `setgroups([www_gid])` - Set supplementary groups
   - `setuid(user_uid)` - Drop user privileges (irreversible)

4. **Post-chroot operation (unprivileged):**
   - Serve web assets from `./web/` inside jail
   - Store firmware in `./firmware/` inside jail
   - Write config backups to `./backups/` inside jail
   - No access to `/etc`, `/usr`, `/bin`, or any path outside jail

**Security properties:**
- Filesystem escape requires kernel vulnerability
- No shell or system binaries available
- Database credentials in memory only (not in filesystem)
- Even if application is compromised, attacker is jailed

**Requirements:**
1. **Use IP address in DSN** - `127.0.0.1` not `localhost` (no DNS resolution after chroot)
2. **PostgreSQL TCP** - Configure PostgreSQL to accept TCP connections (Unix sockets unreachable)
3. **Web assets** - Copy web directory to user's home before starting
4. **Firmware directory** - Create `firmware/` inside jail for firmware storage

Use `-U` flag to disable chroot (just chdir to user's home without chroot).

Example setup:
```bash
# Create user and directories
useradd -r -s /usr/sbin/nologin -d /var/wavecontrol -m _wavecontrol
mkdir -p /var/wavecontrol/{web,firmware,backups}
chown -R _wavecontrol:_wavecontrol /var/wavecontrol

# Copy web assets
cp -r web/* /var/wavecontrol/web/

# Run (chroots to /var/wavecontrol by default)
./wavecontrol
```

### Platform-Specific Files

```
cmd/server/
+-- daemon_unix.go       # Unix privilege dropping
+-- daemon_openbsd.go    # OpenBSD with pledge
+-- daemon_windows.go    # Windows stubs
+-- logging_unix.go      # Unix syslog
+-- logging_windows.go   # Windows logging
```

---

## Scaling

### Capacity

waveControl is designed for large deployments:

| Scale | APs | STAs | Memory | Workers |
|-------|-----|------|--------|---------|
| Small | 100 | 1,000 | ~10 MB | 10 |
| Medium | 500 | 5,000 | ~25 MB | 25 |
| Large | 2,000 | 20,000 | ~55 MB | 50 |

### Resource Defaults

```go
// Poller
WorkerCount     = 50        // Parallel device pollers
JobQueueSize    = 2500      // Buffered job queue
PollInterval    = 30s       // Device poll frequency

// HTTP Client Pool
MaxIdleConns    = 500       // Total idle connections
MaxConnsPerHost = 4         // Per-device limit
RequestTimeout  = 15s       // Per-request timeout

// Database Pool  
MaxOpenConns    = 100       // Active connections
MaxIdleConns    = 25        // Idle connections
ConnMaxLifetime = 5m        // Connection recycling
```

### Circuit Breaker

Failing devices use exponential backoff to prevent resource waste:

```
Failures 1-2:  No backoff (transient errors)
Failures 3:    1 minute backoff
Failures 4:    2 minute backoff
Failures 5:    4 minute backoff
Failures 6:    8 minute backoff
Failures 7+:   15 minute max backoff
```

Manual refresh via API/UI bypasses the circuit breaker.

### Memory Management

- **Stats Store**: Stale STAs (not seen for 5 minutes) automatically removed
- **Circuit Breaker**: Old entries (30+ minutes) automatically cleaned
- **Rate Limiter**: Capped at 10,000 IPs, excess rejected

### Recommended OS Limits (OpenBSD)

```
# /etc/login.conf
daemon:\
    :datasize=1024M:\
    :maxproc=512:\
    :openfiles=4096:\
    :tc=default:
```

### PostgreSQL Tuning

```ini
# postgresql.conf
max_connections = 200
shared_buffers = 256MB
work_mem = 8MB
```

---

## API Endpoints

### Configuration (Admin Only)

```
GET  /api/wavecontrol/settings
     Returns all settings. Secret values are masked.

PATCH /api/wavecontrol/settings
     Atomically update a settings snapshot
     Body: { "settings": {"key":"value"}, "clear": ["secret_key"] }

PATCH /api/wavecontrol/settings/{key}
     Update one setting
     Body: { "value": "..." } or { "clear": true }

# Settings keys:
# - poll_interval (seconds, default: 30)
# - poller_workers (number of workers, default: 50)
# - ap_cred1_user/ap_cred1_pass through ap_cred3_user/ap_cred3_pass
# - sta_cred1_user/sta_cred1_pass through sta_cred3_user/sta_cred3_pass
#   Credential slots default empty and each username/password pair is atomic.
# - firmware_path (default: "firmware", relative to working dir)
# - backup_dir (default: "backups", relative to working dir)
# - zabbix_enabled (default: false)
# - zabbix_listen (default: "127.0.0.1:10050")
# - management_prefixes (JSON array) - CIDR prefixes to filter learned IPs
```

### Device Management

```
GET  /api/wavecontrol/devices
     List all devices (inventory from DB + live stats from memory)
     Query params:
       - status=online|offline|all
       - type=ap|sta|all
       - group=<group_id>

POST /api/wavecontrol/devices
     Add AP(s) - triggers auto-discovery
     Body: { 
       "ips": ["192.168.1.1", "192.168.1.2"],
       "username": "device-user",    // optional only when a configured credential pair applies
       "password": "device-password",
       "discover_stas": true      // optional, default true
     }

DELETE /api/wavecontrol/devices/{id}
     Remove device (and its STAs if AP)

POST /api/wavecontrol/devices/{id}/refresh
     Force immediate poll of device

POST /api/wavecontrol/devices/{id}/reboot
     Immediate operator reboot of the remote radio. Dispatch MUST use the stored platform/flavor:
       - Wave/Wave MLO/AF60: REST POST /api/v1.0/system/reboot
       - LTU/AF5XHD: REST POST /api/v1.0/system/reboot, with airMAX CGI fallback for hybrid builds
       - airMAX/legacy AirFiber: CGI POST /reboot.cgi reboot=yes
     The UI host info pane exposes this as a direct Reboot button. Scheduled reboot jobs must use the same platform-aware service path.

POST /api/wavecontrol/devices/refresh-all
     Force immediate poll of all devices
```

### Real-Time Stats

```
GET  /api/wavecontrol/stats
     Get all device stats (from memory)
     Returns: { "devices": [...], "updated_at": "..." }

GET  /api/wavecontrol/stats/{ip}
     Get single device stats by IP (also accepts MAC)
     Returns full DeviceStats object

# WebSocket for live updates
WS   /api/wavecontrol/ws
     Stream stats updates using the authenticated HttpOnly session cookie
     Messages: {"type":"stats","device_id":123,"data":{...}}
```

### Bulk Operations

```
POST /api/wavecontrol/devices/bulk-add
     Add multiple APs at once
     Body: { 
       "ips": ["10.0.0.1", "10.0.0.2", ...],
       "username": "device-user",
       "password": "device-password",
       "site_id": 1  // optional
     }

POST /api/wavecontrol/devices/bulk-upgrade
     Upgrade multiple devices
     Body: {
       "device_ids": [1, 2, 3],
       "firmware": "GMC.ipq5018.v4.1.0.bin",
       "force": false
     }

POST /api/wavecontrol/devices/bulk-backup
     Backup configs for multiple devices
     Body: { "device_ids": [1, 2, 3] }

POST /api/wavecontrol/devices/batch-config
     Push config changes to multiple devices
     Body: { "device_ids": [1,2,3], "changes": {...} }

POST /api/wavecontrol/devices/dry-run
     Validate operation before executing
     Body: { "operation": "upgrade", "device_ids": [...], "parameters": {...} }
```

### Config Backup

```
POST /api/wavecontrol/devices/{id}/backup
     Backup single device config
     Response: { "path": "...", "size": 1234, "message": "..." }

GET  /api/wavecontrol/devices/{id}/configs
     List device config backups
     Response: [{ "name": "...", "path": "...", "size": 1234, "created_at": "..." }]

POST /api/wavecontrol/devices/{id}/restore
     Restore config from backup
     Body: { "path": "backups/192.168.1.1/config.cfg" }
```

**Credential lookup order for backup/upgrade operations:**

1. Device-specific credentials (`devices.username`, `devices.password`)
2. Global AP credential pairs (`ap_cred1_user`/`ap_cred1_pass`, etc.) for AP operations
3. Global STA credential pairs (`sta_cred1_user`/`sta_cred1_pass`, etc.) for STA operations

Each configured username/password pair is tried as a unit. Different usernames across pairs are supported. There is no compiled-in device credential fallback.

**Device-level backup endpoints:**

Wave/LTU (JSON API):
```
GET /api/v1.0/system/backup
x-auth-token: {auth_token}
Response: Binary config file
```

airMAX (CGI API):
```
# After login via login.cgi (see airMAX Authentication section)
GET /cfg.cgi?timestamp={unix_ms}
Cookie: AIROS_{MAC}={session_id}
Response: Binary config file
```

Note: airMAX config backup only requires session cookie, not CSRF token (read-only operation).

### Sites & Regions

```
GET    /api/wavecontrol/regions
POST   /api/wavecontrol/regions
PATCH  /api/wavecontrol/regions/{id}
DELETE /api/wavecontrol/regions/{id}

GET    /api/wavecontrol/sites
POST   /api/wavecontrol/sites
PATCH  /api/wavecontrol/sites/{id}
DELETE /api/wavecontrol/sites/{id}
```

**Site fields:**
- `name` (string)
- `region_id` (int or null)
- `address` (string or null)
- `gps_lat`, `gps_lon` (float or null) - canonical site coordinates
- `tower_h_m` (float or null) - tower height (meters) for planning/export

### Users & Roles

```
GET    /api/wavecontrol/users
POST   /api/wavecontrol/users
PATCH  /api/wavecontrol/users/{id}
DELETE /api/wavecontrol/users/{id}
GET    /api/wavecontrol/roles
```

### Reports

```
GET    /api/wavecontrol/reports?limit=200&type=health
       List saved reports, optionally filtered by type

POST   /api/wavecontrol/reports/generate
       Generate a new immutable snapshot
       Body: { "type": "health|inventory|performance|chain|rx_mismatch" }

GET    /api/wavecontrol/reports/{id}
       Get report metadata and captured JSON data

GET    /api/wavecontrol/reports/{id}/download?format=json|csv
       Download the original JSON snapshot or report-specific CSV

DELETE /api/wavecontrol/reports/{id}
       Delete a saved report; editor or administrator required

POST   /api/wavecontrol/reports/compare
       Compare two same-type snapshots
       Body: { "report_id_1": 100, "report_id_2": 125 }
```

### Search & Logs

```
GET /api/wavecontrol/search?q={query}
    Search devices by hostname, IP, MAC, model, firmware

GET /api/wavecontrol/logs?limit=100&level=error
    Get system logs
```

### Poller Configuration

```
GET   /api/wavecontrol/poller/config
PATCH /api/wavecontrol/poller/config
      Body: { "poll_interval": 30, "aps_per_worker": 30 }
```

### Bulk Operations Configuration

```
GET   /api/wavecontrol/bulk-ops/config
PATCH /api/wavecontrol/bulk-ops/config
GET   /api/wavecontrol/bulk-ops/stats
```

### Firmware Management

```
GET    /api/wavecontrol/firmware
       List available firmware files (all users)
       Response: [{ "name": "...", "size": 1234, "flavor": "GMC", "platform": "wave", "version": "v4.1.0" }]

POST   /api/wavecontrol/firmware
       Upload firmware file (editor role required)
       Content-Type: multipart/form-data
       Body: firmware=<file>
       Response: { "name": "...", "size": 1234, "flavor": "...", "platform": "...", "version": "..." }

DELETE /api/wavecontrol/firmware/{name}
       Delete firmware file (editor role required)
       Response: { "ok": true, "deleted": "filename.bin" }
```

**Limits:**
- Maximum file size: 1GB
- File extension: must be `.bin`

**Security:**
- Upload validates filename (no path separators, must end in .bin)
- Upload rejects if file already exists (409 Conflict)
- Delete validates path stays within firmware directory
- Both operations require editor or administrator role

**Version Extraction:**

The `firmware_version` field stores an extracted version with any suffixes preserved. This is computed server-side when devices are polled.

Examples:
| Full Firmware String | Extracted Version |
|---------------------|-------------------|
| `GMC.ipq5018.v4.1.0.abc123.251220.0747.bin` | `v4.1.0` |
| `GMC.v4.1.0-beta.abc123.bin` | `v4.1.0-beta` |
| `AFLTU.v2.4.2-cust1.hash.bin` | `v2.4.2-cust1` |
| `XC.qca955x.v8.7.11-dev-rc1.hash.bin` | `v8.7.11-dev-rc1` |
| `MW.ipq53xx.v2.4.1_test.hash.bin` | `v2.4.1_test` |

The extraction algorithm:
1. Finds version marker (part starting with `v` followed by digit)
2. Appends numeric parts (major.minor.patch)
3. Preserves suffixes like `-beta`, `-cust1`, `_rc1`
4. Stops at hash-like (6+ hex chars) or date-like (6/8 digit) parts

This ensures `v4.1.0-beta` is never confused with `v4.1.0` for upgrade comparisons.

---

## Zabbix Bridge Connector

### Overview

A separate listener that speaks Zabbix agent protocol, allowing Zabbix to query device stats without modification.

### Configuration

```
Settings:
  zabbix_enabled: true
  zabbix_listen: "0.0.0.0:10050"
  zabbix_allowed_hosts: "10.0.0.5,192.168.1.0/24"
```

### Security

The Zabbix agent protocol has no built-in authentication. **Always configure `zabbix_allowed_hosts`** to restrict which IPs can query device data.

| Setting | Description |
|---------|-------------|
| `zabbix_allowed_hosts` | Comma-separated list of allowed IPs/CIDRs. Empty = allow all (insecure) |

Examples:
- `10.0.0.5` - Single IP (your Zabbix server)
- `10.0.0.0/24` - CIDR block
- `zabbix.example.com` - Hostname (resolved at startup)
- `10.0.0.5,10.0.0.6,192.168.1.0/24` - Multiple entries

If `zabbix_allowed_hosts` is empty, a warning is logged and all connections are accepted. This is useful for testing but **not recommended for production**.

### Protocol

Zabbix sends requests in format: `<key>[<params>]`

### Supported Keys

```
# Device discovery (for Zabbix LLD)
wavecontrol.devices.discovery
  Returns: JSON array for Low-Level Discovery
  { "data": [
    { "{#MAC}": "AA:BB:CC:DD:EE:FF", "{#IP}": "10.0.0.1", "{#HOSTNAME}": "AP-1", "{#TYPE}": "ap" },
    ...
  ]}

# Device status
wavecontrol.device.status[<ip_or_mac>]
  Returns: 1 (online) or 0 (offline)

wavecontrol.device.uptime[<ip_or_mac>]
  Returns: seconds

# Signal stats
wavecontrol.device.signal[<ip_or_mac>]
  Returns: dBm (aggregate)

wavecontrol.device.signal.60ghz[<ip_or_mac>]
  Returns: dBm (60GHz radio)

wavecontrol.device.signal.5ghz[<ip_or_mac>]
  Returns: dBm (5GHz radio)

wavecontrol.device.signal.chain[<ip_or_mac>,<radio>,<chain>]
  Example: wavecontrol.device.signal.chain[10.0.0.1,60ghz,0]
  Returns: dBm for specific chain

wavecontrol.device.snr[<ip_or_mac>]
  Returns: dB

wavecontrol.device.noise[<ip_or_mac>]
  Returns: dBm

# Rate stats
wavecontrol.device.txrate[<ip_or_mac>]
  Returns: bps

wavecontrol.device.rxrate[<ip_or_mac>]
  Returns: bps

# AP-specific
wavecontrol.ap.peer_count[<ip_or_mac>]
  Returns: number of connected STAs

wavecontrol.ap.peer.signal[<ap_ip>,<sta_ip_or_mac>]
  Returns: dBm for specific STA

# Quality
wavecontrol.device.linkscore[<ip_or_mac>]
  Returns: 0-100

wavecontrol.device.airtime[<ip_or_mac>]
  Returns: percent (0-100)

# Firmware
wavecontrol.device.firmware[<ip_or_mac>]
  Returns: firmware version string
```

### Zabbix Template

A Zabbix template will be provided with:
- Discovery rule for devices
- Item prototypes for all stats
- Trigger prototypes for:
  - Device offline
  - Signal below threshold
  - High airtime utilization
  - Firmware mismatch

---

## Web UI Workflow

### Initial Setup

1. User logs in (first user created via CLI or bootstrap)
2. Navigate to Settings -> Configuration
3. Configure:
   - Up to three explicit AP and STA username/password pairs; no built-in defaults
   - Poll interval
   - Firmware directory path
   - Zabbix settings (if needed)

### Adding Devices

1. Click "Add Device" button
2. Enter one or more AP IP addresses (comma or newline separated)
3. Optionally override username/password
4. Check "Auto-discover STAs" (default: on)
5. Click "Add"
6. System:
   - Connects to each AP
   - Authenticates
   - Fetches device info -> saves to DB
   - Fetches statistics -> discovers STAs
   - For each STA: saves to DB with parent_id
   - All devices appear in tree and table

### Viewing Stats

1. **Device Table** (main view)
   - Shows all devices with real-time stats
   - Columns: Status, Name, IP, Product, SSID, Signal, Distance
   - Click column to expand with detailed radio stats
   
2. **Device Detail View** (click device name)
   - Full stats breakdown
   - 60GHz radio: signal, per-chain signals, SNR, MCS, rates
   - 5GHz radio: signal, per-chain signals, SNR, MCS, rates
   - Historical graphs (future)

3. **Sidebar Tree**
   - Hierarchical view: AP -> STAs
   - Status indicator (green/red/yellow dot)
   - Click to select, double-click to expand

### Stats Display

```
+-------------------------------------------------------------+
| Device: 3150 Jeremiah Ramp                                  |
| Product: Wave-LR  |  IP: 172.24.61.43  |  MAC: AA:BB:CC:... |
+-------------------------------------------------------------+
| 60 GHz Radio                                                |
|   Signal: -52 dBm  |  SNR: 38 dB  |  Noise: -90 dBm        |
|   Chain 0: -53 dBm  |  Chain 1: -51 dBm                    |
|   MCS: 9  |  TX: 867 Mbps  |  RX: 650 Mbps                 |
|   Airtime: 12%  |  Link Score: 85                          |
+-------------------------------------------------------------+
| 5 GHz Radio (Backup)                                        |
|   Signal: -68 dBm  |  SNR: 22 dB  |  Noise: -90 dBm        |
|   Chain 0: -69 dBm  |  Chain 1: -67 dBm  |  Chain 2: -70   |
|   MCS: 7  |  TX: 300 Mbps  |  RX: 280 Mbps                 |
+-------------------------------------------------------------+
| Connection                                                  |
|   Uptime: 14d 3h 22m  |  Distance: 2.17 km                 |
|   Connected to: AP-MAIN (172.24.61.1)                      |
+-------------------------------------------------------------+
```

---

## Environment Variables

Three persistent variables are required:

```bash
# Database connection
WAVECONTROL_DSN="postgres://user:pass@127.0.0.1/wavecontrol"

# Session-signing secret; generate once and retain across restarts
WAVECONTROL_JWT_SECRET="base64-random-secret"

# 32-byte AES key, base64 encoded; generate once and retain permanently
WAVECONTROL_DATA_KEY="base64-32-byte-key"

# Required only while creating the first administrator in an empty database
WAVECONTROL_BOOTSTRAP_USERNAME="wavecontrol-admin"
WAVECONTROL_BOOTSTRAP_PASSWORD="strong-one-time-bootstrap-password"

# Optional: Listen address (default: 127.0.0.1:8080)
WAVECONTROL_ADDR="0.0.0.0:8080"
```

Remove the bootstrap variables after first startup. Rotating `WAVECONTROL_DATA_KEY` without an explicit re-encryption procedure makes stored operational credentials unreadable.

---

## Stats Extraction from Ubiquiti API

### Required API Calls per Device

1. **`GET /api/v1.0/device`** - Static device info
   - `identification.firmware`
   - `identification.firmwareVersion`
   - `identification.model`
   - `identification.product`
   - `identification.mac`
   - `capabilities.device.supportedFirmwares[].flavor`

2. **`GET /api/v1.0/statistics`** - Real-time stats
   - Device stats: uptime, CPU, memory, temperature
   - Wireless stats: radios, signal levels, rates
   - Peer stats: connected STAs with their signal levels

### Stats Fields to Extract (Need API Response Samples)

To accurately map the API response to our stats model, we need sample responses showing:

- Per-radio stats structure (60GHz vs 5GHz)
- Per-chain signal levels
- MCS/rate information
- Quality metrics (link score, airtime)
- Peer/STA signal details

Please provide full JSON responses from:
1. `GET /api/v1.0/statistics` from a Wave AP with STAs
2. `GET /api/v1.0/device` from any Wave device
3. Any other endpoints with useful radio stats

---

## Bulk Actions & Context Menus

### Device Table Actions

**Single Device (right-click):**
- Refresh - Force immediate poll
- View Details - Open detail panel
- Upgrade Firmware - Open upgrade dialog
- Fanout Upgrade - Upgrade AP + all STAs (AP only)
- Reboot - Reboot device
- Delete - Remove from inventory

**Multiple Devices (multi-select + right-click or toolbar):**
- Refresh All - Poll all selected
- Upgrade All - Upgrade selected devices (opens dialog)
- Fanout All - Fanout upgrade all selected APs
- Delete All - Remove selected

### Upgrade Workflow

**Immediate Upgrade:**
```
1. Select device(s) or right-click -> Upgrade
2. Dialog shows:
   - Selected devices with current firmware
   - Available firmware files (auto-filtered by flavor; if a device flavor is missing, WaveControl will infer it by querying the device and persist it)
   - Force checkbox (re-upload if same version)
3. Click "Upgrade Now"
4. System:
   - Compares current vs target firmware
   - Skips devices already at target (unless force)
   - Uploads firmware to remaining devices
   - Triggers reboots
   - Updates status to "upgrading"
   - Background: polls until devices come back online
5. Progress shown in toast notifications + status column
```

**Fanout Upgrade (AP + STAs):**
```
1. Right-click AP -> "Fanout Upgrade"
2. Dialog shows:
   - AP info + firmware
   - All connected STAs + firmware
   - Target firmware per flavor (auto-selected; if a device flavor is missing, WaveControl will infer it by querying the device and persist it)
3. Click "Upgrade All"
4. System:
   - Upgrades all STAs first (parallel)
   - Waits for STAs to come back
   - Upgrades AP last
   - Updates all statuses
```

**Scheduled Upgrade:**
```
1. Select device(s) -> "Schedule Upgrade"
2. Dialog:
   - Target firmware
   - Date/time picker
   - Repeat options (one-time, weekly maintenance window)
   - Pre-checks: notify if device offline at scheduled time
3. Creates scheduled job in database
4. Background scheduler executes at specified time
```

---

## Scheduled Jobs

### Database Schema

```sql
-- Scheduled jobs with progress tracking
CREATE TABLE IF NOT EXISTS scheduled_jobs (
    id SERIAL PRIMARY KEY,
    job_type VARCHAR(32) NOT NULL,       -- 'upgrade', 'reboot', 'refresh'
    device_ids INTEGER[],                 -- Target devices
    parameters JSONB,                     -- Job-specific params
    scheduled_at TIMESTAMP NOT NULL,
    repeat_cron VARCHAR(64),              -- NULL = one-time, else cron expression
    last_run TIMESTAMP,
    next_run TIMESTAMP,
    status VARCHAR(16) DEFAULT 'pending', -- pending, running, completed, failed, cancelled
    progress INTEGER DEFAULT 0,           -- 0-100 percent
    total_devices INTEGER DEFAULT 0,
    completed_devices INTEGER DEFAULT 0,
    error_message TEXT,
    created_by INTEGER REFERENCES users(id),
    created_at TIMESTAMP DEFAULT NOW()
);

CREATE INDEX idx_scheduled_jobs_next ON scheduled_jobs(next_run) WHERE status = 'pending';

-- Maintenance windows
CREATE TABLE IF NOT EXISTS maintenance_windows (
    id SERIAL PRIMARY KEY,
    name VARCHAR(128) NOT NULL,
    
    -- Scope: global, region, or site
    scope VARCHAR(20) DEFAULT 'global',
    region_id INTEGER REFERENCES regions(id) ON DELETE CASCADE,
    site_id INTEGER REFERENCES sites(id) ON DELETE CASCADE,
    
    -- Schedule: day of week + time window
    day_of_week INTEGER[],                -- 0=Sun through 6=Sat, empty=any day
    start_time TIME NOT NULL,
    end_time TIME NOT NULL,
    timezone VARCHAR(64) DEFAULT 'UTC',
    
    -- Options
    allow_jobs VARCHAR(32)[] DEFAULT ARRAY['upgrade', 'reboot'],
    enabled BOOLEAN DEFAULT true,
    
    created_at TIMESTAMPTZ DEFAULT NOW(),
    created_by INTEGER REFERENCES users(id),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);
```

### Scheduler Component

```go
type Scheduler struct {
    db                 *sql.DB
    fwService          *firmware.Service
    wsHub              *websocket.Hub

    // Concurrency control
    jobSem             chan struct{}
    maxConcurrentJobs  int           // From settings (1-50, default 5)
    checkInterval      time.Duration // From settings (5-300s, default 10s)
    
    // Maintenance window enforcement
    respectMaintenance bool          // From settings (default true)
}

func NewScheduler(db *sql.DB, fwService *firmware.Service, wsHub *websocket.Hub) *Scheduler {
    s := &Scheduler{...}
    s.loadSettings()  // Load from database
    s.jobSem = make(chan struct{}, s.maxConcurrentJobs)
    return s
}

func (s *Scheduler) ReloadSettings() {
    // Called when settings change - resizes semaphore if needed
}
```

### Concurrency Control

Jobs are limited by a semaphore-based system:

```go
func (s *Scheduler) checkAndRunJobs(ctx context.Context) {
    // Query due jobs with FOR UPDATE SKIP LOCKED (prevents double-execution)
    rows, _ := s.db.QueryContext(ctx, `
        SELECT id, job_type, device_ids, ...
        FROM scheduled_jobs
        WHERE status = 'pending' 
          AND ((next_run IS NULL AND scheduled_at <= $1)
           OR (next_run IS NOT NULL AND next_run <= $1))
        ORDER BY COALESCE(next_run, scheduled_at)
        LIMIT $2
        FOR UPDATE SKIP LOCKED
    `, now, s.maxConcurrentJobs)

    for _, jobID := range jobIDs {
        select {
        case s.jobSem <- struct{}{}:
            // Got a slot - claim and run job
            job, ok := s.claimJob(ctx, jobID)
            if !ok {
                <-s.jobSem // Release slot
                continue
            }
            
            // Check maintenance windows
            if s.respectMaintenance && !s.isInMaintenanceWindow(ctx, job) {
                s.blockJobForMaintenance(job.ID)
                <-s.jobSem
                continue
            }
            
            go func(j ScheduledJob) {
                defer func() { <-s.jobSem }()
                s.runJob(ctx, j)
            }(job)
            
        default:
            // No slots available
            log.Printf("Concurrency limit reached (%d), deferring", s.maxConcurrentJobs)
            return
        }
    }
}
```

### Maintenance Windows

Jobs only run during configured maintenance windows for their target scope:

```go
func (s *Scheduler) isInMaintenanceWindow(ctx context.Context, job ScheduledJob) bool {
    // Get device's site/region
    var siteID, regionID sql.NullInt64
    s.db.QueryRowContext(ctx, `
        SELECT d.site_id, s.region_id 
        FROM devices d LEFT JOIN sites s ON d.site_id = s.id
        WHERE d.id = $1
    `, job.DeviceIDs[0]).Scan(&siteID, &regionID)

    // Check applicable windows (most specific first: site > region > global)
    rows, _ := s.db.QueryContext(ctx, `
        SELECT id, scope, start_time, end_time, timezone, allow_jobs, day_of_week
        FROM maintenance_windows
        WHERE enabled = true
          AND (scope = 'global' OR 
               (scope = 'region' AND region_id = $1) OR
               (scope = 'site' AND site_id = $2))
        ORDER BY CASE scope WHEN 'site' THEN 1 WHEN 'region' THEN 2 ELSE 3 END
    `, regionID, siteID)

    for rows.Next() {
        // Check day of week
        // Check time window (handles overnight windows like 22:00-06:00)
        // Check if job type is allowed
        if matches {
            return true
        }
    }

    // If no windows defined, allow all jobs
    var windowCount int
    s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM maintenance_windows WHERE enabled = true`).Scan(&windowCount)
    return windowCount == 0
}
```

**Window matching logic:**
- Day of week: Empty array = any day, otherwise must match
- Time window: Supports overnight (e.g., 22:00-06:00)
- Timezone: Converts current time to window's timezone for comparison
- Job types: Array of allowed types (`upgrade`, `reboot`, etc.)
- Scope priority: Site-specific > Region > Global

### Progress Tracking

Jobs report progress via database updates and WebSocket broadcasts:

```go
func (s *Scheduler) runUpgradeJob(ctx context.Context, job ScheduledJob) error {
    totalDevices := len(allDevices)
    completedDevices := 0

    for i, deviceID := range allDevices {
        // Update progress before each device
        progress := int(float64(i) / float64(totalDevices) * 100)
        s.updateJobProgress(job.ID, progress, totalDevices, completedDevices, "")
        s.broadcastJobStatus(job.ID, "running", progress, totalDevices, completedDevices, "")

        // Process device
        result := s.upgradeDevice(ctx, job.ID, deviceID, params)
        completedDevices++

        // Broadcast per-device result
        s.wsHub.BroadcastDeviceUpdate(deviceID, "", map[string]interface{}{
            "job_id":  job.ID,
            "action":  "upgrade",
            "status":  result.Status,
            "message": result.Message,
        })
    }

    return nil
}
```

**WebSocket messages:**
```json
{"type": "job_update", "job_id": 123, "status": "running", "data": {
    "progress": 45,
    "total_devices": 20,
    "completed_devices": 9,
    "error": ""
}}

{"type": "device_update", "device_id": 456, "ip": "10.0.0.5", "data": {
    "job_id": 123,
    "action": "upgrade",
    "status": "success",
    "message": "Upgraded to v4.1.0"
}}
```

### Recurring Schedules

Supported repeat formats:
- `@hourly` - Every hour
- `@daily` - Every 24 hours
- `@weekly` - Every 7 days
- Duration strings: `12h`, `24h`, `168h` (parsed with `time.ParseDuration`)

```go
func (s *Scheduler) calculateNextRun(cron string) *time.Time {
    now := time.Now()
    switch cron {
    case "@hourly":
        next := now.Add(time.Hour)
        return &next
    case "@daily":
        next := now.Add(24 * time.Hour)
        return &next
    case "@weekly":
        next := now.Add(7 * 24 * time.Hour)
        return &next
    default:
        // Try parsing as duration
        d, err := time.ParseDuration(cron)
        if err != nil {
            return nil
        }
        next := now.Add(d)
        return &next
    }
}
```

After a recurring job completes successfully, it's rescheduled:
```go
if job.RepeatCron != "" && status == StatusCompleted {
    nextRun := s.calculateNextRun(job.RepeatCron)
    if nextRun != nil {
        s.scheduleNextRun(job.ID, *nextRun)  // Sets status back to 'pending'
    }
}
```

### Scheduler API Endpoints

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/jobs` | List scheduled jobs with progress |
| POST | `/jobs` | Create scheduled job |
| DELETE | `/jobs/{id}` | Cancel pending job |
| GET | `/maintenance-windows` | List maintenance windows |
| POST | `/maintenance-windows` | Create maintenance window (admin) |
| PATCH | `/maintenance-windows/{id}` | Update maintenance window (admin) |
| DELETE | `/maintenance-windows/{id}` | Delete maintenance window (admin) |
| GET | `/scheduler/settings` | Get scheduler configuration (admin) |
| PATCH | `/scheduler/settings` | Update scheduler configuration (admin) |

### Scheduler Settings

Configurable via API or database:

| Setting | Default | Description |
|---------|---------|-------------|
| `scheduler_max_concurrent` | 5 | Max simultaneous jobs (1-50) |
| `scheduler_check_interval` | 10 | Seconds between checks (5-300) |
| `scheduler_respect_maintenance` | true | Enforce maintenance windows |

Settings are reloaded without restart via `Scheduler.ReloadSettings()`.

---

## Background Poller Architecture

### Poller Component

```go
type Poller struct {
    db           *sql.DB
    stats        *StatsStore
    interval     time.Duration  // From settings.poll_interval
    
    // Connection pool for device API calls
    httpClient   *http.Client
    
    // Worker pool for parallel polling
    workerCount  int
    jobs         chan pollJob
}

type pollJob struct {
    DeviceID   int64
    IP         string
    Username   string
    Password   string
    IsAP       bool
}

func (p *Poller) Start(ctx context.Context) {
    // Start worker pool
    for i := 0; i < p.workerCount; i++ {
        go p.worker(ctx)
    }
    
    // Reload settings periodically
    go p.watchSettings(ctx)
    
    // Main poll loop
    ticker := time.NewTicker(p.interval)
    defer ticker.Stop()
    
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            p.pollAllDevices()
        }
    }
}

func (p *Poller) pollAllDevices() {
    // Get all APs from database (STAs are discovered via AP stats)
    rows, _ := p.db.Query(`
        SELECT id, ip_address, username, password 
        FROM devices 
        WHERE parent_id IS NULL
    `)
    
    for rows.Next() {
        var job pollJob
        rows.Scan(&job.DeviceID, &job.IP, &job.Username, &job.Password)
        job.IsAP = true
        
        select {
        case p.jobs <- job:
        default:
            // Queue full, skip this cycle
        }
    }
}

func (p *Poller) worker(ctx context.Context) {
    for {
        select {
        case <-ctx.Done():
            return
        case job := <-p.jobs:
            p.pollDevice(job)
        }
    }
}

func (p *Poller) pollDevice(job pollJob) {
    // 1. Connect and authenticate
    client := wave.NewClient(job.IP, true)
    if err := client.Login(job.Username, job.Password); err != nil {
        p.stats.SetOffline(job.IP)
        return
    }
    
    // 2. Fetch /statistics
    stats, err := client.GetStatistics()
    if err != nil {
        p.stats.SetOffline(job.IP)
        return
    }
    
    // 3. Update in-memory stats for this device
    p.stats.Update(job.IP, extractDeviceStats(stats))
    
    // 4. If AP, extract and update STA stats
    if job.IsAP {
        for _, peer := range stats.Wireless.Peers {
            peerStats := extractPeerStats(peer)
            p.stats.Update(peer.Common.MgmtIP, peerStats)
            
            // Ensure STA exists in database
            p.ensureSTAExists(job.DeviceID, peer)
        }
    }
    
    // 5. Check if static info changed (firmware version)
    device, _ := client.GetDevice()
    p.checkStaticInfoChanged(job.DeviceID, device)
}
```

### Device Status Logic

Devices have three status states stored in the database `status` column:

| Status    | Meaning                                           | UI Color |
|-----------|---------------------------------------------------|----------|
| `online`  | Device responding and providing stats             | Green    |
| `unknown` | Device responded but couldn't get stats           | Yellow   |
| `offline` | Device unreachable after 3 consecutive failures   | Red      |

#### Status Determination Rules

**"online"**: Device successfully authenticated and returned statistics.

**"unknown"**: Device responded in some way but we couldn't get stats:
- TCP connection established but auth failed (wrong credentials)
- TCP RST received (port closed but host reachable)
- HTTP error returned (device busy, API error)
- Auth succeeded but stats fetch failed
- Any response within the first 2 consecutive failures

**"offline"**: Device completely unreachable:
- Connection timeout (no response)
- No route to host
- Host unreachable (ICMP)
- Must occur 3 consecutive times before marking offline

#### Circuit Breaker / Failure Tracking

```go
type circuitBreaker struct {
    failures    int       // total consecutive failures
    unreachable int       // consecutive network-unreachable failures
    lastFail    time.Time
    nextRetry   time.Time
}

const offlineThreshold = 3  // requires 3 consecutive unreachable before "offline"
```

The poller tracks failures per-device:

1. **First timeout** → status = "unknown", unreachable = 1
2. **Second timeout** → status = "unknown", unreachable = 2  
3. **Third timeout** → status = "offline", unreachable = 3
4. **TCP RST/refused** → status = "unknown", unreachable = 0 (reset)
5. **Auth failure** → status = "unknown", unreachable = 0 (reset)
6. **Success** → status = "online", circuit breaker cleared

This prevents transient network issues from immediately marking devices offline.

#### Error Classification

```go
func isNetworkUnreachable(err error) bool {
    // Returns true for:
    //   - timeout, deadline exceeded
    //   - no route to host
    //   - host unreachable
    // Returns false for:
    //   - connection refused (device responded with RST)
    //   - connection reset (device responded)
    //   - any HTTP error (device responded)
}
```

### Credential Configuration

Credentials are stored as up to 3 username/password pairs for both APs and STAs:

| Setting Key       | Description                    |
|-------------------|--------------------------------|
| `ap_cred1_user`   | AP credential pair 1 username  |
| `ap_cred1_pass`   | AP credential pair 1 password  |
| `ap_cred2_user`   | AP credential pair 2 username  |
| `ap_cred2_pass`   | AP credential pair 2 password  |
| `ap_cred3_user`   | AP credential pair 3 username  |
| `ap_cred3_pass`   | AP credential pair 3 password  |
| `sta_cred1_user`  | STA credential pair 1 username |
| `sta_cred1_pass`  | STA credential pair 1 password |
| `sta_cred2_user`  | STA credential pair 2 username |
| `sta_cred2_pass`  | STA credential pair 2 password |
| `sta_cred3_user`  | STA credential pair 3 username |
| `sta_cred3_pass`  | STA credential pair 3 password |

#### Credential Trial Order

When connecting to a device:

1. Device-specific credentials (stored in `devices.username`/`devices.password`)
2. AP credential pairs 1-3 (for APs) or STA credential pairs 1-3 (for STAs)
3. Fallback: try the other type's credentials

This allows:
- Different usernames for different device groups
- Legacy credential migration (old devices use different username)
- Fallback credentials for misconfigured devices

#### Legacy Credential Migration

The system maintains backward compatibility with the old format:

| Old Setting         | New Equivalent                    |
|---------------------|-----------------------------------|
| `ap_username`       | `ap_cred1_user`                   |
| `ap_passwords` JSON | Multiple `ap_credN_user` entries  |
| `sta_username`      | `sta_cred1_user`                  |
| `sta_passwords` JSON| Multiple `sta_credN_user` entries |

If new-format credentials are empty, the system falls back to reading legacy format.

### Stats Store

```go
type StatsStore struct {
    mu       sync.RWMutex
    devices  map[string]*DeviceStats  // keyed by MAC (lowercase); IP-only devices are keyed by "ip:<addr>"
    byMAC    map[string]string        // MAC -> IP lookup (effective management IP)
    byIP     map[string]string        // IP -> MAC lookup (for lookups without treating IP as identity)
    
    // Last full update time
    lastPoll time.Time
}

func NewStatsStore() *StatsStore {
    return &StatsStore{
        devices: make(map[string]*DeviceStats),
        byMAC:   make(map[string]string),
    }
}

func (s *StatsStore) Update(ip string, stats *DeviceStats) {
    s.mu.Lock()
    defer s.mu.Unlock()
    
    stats.IP = ip
    stats.Online = true
    stats.LastSeen = time.Now()
    
    s.devices[ip] = stats
    if stats.MAC != "" {
        s.byMAC[stats.MAC] = ip
    }
}

func (s *StatsStore) SetOffline(ip string) {
    s.mu.Lock()
    defer s.mu.Unlock()
    
    if stats, ok := s.devices[ip]; ok {
        stats.Online = false
    }
}

func (s *StatsStore) Get(ip string) *DeviceStats {
    s.mu.RLock()
    defer s.mu.RUnlock()
    return s.devices[ip]
}

func (s *StatsStore) GetByMAC(mac string) *DeviceStats {
    s.mu.RLock()
    defer s.mu.RUnlock()
    
    if ip, ok := s.byMAC[mac]; ok {
        return s.devices[ip]
    }
    return nil
}

func (s *StatsStore) List() []*DeviceStats {
    s.mu.RLock()
    defer s.mu.RUnlock()
    
    result := make([]*DeviceStats, 0, len(s.devices))
    for _, stats := range s.devices {
        result = append(result, stats)
    }
    return result
}
```

---

## Web UI Components

### Device Table Columns

| Column | Source | Notes |
|--------|--------|-------|
| [x] | - | Checkbox for multi-select |
| Status | stats.Online | Green/red/yellow dot |
| Device Name | db.hostname | Clickable to detail |
| IP Address | db.ip_address | Monospace |
| Product | db.product | Device model |
| SSID | stats.SSID | For STAs |
| Signal (60G) | stats.Radio60GHz.Signal | With bars |
| Signal (5G) | stats.Radio5GHz.Signal | With bars, per-chain hover |
| Distance | stats.Distance | km |
| Capacity | stats.Capacity.Combined | Mbps |
| Airtime | stats.Airtime | Percent |
| Firmware | db.firmware_version | |

### Device Detail Panel

```
+-------------------------------------------------------------+
| 7610 Brad Jeffers                              [Refresh] [X]|
+-------------------------------------------------------------+
| General                                                     |
|   IP: 172.24.61.164    MAC: 1c:6a:1b:41:ca:1d              |
|   Model: Wave-LR       Firmware: 4.1.0                      |
|   Uptime: 3d 18h 22m   Distance: 3.6 km                     |
+-------------------------------------------------------------+
| 60 GHz Radio                                                |
|   +------------------+------------------+                   |
|   | Signal    -55 dBm| Link Score    92 |                   |
|   | MCS          12  | Airtime     0.1% |                   |
|   | TX Rate  1000 Mbps| RX Rate  1000 Mbps|                  |
|   | Capacity  1617 Mbps| Ideal   1617 Mbps|                  |
|   +------------------+------------------+                   |
+-------------------------------------------------------------+
| 5 GHz Backup Radio                                          |
|   +------------------+------------------+                   |
|   | Signal    -59 dBm| Noise Floor -94  |                   |
|   | Chain 0   -64 dBm| Chain 1   -61 dBm|                   |
|   | SNR         35 dB| Link Score    33 |                   |
|   | MCS           3  | Label     16QAM  |                   |
|   | TX Rate   48 Mbps| RX Rate   48 Mbps|                   |
|   +------------------+------------------+                   |
+-------------------------------------------------------------+
| System                                                      |
|   CPU: 6% | RAM: 52% | Temp: 28degC                          |
|   GPS: 44.0538, -121.3021 (8 sats)                         |
+-------------------------------------------------------------+
| Interfaces                                                  |
|   eth0: v16 kbps  ^8 kbps  (plugged)                       |
|   wlan0: v42 kbps ^9 kbps                                  |
|   ath0:  v20 kbps ^0 kbps                                  |
+-------------------------------------------------------------+
```

### Settings Page

```
+-------------------------------------------------------------+
| Settings                                                    |
+-------------------------------------------------------------+
| Device Discovery                                            |
|   AP Credential 1: [username] [password]                   |
|   AP Credential 2: [username] [password]                   |
|   STA Credential 1: [username] [password]                  |
|   Poll Interval: [30] seconds                               |
+-------------------------------------------------------------+
| Firmware                                                    |
|   Firmware Directory: [firmware          ]                  |
|   (Paths relative to working directory)                     |
|                                                             |
|   Available Files:                                          |
|   [ ] GMC.ipq5018.v4.1.0.0edad4ab.251212.0922.bin  (27 MB)   |
|   [ ] MGMP.ipq807x.v4.1.0.0edad4ab.251212.0923.bin (31 MB)   |
+-------------------------------------------------------------+
| Zabbix Integration                                          |
|   [ ] Enable Zabbix Bridge                                    |
|   Listen Address: [0.0.0.0:10050]                          |
+-------------------------------------------------------------+
|                                              [Save Settings]|
+-------------------------------------------------------------+
```

---

## Implementation Phases

### Phase 1: Core Infrastructure [FIXED]
- [x] Database schema
- [x] HTTP API skeleton
- [x] Web UI framework
- [x] Wave API client

### Phase 2: In-Memory Stats & Polling [FIXED]
- [x] StatsStore implementation
- [x] Background poller with worker pool
- [x] Device discovery flow
- [x] Auto STA discovery from AP stats

### Phase 3: Web UI - Real-time Display [FIXED]
- [x] Device table with live stats
- [x] Device detail panel
- [x] Context menus (right-click actions)
- [x] Multi-select + bulk actions toolbar
- [x] Settings page

### Phase 4: Firmware Upgrades [FIXED]
- [x] Upgrade workflow implementation
- [x] Fanout upgrade (AP + STAs)
- [x] Progress tracking
- [x] Scheduled upgrades

### Phase 5: Integrations [FIXED]
- [x] Zabbix bridge connector
- [x] WebSocket for live stats push
- [x] Alerting system

### Phase 6: LTU & airMAX Support [FIXED]
- [x] LTU API mapping (hybrid endpoints)
- [x] airMAX API client
- [x] Unified stats model

---

## Future Considerations

1. **Stats History** - Ring buffer in memory for last N samples, or optional InfluxDB/TimescaleDB for long-term storage

2. **Multi-tenant** - Separate device groups with different credentials/access

3. **API Keys** - For Zabbix and other integrations without full user auth

---

## Zabbix Integration

waveControl includes a native Zabbix agent bridge that speaks the Zabbix agent protocol on port 10050.

### Configuration

Enable via Settings page or database:
```sql
UPDATE settings SET value = 'true' WHERE key = 'zabbix_enabled';
UPDATE settings SET value = '0.0.0.0:10050' WHERE key = 'zabbix_listen';
```

### Supported Item Keys

**Discovery:**
- `wavecontrol.discovery` - LLD JSON of all devices

**Device metrics:**
- `wavecontrol.device[IP,metric]`
  - `online`, `uptime`, `last_seen`
  - `cpu`, `ram.usage`, `ram.free`
  - `temp.cpu`, `temp.radio60`, `temp.radio5`
  - `gps.fix`, `gps.lat`, `gps.lon`, `gps.sats`
  - `wireless.tx_rate`, `wireless.rx_rate`
  - `radio60.capacity`, `radio60.frequency`
  - `radioltu.capacity`, `radioltu.frequency`
  - `link_score.dl`, `link_score.ul`
  - `peer_count`

**Peer (STA) metrics:**
- `wavecontrol.peer[AP_IP,STA_MAC,metric]`
  - `ip`, `hostname`, `firmware`
  - `distance`, `uptime`
  - `tx_bytes`, `rx_bytes`, `tx_rate`, `rx_rate`
  - `signal`, `signal.chain0`, `signal.chain1`
  - `cinr.dl`, `cinr.ul` (directional CINR in dB; when available on Wave/LTU/airMAX)
  - `mcs.tx`, `mcs.rx`
  - `airtime.dl`, `airtime.ul`
  - `link_score.dl`, `link_score.ul`
  - `capacity`

**Counts:**
- `wavecontrol.count[]` - Total devices
- `wavecontrol.count[online]` - Online count
- `wavecontrol.count[offline]` - Offline count
- `wavecontrol.count[ap]` - AP count

### Example Zabbix Template Items

```
# AP capacity
wavecontrol.device[192.168.1.1,radio60.capacity]

# STA signal
wavecontrol.peer[192.168.1.1,00:11:22:33:44:55,signal]

# Online device count
wavecontrol.count[online]
```

### LLD Discovery Macros

The discovery endpoint returns:
- `{#IP}` - Device IP
- `{#MAC}` - Device MAC  
- `{#HOSTNAME}` - Device name
- `{#PLATFORM}` - wave/ltu
- `{#ISAP}` - 1 for AP, 0 for STA

---

## Firmware Flavor Reference

### Wave Platform
| Flavor | Device |
|--------|--------|
| `GMC` | Wave Long-Range (60GHz) |
| `GMP` | Wave Pro (60GHz) |
| `MGMP` | Wave AP (60GHz) |
| `GP` | AirFiber 60 (60GHz PtP, uses Wave API) |
| `MW` | Wave MLO (5GHz/6GHz, uses Wave API) |

### LTU/AirFiber 5XHD Platform
| Flavor | Device |
|--------|--------|
| `AFLTUROCKET` | LTU-Rocket (AP) |
| `AFLTU` | LTU, LTU-LR (AP/STA) |
| `AF5XHD` | airFiber 5XHD (uses LTU API) |

### airMAX AC Platform (AirOS 8)
| Flavor | Device |
|--------|--------|
| `XC` | Rocket 5AC, PowerBeam 5AC, LiteBeam 5AC, NanoStation 5AC, Prism, IsoStation, LiteAP |
| `2XC` | AC Gen2 variant |
| `WA` | AC variant |
| `2WA` | AC Gen2 variant |

### airMAX M Platform (AirOS 5/6)
| Flavor | Device |
|--------|--------|
| `XM` | Rocket M5, NanoStation M5, Bullet M2, NanoBridge M5 |
| `XW` | M series variant |

### AirFiber Platform
| Flavor | Device |
|--------|--------|
| `AF11` | AirFiber 11 |
| `AF24` | AirFiber 24 |
| `AF2X` | AirFiber 2X |
| `AF3X` | AirFiber 3X |
| `AF5` | AirFiber 5 |
| `AF5X` | AirFiber 5X |

Notes:
- airFiber devices are polled via the legacy airOS/airMAX-style JSON endpoints.
- Device role is derived from `wireless.mode == "airfiber"` plus `wireless.opmode` (`master` / `slave`).
- The remote endpoint is represented as a normal peer/station in the UI by synthesizing a peer from the `airfiber.*` block (e.g. `remotemac`, `remoteip`, and RX power if available).
- Remote hostname/model are not guaranteed to be available on all firmwares; when missing, the UI will fall back to identifying the peer by MAC/IP.

---

## Web UI Features (Phase 3 Complete)

### Device Table
- Grouped display: APs first, then their STAs indented below
- Live stats: signal bars, capacity, distance, firmware version
- Peer count badges for APs
- Status indicators (online/offline/unknown)
- Click row to show detail panel

### Context Menu (Right-Click)
- Refresh: Refresh - Force immediate poll
- Upgrade: Upgrade Firmware - Open upgrade dialog
- Upgrade: Fanout Upgrade - Upgrade AP + all STAs (AP only)
- Copy: Copy IP / Copy MAC
- Link: Open Device UI - Opens device web interface
- x Delete

### Bulk Actions Toolbar
Appears when checkboxes selected:
- Refresh All - Refresh selected devices
- Upgrade All - Bulk upgrade with auto-flavor matching
- Delete All - Remove selected devices
- Cancel - Clear selection

### Device Detail Panel
Slide-out panel showing:
- General info (IP, MAC, product, firmware, flavor, uptime)
- 60GHz/LTU radio stats (signal, CINR, MCS, airtime, capacity)
- 5GHz backup radio stats
- Link scores
- Quick action buttons

### Settings Page (Admin only)
- Poll interval
- Explicit AP and STA credential pairs
- Firmware directory path
- Server listen address
- Zabbix enable/disable and listen address

### Bulk Add Modal
- Paste multiple IPs (one per line or comma-separated)
- Shared credentials for all
- Shows per-device success/failure results

---

## Session and Stale Data Rules

WaveControl's UI is a single-page app (SPA), so *in-memory* state can survive navigation. To avoid
stale or misleading information after a logout/login cycle:

- Login establishes an `HttpOnly`, `SameSite=Strict` session cookie; JavaScript never receives or stores the signed token.
- On successful login, the UI performs a full page reload. The normal bootstrap path runs (`api.me()`, `api.devices()`, then WebSocket connect) using the cookie.
- State-changing cookie requests require same-origin validation plus `X-WaveControl-CSRF: 1`.
- Logout disconnects WebSocket, increments the user's server-side `auth_version`, clears the cookie, and returns to the login screen.

Rationale: This guarantees that *all pages* start from fresh server data and no stale page-level
cache (Quality aggregates, job lists, etc.) survives across sessions.

## WebSocket Real-Time Updates

waveControl uses WebSocket for real-time stats updates instead of polling.

### Connection
```javascript
// The browser automatically includes the HttpOnly cookie during the same-origin upgrade.
const url = `ws://host/api/wavecontrol/ws`
const ws = new WebSocket(url)
```

### Message Types

| Type | Direction | Description |
|------|-----------|-------------|
| `stats_update` | Server->Client | Device stats updated |
| `device_add` | Server->Client | New device discovered (STA auto-add) |
| `device_update` | Server->Client | Device changed |
| `job_update` | Server->Client | Scheduled job status change |
| `ping` | Client->Server | Keepalive |
| `pong` | Server->Client | Keepalive response |

### Message Format
```json
{
  "type": "stats_update",
  "device_id": 123,
  "device_mac": "aa:bb:cc:dd:ee:ff",
  "device_ip": "192.168.1.1",
  "data": { /* DeviceStats object */ },
  "timestamp": 1703287200
}
```

**Identity rules:** `device_id` (DB primary key) and `device_mac` (normalized lowercase) are authoritative identifiers.
`device_ip` is included as a fallback only (e.g., when a MAC is unknown). The UI must never use IP
as the identity for devices that have a MAC, because IP reuse causes cross-linking (wrong stats
on the wrong row when a different device is assigned the same IP).

### Frontend Data Flattening

The WebSocket sends `DeviceStats` with nested structure, but the UI table expects flat fields. The frontend flattens on receive:

```javascript
// Backend sends (nested)
{
  "gps": { "fix": true, "lat": 44.123, "lon": -121.456 },
  "wireless": {
    "radio_60ghz": { "signal": -55, "capacity": { "combined": 900000000 } },
    "radio_5ghz": { "signal": -65, "signal_per_chain": [-63, -64] }
  }
}

// Frontend flattens to
{
  "gps_lat": 44.123,
  "gps_lon": -121.456,
  "signal_60ghz": -55,
  "capacity_60ghz": 900000000,
  "signal_5ghz": -65,
  "signal_per_chain": [-63, -64]
}
```

This allows incremental DOM updates without full re-renders.

### Derived View Refresh

Some pages are not simple row views and cannot be updated purely by patching a single row. For
example, **Quality** computes aggregates (counts, worst-client lists, distributions) across all
devices and stations.

Rules:
- While the Quality page is active, the UI must refresh it when underlying data changes.
- The refresh must be **debounced/throttled** (to protect performance on large installs) and should
  preserve scroll position and expansion state.
- Current implementation clamps full re-render frequency to roughly **≤ 1 update per ~2 seconds**
  while the page is open.

---

## Scheduled Jobs

### Job Types

| Type | Description |
|------|-------------|
| `upgrade` | Firmware upgrade |
| `reboot` | Device reboot |
| `refresh` | Force poll refresh |

### Repeat Options

| Cron | Description |
|------|-------------|
| (empty) | One-time job |
| `@hourly` | Run every hour |
| `@daily` | Run every 24 hours |
| `@weekly` | Run every 7 days |
| `30m`, `1h`, `24h` | Custom interval |

### API Endpoints

**List Jobs:**
```
GET /api/wavecontrol/jobs?status=pending&limit=50
```

**Create Job:**
```json
POST /api/wavecontrol/jobs
{
  "job_type": "upgrade",
  "device_ids": [1, 2, 3],
  "parameters": {
    "force": false,
    "fanout": true
  },
  "scheduled_at": "2024-01-15T03:00:00Z",
  "repeat_cron": "@daily"
}
```

**Cancel Job:**
```
DELETE /api/wavecontrol/jobs/{id}
```

### Upgrade Parameters

| Parameter | Type | Description |
|-----------|------|-------------|
| `firmware_file` | string | Specific firmware path (empty = auto-select) |
| `force` | bool | Upgrade even if same version |
| `fanout` | bool | For APs: upgrade STAs first |

### UI Access

Scheduled jobs are managed from the Settings page (admin only):
- View pending/running/completed jobs
- Schedule new upgrade or reboot jobs
- Cancel pending jobs
- View job history with status

---

## Async Job Runs

The async job system handles long-running operations (upgrades, backups, bulk operations) without blocking HTTP requests. Jobs run in the background with progress streamed via WebSocket.

### Architecture

```
+--------------+     POST /job-runs     +--------------+
|   Client     | ---------------------->|   API        |
|   (UI)       |<-----------------------|   Handler    |
+--------------+   { job_id: "..." }    +------+-------+
       |                                       |
       |                                       v
       | WS                            +--------------+
       |                               |  Job Runner  |
       |                               |  (goroutine) |
       |                               +------+-------+
       |                                      |
       |    job_progress / job_event          |
       |<-------------------------------------+
```

### Job Types

| Type | Description |
|------|-------------|
| `upgrade` | Single device firmware upgrade |
| `bulk_upgrade` | Multiple devices in parallel |
| `fanout_upgrade` | AP + all connected STAs |
| `backup` | Configuration backup |
| `reboot` | Device reboot |
| `refresh` | Force poll refresh |

### Database Tables

**job_runs** - Job execution instances:
```sql
- id (UUID)
- job_type
- status (pending, running, completed, failed, cancelled)
- progress (0-100)
- total_steps, completed_steps
- device_ids[]
- parameters (JSONB)
- result (JSONB)
- error_message
- created_at, started_at, completed_at
- created_by
```

**job_events** - Progress log:
```sql
- id
- job_id (FK)
- event_time
- event_type (started, progress, step_complete, warning, error, completed)
- device_id (optional)
- message
- data (JSONB)
```

### API Endpoints

**Start Job:**
```json
POST /api/wavecontrol/job-runs
{
  "job_type": "bulk_upgrade",
  "device_ids": [1, 2, 3],
  "parameters": {
    "firmware_file": "GMC.ipq5018.v4.1.0.bin",
    "force": false
  }
}
Response:
{
  "job_id": "550e8400-e29b-41d4-a716-446655440000",
  "status": "pending",
  "message": "Job started"
}
```

**Get Job Status:**
```
GET /api/wavecontrol/job-runs/{id}
Response:
{
  "id": "550e8400-...",
  "job_type": "bulk_upgrade",
  "status": "running",
  "progress": 66,
  "total_steps": 3,
  "completed_steps": 2,
  "device_ids": [1, 2, 3],
  ...
}
```

**Get Job Events:**
```
GET /api/wavecontrol/job-runs/{id}/events?limit=100
Response:
[
  { "event_type": "started", "message": "Job started", "event_time": "..." },
  { "event_type": "progress", "device_id": 1, "message": "Starting upgrade", ... },
  { "event_type": "step_complete", "device_id": 1, "message": "Upgrade success", ... },
  ...
]
```

**List Jobs:**
```
GET /api/wavecontrol/job-runs?status=running&limit=50
```

**Cancel Job:**
```
DELETE /api/wavecontrol/job-runs/{id}
```

### WebSocket Events

Jobs broadcast progress in real-time:

**job_update** - Status change:
```json
{
  "type": "job_update",
  "data": {
    "job_id": "550e8400-...",
    "job_type": "bulk_upgrade",
    "status": "completed",
    "progress": 100
  }
}
```

**job_progress** - Progress update:
```json
{
  "type": "job_progress",
  "data": {
    "job_id": "550e8400-...",
    "progress": 66,
    "completed_steps": 2,
    "total_steps": 3
  }
}
```

**job_event** - Step event:
```json
{
  "type": "job_event",
  "data": {
    "job_id": "550e8400-...",
    "event_type": "step_complete",
    "device_id": 1,
    "message": "Upgrade success"
  }
}
```

### Concurrency

- Max 10 concurrent jobs (configurable)
- Jobs queue when limit reached
- Each bulk upgrade runs 5 devices in parallel
- Jobs can be cancelled while running

### UI Integration

The UI should:
1. Start jobs via POST, receive job_id immediately
2. Subscribe to WebSocket for real-time updates
3. Display progress bar based on `progress` field
4. Show event log from `/job-runs/{id}/events`
5. Allow cancellation of running jobs

---

## Implementation Status

### Phase 1: Core Infrastructure [FIXED]
- Database schema
- Wave API client
- Device discovery

### Phase 2: Stats & Polling [FIXED]
- In-memory stats store
- Background poller
- Auto-STA discovery

### Phase 3: Web UI [FIXED]
- Device table with live stats
- Detail panel
- Context menus
- Bulk actions
- Settings page

### Phase 4: Scheduled Upgrades [FIXED]
- Scheduler service
- Job management API
- Schedule UI in Settings

### Phase 5: Real-Time Updates [FIXED]
- WebSocket hub
- Real-time stats broadcast
- Job status notifications

### Phase 6: Integrations [FIXED]
- Zabbix agent bridge
- LTU device support

### Phase 7: Async Job System [FIXED]
- Job runner with background execution
- job_runs and job_events tables
- WebSocket progress streaming
- Job cancellation support
- REST API for job management

### Phase 8: Security & Operations [FIXED]
- Configurable TLS verification (insecure, TOFU, strict)
- Certificate pinning with trust-on-first-use
- Per-device and per-site certificate management
- Alert rules with threshold monitoring
- Email, webhook, and Zabbix notification channels
- Bulk operations controller with concurrency limits
- Exponential backoff and retry strategies
- Dry-run mode for operation validation

---

## TLS Certificate Management

waveControl supports three TLS verification modes for device communication:

### Verification Modes

1. **insecure** (default) - Skip certificate verification entirely. Maintains backward compatibility but provides no MITM protection.

2. **tofu** (Trust-On-First-Use) - Automatically pins device certificates on first connection. Subsequent connections verify against the pinned fingerprint. Provides good security with minimal configuration.

3. **strict** - Require valid CA-signed certificates. Most secure but requires proper PKI infrastructure.

### API Endpoints

```
GET    /api/wavecontrol/tls/mode                 # Get current mode
PATCH  /api/wavecontrol/tls/mode                 # Set mode {"mode": "tofu"} (admin)

GET    /api/wavecontrol/tls/certs                # List all pinned certs
GET    /api/wavecontrol/tls/certs/stats          # Stats (total/verified/pending/changed/expired/no_cert)
GET    /api/wavecontrol/tls/certs/pending        # List pending/changed certs
POST   /api/wavecontrol/tls/certs/learn          # Learn missing certs in bulk (admin)
POST   /api/wavecontrol/tls/certs/verify-all     # Verify pending/changed in bulk (admin)
DELETE /api/wavecontrol/tls/certs/pending        # Clear ALL pending/changed certs (editor)

GET    /api/wavecontrol/devices/{id}/cert         # Get pinned cert info
GET    /api/wavecontrol/devices/{id}/cert/current # Get current cert without pinning
POST   /api/wavecontrol/devices/{id}/cert/pin     # Pin current device cert (editor)
POST   /api/wavecontrol/devices/{id}/cert/verify  # Mark pinned cert verified (editor)
DELETE /api/wavecontrol/devices/{id}/cert         # Unpin device cert (editor)
DELETE /api/wavecontrol/sites/{id}/certs          # Unpin all certs at site (editor)
```

### Database Schema

```sql
CREATE TABLE device_certs (
    id SERIAL PRIMARY KEY,
    device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    fingerprint VARCHAR(64) NOT NULL,  -- SHA-256 fingerprint (hex)
    subject TEXT,
    issuer TEXT,
    not_before TIMESTAMPTZ,
    not_after TIMESTAMPTZ,
    pinned_at TIMESTAMPTZ DEFAULT NOW(),
    pinned_by INTEGER REFERENCES users(id),
	    verified BOOLEAN DEFAULT FALSE,
	    verified_at TIMESTAMPTZ,
	    verified_by INTEGER REFERENCES users(id),
	    previous_fingerprint VARCHAR(64),
	    changed_at TIMESTAMPTZ,
    UNIQUE(device_id)
);
```

---

## Alerting System

WaveControl alerting is limited to Ubiquiti AP and STA inventory/telemetry. The operating system hosting WaveControl is not an alert target.

### Rule model

A rule contains:

- inventory scope: `all`, `site`, or `device`;
- role target: `all`, `ap`, or `sta`;
- optional enforcement of `devices.alertable` and `alert_silenced_until`;
- metric, operator, threshold, and persistence duration;
- severity: `auto`, `info`, `warning`, or `critical`;
- zero or more notification channels;
- optional clear/recovery delivery;
- post-trigger cooldown.

Supported metrics are `signal_60ghz`, `signal_5ghz`, `signal_6ghz`, `signal_ltu`, `cpu`, `temperature`, `ram`, `offline_duration`, `capacity`, `peer_count`, `link_score`, `interference`, `chain_imbalance`, and `gps_sync`. `chain_imbalance` is the largest valid per-chain receive-signal spread reported by any radio. `gps_sync` is available only when a live GPS-sync state is present (or a positive parsed synchronized state), and is `1` while synchronized and `0` while a live state reports unsynchronized. A missing metric does not compare as zero; the device is skipped and an existing occurrence is closed as no longer applicable.

Evaluation order is: enabled rule, scope, role, device alert policy, metric availability, condition, persistence, cooldown, durable occurrence/outbox creation.

### Occurrence lifecycle

- One rule/device pair has at most one active occurrence.
- Acknowledgement marks the occurrence seen but does not alter evaluation or recovery.
- Automatic recovery, manual resolution, device policy changes, and rule changes all use one transactional close path.
- Manual resolution preserves the previous trigger time; an ongoing condition cannot bypass cooldown.
- Trigger notifications that have not left the outbox are canceled when the occurrence closes.
- Recovery is queued only for channels where the trigger was sent or was concurrently in flight and only when `notify_recovery` is true.
- Rule edits and deletion resolve old occurrences and clear old duration state before the new definition is loaded.

### Durable notification delivery

Channels are `email`, `webhook`, `zabbix`, and `sysmon`. Rules with no channel still create in-application history.

`alert_notification_outbox` is unique by `(alert_id, channel, event)` where event is `triggered` or `resolved`. Workers claim with `FOR UPDATE SKIP LOCKED` and recover stale claims. Email, webhook, and Zabbix stop after eight exponentially backed-off attempts. sysmon-web retries after 5 seconds, doubles to a 60-second cap, and remains retryable while that trigger or matching recovery is still relevant. The alert API returns safe per-channel status, attempt count, next attempt, sent time, and last error.

Webhook delivery must reject unsafe schemes, embedded credentials, loopback, private, link-local, multicast, documentation, and metadata destinations, including redirected/resolved destinations.

### sysmon-web alerter

Admin settings are:

- `sysmon_alerter_enabled`
- `sysmon_alerter_host`
- `sysmon_alerter_port` (default 1347)
- `sysmon_alerter_name`
- `sysmon_alerter_token` (encrypted and masked)
- `sysmon_alerter_application`
- `sysmon_alerter_ca_pem`

The client uses pinned TLS, authenticates with `ALERTER`, maintains one serialized long-lived connection, sends keepalive `PING`, and accepts only `333` success or `444` refusal lines. Trigger mapping is critical→`CRITICAL`, warning/info→`WARNING`; recovery→`OK`. Object identity is stable as `device-<device-id>-rule-<rule-id>`.

### API

```
GET    /api/wavecontrol/alerts/rules
POST   /api/wavecontrol/alerts/rules
PATCH  /api/wavecontrol/alerts/rules/{id}
DELETE /api/wavecontrol/alerts/rules/{id}
GET    /api/wavecontrol/alerts
POST   /api/wavecontrol/alerts/{id}/acknowledge
POST   /api/wavecontrol/alerts/{id}/resolve
GET    /api/wavecontrol/alerts/channels
POST   /api/wavecontrol/alerts/channels/sysmon/test
```

The sysmon test endpoint is administrator-only and performs TLS, authentication, `PING`, and `QUIT` without sending an alert.

---

## Poller Runtime Configuration

Runtime-configurable settings for the device polling system. Changes take effect immediately without restart.

### Configuration

| Setting | Default | Range | Description |
|---------|---------|-------|-------------|
| `poll_interval` | 30 | 10-300 | Seconds between poll cycles |
| `aps_per_worker` | 30 | 5-100 | APs assigned to each worker thread |
| `worker_count` | 50 | (read-only) | Number of worker threads (calculated at startup) |

### API Endpoints

```
GET   /api/wavecontrol/poller/config   # Get configuration
PATCH /api/wavecontrol/poller/config   # Update configuration (admin only)
```

### Example

```json
PATCH /api/wavecontrol/poller/config
{
  "poll_interval": 60,
  "aps_per_worker": 50
}
```

Changes to `poll_interval` are applied immediately to the running poller. The `aps_per_worker` setting affects worker distribution on the next poll cycle.

---

## Bulk Operations Controller

Smart concurrency control for large-scale operations. Configuration available via Settings page (admin) or API.

### Configuration

| Setting | Default | Range | Description |
|---------|---------|-------|-------------|
| `max_global_concurrent` | 10 | 1-100 | Total concurrent operations across all jobs |
| `max_per_job` | 5 | 1-50 | Concurrent devices per bulk job |
| `max_per_ap` | 3 | 1-20 | Concurrent STAs during fanout |
| `max_retries` | 3 | 0-10 | Retry attempts for transient failures |
| `initial_backoff` | 2s | - | Initial retry delay |
| `max_backoff` | 60s | - | Maximum retry delay |
| `backoff_multiplier` | 2.0 | - | Exponential backoff factor |
| `operation_timeout` | 5m | - | Timeout per operation |
| `min_delay_between_ops` | 500ms | - | Rate limiting delay |

### API Endpoints

```
GET   /api/wavecontrol/bulk-ops/config   # Get configuration
PATCH /api/wavecontrol/bulk-ops/config   # Update configuration
GET   /api/wavecontrol/bulk-ops/stats    # Get operation statistics
POST  /api/wavecontrol/devices/dry-run   # Validate operation compatibility
```

### Dry-Run Mode

Validate operations before executing:

```json
POST /api/wavecontrol/devices/dry-run
{
  "operation": "upgrade",
  "device_ids": [1, 2, 3, 4, 5],
  "parameters": {
    "firmware_file": "WA.v2.5.0.bin",
    "force": false
  }
}
```

Response:
```json
{
  "compatible": 4,
  "incompatible": 1,
  "total": 5,
  "results": [
    {
      "device_id": 1,
      "device_ip": "192.168.1.100",
      "hostname": "AP-01",
      "compatible": true,
      "current_version": "2.4.5",
      "target_version": "2.5.0",
      "flavor": "WA",
      "warnings": []
    },
    {
      "device_id": 3,
      "device_ip": "192.168.1.102",
      "hostname": "AP-03",
      "compatible": false,
      "issues": ["Device is offline"]
    }
  ]
}
```

### Retry Strategy

Operations that fail with transient errors are automatically retried with exponential backoff:

1. Initial failure -> wait 2s -> retry
2. Second failure -> wait 4s -> retry
3. Third failure -> operation marked as failed

Retryable errors include:
- Connection timeouts
- Connection refused/reset
- Network unreachable
- HTTP 502/503/504

### Fanout Safety

When upgrading AP + STAs (fanout mode):
1. STAs are upgraded first (max 3 concurrent per AP)
2. If >50% of STAs fail, AP upgrade is skipped
3. AP is upgraded last to maintain connectivity

### STA Credential Retry

**Problem:** APs and STAs often have different credentials. During fanout upgrades, STAs may fail authentication if they use different username/password than the AP.

**Solution:** Automatic detection and retry flow:

1. **Detection:** When upgrade completes, results are checked for `auth_failed` status
2. **Prompt:** If auth failures detected, modal prompts for STA credentials
3. **Retry:** `POST /devices/retry-upgrade` retries failed devices with new credentials
4. **Storage:** On successful retry, credentials are saved to device record in DB

**API Endpoint:**
```
POST /api/v1/devices/retry-upgrade
{
  "device_ids": [123, 456],
  "username": "sta_user",
  "password": "sta_pass",
  "force": false
}
```

**Response:**
```json
{
  "results": [
    {"device_id": 123, "status": "success", "message": "upgrade initiated"},
    {"device_id": 456, "status": "auth_failed", "message": "login failed"}
  ]
}
```

**Status Values:**
- `success` - Upgrade started, credentials saved to DB
- `auth_failed` - Authentication still failing
- `failed` - Other failure (network, firmware mismatch, etc.)
- `skipped` - Already at target version

**Credential Storage:**
```sql
UPDATE devices SET username = $1, password = $2 WHERE id = $3
```

Credentials are stored per-device, allowing mixed environments where APs and STAs use different authentication.

**UI Flow:**
1. User initiates fanout upgrade on AP
2. Job runs: STAs upgraded first, then AP
3. If any STA returns `auth_failed`, job completes with partial success
4. Auth Retry Modal appears showing failed devices
5. User enters STA credentials and clicks "Retry"
6. Successful retries update device credentials in DB
7. Future upgrades use stored per-device credentials

### Job Event Visibility

**Requirement:** 100% visibility into all job events, especially failures.

**Job Panel Enhancements:**
- Failure count badge prominently displayed in job header
- Jobs with failures automatically expand to show last 10 events (vs 5 for success)
- Failure events highlighted with red left border and background
- Progress bar shows warning gradient when failures detected
- "View All Events" button shows total count plus failure count

**Failure Detection:**
Events are classified as failures if message contains:
- "failed" (case-insensitive)
- "error" (case-insensitive)  
- "timeout" (case-insensitive)
- event_type is "error"

**Events Modal:**
- Full event history available via "N Events (X failed) >" button
- Events fetched from API (up to 500) with fallback to local cache
- Events sorted newest-first
- Each event shows: timestamp, type, message, device link
- Failure events visually highlighted

**Auto-Expand Behavior:**
- Jobs with `failureCount > 0` automatically show expanded event list
- Jobs with status "failed" automatically show expanded event list
- Expanded view shows 300px max-height (scrollable)

### Navigation Structure

The main navigation now consists of:
- **Dashboard** (default) - Device list with tree view, stats, quick actions
- **Map** - Leaflet map with GPS markers and link lines
- **Quality** - AP<->client quality monitoring (Signal Levels + Modulation Rates) with issues queue
- **Config** - Backup, restore, and batch configuration
- **Scheduler** - Schedule firmware upgrades, backups, and other jobs
- **Reports** - Generate and download network reports
- **Settings** - System configuration (admin)

### Map View

Uses Leaflet with CARTO dark tiles. Features:
- Device markers colored by status (green=online, red=offline). Marker dot size scales with map zoom (smaller when zoomed out, with a minimum size so they remain visible).
- Link lines between APs and STAs, colored by signal quality
- Popup with device info on click
- Center All button to fit all devices in view
- Toggle links and labels

**KMZ Export:**

Two export options available in the map toolbar:
- **Export APs (KMZ)** - Exports only AP devices with GPS coordinates
- **Export All (KMZ)** - Exports all devices (APs and STAs) with GPS coordinates

KMZ files can be opened in Google Earth, Google Maps, and other GIS applications. Export includes:
- Device name as placemark label (preserved in KMZ)
- Description with device type, status, IP, MAC, product, signal, site, distance
- Color-coded markers: green for online APs, blue for online STAs, red for offline
- Coordinate data in KML format inside the KMZ zip container

### Quality Page

AP<->client quality analytics optimized for large networks (5000+ devices). Table-driven views with an issues queue for NOC workflow.

**Tabs:**

1. **Signal Levels** (default) - Ranked AP table sorted by worst first
2. **Modulation Rates** - Same layout as Signal Levels, but evaluates modulation / MCS / link rate health
3. **Issues Queue** - Prioritized list of combined problems to fix (signal + modulation)
4. **Mismatches** - Data quality view (identifier collisions / duplicates) with delete tools

#### Signal Levels Table

**Columns:**
- Expand toggle, Status, AP Name, Site, Client count
- Distribution bar (visual Good/Fair/Poor/Offline breakdown)
- Poor count, Poor %, Worst signal, Avg signal, Health %

**Expandable Rows:**
- Click row to expand and show worst 10 clients
- "View all clients" opens full list in details panel
- Client rows are clickable to show device details

#### Modulation Rates Table

Same layout as Signal Levels, but the client metric is **modulation** (best-effort, based on per-peer radio MCS where available, else Tx/Rx rate proxy).

**No Data vs Offline:**
- **Offline** = client STA status is not `online` (stale/missing reachability).
- **No Data** = client STA is `online` but WaveControl cannot currently derive a modulation value (missing MCS / rate fields). This must **not** be counted as Offline.

**Columns:**
- Expand toggle, Status, AP Name, Site, Client count
- Distribution bar (visual Good/Fair/Poor/No Data/Offline breakdown)
- Poor count, Poor %, Worst modulation, Avg modulation, Health %

#### Issues Queue

The Issues Queue lists **both** bad Signal Levels and bad Modulation Rates.

**Issue Types:**
- **ap_quality** - APs with high poor signal and/or high poor modulation and/or many offline clients (combined into a single AP issue)
- **ap_interference** - AP radios with high interference airtime (Wave channel utilization).
  - Default thresholds: **Warning ≥ 10%**, **Critical ≥ 25%**.
  - Thresholds are configurable via **Settings → Quality Thresholds** (`settings` keys: `interference_warning_pct`, `interference_critical_pct`).
- **critical_signal** - Individual clients significantly below the poor threshold (10dB below)
- **critical_modulation** - Individual clients with extremely low modulation (device/profile-specific)

#### Global Warning Panel

WaveControl includes a small, collapsible warning panel that only appears when **at least one device crosses a configured warning/critical threshold**.

Current implementation:
- Warnings are computed in the browser (front-end) from the latest device snapshot plus current settings.
- Active warnings are displayed immediately; previously-seen warnings are retained in a local history list.

User experience:
- Hidden by default when there are no active warnings.
- When visible, the panel can be expanded/collapsed.
- Expanded view shows:
  - **Active** warnings (currently exceeding thresholds)
  - **History** (warnings that were previously active but have since cleared)
- A **Clear Old** button removes history entries (active warnings are never removed by clear).

Persistence:
- Panel state and history are stored in `localStorage`:
  - `warningPanelState`
  - `warningPanelHistory`
  - `warningPanelDismissedAt`

#### Mismatches

The Mismatches tab is a data-quality tool. It highlights and allows deletion of:
- **Duplicate MAC groups** (case-insensitive)
- **IP collisions** (one IP used by multiple MACs; includes SSID context column)

#### Signal Quality Thresholds

Uses unified `SIGNAL_THRESHOLDS` - see "Signal Quality Thresholds" section under Signal Semantics:
- 60GHz: Good >-55, Fair -55 to -65, Poor <-65
- 5GHz/LTU/airMAX: Good >-62, Fair -62 to -70, Poor <-70

#### Modulation Quality Thresholds

Best-effort heuristics used in the UI (see `web/js/app.js`):
- **Wave 60 / LTU (MCS-based):** Good = within top 3-4 MCS of ideal (diff<=3), Fair = diff<=6, Poor below.
- **airMAX (MCS-based when available):** AirOS 8+ exposes per-STA `tx_idx`/`rx_idx` (+ `tx_nss`/`rx_nss`). Good = within top 3-4 MCS of ideal (diff<=3), Fair = diff<=6, Poor below.
- **airMAX fallback (rate proxy):** If MCS indices are missing, use Mbps proxy: M (≥130 / ≥65 / <65), AC (≥200 / ≥120 / <120).
- **airFiber (mixed):** Prefer "x" modulation labels (≥8 / ≥6 / <6). Otherwise use Mbps proxy (≥500 / ≥200 / <200).

**Performance:**
- Table-based view scales to thousands of APs
- Expandable rows show only worst 10 clients (not all)
- No graph rendering overhead - instant sort/filter/search

**No Data vs Offline in Modulation view:**
- **Offline** = STA status is not `online` (offline or unknown)
- **No Data** = STA is online but modulation cannot be computed (missing MCS + missing/0-rate proxy)
### Config Backup/Restore

**API Endpoints:**
```
POST /api/wavecontrol/devices/{id}/backup     - Backup single device
POST /api/wavecontrol/devices/{id}/restore    - Restore from backup
GET  /api/wavecontrol/devices/{id}/configs    - List device backups
POST /api/wavecontrol/devices/bulk-backup     - Backup multiple devices
POST /api/wavecontrol/devices/batch-config    - Push config to multiple devices
```

**Batch Config Changes:**
- SSID
- Channel
- TX Power
- Password

### AP Antenna Parameters

The **Config** tab includes an optional Antenna Parameters editor. This is intended for **future RF modeling/planning** and does not affect poller behavior.

**Database fields (devices table):**
- `antenna_model` (varchar(64)) - preset id (example: `wave-ap`, `wave-ap-gen2`)
- `antenna_override` (bool) - when true, beamwidth values are treated as user overrides
- `antenna_azimuth_deg` (float) - sector azimuth/heading in degrees (0..360)
- `antenna_downtilt_deg` (float) - **mechanical** downtilt in degrees (may be negative)
- `antenna_electrical_downtilt_deg` (float) - **electrical** downtilt in degrees (defaults to `0`)
- `antenna_beamwidth_h_deg` (float) - horizontal beamwidth in degrees
- `antenna_beamwidth_v_deg` (float) - vertical beamwidth in degrees

**Sector planning/export fields (devices table):**
- `radius_m` (float) - expected sector reach/radius in meters (optional)
- `tech` (int) - optional planning tool technology code (operator-defined)
- `down_mbps` (float) - expected downlink capacity (Mbps) for planning/export (optional)
- `up_mbps` (float) - expected uplink capacity (Mbps) for planning/export (optional)
- `latency_ms` (float) - expected latency (ms) for planning/export (optional)
- `bizres` (varchar(1)) - business/residential code for planning/export: `B` (Business), `R` (Residential), `X` (Both, default)

**Defaults:**
- Integrated-antenna products show preset defaults for **beamwidth** and **electrical downtilt** (Wave, airFiber 60, etc).
- If **Override** is not enabled, **beamwidth and electrical downtilt are locked** to the selected preset.
- If **Override** is enabled, the user can enter custom beamwidth values and (if needed) override electrical downtilt.

**UI behavior:**
- The Antenna Parameters editor is a single-scroll table (no nested scroll panes).
- Column headers are sticky while scrolling.
- The grid is compact/fixed-layout so it fits a typical desktop display without forcing unnecessary horizontal scrolling.
- While the modal is open, live device updates patch rows **in place** (preserving scroll position).
- Edits are **autosaved when the user leaves a row** (blur / click-away / tab away).
- Modified fields are outlined **blue** while the row is dirty; the blue dirty state is only cleared **after** the save is confirmed by the server.
- To prevent accidental loss of work, rows with unsaved edits are treated as **dirty** and are not overwritten by live updates.
- There is **no per-row Save button** in the antenna editor.

**Built-in antenna presets (UI):**
- Wave: `wave-ap`, `wave-ap-gen2`, `wave-ap-micro`, `wave-pro`, `wave-nano`, `wave-pico`, `wave-lr`
- airFiber 60: `af60-lr`, `af60-hd`, `af60-xg`, `af60-xr`
- LTU: `ltu-lite`, `ltu-lr`
- airMAX (integrated dishes): `pbe-5ac-400`, `pbe-5ac-500`, `lbe-5ac-xr`
- airMAX / airPrism sector antennas: `am-9m13`, `am-2g15-120`, `am-2g16-90`, `am-3g18-120`, `am-5g16-120`,
  `am-5g17-90`, `am-5g19-120`, `am-5g20-90`, `am-5ac21-60`, `am-5ac22-45`, `ap-5ac-90-hd`
- Titanium sectors (beamwidth variants): `am-v2g-ti-60|90|120`, `am-v5g-ti-60|90|120`, `am-m-v5g-ti-60|90|120`

The preset values are intended for UI defaults and RF modeling, and may be overridden per device.

**API Endpoint:**
```
PATCH /api/wavecontrol/devices/{id}/antenna
```

**Downtilt semantics:**
- If electrical downtilt is present (common on many integrated sector antennas), the effective downtilt for RF modeling is:
  - `effective_downtilt_deg = (antenna_downtilt_deg || 0) + (antenna_electrical_downtilt_deg || 0)`

Payload:
```json
{
  "antenna_model": "wave-ap",
  "antenna_override": false,
  "antenna_azimuth_deg": 90,
  "antenna_downtilt_deg": -2.5,
  "antenna_electrical_downtilt_deg": 0,
  "antenna_beamwidth_h_deg": 30,
  "antenna_beamwidth_v_deg": 3,
  "radius_m": 3000,
  "tech": 72,
  "down_mbps": 100,
  "up_mbps": 20,
  "latency_ms": 50,
  "bizres": "R"
}
```

### Sector CSV Export

Wavecontrol can export a site/sector CSV for external planning tools.

The export is available from the **Config** tab: **Sector CSV Export → Export CSV**.

CSV header / columns:
```
site_id,sector_id,site_lat,site_lon,azimuth,beam,radius_m,tech,down,up,latency,tower_h_m,bizres
```

Notes:
- `site_id` is the **site name**.
- `sector_id` is the AP's **SSID**.
- `site_lat`/`site_lon` prefer the **site** GPS (`gps_lat`/`gps_lon`). If a site does not have GPS coordinates, the export falls back to the AP device GPS (`gps_lat`/`gps_lon`) when available (less accurate).
- `azimuth`/`beam` come from the AP antenna parameters (`antenna_azimuth_deg`, `antenna_beamwidth_h_deg`).
- `radius_m`, `tech`, `down_mbps`, `up_mbps`, `latency_ms`, `bizres` are optional per-AP planning fields.
- The export modal provides both a **Preview** (table) and a **Raw CSV** view, with **Copy** and **Download** actions.


### Reports

Reports are durable, immutable JSONB snapshots. A saved report must render its captured data; current live values must never silently replace values from the original snapshot. Every newly generated report includes `report_version`, `report_type`, `report_name`, `scope`, and `generated_at`.

**Supported report types:**

1. `health` — authoritative availability, AP/STA split, explicit metric coverage, band-aware STA signal quality, CPU/memory/temperature exceptions, flap/reboot counters, firmware/site summaries, and severity-ranked offenders.
2. `inventory` — the complete AP/STA inventory with platform family, firmware, region/site, parent relationship, status, and last-seen data.
3. `performance` — aggregate AP/STA rates, captured throughput-history samples, platform/site aggregates, measured STA signal distribution, capacity-risk APs, full measured device rows, and a bounded missing-metric list.
4. `chain` — device-radio and peer-link chain spread above the configured threshold after sanitizing placeholder values.
5. `rx_mismatch` — AP/STA receive-level deltas above the configured threshold.

**Data authority and coverage:**
- Database inventory is authoritative for device count, AP/STA role, site/region, parent relationship, firmware, and persisted status.
- In-memory metrics are joined to inventory by normalized lowercase MAC.
- Each report includes `coverage.inventory_devices`, `coverage.metrics_devices`, `coverage.missing_metrics`, `coverage.signal_samples`, and `coverage.coverage_pct` where applicable.
- Radio evaluation covers 60 GHz, 6 GHz, 5 GHz, LTU, and additional reported radio entries.
- STA signal grading uses 60 GHz thresholds for 60 GHz links and the standard 5 GHz/LTU thresholds for other supported bands.
- Offender, weak-signal, chain, RX-mismatch, capacity-risk, flap, and reboot lists are deterministically severity-ranked.

**Performance history:**
- The stats store retains approximately 60 samples, nominally 30 minutes at a 30-second interval.
- `performance` generation copies that ring into `throughput_history` inside the report JSON.
- The report viewer draws the chart from the captured array.
- For a legacy report without a `throughput_history` field, the viewer may offer current history only with an explicit warning that the chart is not part of the saved snapshot.

**Report UI:**
- The page presents report-type cards, searchable/filterable report history, role-aware generation/deletion, and same-type comparison selection.
- Saved reports open in a full-screen responsive modal with metadata strip, metric cards, sortable tables, report-specific tabs, print output, and JSON/CSV actions.
- Comparison deltas are calculated as report 2 minus report 1 and use metric-aware positive/negative coloring.
- Viewers can list, open, compare, print, and download. Generation and deletion require editor or administrator privileges.

**CSV:**
- All five types support CSV.
- Performance CSV includes both AP and STA rows.
- Inventory CSV includes role, parent, site/region, platform, status, and last-seen fields.
- Health CSV includes summary/coverage metrics and ranked offenders.
- Empty `chain` and `rx_mismatch` reports return a valid header-only CSV.

**API endpoints:**
```
GET    /api/wavecontrol/reports?limit=200&type=health
POST   /api/wavecontrol/reports/generate
GET    /api/wavecontrol/reports/{id}
GET    /api/wavecontrol/reports/{id}/download?format=json
GET    /api/wavecontrol/reports/{id}/download?format=csv
DELETE /api/wavecontrol/reports/{id}
POST   /api/wavecontrol/reports/compare
```

### Modal and Dialog System

All application dialogs use the shared modal runtime. Browser-native `alert()`, `confirm()`, and `prompt()` calls are prohibited in loaded application JavaScript.

Required behavior:
- semantic dialog role/ARIA state;
- initial focus, tab-key focus trap, and focus restoration;
- Escape handling and explicit backdrop policy;
- body scroll lock while any modal is open;
- responsive standard, wide, extra-wide, and full-screen shells;
- internally scrollable body with stable header/footer actions;
- consistent dark/light form controls, including selects and time inputs;
- consequence text for destructive or operational confirmations;
- loading, empty, validation, and error states that do not rely on native popups.

Dynamic certificate, drilldown, scheduler, job-detail, user/device, firmware, configuration, Ultra Debug, and maintenance-window dialogs must use this runtime. Editing a maintenance window must populate the existing record and issue `PATCH /maintenance-windows/{id}`; creating a new record uses `POST /maintenance-windows`.

### Database Schema Additions

Configuration backups are stored as permission-restricted files beneath the configured backup directory; metadata is derived from the filesystem rather than a `device_configs` table.

```sql
-- Reports
CREATE TABLE reports (
    id SERIAL PRIMARY KEY,
    type VARCHAR(50),
    data JSONB,
    device_count INTEGER,
    created_at TIMESTAMPTZ,
    created_by INTEGER REFERENCES users(id)
);
```

---

## Complete Feature List

| Feature | Status |
|---------|--------|
| Device Discovery | [FIXED] |
| AP/STA Tree View | [FIXED] |
| Real-time Stats | [FIXED] |
| WebSocket Updates | [FIXED] |
| Firmware Upgrades | [FIXED] |
| Fanout Upgrades | [FIXED] |
| Scheduled Jobs | [FIXED] |
| Recurring Schedules | [FIXED] |
| Maintenance Windows | [FIXED] |
| Concurrency Controls | [FIXED] |
| Async Job System | [FIXED] |
| Zabbix Integration | [FIXED] |
| Map View | [FIXED] |
| Quality Page | [UPDATED] |
| Config Backup/Restore | [FIXED] |
| Batch Configuration | [FIXED] |
| Reports | [FIXED] |
| Alert Rules | [FIXED] |
| TLS/Certificate Management | [FIXED] |
| Bulk Operations Controller | [FIXED] |
| RBAC (4 roles) | [FIXED] |
| LTU Support | [FIXED] |
| Wave Support | [FIXED] |
| airMAX Support | [FIXED] |

---

## airMAX Support

### API Differences

| Aspect | Wave/LTU | airMAX |
|--------|----------|--------|
| Auth | POST JSON `/api/v1.0/user/login` | POST form `/login.cgi` |
| Token | `x-auth-token` header | Cookie-based session |
| Status | `/api/v1.0/statistics` (array) | `/status.cgi` (object) |
| Peers | `/api/v1.0/wireless/peers` | `wireless.sta[]` in status |
| Config | `/api/v1.0/system/config` | `/getcfg.cgi` / `/writecfg.cgi` |

### airMAX Authentication (AirOS 5/6/8)

Authentication flow for airMAX devices:

```
Step 1: Establish session via login.cgi
GET /login.cgi                    # Establish session cookie
POST /login.cgi                   # Submit credentials
Content-Type: application/x-www-form-urlencoded
Body: username=${DEVICE_USER}&password=${DEVICE_PASSWORD}

Response sets: Cookie: AIROS_{MAC}={session_id}

Step 2: Get CSRF token (required for AirOS 8/LTU write operations)
POST /api/auth
Content-Type: application/json
Cookie: AIROS_{MAC}={session_id}  # Use session from step 1
Body: {"username":"device-user","password":"device-password"}

Response header: X-CSRF-ID: {csrf_token}
```

**Key points:**
- `login.cgi` must use `application/x-www-form-urlencoded` (NOT multipart)
- `/api/auth` returns 403 if called without existing session cookie
- CSRF token required for: `fwupl.cgi`, `fwflash.cgi`, write operations
- CSRF token NOT required for: `status.cgi`, `cfg.cgi`, read operations
- Older AirOS 5/6 may not support `/api/auth` - CSRF will be empty

### airMAX Firmware Upgrade

**AirOS 8+ (has CSRF token from `/api/auth`):**
```
Step 1: Upload firmware
POST /fwupl.cgi
Content-Type: multipart/form-data
X-CSRF-ID: {csrf_token}
X-Requested-With: XMLHttpRequest

Field: fwfile={firmware_binary}

Step 2: Trigger flash
POST /fwflash.cgi
Content-Type: application/x-www-form-urlencoded
X-CSRF-ID: {csrf_token}
X-Requested-With: XMLHttpRequest

Body: do_update=1
```

**AirOS 5/6 (no CSRF token, cookie auth only):**
```
Step 1: Get token from page
GET /system.cgi
Parse HTML for: <input type="hidden" name="token" value="...">

Step 2: Upload firmware
POST /system.cgi
Content-Type: multipart/form-data
X-Requested-With: XMLHttpRequest

Fields: token={scraped_token}, fwfile={firmware_binary}

Step 3: Trigger flash
POST /fwflash.cgi
Content-Type: application/x-www-form-urlencoded
X-Requested-With: XMLHttpRequest

Body: do_update=1
```

**Notes:**
- Flash trigger (`/fwflash.cgi`) may cause immediate reboot - use short timeout (10s)
- Connection errors after flash trigger are expected (device rebooting)
- Job status transitions to "rebooting" after successful flash trigger

### Wave Firmware Upgrade

Wave devices use the JSON API:

```
Step 1: Upload firmware
POST /api/v1.0/system/upgrade/direct
Content-Type: multipart/form-data
x-auth-token: {auth_token}

Field: file={firmware_binary}

Step 2: Poll status (optional)
GET /api/v1.0/system/upgrade
x-auth-token: {auth_token}

Response: {"status": "uploading|flashing|done|failed", "warnings": [], "failureReason": ""}
```

**Notes:**
- Device reboots automatically after successful upload
- Status polling is optional - upload returns 200 on success
- Job status transitions to "rebooting" after successful upload

### LTU Firmware Upgrade

LTU uses a **hybrid approach** - Wave JSON API for upload, airMAX CGI for flash trigger:

```
Step 1: Login via Wave API
POST /api/v1.0/user/login
Content-Type: application/json
Body: {"username":"device-user","password":"device-password"}

Response header: x-auth-token: {token}

Step 2: Upload firmware via Wave API
POST /api/v1.0/system/upgrade/direct
Content-Type: multipart/form-data
x-auth-token: {token}

Field: file={firmware_binary}

Step 3: Wait for verification
GET /api/v1.0/system/upgrade
x-auth-token: {token}

Response: {"status": "finished", ...}

Step 4: Login via airMAX CGI (for CSRF token)
POST /login.cgi (form-urlencoded) -> session cookie
POST /api/auth (JSON) -> X-CSRF-ID header

Step 5: Trigger flash via airMAX CGI
POST /fwflash.cgi
Content-Type: application/x-www-form-urlencoded
X-CSRF-ID: {csrf_token}
X-Requested-With: XMLHttpRequest

Body: do_update=1
```

**Notes:**
- LTU does NOT auto-reboot after Wave upload (unlike Wave devices)
- Must explicitly trigger flash via fwflash.cgi with CSRF token
- Config backup uses Wave API: `GET /api/v1.0/system/backup`
- Status polling uses Wave API: `GET /api/v1.0/status`

### airMAX status.cgi Field Mapping

```
host.hostname      -> DeviceStats.Hostname
host.devmodel      -> devices.product
host.fwversion     -> devices.firmware
host.uptime        -> DeviceStats.Uptime
host.cpuload       -> DeviceStats.CPU[0].Usage
host.temperature   -> DeviceStats.Temperature.CPU
host.totalram      -> DeviceStats.RAM.Total
host.freeram       -> DeviceStats.RAM.Free

wireless.essid     -> devices.ssid
wireless.mode      -> devices.role (ap-ptmp->ap, sta-ptmp->sta)
wireless.frequency -> devices.frequency
wireless.chanbw    -> devices.channel_width
wireless.txpower   -> RadioStats.TxPower
wireless.noisef    -> RadioStats.NoiseFloor
wireless.signal    -> RadioStats.Signal (STA mode)
wireless.polling.dcap -> Wireless.TxRate (x1000 for bps)
wireless.polling.ucap -> Wireless.RxRate

wireless.sta[].mac        -> PeerStats.MAC
wireless.sta[].lastip     -> PeerStats.IP
wireless.sta[].signal     -> PeerStats.Signal
wireless.sta[].distance   -> PeerStats.Distance
wireless.sta[].uptime     -> PeerStats.Uptime
wireless.sta[].stats.*    -> PeerStats.TxBytes/RxBytes
wireless.sta[].airmax.*   -> PeerStats capacity
wireless.sta[].remote.*   -> PeerStats hostname/model/firmware

gps.lat            -> GPS.Lat / devices.gps_lat
gps.lon            -> GPS.Lon / devices.gps_lon
gps.fix            -> GPS.Fix
gps.sats           -> GPS.Sats
gps.alt            -> GPS.Alt
```

### Supported airMAX Models

- Rocket 5AC, Rocket 5AC Lite, Rocket Prism 5AC Gen2
- PowerBeam 5AC, PowerBeam 5AC Gen2, PowerBeam 5AC ISO
- NanoStation 5AC, NanoStation 5AC Loco
- LiteBeam 5AC, LiteBeam 5AC Gen2
- LiteAP GPS, IsoStation
- airFiber 5, airFiber 5X, airFiber 5XHD
- And other AirOS 8.x compatible devices

---

## Implementation Details

### UI Design (airControl-Inspired)

The web interface follows Ubiquiti's airControl aesthetic:

**Login Page:**
- Centered card with waveControl logo
- Dark theme with accent colors
- Clean typography (Lato font family)

**Main Interface:**
- Top header with navigation tabs: Dashboard, Map, Quality, Config, Scheduler, Reports, Settings
- Left sidebar with hierarchical device tree (collapsible)
- Main content area with device table
- Right detail panel (slides in on device selection)
- Status badges in header showing Online/Offline/Unknown counts

**Branding:**
- Product name: waveControl (lowercase 'w')
- Logo: Custom SVG with wave motif
- Color scheme: Dark background (#1a1a2e), cyan accent (#00d4ff)

### Device Table

**Columns (toggleable via = menu):**
| Column | Description | Default |
|--------|-------------|---------|
| Status | Online/Offline indicator | Visible |
| Name | Hostname or IP fallback | Visible |
| IP | Management IP address | Visible |
| MAC | Device MAC address | Hidden |
| Product | Device model name | Visible |
| Site | Assigned site name | Visible |
| 60 GHz | Signal from 60GHz radio | Visible |
| 5 GHz | Combined 5GHz signal (MRC calculated) | Visible |
| 5 GHz C0 | Chain 0 signal level | Hidden |
| 5 GHz C1 | Chain 1 signal level | Hidden |
| Distance | Link distance in km | Visible |
| Capacity | Combined throughput | Visible |
| Firmware | Simplified version string | Visible |

**5GHz Signal Columns:**
- **5 GHz** (default): MRC-combined value from both chains using formula `10 * log10(10^(c0/10) + 10^(c1/10))`
- **5 GHz C0/C1**: Individual chain values, hidden by default but can be enabled for troubleshooting

**Features:**
- Hierarchical sort: Region -> Site -> AP -> STA
- Multi-column sort with shift-click
- Search filters across all columns (including hidden)
- Incremental DOM updates via WebSocket (no full re-renders)
- Row click opens detail panel
- Right-click context menu for actions

### Device Detail Panel

Split-view panel showing comprehensive device information:

**Layout:**
- Fixed 350px width on right side
- Header with hostname, IP, and close button
- Tabbed sections: Overview, Radio, Peers, System

**Overview Tab:**
- Device identity (MAC, model, firmware full string)
- Connection status and uptime
- GPS coordinates with map link
- Site/Region assignment

**Radio Tab (per-radio for Wave):**
- Frequency and channel width
- TX power (EIRP and conducted)
- Signal levels with per-chain breakdown
- MRC combined signal calculation
- Noise floor and SNR
- MCS/modulation info
- Airtime utilization (DL%, UL%, Interference%)
- Capacity stats (DL/UL/Combined)

**Peers Tab (APs only):**
- Connected station list
- Per-peer signal and distance
- Traffic counters
- Link scores

**System Tab:**
- CPU usage (per-core)
- RAM usage
- Temperature sensors
- Interface statistics
- Orientation (tilt/roll)

**Auto-Update:**
- Stats refresh via WebSocket
- Visual indicators for changing values

### Per-Chain Signal Display

For devices with multiple antenna chains:

**Maximum Ratio Combining (MRC):**

MRC is the standard method for combining per-chain RSSI values. It models how the receiver combines signals from multiple antennas - the combined power is the sum of individual powers (in linear scale):

```
Combined (dBm) = 10 x log10(10^(C0/10) + 10^(C1/10) + ...)
```

This is computed **server-side in Go** (`stats.CombineSignals()`) to avoid repeated Math.pow/log10 calls in JavaScript.

**Examples:**
| Chain 0 | Chain 1 | Combined | Gain |
|---------|---------|----------|------|
| -63 dBm | -63 dBm | -60 dBm | +3 dB (equal signals) |
| -60 dBm | -65 dBm | -59 dBm | +1 dB (one stronger) |
| -55 dBm | -70 dBm | -55 dBm | ~0 dB (dominant chain) |

**Applies to all platforms:**
- Wave (60GHz and 5GHz backup)
- LTU (main radio)  
- airMAX (5GHz)
- AirFiber (point-to-point)

**UI Display:**
- Main signal column shows MRC-combined value
- Detail panel shows per-chain breakdown
- Chain toggle button in table header

**Zabbix Keys:**
```
wavecontrol.signal.5ghz[{#MAC}]           # Combined signal
wavecontrol.signal.5ghz.chain0[{#MAC}]    # Chain 0
wavecontrol.signal.5ghz.chain1[{#MAC}]    # Chain 1
```

### Scheduler Tab

Dedicated page for job management with hierarchical device selection:

**Device Picker Tree:**
```
[D] Region A
  [S] Site 1
    [AP] AP-Tower1
      [STA] STA-Client1
      [STA] STA-Client2
    [AP] AP-Tower2
  [S] Site 2
[D] Region B
Unassigned Sites
Unassigned Devices
```

**Selection Behavior:**
- Checkbox at each level
- Checking parent selects all children
- Unchecking parent deselects all children
- Search/filter within tree

**Job Types:**
| Type | Description |
|------|-------------|
| Firmware Upgrade | Upload and apply firmware |
| Reboot | Restart device |
| Config Backup | Save device configuration |

**Scheduling Options:**
- Run Now: Execute immediately
- Schedule: Pick date/time for execution
- Repeat: Cron expression for recurring jobs

### Config Backup System

Filesystem-based backup storage organized by AP/STA hierarchy:

**Directory Structure:**
```
backups/                            # Relative to working directory
+-- 192.168.1.1/                    # AP IP address
|   +-- AP-Tower_20241223-143022.cfg    # AP configs
|   +-- AP-Tower_20241223-160045.cfg
|   +-- 192.168.1.10/                   # STA IP subdirectory
|   |   +-- STA-Site1_20241223-150000.cfg
|   +-- 192.168.1.11/                   # Another STA
|       +-- STA-Site2_20241223-150000.cfg
+-- 192.168.1.2/                    # Another AP
|   +-- ...
```

**Structure Rules:**
- APs: Configs stored directly under AP IP directory
- STAs: Configs stored under `{AP_IP}/{STA_IP}/`
- Filename format: `{hostname}_{YYYYMMDD-HHMMSS}.cfg`

**API Endpoints:**
```
POST /api/wavecontrol/devices/{id}/backup
  Response: {path, size, message}

GET /api/wavecontrol/devices/{id}/configs
  Response: [{name, path, size, created_at}, ...]

POST /api/wavecontrol/devices/{id}/restore
  Body: {path: "relative/path/to/file.cfg"}
```

**Settings:**
- `backup_dir`: Base directory (default: `backups`, relative to working dir)

### Firmware Upgrade Ordering

When upgrading multiple devices, the system maintains link connectivity:

**Upgrade Sequence:**
```
For each AP in selection:
  1. Identify all STAs connected to this AP
  2. Upgrade STAs in parallel
  3. Wait for all STA upgrades to complete
  4. Upgrade the AP

Then: Upgrade any standalone STAs (AP not in selection)
```

**Rationale:**
- STAs remain connected during their upgrade (AP still up)
- AP upgrade happens last, after all STAs are updated
- Minimizes service disruption

### Site/Region Hierarchy

Organization structure for large deployments:

**Schema:**
```sql
CREATE TABLE regions (
  id SERIAL PRIMARY KEY,
  name VARCHAR(255) NOT NULL,
  description TEXT
);

CREATE TABLE sites (
  id SERIAL PRIMARY KEY,
  name VARCHAR(255) NOT NULL,
  region_id INTEGER REFERENCES regions(id),
  gps_lat DOUBLE PRECISION,
  gps_lon DOUBLE PRECISION,
  description TEXT
);

ALTER TABLE devices ADD COLUMN site_id INTEGER REFERENCES sites(id);
```

**Site Inheritance:**
- STAs automatically inherit `site_id` from parent AP
- On role change (AP<->STA): site_id cleared
- On SSID change: site_id cleared (device moved to different network)

**API Endpoints:**
```
GET/POST /api/wavecontrol/regions
GET/PUT/DELETE /api/wavecontrol/regions/{id}

GET/POST /api/wavecontrol/sites
GET/PUT/DELETE /api/wavecontrol/sites/{id}
GET /api/wavecontrol/sites/{id}/devices
```

### Poller Threading

Dynamic worker pool scaling based on AP count:

**Configuration:**
- Setting: `aps_per_worker` (default: 30)
- Maximum workers: 100

**Calculation:**
```
workers = min(100, ceil(AP_count / aps_per_worker))
```

**Example:**
- 1000 APs / 30 = 34 workers
- 3000 APs / 30 = 100 workers (capped)

**Behavior:**
- Worker count adjusts each poll cycle
- Logs worker changes when AP count changes significantly

### WebSocket Real-Time Updates

Efficient incremental DOM updates:

**Connection:**
```javascript
ws://host/api/wavecontrol/ws  # authenticated by the session cookie
```

**Message Types:**
| Type | Direction | Description |
|------|-----------|-------------|
| `stats_update` | Server->Client | Device stats changed |
| `device_add` | Server->Client | New device discovered |
| `device_remove` | Server->Client | Device removed |
| `job_update` | Server->Client | Job status changed |
| `ping/pong` | Bidirectional | Keepalive |

**Incremental Updates:**
- Only changed rows are updated in DOM
- No full table re-renders
- Smooth transitions for value changes

### Error Handling

**Error Message Format:**

All device operation errors include request context for debugging:
```
<operation> failed: <METHOD> <URL> [<key params>] -> <result>
```

Examples:
```
flash trigger failed: POST https://172.24.82.89/fwflash.cgi [X-CSRF-ID=true, X-Requested-With=XMLHttpRequest] -> status 403: ...

firmware upload failed: POST https://172.24.82.89/fwupl.cgi [X-CSRF-ID=true, file=WA.v2.4.1.bin, size=12345678] -> status 500: ...

login failed: POST https://172.24.82.89/api/v1.0/user/login [user=ubnt] -> status 401: invalid credentials

login failed: POST https://172.24.82.89/login.cgi [user=ubnt] -> no AIROS session cookie (got 0 cookies)

upload failed: POST https://172.24.82.89/api/v1.0/system/upgrade/direct [x-auth-token=set, file=LTU.bin, size=9876543] -> status 413: file too large
```

Key parameters shown:
- HTTP method and full URL (identifies exact endpoint)
- CSRF token presence (true/false, not the actual token)
- Auth token presence (set/not set)
- Username for login attempts
- Filename and size for uploads
- HTTP status code and response body on failure

This format allows copy/paste of errors for debugging without needing server logs.

**Toast Notifications:**
- Success: Green, auto-dismiss after 3s
- Warning: Yellow, auto-dismiss after 5s
- Error: Red, persistent until dismissed
- Copy button on errors for support

**Network Errors:**
- Banner appears when server unreachable
- Auto-reconnect attempts
- Queue actions during offline

### Zabbix Integration

Full Zabbix agent protocol implementation:

**Connection:**
- Listens on configurable address (default: `127.0.0.1:10050`)
- Supports passive checks (Zabbix server queries)

**Key Format:**
```
wavecontrol.<metric>[<mac>]
wavecontrol.<metric>[<mac>,<radio>]
```

**Discovery Keys:**
```
wavecontrol.discovery.devices     # All devices
wavecontrol.discovery.aps         # Access points only
wavecontrol.discovery.stas        # Stations only
```

**Metric Keys (per device):**
```
# Status
wavecontrol.status[{#MAC}]        # 1=online, 0=offline
wavecontrol.uptime[{#MAC}]        # Seconds

# System
wavecontrol.cpu[{#MAC}]           # CPU usage %
wavecontrol.memory[{#MAC}]        # RAM usage %
wavecontrol.temperature[{#MAC}]   # Temperature degC

# Signal (radio-specific)
wavecontrol.signal.60ghz[{#MAC}]  # 60 GHz signal dBm
wavecontrol.signal.5ghz[{#MAC}]   # 5 GHz signal dBm
wavecontrol.signal.5ghz.chain0[{#MAC}]  # Per-chain
wavecontrol.signal.5ghz.chain1[{#MAC}]

# Traffic
wavecontrol.tx.rate[{#MAC}]       # TX rate bps
wavecontrol.rx.rate[{#MAC}]       # RX rate bps
wavecontrol.tx.bytes[{#MAC}]      # TX bytes total
wavecontrol.rx.bytes[{#MAC}]      # RX bytes total

# Wireless
wavecontrol.frequency[{#MAC}]     # Frequency MHz
wavecontrol.distance[{#MAC}]      # Distance meters
wavecontrol.peers[{#MAC}]         # Peer count (APs)
wavecontrol.noise[{#MAC}]         # Noise floor dBm
wavecontrol.airtime[{#MAC}]       # Airtime %
```

### Platform Support Matrix

| Platform | Auth Method | Stats Endpoint | Firmware Prefix |
|----------|-------------|----------------|-----------------|
| Wave | POST JSON `/api/v1.0/user/login` | `/api/v1.0/statistics` | GMC, GMP, MGMP, GP, MW |
| LTU | POST JSON `/api/v1.0/user/login` | `/api/v1.0/statistics` | AFLTU, AFLTUROCKET, AF5XHD |
| airMAX | See airMAX Auth below | `/status.cgi` | XC, 2XC, WA, 2WA, XM, XW |
| AirFiber | Same as airMAX | `/status.cgi` | AF11, AF24, AF2X, AF3X, AF5, AF5X |

#### airMAX Authentication

airMAX devices have different authentication methods depending on firmware version:

| AirOS Version | Auth Endpoint | Content-Type | CSRF Token | Notes |
|---------------|---------------|--------------|------------|-------|
| AirOS 5.x-6.x | `/login.cgi` | application/x-www-form-urlencoded | Not required | Returns `AIROS_*` session cookie |
| AirOS 8.x/LTU | `/login.cgi` + `/api/auth` | form-urlencoded + JSON | Required (`X-CSRF-ID` header) | Session cookie from login.cgi, CSRF from /api/auth |

**Authentication Flow (handled by `airmax.Client.Login()`):**

1. Try HTTPS `login.cgi` (form-urlencoded) - works on all AirOS versions
2. Try HTTP `login.cgi` (fallback if HTTPS fails)
3. If session established, try `/api/auth` to get CSRF token (needed for AirOS 8/LTU write ops)
4. Try HTTPS `/api/auth` alone (fallback)
5. Try HTTP `/api/auth` alone (fallback)

**Why login.cgi first:** The `/api/auth` endpoint returns 403 if called without an existing session cookie. By doing `login.cgi` first to establish the session, then calling `/api/auth`, we can get the CSRF token needed for firmware upgrades on AirOS 8/LTU devices.

**CSRF Token Handling:**
- AirOS 8/LTU returns `X-CSRF-ID` header on successful `/api/auth`
- This token MUST be included in write requests (`fwupl.cgi`, `fwflash.cgi`)
- CSRF NOT required for read requests (`status.cgi`, `cfg.cgi`)
- The `airmax.Client` automatically captures and includes this token

**Firmware Upgrade Endpoints:**
- AirOS 8/LTU (with CSRF): `/fwupl.cgi` then `/fwflash.cgi`
- AirOS 5/6 (no CSRF): `/system.cgi` then `/fwflash.cgi`

Form field name: `fwfile` (for all versions)
Headers required: `X-CSRF-ID` (AirOS 8/LTU), `X-Requested-With: XMLHttpRequest` (all versions)

### Environment Variables

Three persistent environment variables are required:

| Variable | Description | Required |
|----------|-------------|----------|
| `WAVECONTROL_DSN` | PostgreSQL connection string | Yes |
| `WAVECONTROL_JWT_SECRET` | Session-signing secret | Yes |
| `WAVECONTROL_DATA_KEY` | Base64-encoded 32-byte data-encryption key | Yes |
| `WAVECONTROL_BOOTSTRAP_USERNAME` | Initial administrator username | Empty database only |
| `WAVECONTROL_BOOTSTRAP_PASSWORD` | Initial administrator password | Empty database only |

All other configuration is stored in the `settings` table and configurable via the web UI.

### Database Settings

| Key | Default | Description |
|-----|---------|-------------|
| `poll_interval` | 30 | Seconds between poll cycles |
| `ap_cred1_user` … `ap_cred3_pass` | empty | Three explicit AP credential pairs |
| `sta_cred1_user` … `sta_cred3_pass` | empty | Three explicit STA credential pairs |
| `firmware_path` | firmware | Firmware file directory (relative to working dir) |
| `backup_dir` | backups | Config backup directory (relative to working dir) |
| `aps_per_worker` | 30 | APs per poller thread |
| `listen_addr` | 127.0.0.1:8080 | HTTP listen address |
| `zabbix_enabled` | false | Enable Zabbix bridge |
| `zabbix_listen` | 127.0.0.1:10050 | Zabbix agent listen address |

### Bulk Add with Retry

When adding multiple devices via bulk add, failures are handled gracefully:

**Workflow:**
1. Enter IPs (comma or newline separated)
2. Provide credentials
3. Click "Add All"
4. Results show [ok]/[x] per IP with error messages
5. **Automatic cleanup:** Successful IPs are removed from textarea, failed IPs remain
6. Update credentials if needed and click "Add All" again to retry failures

**Automatic IP List Management:**
- On partial success: Textarea auto-updates to contain only failed IPs
- On full success: Textarea cleared, modal auto-closes after 1.5s
- On full failure: Textarea unchanged, allowing credential adjustment

**Success Behavior:**
- Full success: Modal auto-closes after 1.5s
- Partial success: Warning toast with counts, failed IPs remain in list
- Full failure: Error toast, all IPs remain for retry

**Info Messages:**
- Partial: "X successful IPs removed from list. Y failed IPs remain - update credentials if needed and retry."
- Full failure: "All IPs failed. Check credentials and try again."

### Auto-Discovery of New STAs

When polling an AP, any new STAs not previously in the database are automatically added:

**Backend Process:**
1. Poller fetches AP statistics
2. Extracts peer list from response
3. For each peer MAC not in database:
   - Inserts new device record
   - Inherits site_id from parent AP
   - Logs: `"Auto-discovered new STA: MAC (IP) on AP ID"`
4. Broadcasts `device_add` WebSocket message

**Frontend Handling:**
```javascript
case 'device_add':
  // Add to store if not exists
  // Refresh tree view
  // Show toast: "Discovered new STA: hostname"
  // Update counts
```

**Benefits:**
- No manual STA entry required
- Real-time UI updates
- Automatic site inheritance
- Full audit trail in logs

### File Structure

```
wavecontrol/
+-- SPEC.md                    # This specification
+-- README.md                  # Quick start guide
+-- AUDIT.md                   # Code audit notes
+-- schema.sql                 # Database schema
+-- go.mod                     # Go module definition
+-- cmd/
|   +-- server/
|       +-- main.go            # Entry point
|       +-- api.go             # HTTP handlers
|       +-- discovery.go       # Device discovery
+-- internal/
|   +-- airmax/
|   |   +-- client.go          # airMAX API client
|   +-- wave/
|   |   +-- client.go          # Wave/LTU API client
|   +-- firmware/
|   |   +-- service.go         # Firmware management
|   +-- poller/
|   |   +-- poller.go          # Background polling
|   +-- scheduler/
|   |   +-- scheduler.go       # Job scheduler
|   +-- stats/
|   |   +-- store.go           # In-memory stats
|   +-- websocket/
|   |   +-- hub.go             # WebSocket hub
|   +-- zabbix/
|       +-- bridge.go          # Zabbix agent bridge
+-- web/
|   +-- index.html             # SPA entry point
|   +-- css/
|   |   +-- styles.css         # All styles
|   +-- js/
|       +-- app.js             # Main application
|       +-- api.js             # API client
|       +-- store.js           # State management
|       +-- components.js      # UI components
+-- docs/
    +-- API.md                 # REST API reference
    +-- WAVE_API_MAPPING.md    # Wave field mapping
    +-- AIRMAX_API_MAPPING.md  # airMAX field mapping
    +-- AIRMAX_WEB_API.md      # airMAX Web UI API
    +-- FIRMWARE_UPGRADE.md    # Upgrade tool spec
    +-- examples/              # JSON response samples
        +-- wave_ap.json
        +-- wave_sta.json
        +-- ltu_ap.json
        +-- ltu_sta.json
        +-- airmax_ap.json
        +-- airmax_sta.json
```

### airMAX Implementation Details

#### Authentication Flow

airMAX devices (AirOS) use a different authentication flow than Wave/LTU:

1. **GET `/login.cgi`** - Establish initial session
2. **POST `/login.cgi`** - Submit credentials as multipart form-data
   - Fields: `username`, `password`
   - Response sets `AIROS_*` session cookie
3. **GET `/status.cgi`** - Fetch device status with cookie auth

**Cookie Validation:**
- Login is only successful if response includes an `AIROS_` prefixed cookie
- This distinguishes real airMAX devices from Wave devices (which return 200 OK but no AIROS cookie)

**Password Iteration:**
- `LoginWithPasswords()` tries multiple passwords
- Only network errors (connection refused, timeout) abort early
- HTTP status errors (401, 403) continue to next password

#### Signal Semantics

Understanding signal perspectives is critical for wireless monitoring:

**airMAX Signal Fields:**

*AP Perspective (Local RX) - stored in `Radio5GHz`:*
- `sta.Signal` / `sta.RSSI` - Combined signal AP receives from STA
- `sta.ChainRSSI[]` - Per-chain signal AP receives from STA (Chain 0, Chain 1)
- `sta.NoiseFloor` - Noise floor at AP

*STA Perspective (Remote) - stored in `RemoteSignal`:*
- `sta.Remote.Signal` - What the STA reports receiving from the AP
- `sta.Remote.ChainRSSI[]` - Per-chain signal at STA
- `sta.Remote.NoiseFloor` - Noise floor at STA

**Wave/LTU Signal Fields:**

*AP Perspective (Local RX) - from `peers[].local[]`:*
- `local[id=main].linkQuality.signal` - 60GHz/LTU signal AP receives
- `local[id=backup].linkQuality.signal` - 5GHz backup signal
- `local[].linkQuality.signalPerChain[]` - Per-chain signals (5GHz has 2 chains, 60GHz has 1)

*STA Perspective (Remote RX) - from `peers[].remote[]`:*
- `remote[id=main].linkQuality.signal` - 60GHz/LTU signal STA receives
- `remote[id=backup].linkQuality.signal` - 5GHz backup signal at STA
- `remote[].linkQuality.signalPerChain[]` - Per-chain signals

**Dashboard Display:**
- Main signal column shows AP RX signal (`peer.Radio*.Signal`)
- Detail panel shows both AP RX and STA RX perspectives side by side
- Per-chain signals available when reported by device

#### RSSI to dBm Conversion (airMAX Only)

airMAX devices (AirOS) report signal strength in two different formats that must be handled correctly:

**Format 1: dBm (negative values)**
Values like `-65` are already in standard decibel-milliwatts format and are used directly.

**Format 2: RSSI (positive values)**
Values like `35` are relative "Received Signal Strength Indicator" values that require conversion.

**Conversion Formula:**
```
dBm = RSSI - 95
```

**Examples:**
| ChainRSSI | Converted dBm | Quality |
|-----------|---------------|---------|
| 40 | -55 dBm | Excellent |
| 35 | -60 dBm | Good |
| 30 | -65 dBm | Good |
| 25 | -70 dBm | Fair |
| 20 | -75 dBm | Poor |

**Where RSSI conversion applies:**
- `wireless.sta[].chainrssi[]` - Per-chain values from station list
- `wireless.sta[].rssi` - Overall RSSI when reported as positive
- `wireless.sta[].remote.chainrssi[]` - Remote station per-chain values

**Where RSSI conversion does NOT apply:**
- Values already negative (already in dBm)
- `wireless.noisef` / `noisefloor` - Already in dBm (typically -94 to -90)
- Wave/LTU devices - These always report dBm directly

**IMPORTANT - Common Misconception:**
Do NOT use `noise_floor - RSSI` for conversion. The correct formula is simply `RSSI - 95` using a fixed offset. The noise floor is only used for SNR calculation AFTER both signal and noise are in dBm:
```
SNR = signal_dBm - noise_floor_dBm
```

**Implementation:**
All RSSI-to-dBm conversion is performed server-side in Go (`internal/airmax/client.go`). The JavaScript frontend receives pre-converted dBm values and never performs RSSI conversion.

#### Signal Quality Thresholds

The UI uses band-specific thresholds for signal quality classification. All code uses the unified `SIGNAL_THRESHOLDS` constant defined in `app.js`:

```javascript
const SIGNAL_THRESHOLDS = {
  '60ghz': { good: -55, fair: -65 },  // Wave 60GHz
  '5ghz':  { good: -62, fair: -70 }   // 5GHz, 2GHz, LTU, airMAX
}
```

| Band | Good | Fair | Poor |
|------|------|------|------|
| 60GHz | > -55 dBm | -55 to -65 dBm | < -65 dBm |
| 5GHz/2GHz/LTU/airMAX | > -62 dBm | -62 to -70 dBm | < -70 dBm |

**Notes:**
- 2GHz (airMAX M) uses the same thresholds as 5GHz - RF propagation characteristics are similar enough that the same quality buckets apply
- 60GHz has tighter thresholds because the technology is more sensitive to signal degradation
- All signal-related UI code must use helper functions (`getSignalQuality()`, `getSignalClass()`, `getDeviceBand()`) rather than hardcoded values
- Thresholds are duplicated in `components.js` for functions that don't import from `app.js`
- **Offline detection:** Devices with signal <= -100 dBm or no signal reading are classified as "offline" regardless of their `online` status flag. Signal values of -100 dBm indicate "no reading" rather than an actual measurement.

**Helper Functions:**
- `getDeviceBand(device)` - Returns '60ghz' or '5ghz' based on which signal fields are populated
- `getSignalQuality(level, band)` - Returns 'good', 'fair', 'poor', or '' for CSS class assignment
- `getSignalClass(level, band)` - Returns CSS class: 'signal-excellent', 'signal-good', 'signal-fair', 'signal-poor'
- `getThresholdLabel(band)` - Returns threshold values for legend display

#### airMAX Capacity and CINR

airMAX AC devices report capacity and CINR per station:

```json
"sta[].airmax": {
  "downlink_capacity": 85800,  // kbps - capacity AP->STA
  "uplink_capacity": 34320,    // kbps - capacity STA->AP
  "rx": { "cinr": 30 },        // CINR for downlink
  "tx": { "cinr": 32 }         // CINR for uplink
}
```

These are stored in `Radio5GHz.Capacity` and `Radio5GHz.CINR`:
- Values are in kbps, converted to bps (x1000) in poller
- CINR (Carrier-to-Interference-plus-Noise Ratio) is similar to SNR but includes interference

#### Directional RF Metrics (CINR, SNR, EVM)

WaveControl treats certain RF metrics as *directional* so we can compare **Downlink** vs **Uplink** health:

- **Downlink (dl)**: AP -> STA (TX on AP; RX on STA)
- **Uplink (ul)**: STA -> AP (TX on STA; RX on AP)

**CINR**
- Units: **dB** (higher is better)
- Where it comes from:
  - airMAX AC: derived from `airmax.tx.cinr` (dl) and `airmax.rx.cinr` (ul)
  - Wave/LTU: derived from Wave API link stats where available
- Stored as: `radio_*.cinr.{dl,ul}`

**SNR**
- Units: **dB** (higher is better)
- Computed as: `signal - noise_floor`
- Stored (when available) as: `radio_*.snr` and `radio_*.remote_snr` (remote = far-end reading)
- **Wave caveat:** Wave `noise_floor` is a long-term average, so SNR should be treated as an estimate.

**EVM**
- Units: **dB** (higher is better in the UI)
- Currently sourced from **airMAX** per-chain EVM time series. The poller takes a percentile value and
  re-maps it into a "higher is better" representation before storing it.
- Stored as: `radio_*.evm.{dl,ul}`

#### Directional Diagnosis (Dashboard + Host Detail)

The UI computes a simple directional diagnosis for each STA using the available directional metrics:

1. Prefer CINR if present (dl/ul separately)
2. Fall back to SNR (remote_snr for dl, snr for ul) if CINR is missing
3. If EVM is present, it is combined with the CINR/SNR signal (worst-quality wins)

**Thresholds** (defined in `DIR_THRESHOLDS` in `app.js` and `components.js`):

```js
const DIR_THRESHOLDS = {
  cinr: { good: 20, fair: 15, poor: 12 },
  snr:  { good: 25, fair: 18, poor: 12 },
  evm:  { good: 17, fair: 13, poor: 9 }
}
```

**UI exposure**
- **Dashboard:** Optional `Dir` column (enable via the column selector). Shows DL/UL badges per device.
  - For APs, this is an aggregate summary across connected STAs.
- **Host detail pane:** A "Directional Diagnosis" section is shown:
  - For STAs:
    - Two rows (Downlink and Uplink). Each row shows **one** badge (**DL** on the Downlink row, **UL** on the Uplink row) plus the underlying CINR/SNR/EVM values (when present).
    - If the device does not report any directional metrics yet, the section shows a short *"metrics unavailable"* note instead of rendering empty values.
  - For APs:
    - Stations analyzed count + separate Downlink/Uplink rows with a single **DL**/**UL** badge and **poor/fair/good** counts.
    - If some stations have no directional metrics, the counts include an additional **no data** bucket (these stations do not appear in the "worst" lists).
    - The "Worst Downlink/Uplink stations" lists only appear when there are **poor/fair** stations in that direction (to avoid noisy output when everything is good or when metrics are unavailable).
    - A short *"metrics unavailable"* note is shown when directional metrics are missing; otherwise the section stays data-only to avoid extra UI clutter.

These diagnoses are heuristic—use AP-wide patterns (many STAs degraded in one direction) to distinguish
AP-wide TX vs AP-wide RX issues from a single STA with localized RX/TX issues.

#### Platform Detection and Radio Assignment

Devices using the Wave API (Wave, Wave MLO, LTU) are detected primarily by firmware prefix:

| Prefix | Platform | Notes |
|--------|----------|-------|
| `GMC.`, `GMP.`, `MGMP.`, `MW.` | wave | Wave API devices (includes classic Wave and Wave MLO). **Radio-band assignment must be inferred from API payloads** (do not hardcode 60/5 based on prefix). |
| `AFLTU`, `AFLTUROCKET`, `AF5XHD` | ltu | LTU API devices. Single 5 GHz radio. |

**Why this matters:**
- Classic Wave devices generally have **60 GHz main + 5 GHz backup** radios (older mapping: `main→60`, `backup→5`).
- Wave MLO devices (MLO5/MLO6) expose **two radios without 60 GHz** (e.g., 5+5 or 5+6), and may still use firmware prefixes like `MW.`.
- Therefore: **never assume `id == "main"` implies 60 GHz**. Infer the band from the radio data itself.

**Wave / Wave MLO radio-band inference (recommended):**
- Build a `radioID → band` map from `/api/v1.0/statistics` → `wireless.radios[]`.
- Infer band using (in priority order):
  1. If `afc` is present → **6 GHz** (AFC exists only on 6 GHz radios).
  2. If `frequency.tx` (MHz) is present:
     - `>= 57000` → **60 GHz**
     - `>= 5925` (or within the 5945–7125 range) → **6 GHz**
     - otherwise → **5 GHz**
  3. If `frequency.tx` is missing/null and no `afc`: use `channelWidth.tx` / `channelWidth.rx` as a hint:
     - `>= 1000` → **60 GHz**
     - otherwise → **unknown** (leave unassigned / show unknown in UI)

**Wave / Wave MLO radio-slot mapping (current schema: one slot per band):**
- First 60 GHz radio → `Radio60GHz`
- First 5 GHz radio → `Radio5GHz`
- First 6 GHz radio → `Radio6GHz`
- If a device has a *second* 5 GHz radio (e.g., **MLO5**), store it in `Radio6GHz` but label it **“5 GHz #2”** in UI until we migrate to a dynamic `radios[]` model.

**Wave / Wave MLO peers / stations mapping:**
- Do **not** map peers/stations to a band by `id` (`main`/`backup`) alone.
- Use the `radioID → band` map derived above and attach each peer to the correct band slot.


**LTU-specific features:**
- CINR (Carrier-to-Interference-plus-Noise Ratio) instead of pure noise floor
- `signalPerChain` for per-antenna signals
- Channel widths: 10, 20, 30, 40, 50, 60, 80, 100 MHz

#### Discovery Fallback

Device discovery tries Wave API first, then falls back to airMAX:

```go
func discoverDevice(ip, username, password string) (*DeviceInfo, error) {
    // Try Wave API first
    info, err := discoverWaveDevice(ip, username, password)
    if err == nil {
        return info, nil
    }
    
    // Wave failed, try airMAX
    log.Printf("Wave failed (%v), trying airMAX", err)
    return discoverAirMAXDevice(ip, username, password)
}
```

**Platform Detection:**
- Wave: Returns `x-auth-token` header on login
- airMAX: Returns `AIROS_*` cookie on login

#### Lenient JSON Parsing

AirOS firmware versions return inconsistent JSON types. The parser handles this gracefully:

1. **Strict Parse First** - Try direct unmarshal into typed structs
2. **Lenient Fallback** - If strict fails, parse into `map[string]json.RawMessage`
3. **Section-by-Section** - Parse each top-level key separately
4. **Log All Errors** - Report every parse failure but continue with available data

```go
// Strict failed - try lenient parsing
var raw map[string]json.RawMessage
json.Unmarshal(body, &raw)

// Parse each section, log errors but continue
if hostRaw, ok := raw["host"]; ok {
    if err := json.Unmarshal(hostRaw, &status.Host); err != nil {
        log.Printf("host parse error: %v", err)
        parseHostLenient(hostRaw, &status.Host)
    }
}
// ... repeat for wireless, interfaces, stations, gps, etc.
```

**Manual Field Extraction:**
For sections that fail typed parsing, extract fields manually from `map[string]any`:

```go
func parseHostLenient(raw json.RawMessage, host *HostInfo) {
    var m map[string]any
    json.Unmarshal(raw, &m)
    
    if v, ok := m["hostname"].(string); ok {
        host.Hostname = v
    }
    if v, ok := m["uptime"].(float64); ok {
        host.Uptime = int64(v)
    }
    // ... etc
}
```

#### airMAX Status Struct Types

Real AirOS devices return numeric JSON types (not strings):

```json
{
  "host": {
    "hostname": "AB1N",
    "uptime": 1932205,           // int64
    "temperature": 29,           // float64
    "cpuload": 31.000000,        // float64
    "totalram": 129961984,       // int64
    "freeram": 77975552          // int64
  },
  "wireless": {
    "frequency": 5180,           // int
    "chanbw": 40,                // int
    "txpower": 27,               // int
    "compat_11n": 1              // int
  }
}
```

**Go Struct Types:**

```go
type HostInfo struct {
    Hostname    string  `json:"hostname"`
    DevModel    string  `json:"devmodel"`
    FWVersion   string  `json:"fwversion"`
    Uptime      int64   `json:"uptime"`
    Temperature float64 `json:"temperature"`
    CPULoad     float64 `json:"cpuload"`
    TotalRAM    int64   `json:"totalram"`
    FreeRAM     int64   `json:"freeram"`
    // ...
}

type WirelessInfo struct {
    ESSID       string          `json:"essid"`
    Mode        string          `json:"mode"`
    Frequency   int             `json:"frequency"`
    ChanBW      int             `json:"chanbw"`
    TXPower     int             `json:"txpower"`
    Polling     json.RawMessage `json:"polling"` // Can be string or object
    // ...
}
```

**Polling Field:**
The `polling` field can be either:
- A string: `"polling": "enabled"`
- An object: `"polling": { "dcap": 1000, "ucap": 500, ... }`

Using `json.RawMessage` allows handling both cases:

```go
func (w *WirelessInfo) GetPollingInfo() *PollingInfo {
    if len(w.Polling) == 0 || w.Polling[0] == '"' {
        return nil  // Empty or string value
    }
    var info PollingInfo
    json.Unmarshal(w.Polling, &info)
    return &info
}
```

#### PostgreSQL INET Type Handling

The `ip_address` column uses PostgreSQL's `INET` type. When casting to text, it includes CIDR notation:

```sql
-- Returns: 172.20.3.39/32
SELECT ip_address::text FROM devices;

-- Returns: 172.20.3.39 (clean IP)
SELECT host(ip_address) FROM devices;
```

**Fix:** Always use `host(ip_address)` when retrieving IPs for API calls to avoid malformed URLs like `https://172.20.3.39/32/api/...`

#### Poller Platform Routing

The poller routes devices to platform-specific handlers:

```go
func (p *Poller) pollDevice(job pollJob) {
    switch job.Platform {
    case "wave":
        success := p.pollDeviceWave(job)
        if !success {
            // Wave failed, try airMAX fallback
            p.pollDeviceAirMAX(job)
        }
    case "airmax":
        p.pollDeviceAirMAX(job)
    default:
        // Unknown platform, try Wave first
        if !p.pollDeviceWave(job) {
            p.pollDeviceAirMAX(job)
        }
    }
}
```

**Platform Field Updates:**
When a device successfully responds to a different protocol than expected, the platform field is updated in the database to optimize future polls.



## Frontend real-time synchronization and stale-session guardrails

- WebSocket updates are treated as **incremental patches**, but the client must still perform a **periodic full reconcile** of `/api/wavecontrol/devices` to converge any missed transitions (for example online/offline flips lost to websocket backpressure or long-lived-tab drift).
- The detail/host panel must stay current even when only one device misses a websocket patch. When the detail panel is open, the client should periodically re-fetch the selected device (`/api/wavecontrol/devices/{id}`) if that device has not been updated recently.
- The client must track the last successful **server synchronization** time from:
  - successful authenticated HTTP responses, and
  - websocket traffic / pong acknowledgements.
- If the websocket appears connected but goes **silent** past a timeout, the client should force a websocket reconnect.
- If the tab has not synchronized with the server for a prolonged period, the client must **fail closed** by returning to the login screen instead of silently showing stale data.


## Reports
### Chain Imbalance report
- A report type named `chain` exists in the Reports section.
- It flags device radios and peer radios whose per-chain signal spread exceeds **5 dB**.
- Before computing spread, the report must sanitize AirMAX/AirOS per-chain arrays:
  - ignore zero placeholder entries;
  - preserve negative values exactly as dBm chain readings, even when they are close to the reported noise floor;
  - do not infer that a negative chain value is a noise-floor placeholder merely because it matches or nearly matches `noisefloor`;
  - if fewer than two valid chain signals remain, do not emit a chain-imbalance finding for that side.
- The report includes:
  - scope (`device` or `peer`)
  - direction (`device`, `ap_rx`, `sta_rx`)
  - band label
  - hostname / IP
  - parent AP context (for peers)
  - site
  - chain values
  - min / max / spread in dB
- CSV export is supported for chain reports.

## Device add / bulk add consistency
- `Add IP` and `Bulk Add` must use the same discovery + upsert pipeline.
- Device matching for add/upsert must be **case-insensitive on MAC** (`lower(mac)`), to avoid divergence with historical mixed-case rows.
- `Add IP` is treated as a long-running device-discovery operation and should use the same long-operation timeout class as bulk add.


## Dashboard exclusion controls
- The left-hand device tree must not show redundant AP/DIR badges; hostname, status dot, and child count provide the primary hierarchy cues.
- Dashboard/Devices pages provide a compact **Focus** control in the dashboard toolbar.
- Users can exclude device families from the dashboard view:
  - airMAX
  - airFiber
  - LTU
  - Wave 60
  - Wave MLO5
  - Wave MLO6
- Users can also exclude frequency bands from the dashboard view:
  - 2 GHz, 3 GHz, 5 GHz, 6 GHz, 11 GHz, 16 GHz, 24 GHz, 60 GHz
- Exclusions apply to:
  - the main dashboard/devices list
  - the left-hand tree while on dashboard/devices
  - dashboard status counts while on dashboard/devices
- Exclusions are client-side view controls and must not mutate server state.


## Dashboard focus filters
- The dashboard/devices view includes a compact top-right focus control for exclusion filters and a compact column-menu control.
- The header must not waste horizontal space with large title/count text.
- Visible/hidden totals are shown as a compact status pill at the bottom-right of the dashboard area.
- Dashboard exclusion filters are toggled with direct on/off buttons and must be individually reversible as well as clearable via a single "Clear all" action.
- Left tree view should not show redundant AP/DIR badges; hostname space is preferred.

### Dashboard header controls
- The dashboard exclusion (focus) control and column-menu control live in the **same header row as the table columns**, not in a separate toolbar line.
- The first checkbox column header is intentionally blank; there is **no select-all checkbox embedded in the header**.
- Visible/hidden totals are shown in a compact status pill at the bottom-right of the dashboard view.


### Reports: Chain Imbalance
- Peer-side chain imbalance rows are combined by link and indicate whether the chain spread issue is on:
  - `both`
  - `ap_only`
  - `sta_only`
- Device-radio rows remain valid and use `mismatch_side = device`.

### Reports: RX Level Mismatch
- A report type named `rx_mismatch` exists.
- It flags links where the AP RX level and STA RX level for the same link differ by more than **8 dB**.
- Each row includes:
  - band
  - AP hostname/IP
  - STA hostname/IP
  - site
  - AP RX dBm
  - STA RX dBm
  - delta in dB
  - which side is stronger (`ap_rx` or `sta_rx`)
- CSV export is supported.


### Report thresholds
- Report generation thresholds are configurable in **Settings → Report Thresholds**.
- Settings keys:
  - `chain_imbalance_threshold_db` (default `5`)
  - `rx_mismatch_threshold_db` (default `8`)
- The **Chain Imbalance** and **RX Level Mismatch** reports must use the saved settings values instead of hardcoded constants.

### Dashboard header controls
- Dashboard filter and column controls are rendered inline in the header row.
- These controls must be fully toggleable:
  - clicking the filter button opens/closes the exclusion menu
  - clicking an active exclusion toggles it off
  - `Clear all` disables all exclusions
  - clicking the column button opens/closes the column menu
- The controls should use container-scoped event handling and must continue working after re-renders.


### Report viewer sorting and affected IP
- The **Chain Imbalance** and **RX Level Mismatch** reports support client-side sorting by clicking column headers.
- Both reports include an **Affected IP** column:
  - for peer/link findings, this is the STA IP when available
  - otherwise it falls back to the AP/device IP
- CSV exports for these reports include the same affected-IP context.


## Dashboard header controls
- Dashboard filter/focus and column-selection menus are rendered inline in the header row.
- Their dropdowns use viewport-positioned overlays so they are not clipped by sticky headers, table overflow, or virtual-scroll wrappers.
- Clicking the focus or column button must open/close the respective menu reliably for both regular and virtual dashboard renders.

## Report export consistency
- CSV export for `rx_mismatch` must include the MAC column and all declared headers/fields.
- Report viewer titles should use human-readable names for custom report types.

## Alert target policy

Alert evaluation is server-authoritative and uses two layers:

1. Alert rule targeting: `scope` narrows by inventory scope (`all`, `site`, or `device`), `target_role` narrows by role (`all`, `ap`, `sta`), and `require_alertable` controls whether per-device alert policy is honored.
2. Device alert policy: `devices.alertable`, `devices.alert_silenced_until`, and `devices.alert_notes` define operator intent for each inventory row.

Evaluation order is: enabled rule, scope match, role match, alertable/silence gate, metric existence, threshold condition, persistence, cooldown, transactional occurrence/outbox creation. Full lifecycle semantics are defined in the Alerting System section and `docs/ALERTING.md`.


## Host build and installation workflow

The repository root provides one `Makefile` for GNU make on Linux and BSD make on OpenBSD. It must remain within their shared pmake-compatible feature subset: ordinary variables and targets, `.PHONY`, shell recipes, and no GNU-only make functions or pattern-specific behavior. The normal workflow is an unprivileged `make`, followed by privileged `make install`.

The Unix installer must:

- Create the `_wavecontrol` user/group when absent and require `/var/wavecontrol` as the account home.
- Install the binary under `/usr/local/bin`, schema/migrations/documentation under `/usr/local/share`, web assets under `/var/wavecontrol/web`, and the native systemd or rc.d definition.
- Create writable `/var/wavecontrol/firmware` and `/var/wavecontrol/backups` directories without making the web tree writable by the daemon.
- Install the canonical `wavecontrol.env.example` and create `/etc/wavecontrol/wavecontrol.env` only when it does not already exist. Reinstallation may harden its mode but must never replace its contents.
- Support staged packaging through `DESTDIR` and explicit Linux/OpenBSD service selection without creating accounts on the build host.

`make env` follows the same create-if-missing rule for a repository-local development environment. `make run` uses an isolated `.wavecontrol` runtime directory and must refuse a root development launch.

The canonical environment sample is shared by systemd, OpenBSD rc.d, and Windows. It uses the common one-line `NAME=value` syntax, documents persistent key handling, and keeps first-user bootstrap credentials disabled until explicitly configured.

Windows packaging is implemented by `windows/build.ps1`. The optional `windows/WaveControl.proj` MSBuild entry point must invoke that same packager rather than maintain a second packaging implementation. A package contains both `wavecontrol.env.example` and an automatically created `wavecontrol.env`; `-KeepExisting` preserves the populated active file and runtime data while replacing packaged assets.


## Windows host runtime

Windows is a supported WaveControl server host, not a monitored endpoint type. `GOOS=windows` builds must use `CGO_ENABLED=0` and include the same Ubiquiti poller, PostgreSQL storage, web UI, alert engine, and report implementation as Unix builds.

The Windows executable runs in the foreground. It must not change to a user-profile directory implicitly. When no explicit `-workdir` is provided and a relative web root is used, it may prefer the executable directory if the current directory lacks `web/index.html`. Release packaging must include `wavecontrol.exe`, `web/`, `schema.sql`, migrations, run/environment examples, and writable firmware/backup directories. PowerShell launch scripts must pass explicit executable-relative `-workdir` and `-webroot` values.
