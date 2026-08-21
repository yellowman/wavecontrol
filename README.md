# waveControl

Device management and monitoring system for Ubiquiti Wave (60GHz, 5GHz/6GHz MLO), LTU, and airMAX (M/AC) wireless devices. Provides real-time visibility into device stats only available through the Ubiquiti HTTP API, with a focus on radio-specific metrics like per-chain signal levels. Includes a Zabbix bridge to retrieve these statistics since they are not available from the Ubiquiti device SNMP server.

## Built for Large Networks

waveControl is designed from the ground up to handle **large ISP deployments with thousands of devices** without the browser slowdowns that plague other network management tools.

### Virtual Scrolling

The device dashboard uses **virtual scrolling** - only the rows visible on screen (plus a small buffer) are actually rendered in the DOM. This means:

- **5,000 devices** renders as fast as 50 devices
- No browser freezing when scrolling through massive device lists
- Instant search and filtering (data stays in memory, only rendering is virtualized)
- WebSocket updates are batched to prevent UI stutter during poll cycles

Previous tools like UNMS/UISP would grind to a halt with 500+ devices. waveControl handles 5,000+ without breaking a sweat.

### Memory-Efficient Architecture

- **Static data in PostgreSQL**: Device inventory, credentials, firmware versions
- **Live stats in memory**: Signal levels, rates, peer counts - never hits the database
- **Incremental updates**: Only changed values are pushed to the browser

## Features

### Device Management
- **Dashboard**: Device list with AP/STA tree view, live stats, context menus
- **Auto-Discovery**: Add AP IP, automatically discovers all connected STAs
- **Replacement AP/STA adoption**: MAC mismatch detection with explicit, audited “Learn replacement MAC” workflow
- **Search**: Full-text search across hostname, IP, MAC, model, firmware
- **Bulk Operations**: Multi-select devices for batch actions with configurable concurrency
- **Site/Region Organization**: Hierarchical grouping with bulk site assignment

### Monitoring
- **Real-time Stats**: Per-chain signal levels, TX/RX rates, capacity, airtime
- **WebSocket Updates**: Live stats push without polling
- **Alerting**: Ubiquiti AP/STA rules with scope/role targeting, persistence, explicit or automatic severity, acknowledgement/recovery lifecycle, per-channel delivery state, and durable email, webhook, Zabbix, or sysmon-web delivery
- **Map View**: Leaflet/OpenStreetMap with GPS markers and signal-colored links
- **KMZ Export**: Export device locations to Google Earth (APs-only or all devices)
- **Quality**: Table-based AP<->client quality monitoring (Signal + Modulation) with issues queue

### Operations
- **Firmware Upgrades**: Single, bulk, fanout (AP+STAs), scheduled
- **Firmware Management**: Upload, delete firmware via web UI (up to 1GB)
- **Config Management**: Backup, restore, batch push configurations
- **Scheduled Jobs**: Upgrades, reboots, refresh with repeat options
- **Maintenance Windows**: Define maintenance periods for scheduled jobs; radio alerts use explicit per-device alertability and silence controls
- **Reports**: Versioned health, inventory, performance, chain-imbalance, and RX-mismatch snapshots

### Reports

Reports are immutable, versioned snapshots stored in PostgreSQL. Opening an existing report renders the data captured at generation time; it does not silently replace saved values with the current dashboard state. The Reports page provides searchable history, same-type comparison, print output, and JSON/CSV downloads.

**Report Types:**

| Type | Contents |
|------|----------|
| **Network Health** | Authoritative inventory availability, AP/STA split, metric coverage, band-aware subscriber signal quality, system pressure, stability, firmware distribution, site summaries, and ranked operational exceptions |
| **Device Inventory** | Complete AP/STA inventory with status, platform family, firmware, region/site placement, parent AP, and last-seen data |
| **Performance Summary** | Captured aggregate throughput history, AP/STA rates, platform/site aggregates, subscriber signal distribution, capacity-risk APs, and missing-metric coverage |
| **Chain Imbalance** | Ranked device-radio and peer-link findings whose sanitized per-chain spread exceeds the configured threshold |
| **RX Level Mismatch** | Ranked links whose AP-side and STA-side receive levels disagree beyond the configured threshold |

**Snapshot and coverage behavior:**
- Inventory counts and status come from the database; live measurements are matched by normalized MAC address.
- Every report records a schema version, generation timestamp, inventory scope, and explicit metric coverage.
- Performance reports store the current in-memory throughput-history ring inside the report. Reopening the report uses those captured samples.
- Legacy performance reports that predate captured history are clearly labeled before the UI offers current history as a fallback.
- Radio selection includes Wave 60 GHz, MLO 6 GHz, 5 GHz, LTU, and additional reported radios.
- Health offenders and RF diagnostics are severity-ranked rather than returned in arbitrary iteration order.
- Performance CSV includes both AP and STA rows. Empty diagnostic reports still export a valid header-only CSV.

**Report workflow:**
- Editors and administrators can generate and delete snapshots; viewers can inspect, compare, print, and download them.
- Search report history by type, creator, date, or report ID and filter by report type.
- Compare two snapshots of the same type. Deltas are calculated as newer minus older and use metric-aware improvement/degradation coloring.
- Sort device, site, platform, offender, and diagnostic tables directly in the full-screen report viewer.
- Download the original JSON snapshot or a report-specific CSV representation.

**Data Retention:**

| Data Type | Storage | Retention |
|-----------|---------|-----------|
| Reports | PostgreSQL | Permanent until explicitly deleted |
| Throughput History | In memory and copied into each performance report | Approximately 30 minutes live; permanent inside the saved report |
| Stability Tracking | In memory and summarized into each health report | 24-hour rolling live window; summary permanent inside the saved report |

### Modal and Dialog UX

waveControl uses one responsive modal runtime rather than browser-native `alert()`, `confirm()`, or `prompt()` dialogs. Static and dynamically generated dialogs share the same header, sections, form controls, consequences, and footer actions.

- Focus is moved into the dialog, trapped while open, and restored to the launching control on close.
- Escape and backdrop behavior are explicit; body scrolling is locked while a modal is active.
- Inputs, selects, text areas, day pickers, choice cards, warnings, loading states, and destructive confirmations use the same dark/light-theme styling.
- Long workflows use wide or full-screen shells with internal scrolling and responsive mobile layouts.
- Certificate management, drilldown lists, scheduled jobs, job details, user/device actions, firmware/configuration operations, Ultra Debug, and maintenance windows use the shared dialog system.
- Maintenance-window editing loads the existing record and sends an update rather than accidentally creating a duplicate.

### Integration
- **Zabbix Bridge**: Native agent protocol on port 10050
- **TLS Management**: Pin certificates, trust-on-first-use, or skip verification
- **REST API**: Cookie-authenticated API for the browser and same-origin automation
- **Durable Notifications**: Retryable trigger/recovery delivery through a PostgreSQL outbox, including sysmon-web CRITICAL/WARNING/OK reporting

### Security
- **Role-Based Access**: Viewer, Creator, Editor, Administrator roles
- **Privilege Dropping**: Drops to unprivileged user after startup
- **OpenBSD Support**: pledge(2) syscall restriction

### Supported Ubiquiti platforms
| Platform | Models | API Type |
|----------|--------|----------|
| Wave 60GHz | GMC, GMP, MGMP | REST JSON |
| Wave MLO | MW | REST JSON |
| AirFiber 60 | GP | REST JSON (Wave) |
| LTU | AFLTUROCKET, AFLTU | REST JSON |
| AirFiber 5XHD | AF5XHD | REST JSON (LTU) |
| airMAX AC | XC, 2XC, WA, 2WA | CGI/JSON |
| airMAX M | XM, XW | CGI/JSON |
| AirFiber (future) | AF11, AF24, AF2X, AF3X, AF5, AF5X | REST JSON |

### WaveControl host operating systems

| Host OS | Runtime | Notes |
|---|---|---|
| Linux | Native Go binary | systemd example included |
| OpenBSD | Native Go binary | rc.d and pledge support included |
| FreeBSD | Native Go binary | Foreground or service wrapper |
| Windows x64/ARM64 | Native console `.exe` | PowerShell build/run packaging included; still monitors Ubiquiti devices only |

See [Windows host installation](docs/WINDOWS_HOST.md).

## Navigation

| Page | Description |
|------|-------------|
| Dashboard | Device table with tree, stats, context menus, bulk actions |
| Map | Geographic view with GPS markers, signal-colored links, KMZ export |
| Quality | Signal + Modulation quality tables, issues queue, mismatches |
| Config | Backup/restore, batch configuration push |
| Reports | Generate, inspect, compare, print, and download saved network snapshots |
| Settings | Polling, credentials, alert delivery, sysmon-web, Zabbix, users, TLS, and scheduled jobs |

### Quality Page

The Quality page provides **table-driven analytics** for AP<->Client monitoring at scale. Instead of showing 5,000 nodes in an unreadable graph, it uses ranked tables and an issues queue.

**Tabs:**

1. **Signal Levels** (default) - Ranked AP table with expandable client rows
2. **Modulation Rates** - Same layout, but evaluates modulation/MCS/rate health
3. **Issues Queue** - Combined signal + modulation issues
4. **Mismatches** - Data-quality view for identifier collisions/duplicates

**Signal Levels Features:**
- Columns: Status, Name, Site, Clients, Distribution bar, Poor count, Poor %, Worst signal, Health
- **Default sort: Worst first** (poor % descending)
- Click row to expand and see worst 10 clients
- "View all clients" opens full list in details panel
- Click AP name or client row to see device details

**Issues Queue Types:**
- **AP quality** - APs with high poor signal and/or poor modulation and/or many offline clients (combined)
- **Critical signal** - Individual clients significantly below poor threshold
- **Critical modulation** - Individual clients with extremely low modulation

**Signal Quality Thresholds:**
- 60GHz: Good >-55, Fair -55 to -65, Poor <-65
- 5GHz/2GHz/LTU/airMAX: Good >-62, Fair -62 to -70, Poor <-70

**Site Filtering:**
- Dropdown to focus on one site at a time
- Essential for large networks with 500+ APs

## Architecture

```
+-------------------------------------------------------------+
|                      waveControl Server                      |
+--------------+--------------+--------------+----------------+
|   HTTP API   |   Poller     | Stats Store  | Firmware Svc   |
|   (chi)      |  (30s loop)  | (in-memory)  |  (upgrades)    |
+--------------+--------------+--------------+----------------+
| Alert Manager| Notification Outbox         | WebSocket Hub  |
+--------------+-----------------------------+----------------+
|                     PostgreSQL (inventory)                   |
+-------------------------------------------------------------+
```

**Data Split:**
- **Database**: Static inventory, encrypted operational credentials, alert history, and the notification outbox
- **Memory**: Real-time stats (signal, rates, uptime, peer details)
- **Browser**: Same-origin REST plus an authenticated WebSocket for live updates

## Installation

### Prerequisites

- Go 1.21+
- PostgreSQL 14+
- (Optional) nginx or OpenBSD relayd for reverse proxy (WebSocket support required)

### PostgreSQL Setup

#### Linux (Debian/Ubuntu)

```bash
# Install PostgreSQL
apt install postgresql postgresql-contrib

# Create user and database
sudo -u postgres psql <<EOF
CREATE USER wavecontrol WITH PASSWORD 'your-password-here';
CREATE DATABASE wavecontrol OWNER wavecontrol;
GRANT ALL PRIVILEGES ON DATABASE wavecontrol TO wavecontrol;
EOF

# Load schema
psql -U wavecontrol -h localhost wavecontrol < schema.sql
```

#### OpenBSD

```bash
# Install PostgreSQL
pkg_add postgresql-server

# Initialize and start
initdb -D /var/postgresql/data -U postgres
rcctl enable postgresql
rcctl start postgresql

# Create user and database
psql -U postgres <<EOF
CREATE USER wavecontrol WITH PASSWORD 'your-password-here';
CREATE DATABASE wavecontrol OWNER wavecontrol;
GRANT ALL PRIVILEGES ON DATABASE wavecontrol TO wavecontrol;
EOF

# Load schema
psql -U wavecontrol wavecontrol < schema.sql
```

#### FreeBSD

```bash
# Install PostgreSQL
pkg install postgresql16-server

# Initialize and start
/usr/local/etc/rc.d/postgresql initdb
sysrc postgresql_enable=YES
service postgresql start

# Create user and database
sudo -u postgres psql <<EOF
CREATE USER wavecontrol WITH PASSWORD 'your-password-here';
CREATE DATABASE wavecontrol OWNER wavecontrol;
GRANT ALL PRIVILEGES ON DATABASE wavecontrol TO wavecontrol;
EOF

# Load schema
psql -U wavecontrol -h localhost wavecontrol < schema.sql
```

#### Windows host

Install PostgreSQL 14+ locally or use a reachable PostgreSQL server, then create the database and load `schema.sql` with `psql.exe`. The Windows package/build procedure is documented in [docs/WINDOWS_HOST.md](docs/WINDOWS_HOST.md).


### Linux and OpenBSD: make workflow

The repository includes a single portable `Makefile` written for the common GNU make/BSD make (pmake) subset. Use the native `make` command on both Linux and OpenBSD.

Build as an ordinary user:

```sh
make
```

For a release-oriented local check:

```sh
make check
```

`make check` verifies Go formatting, confirms that the three distributed environment templates are identical, runs `go test ./...`, and runs `go vet ./...`.

Install from the same source tree after a successful build:

```sh
# Linux
sudo make install

# OpenBSD
doas make install
```

`make install` performs the complete host installation:

- Creates the `_wavecontrol` user and group when absent, with `/var/wavecontrol` as its home.
- Installs the binary as `/usr/local/bin/wavecontrol`.
- Installs immutable web assets under `/var/wavecontrol/web` and writable firmware/configuration-backup directories under `/var/wavecontrol`.
- Installs `schema.sql`, migrations, and documentation under `/usr/local/share`.
- Installs the systemd unit on Linux or the rc.d script on OpenBSD.
- Installs `/etc/wavecontrol/wavecontrol.env.example` and creates `/etc/wavecontrol/wavecontrol.env` automatically **only when the active file does not already exist**. Upgrades never overwrite the populated environment file.

Edit the installed environment file before starting the service:

```sh
sudoedit /etc/wavecontrol/wavecontrol.env       # Linux
# or
doas vi /etc/wavecontrol/wavecontrol.env       # OpenBSD
```

Generate the persistent secrets once:

```sh
openssl rand -base64 48   # WAVECONTROL_JWT_SECRET
openssl rand -base64 32   # WAVECONTROL_DATA_KEY
```

Set the PostgreSQL DSN, replace both secret placeholders, and—only for the first start of an empty database—uncomment the bootstrap username and password. After the administrator account is created, comment or remove the two bootstrap variables and restart WaveControl. Never rotate `WAVECONTROL_DATA_KEY` on an existing database without first re-encrypting the stored credentials.

Load a new database from the source tree or the installed schema:

```sh
psql -U wavecontrol -h 127.0.0.1 wavecontrol < schema.sql
# installed copy:
psql -U wavecontrol -h 127.0.0.1 wavecontrol < /usr/local/share/wavecontrol/schema.sql
```

Enable and start the installed service through the same Makefile:

```sh
# Linux
sudo make enable start
sudo make status

# OpenBSD
doas make enable start
doas make status
```

For an upgrade, stop or leave the service running as operationally appropriate, build as the normal account, rerun `make install` as root, and then run `make restart`. The installer replaces packaged assets while preserving `/etc/wavecontrol/wavecontrol.env`, firmware, and configuration backups.

Useful targets:

| Target | Purpose |
|---|---|
| `make` or `make build` | Build `build/wavecontrol` for the current host |
| `make check` | Formatting, environment-template, tests, and vet |
| `make env` | Create a local `wavecontrol.env` from the sample without overwriting it |
| `make run` | Run a foreground development instance using `.wavecontrol/` |
| `make cross-linux TARGET_ARCH=arm64` | Cross-build a Linux executable |
| `make cross-openbsd TARGET_ARCH=amd64` | Cross-build an OpenBSD executable |
| `make cross-windows TARGET_ARCH=amd64` | Cross-build only the Windows executable |
| `make install` | Complete Linux/OpenBSD installation |
| `make enable`, `start`, `restart`, `status`, `stop` | Manage the native service |
| `make uninstall` | Remove program files while preserving configuration and runtime data |
| `make help` | Show the target summary |

A packaging system can stage files without modifying the build host:

```sh
make install DESTDIR=/tmp/wavecontrol-linux INSTALL_OS=Linux

make cross-openbsd TARGET_ARCH=amd64
make install DESTDIR=/tmp/wavecontrol-openbsd INSTALL_OS=OpenBSD \
    BINARY=build/openbsd-amd64/wavecontrol
```

### Development run

Create and edit a repository-local environment file, then run in the foreground:

```sh
make env
vi wavecontrol.env
make run
```

The local environment file is mode `0600` and ignored by Git. Runtime firmware and backup data are kept under `.wavecontrol/`, also ignored by Git.

### Windows build and package workflow

Windows remains only a host for WaveControl; the monitored devices are still Ubiquiti radios. The complete package—not a bare `.exe`—should be built from PowerShell:

```powershell
Set-ExecutionPolicy -Scope Process Bypass
.\windows\build.ps1
notepad .\dist\windows-amd64\wavecontrol.env
.\dist\windows-amd64\run-wavecontrol.ps1
```

The builder runs the Go tests, compiles a native Windows executable with `CGO_ENABLED=0`, and packages the web UI, migrations, schema, documentation, runtime directories, and launch script. It installs both `wavecontrol.env.example` and an initial `wavecontrol.env` automatically. An existing active environment file is preserved when rebuilding with `-KeepExisting`:

```powershell
.\windows\build.ps1 -Architecture arm64
.\windows\build.ps1 -KeepExisting
```

For Windows build agents or developers who prefer a project-builder entry point, the repository also includes a normal MSBuild project that invokes the same PowerShell packager:

```powershell
# .NET SDK
dotnet msbuild .\windows\WaveControl.proj /t:Package /p:Architecture=amd64

# Visual Studio Build Tools
msbuild .\windows\WaveControl.proj /t:Package /p:Architecture=arm64
```

Optional MSBuild properties include `/p:SkipTests=true`, `/p:KeepExisting=true`, `/p:OutputDirectory=C:\WaveControlBuild`, and `/p:PowerShellExe=pwsh.exe`.

The launch script accepts quoted or unquoted values in `wavecontrol.env`, validates the required DSN and persistent secrets, creates the runtime directories, and supplies executable-relative working and web-root paths. This makes launches from Explorer, Task Scheduler, or a service wrapper independent of the caller's working directory. See [Windows host installation](docs/WINDOWS_HOST.md) for PostgreSQL, ACL, startup, firewall, and upgrade details.

### Installed directory layout

```
/usr/local/bin/
+-- wavecontrol

/usr/local/share/wavecontrol/
+-- schema.sql
+-- migrations/

/etc/wavecontrol/
+-- wavecontrol.env.example
+-- wavecontrol.env          # generated once; operator-owned secrets

/var/wavecontrol/
+-- web/                     # installed read-only application assets
+-- firmware/                # writable firmware uploads
+-- backups/                 # writable device configuration backups
```

| Directory | Purpose | Writable by WaveControl |
|---|---|---|
| `/var/wavecontrol/web` | Static browser assets | No |
| `/var/wavecontrol/firmware` | Firmware uploads | Yes |
| `/var/wavecontrol/backups` | Device configuration backups | Yes |

### Command Line Flags

```
-d              Debug mode (foreground, verbose logging to stderr)
-web            Standalone HTTP server mode (implies -d)
-addr string    Listen address (default: from settings or 127.0.0.1:8080)
-webroot string Path to web directory (default: "web")
-workdir string Working directory for relative web, firmware, and backup paths
-pidfile string PID file path in daemon mode (default: /var/run/wavecontrol.pid; empty on Windows)
-U              Unchrooted mode (skip chroot, just chdir to user's home)
-u string       User to run as (default: _wavecontrol, www, or nobody)
```

### Security Features

By default, waveControl:

1. **Daemonizes** - Forks to background (use `-d` to stay in foreground)
2. **Chroots** - If started as root, chroots to user's home directory
3. **Drops privileges** - Switches to `_wavecontrol`, `www`, or `nobody` user
4. **Logs to syslog** - Errors go to syslog in daemon mode; use `-d` for stderr
5. **OpenBSD pledge** - Uses pledge(2) to restrict syscalls on OpenBSD

Use `-U` to skip chroot (just chdir to user's home without chroot).

**Chroot Semantics (when running as root):**

The chroot jail provides defense-in-depth by restricting filesystem access. The actual startup order is:

1. **Before chroot:** Resolves the working/web paths, daemonizes, initializes logging, and writes the PID file
2. **Chroot call:** Changes the filesystem root to the service user's home (for example, `/var/wavecontrol`)
3. **Privilege drop:** Switches to the unprivileged account with `setgid`/`setuid`, then applies the OpenBSD pledge
4. **Inside the jail:** Loads the application secrets, connects to PostgreSQL, initializes services, binds the HTTP listener, and begins polling

Only files inside the jail are accessible after step 2; `/etc`, `/usr`, and host resolver/certificate files are otherwise unreachable.

This means:
- Firmware files must be inside the chroot (default: `./firmware/` relative to home)
- Config backups are stored inside the chroot (default: `./backups/`)
- Web assets are served from inside the chroot (default: `./web/`)
- No shell access, no system binaries, no ability to escape the jail

**Chroot requirements:**
- Use IP address in DSN (`127.0.0.1` not `localhost`) - no DNS resolution after chroot
- PostgreSQL must accept TCP connections (not Unix socket paths)
- Web assets, firmware, backups, and any filesystem-based certificate material needed at runtime must be inside the jail
- Use `-U` when an integration must use host resolver or CA files that are intentionally kept outside `/var/wavecontrol`

### Reverse Proxy Setup

waveControl listens on `127.0.0.1:8080` by default. Use a reverse proxy for TLS termination and public access.

**Important:** WebSocket support is required for real-time updates. OpenBSD httpd does not support WebSockets; use relayd instead.

#### nginx

**Important:** Proxy the entire root path (`location /`) to ensure all static assets including `/favicon.svg` are served correctly.

```nginx
server {
    listen 443 ssl;
    server_name wavecontrol.example.com;

    ssl_certificate /etc/ssl/wavecontrol.crt;
    ssl_certificate_key /etc/ssl/private/wavecontrol.key;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;

        # WebSocket support
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_read_timeout 86400;
    }
}

server {
    listen 80;
    server_name wavecontrol.example.com;
    return 301 https://$server_name$request_uri;
}
```

#### OpenBSD relayd

**Important:** Proxy the entire connection to ensure all paths including `/favicon.svg` are served correctly.

```
# /etc/relayd.conf

log connection

table <wavecontrol> { 127.0.0.1 }

http protocol "wavecontrol" {
    match request header append "X-Forwarded-For" value "$REMOTE_ADDR"
    match request header append "X-Forwarded-Proto" value "https"
    match request header append "Host" value "$HOST"

    # WebSocket support
    http websockets
}

relay "wavecontrol-https" {
    listen on egress port 443 tls
    protocol "wavecontrol"
    forward to <wavecontrol> port 8080
}

relay "wavecontrol-http" {
    listen on egress port 80
    forward to <wavecontrol> port 8080
}
```

Enable and start:

```bash
rcctl enable relayd
rcctl start relayd
```

### Running as a Service

The portable Makefile installs and manages the appropriate native service definition:

```sh
# Linux/systemd
sudo make enable start
sudo make status

# OpenBSD rc.d
doas make enable start
doas make status
```

The Linux unit runs the server in the foreground as `_wavecontrol` and reads the root-owned `/etc/wavecontrol/wavecontrol.env`. The OpenBSD rc.d script starts as root so WaveControl can chroot and drop privileges to `_wavecontrol`; it sources the same root-owned environment file before doing so. Neither service generates secrets at startup.

On Windows, run `run-wavecontrol.ps1` under a dedicated non-administrator account through Task Scheduler or a service wrapper with restart-on-failure configured. The script always runs WaveControl in the foreground and uses absolute package-relative paths.

## Quick Start

For a source-tree development instance:

```sh
# Create and load PostgreSQL first.
psql -U postgres -c "CREATE DATABASE wavecontrol;"
psql -U postgres wavecontrol < schema.sql

# Create the local sample and edit the DSN/secrets/bootstrap values.
make env
vi wavecontrol.env

# Build and run in the foreground.
make run
```

Open `http://127.0.0.1:8080`, sign in with the explicit bootstrap account, then remove the bootstrap variables from `wavecontrol.env` before the next restart.

For an installed Linux or OpenBSD host, use the full `make`, privileged `make install`, environment edit, and `make enable start` sequence in the Installation section instead.

## Security & Privileges

When running as root (typical for daemon mode), wavecontrol automatically drops privileges after startup.

### Privilege Dropping

If no `-u` flag is specified, wavecontrol tries these users in order:
1. `_wavecontrol` (recommended - create dedicated user)
2. `www`
3. `nobody`

To specify a user explicitly: `wavecontrol -u myuser`

### Working Directory

All relative paths (`firmware_path`, `backup_dir`) are resolved from the working directory, which is determined as follows:

| Scenario | Working Directory |
|----------|-------------------|
| Running as root (default) | `/` (chrooted to user's home) |
| Running as root with `-U` | User's home directory (no chroot) |
| Running with `-d` (debug) | Current directory |
| systemd with `WorkingDirectory=` | Specified directory |

**Example directory structure** (user `_wavecontrol` with home `/var/wavecontrol`):
```
/var/wavecontrol/           # Working directory (user's home)
+-- web/                    # Web assets (copied from archive)
|   +-- index.html
|   +-- js/
|   +-- css/
+-- firmware/               # firmware_path = "firmware"
+-- backups/                # backup_dir = "backups" (auto-created)
```

### Unchrooted Mode

By default, wavecontrol chroots to the user's home directory when running as root. To disable chroot (just chdir without chroot):

```bash
wavecontrol -U
```

This is useful if you need access to system paths outside the user's home directory.

### Debug Mode

With `-d` flag:
- No daemonization (stays in foreground)
- Logs to stderr instead of syslog
- Still drops privileges if running as root
- Working directory is current directory (not user's home)

## Configuration

All configuration is done via the web UI Settings page:

### General Settings

| Setting | Default | Description |
|---------|---------|-------------|
| `ap_cred1_user` … `ap_cred3_pass` | empty | Up to three AP username/password pairs |
| `sta_cred1_user` … `sta_cred3_pass` | empty | Up to three STA username/password pairs |
| `firmware_path` | `firmware` | Directory containing firmware files (relative) |
| `backup_dir` | `backups` | Directory for config backups (relative) |
| `listen_addr` | `127.0.0.1:8080` | HTTP listen address |
| `zabbix_enabled` | `false` | Enable Zabbix bridge |
| `zabbix_listen` | `127.0.0.1:10050` | Zabbix agent listen address |
| `smtp_host` / `smtp_port` | empty / `25` | Global SMTP endpoint for alert rules |
| `zabbix_server` | empty | Outbound Zabbix sender/trapper endpoint |
| `sysmon_alerter_enabled` | `false` | Enable sysmon-web alerter delivery |
| `sysmon_alerter_host` / `sysmon_alerter_port` | empty / `1347` | sysmon-web TLS agent listener |
| `sysmon_alerter_name` | `wavecontrol` | Minted sysmon-web alerter identity |
| `sysmon_alerter_token` | empty | Encrypted sysmon-web bearer token |
| `sysmon_alerter_ca_pem` | empty | Pinned sysmon-web certificate or CA PEM |
| `cors_origins` | (empty) | Additional exact HTTP(S) origins; wildcard-all is rejected for cookie authentication |
| `csp_img_sources` | (empty) | Additional CSP img-src domains for map tiles (space-separated) |
| `csp_connect_sources` | (empty) | Additional CSP connect-src domains for APIs (space-separated) |

**Map tile provider examples for `csp_img_sources`:**
- Mapbox: `https://*.tiles.mapbox.com https://api.mapbox.com`
- Google: `https://*.googleapis.com https://*.gstatic.com`
- Bing: `https://*.virtualearth.net`
- Stamen: `https://stamen-tiles.a.ssl.fastly.net`

### Alert delivery and lifecycle

Alert rules and occurrences are managed on the Alerts page; global SMTP, Zabbix sender, and sysmon-web endpoints are configured under **Settings → Alert Delivery**. The page shows runtime channel readiness, and each alert shows its durable trigger/recovery delivery state. See [Alerting](docs/ALERTING.md) and [sysmon-web alerter integration](docs/SYSMON_WEB_ALERTER.md).

### Poller Configuration (Admin only)

Configurable via Settings page or API (`GET/PATCH /api/wavecontrol/poller/config`):

| Setting | Default | Range | Description |
|---------|---------|-------|-------------|
| `poll_interval` | `30` | 10-300 | Seconds between device polls |
| `aps_per_worker` | `30` | 5-100 | APs assigned to each worker thread |
| `worker_count` | `50` | (read-only) | Number of worker threads |

### Bulk Operations (Admin only)

Configurable via Settings page or API (`GET/PATCH /api/wavecontrol/bulk-ops/config`):

| Setting | Default | Range | Description |
|---------|---------|-------|-------------|
| `max_global_concurrent` | `10` | 1-100 | Total concurrent operations across all jobs |
| `max_per_job` | `5` | 1-50 | Max concurrent within a single bulk job |
| `max_per_ap` | `3` | 1-20 | Max concurrent STAs per AP during fanout |
| `max_retries` | `3` | 0-10 | Retries for transient errors (timeout, 502/503/504) |
| `initial_backoff` | `2s` | - | Initial retry delay |
| `max_backoff` | `60s` | - | Maximum retry delay |
| `backoff_multiplier` | `2.0` | - | Exponential backoff multiplier |

Bulk operation stats available via `GET /api/wavecontrol/bulk-ops/stats`.

### Management IP Prefixes (airMAX IP Filtering)

airMAX devices have a known issue where they report unreliable IP addresses. Instead of reporting the configured management IP, airMAX devices report the "last seen" IP from wireless frames - which could be a customer's LAN IP (192.168.x.x), a DHCP lease from the wrong network, or literally any IP address that happens to traverse the radio link.

This makes airMAX device IPs untrustworthy for:
- **Device identification** - matching devices between poll cycles
- **Remote UI access** - opening the device's web interface from waveControl
- **Configuration changes** - pushing settings via the API
- **Firmware upgrades** - uploading and applying firmware updates
- **Backup/restore** - retrieving or pushing device configurations

**Management IP Prefixes** solves this by filtering which IPs are learned and stored. Only IP addresses that fall within your configured management network ranges are accepted - everything else is ignored.

| Setting | Default | Description |
|---------|---------|-------------|
| `management_prefixes` | `[]` | JSON array of CIDR ranges (one per line in UI) |

**Configure in Settings > Management IP Prefixes**

Enter one or more CIDR prefixes (one per line). Multiple ranges are fully supported:
```
172.24.0.0/16
10.0.0.0/8
192.168.100.0/24
```

**Behavior:**

| Prefixes Configured | Reported IP | Result |
|---------------------|-------------|--------|
| None (empty) | Any | [Y] Accepted (default, all IPs learned) |
| `172.24.0.0/16` | `172.24.34.24` | [Y] Accepted (matches prefix) |
| `172.24.0.0/16` | `192.168.1.50` | [N] Ignored (customer LAN - garbage) |
| `172.24.0.0/16` | `10.0.0.1` | [N] Ignored (wrong network) |
| `172.24.0.0/16, 10.0.0.0/8` | `10.0.0.1` | [Y] Accepted (matches second prefix) |

**How it works:**

- **New devices**: Only inserted into database if IP matches at least one configured prefix
- **Existing devices**: IP only updated if new IP matches a prefix; otherwise existing (valid) IP is preserved
- **No prefixes configured**: All IPs accepted (backward compatible, but not recommended for airMAX)

**IP-Based Device Matching (airMAX only):**

When management prefixes are enabled, waveControl can use IP addresses as a secondary identifier for airMAX devices. This is particularly useful when:
- The AP doesn't report MAC addresses for connected STAs
- MAC address is temporarily unavailable during a poll cycle

**Important**: IP-based matching is *only* enabled for airMAX devices *and only* when management prefixes are configured. This ensures IPs used for matching are trustworthy management addresses, not garbage. MAC address remains the authoritative identifier in all cases.

**Benefits:**

1. **Reliable management operations**: Devices are always reachable at their stored IP for upgrades, config changes, and remote UI access
2. **Stable device matching**: When prefixes are enabled, IP can be used as a fallback identifier for airMAX devices when MAC is unavailable
3. **Filter out garbage**: Customer LAN IPs, random DHCP leases, and other spurious IPs are ignored
4. **Clean database**: No more duplicate device entries caused by IP changes

**Recommended setup:**

Define all your management network ranges:
```
172.16.0.0/12    # Primary management backbone
10.255.0.0/16   # Out-of-band management network
192.168.200.0/24 # Tower management VLAN
```

**Note**: You should configure *all* networks where your devices may have valid management IPs. Devices with IPs outside these ranges will either keep their existing IP (if already in database) or be skipped entirely (if new).

See [SPEC.md](SPEC.md#management-ip-prefix-filter) for implementation details.

See [Security & Privileges](#security--privileges) for how relative paths are resolved.

Environment variables:
- `WAVECONTROL_DSN` — required PostgreSQL connection string
- `WAVECONTROL_JWT_SECRET` — required persistent session-signing key
- `WAVECONTROL_DATA_KEY` — required persistent 32-byte AES key, base64 encoded
- `WAVECONTROL_BOOTSTRAP_USERNAME` and `WAVECONTROL_BOOTSTRAP_PASSWORD` — required only while creating the first user in an empty database

## API Endpoints

### Authentication
- `POST /api/wavecontrol/auth/login` — verifies credentials and sets a Secure/HttpOnly/SameSite session cookie
- `POST /api/wavecontrol/auth/logout` — revokes the current user's sessions and clears the cookie
- `GET /api/wavecontrol/me` — returns the current user

Protected browser API requests use the HttpOnly cookie. State-changing requests must be same-origin and include `X-WaveControl-CSRF: 1`; the JavaScript client adds this header automatically.

### Devices
- `GET /api/wavecontrol/devices` - List all devices with live stats
- `POST /api/wavecontrol/devices` - Add single device
- `POST /api/wavecontrol/devices/bulk-add` - Add multiple APs
- `GET /api/wavecontrol/devices/{id}` - Get device with full stats
- `DELETE /api/wavecontrol/devices/{id}` - Delete device
- `POST /api/wavecontrol/devices/{id}/refresh` - Force poll
- `POST /api/wavecontrol/devices/{id}/reboot` - Immediate platform-aware radio reboot

### Stats (real-time from memory)
- `GET /api/wavecontrol/stats` - All device stats
- `GET /api/wavecontrol/stats/{ip}` - Single device stats

### Firmware
- `GET /api/wavecontrol/firmware` - List available firmware files
- `POST /api/wavecontrol/firmware` - Upload firmware (editor, max 1GB)
- `DELETE /api/wavecontrol/firmware/{name}` - Delete firmware (editor)
- `POST /api/wavecontrol/devices/{id}/upgrade` - Upgrade single device
- `POST /api/wavecontrol/devices/{id}/upgrade-fanout` - Upgrade AP + all STAs
- `POST /api/wavecontrol/devices/bulk-upgrade` - Upgrade multiple devices

### Settings
- `GET /api/wavecontrol/settings` - All settings
- `PATCH /api/wavecontrol/settings` - Atomically update a settings form
- `PATCH /api/wavecontrol/settings/{key}` - Update one setting

## Supported Devices

### Wave Platform
- Wave AP (MGMP flavor)
- Wave Long-Range (GMC flavor)
- Wave Pro (GMP flavor)

### LTU Platform
- LTU-Rocket (AFLTUROCKET flavor)
- LTU, LTU-LR (AFLTU flavor)

## Stats Available

### From AP (device stats)
- CPU usage, RAM, temperatures
- GPS coordinates
- Radio stats (frequency, power, capacity)
- Interface stats (eth0, wlan0, etc.)

### From AP (per-STA stats via peers[])
- Signal level (per-chain for 5GHz/LTU)
- Distance
- MCS index
- Airtime utilization
- Link scores
- Traffic rates
- CINR (LTU only)

## Project Structure

```
wavecontrol/
+-- Makefile              # GNU make/BSD make build, install, and service workflow
+-- wavecontrol.env.example # Canonical cross-platform environment template
+-- cmd/server/
|   +-- main.go        # HTTP server, poller startup
|   +-- api.go         # REST API handlers
|   +-- discovery.go   # Device discovery via Wave API
+-- internal/
|   +-- stats/store.go    # In-memory stats store
|   +-- poller/poller.go  # Background device polling
|   +-- firmware/service.go  # Upgrade logic
|   +-- wave/client.go    # Wave API client
+-- web/
|   +-- index.html     # SPA HTML
|   +-- css/styles.css # Dark/light theme
|   +-- js/            # Frontend modules
+-- rc.d/wavecontrol          # OpenBSD rc.d script
+-- systemd/wavecontrol.service # Linux systemd unit
+-- windows/build.ps1          # Complete Windows packager
+-- windows/WaveControl.proj   # Optional MSBuild entry point
+-- schema.sql                 # PostgreSQL schema
+-- go.mod
```

## License

MIT License

Copyright (c) 2025 Chris Cappuccio

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.

## Zabbix Integration

Enable the Zabbix bridge via Settings page or directly in database:

```sql
UPDATE settings SET value = 'true' WHERE key = 'zabbix_enabled';
UPDATE settings SET value = '0.0.0.0:10050' WHERE key = 'zabbix_listen';
UPDATE settings SET value = '10.0.0.5,192.168.1.0/24' WHERE key = 'zabbix_allowed_hosts';
-- Restart required
```

### Security

**Always configure `zabbix_allowed_hosts`** to restrict which IPs can query device data. The setting accepts comma-separated IPs, CIDRs, or hostnames:

```
10.0.0.5                    # Single Zabbix server IP
10.0.0.5,10.0.0.6           # Multiple servers
192.168.1.0/24              # CIDR block
zabbix.example.com          # Hostname (resolved at startup)
```

If `zabbix_allowed_hosts` is empty, all connections are accepted (a warning is logged at startup).

**Note:** The Zabbix agent protocol has no encryption. If you need encrypted transport:
1. Bind to `127.0.0.1` and use SSH tunneling from the Zabbix server
2. Use a VPN between the Zabbix server and waveControl host

### Zabbix Item Keys

**Important:** 60GHz and 5GHz signals are ALWAYS kept separate - never combined.

```
# Discovery (LLD)
wavecontrol.discovery

# Device metrics
wavecontrol.device[192.168.1.1,uptime]
wavecontrol.device[192.168.1.1,radio60.capacity]
wavecontrol.device[192.168.1.1,peer_count]

# Peer (STA) signal metrics - RADIO SPECIFIC (never combined)

# 60GHz (Wave primary radio)
wavecontrol.peer[192.168.1.1,AA:BB:CC:DD:EE:FF,signal.60ghz]          # combined
wavecontrol.peer[192.168.1.1,AA:BB:CC:DD:EE:FF,signal.60ghz.chain0]
wavecontrol.peer[192.168.1.1,AA:BB:CC:DD:EE:FF,signal.60ghz.chain1]
wavecontrol.peer[192.168.1.1,AA:BB:CC:DD:EE:FF,signal.60ghz.combined] # calculated from chains

# 5GHz (unified: Wave backup, airMAX AC, LTU)
wavecontrol.peer[192.168.1.1,AA:BB:CC:DD:EE:FF,signal.5ghz]           # combined
wavecontrol.peer[192.168.1.1,AA:BB:CC:DD:EE:FF,signal.5ghz.chain0]
wavecontrol.peer[192.168.1.1,AA:BB:CC:DD:EE:FF,signal.5ghz.chain1]
wavecontrol.peer[192.168.1.1,AA:BB:CC:DD:EE:FF,signal.5ghz.combined]  # calculated from chains

# Signal combination formula: 10 * log10(10^(c0/10) + 10^(c1/10))
# Example: -63/-63 chains = -60 combined (3dB MRC gain)

# Other peer metrics
wavecontrol.peer[192.168.1.1,AA:BB:CC:DD:EE:FF,cinr.dl]
wavecontrol.peer[192.168.1.1,AA:BB:CC:DD:EE:FF,cinr.ul]
wavecontrol.peer[192.168.1.1,AA:BB:CC:DD:EE:FF,distance]

# Counts
wavecontrol.count[online]
wavecontrol.count[offline]
```

### LLD Macros

- `{#IP}` - Device IP
- `{#MAC}` - Device MAC
- `{#HOSTNAME}` - Device name
- `{#PLATFORM}` - wave/ltu
- `{#ISAP}` - 1 for AP, 0 for STA

## Supported Device Flavors

### Wave Platform
| Flavor | Device |
|--------|--------|
| `GMC` | Wave Long-Range |
| `GMP` | Wave Pro |
| `MGMP` | Wave AP |

### LTU/AirFiber Platform
| Flavor | Device Type |
|--------|-------------|
| `AFLTUROCKET` | LTU-Rocket (AP) |
| `AFLTU` | LTU, LTU-LR (AP/STA) |
| `AF5XHD` | airFiber 5XHD |

## Scheduled Jobs

Schedule firmware upgrades or device reboots from the Settings page.

### API

```bash
# Establish a cookie session.
curl -sS -c cookies.txt \
  -H 'Content-Type: application/json' \
  -d '{"username":"operator","password":"your-password"}' \
  http://localhost:8080/api/wavecontrol/auth/login

# List jobs.
curl -sS -b cookies.txt http://localhost:8080/api/wavecontrol/jobs

# Schedule an upgrade. All state-changing cookie requests need the CSRF header.
curl -sS -b cookies.txt -X POST \
  -H 'X-WaveControl-CSRF: 1' \
  -H 'Content-Type: application/json' \
  -d '{
    "job_type": "upgrade",
    "device_ids": [1, 2, 3],
    "parameters": {"force": false, "fanout": true},
    "scheduled_at": "2026-08-20T03:00:00Z",
    "repeat_cron": "@daily"
  }' \
  http://localhost:8080/api/wavecontrol/jobs

# Cancel a job.
curl -sS -b cookies.txt -X DELETE \
  -H 'X-WaveControl-CSRF: 1' \
  http://localhost:8080/api/wavecontrol/jobs/123
```

### Repeat Options
- Empty: One-time job
- `@hourly`, `@daily`, `@weekly`: Standard intervals
- Duration: `30m`, `1h`, `24h` etc.

## WebSocket

Connect to `/api/wavecontrol/ws` for real-time updates. The connection receives:
- `stats_update`: Device stats changed
- `device_update`: Device added/removed
- `job_update`: Scheduled job status change

```javascript
const ws = new WebSocket('ws://localhost:8080/api/wavecontrol/ws');
ws.onmessage = (event) => {
  const msg = JSON.parse(event.data);
  console.log(msg.type, msg.data);
};
```

## Supported Platforms

| Platform | API Type | Login | Status Endpoint | Features |
|----------|----------|-------|-----------------|----------|
| Wave 60GHz (GMC/GMP/MGMP) | REST JSON | `/api/v1.0/user/login` | `/api/v1.0/statistics` | Full stats, peers, GPS |
| Wave MLO (MW) | REST JSON | `/api/v1.0/user/login` | `/api/v1.0/statistics` | Full stats, peers, GPS |
| AirFiber 60 (GP) | REST JSON | `/api/v1.0/user/login` | `/api/v1.0/statistics` | Full stats, peers, GPS |
| LTU (AFLTU/AFLTUROCKET) | REST JSON (Wave API) | `/api/v1.0/user/login` | `/api/v1.0/statistics` | Full stats, peers, GPS |
| AirFiber 5XHD (AF5XHD) | REST JSON (LTU API) | `/api/v1.0/user/login` | `/api/v1.0/statistics` | Full stats, peers, GPS |
| airMAX AC (XC/WA) | Form/CGI | `/login.cgi` | `/status.cgi` | Full stats, STAs, GPS (AirOS 8) |
| airMAX M (XM/XW) | Form/CGI | `/login.cgi` | `/status.cgi` | Full stats, STAs, GPS (AirOS 5) |

**Note:** LTU and AirFiber 5XHD devices use the same REST API as Wave devices (`/api/v1.0/*`). AirFiber 60 uses the Wave API with 60GHz radio mapping.

### Auto-Detection

When adding a device, waveControl automatically detects the platform:
1. First tries Wave/LTU API (`/api/v1.0/user/login`)
2. Falls back to airMAX API (`/login.cgi` + `/status.cgi`)

You can also specify the platform explicitly when adding via the `platform` field.

### airMAX Features

For airMAX devices, waveControl extracts:
- **Device Info**: Hostname, model, firmware, uptime, temperature
- **Wireless**: SSID, frequency, channel width, TX power, noise floor
- **Connected Stations**: MAC, IP, signal, distance, capacity, remote device info
- **GPS**: Latitude, longitude, altitude, satellite count
- **Interfaces**: eth0 status, speed, cable length
- **AirMax Stats**: Capacity, priority, CINR, EVM data

### Firmware Platform Reference

| Prefix | AirOS | Platform | Devices |
|--------|-------|----------|---------|
| `XC.` | 8 | airMAX AC | Rocket 5AC, PowerBeam 5AC, LiteBeam 5AC, NanoStation 5AC, Prism, IsoStation, LiteAP |
| `2XC.` | 8 | airMAX AC | AC Gen2 variant |
| `WA.` | 8 | airMAX AC | AC variant |
| `2WA.` | 8 | airMAX AC | AC Gen2 variant |
| `XM.` | 5/6 | airMAX M | Rocket M5, NanoStation M5, Bullet M2, NanoBridge M5 |
| `XW.` | 5/6 | airMAX M | M series variant |
| `GMC.` | - | Wave | Wave AP (60GHz) |
| `GMP.` | - | Wave | Wave Pico (60GHz) |
| `MGMP.` | - | Wave | Wave Mega (60GHz) |
| `MW.` | - | Wave MLO | Wave MLO (5GHz/6GHz) |
| `GP.` | - | Wave (AF60) | AirFiber 60, AirFiber 60-LR (60GHz, uses Wave API) |
| `AFLTUROCKET.` | - | LTU | LTU Rocket (AP) |
| `AFLTU.` | - | LTU | LTU (STA) |
| `AF5XHD.` | - | LTU | airFiber 5XHD (uses LTU/Wave API) |
| `AF11.` | - | AirFiber | AirFiber 11 (future support) |
| `AF24.` | - | AirFiber | AirFiber 24 (future support) |
| `AF2X.` | - | AirFiber | AirFiber 2X (future support) |
| `AF3X.` | - | AirFiber | AirFiber 3X (future support) |
| `AF5.` | - | AirFiber | AirFiber 5 (future support) |
| `AF5X.` | - | AirFiber | AirFiber 5X (future support) |

### Example Firmware Files

```
# airMAX AC (AirOS 8)
XC.v8.7.8.46705.220201.1816.bin              # QCA955x chipset
2XC.v8.7.8.46705.220201.1820.bin             # Gen2 variant
WA.v8.7.8.46705.220201.1819.bin              # WA variant
2WA.v8.7.8.46705.220201.1816.bin             # Gen2 WA variant

# airMAX M (AirOS 5/6)
XM.v6.1.11.32949.190328.1126.bin             # AR7240/AR9342 chipset
XW.v6.3.0.bin                                # XW variant

# Wave 60GHz
GMC.ipq5018.v4.1.0.0edad4ab.251212.0922.bin  # Wave AP
GMP.ipq806x.v4.1.0.0edad4ab.251212.0923.bin  # Wave Pico
MGMP.ipq807x.v4.1.0.0edad4ab.251212.0923.bin # Wave Mega
MW.ipq53xx.v2.4.2.69fdca7b.251223.1612.bin   # Wave MLO
GP.af60.v2.1.0.bin                           # AirFiber 60

# LTU
aflturocket.amesoc3.v2.4.0.00017.250811.1108-squashfs.bin  # LTU Rocket
afltu.amesoc3.v2.3.4.00007.240613.1106-squashfs.bin        # LTU STA
af5xhd.amesoc3.v2.3.4.00007.240613.1106-squashfs.bin       # airFiber 5XHD

# AirFiber (future support)
AF5X.v4.1.0.bin                              # AirFiber 5X
AF24.v4.1.0.bin                              # AirFiber 24
```

Place firmware files in the configured firmware directory (default: `firmware/` relative to working directory).

waveControl auto-detects the firmware platform from the filename prefix and extracts the version number. Version matching handles both old formats (`GMC.v1.2.3.bin`) and new formats with SoC names (`GMC.ipq5018.v4.1.0.hash.date.bin`).


### Alert targeting

Alert rules support role targets (`all`, `ap`, `sta`) and can require the per-device `alertable` flag. Operators can mark individual devices alertable/not-alertable, temporarily silence them, add alert notes, and bulk-update selected devices from the dashboard. Auto-discovered STAs default to non-alertable; directly added/managed devices default to alertable.
