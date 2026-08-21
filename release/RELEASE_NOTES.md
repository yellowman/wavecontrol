# WaveControl v145: portable installation, Windows host, expanded alerts, and sysmon-web delivery

Release date: 2026-08-20  
Base commit: `0e3cb186d869e63a74ed69694a9d07cffbc94ea5`

## Scope boundary

Windows support means that the WaveControl server can be built and run on a Windows host. It does **not** add Windows endpoint monitoring. WaveControl continues to poll and alert on Ubiquiti APs and subscriber radios only.

## Portable build and installation workflow

- Root `Makefile` works with GNU make on Linux and BSD make on OpenBSD without GNU-only make functions.
- Normal workflow is `make`, privileged `make install`, edit `/etc/wavecontrol/wavecontrol.env`, then `make enable start`.
- The installer creates the `_wavecontrol` account when absent, installs runtime assets and the native service definition, supports `DESTDIR`, and preserves configuration/firmware/backups during upgrades.
- `wavecontrol.env.example` is the canonical cross-platform sample. Unix installation creates the active file only when absent; `make env` does the same for development.
- Windows packaging now creates the active environment file automatically and preserves it with `-KeepExisting`.
- `windows/WaveControl.proj` provides an optional MSBuild entry point while delegating to the same PowerShell packager.

## Windows hosting

- Native `windows/amd64` and `windows/arm64` builds with `CGO_ENABLED=0`.
- Executable-relative web-asset discovery when launched from Explorer, Task Scheduler, or a service wrapper.
- Explicit `-workdir` and `-webroot` support plus `WAVECONTROL_WORKDIR` and `WAVECONTROL_WEBROOT` overrides.
- No Windows-only working-directory mutation in privilege setup.
- `windows/build.ps1` runs tests, builds a complete deployable directory, copies web/schema/migrations/docs, creates runtime directories, and hashes the executable.
- `windows/run-wavecontrol.ps1` loads and validates a local environment file, validates persistent secrets, and starts the server with absolute paths.
- Windows/PostgreSQL setup and upgrade instructions are in `docs/WINDOWS_HOST.md`.

## Alert lifecycle

Each rule/device pair has one durable alert occurrence:

1. The metric must remain true for the configured persistence delay.
2. WaveControl creates an in-app alert and durable per-channel outbox records in one transaction.
3. Acknowledging an alert records operator attention but does not claim the condition is fixed.
4. Recovery resolves active or acknowledged alerts automatically.
5. Optional recovery notifications are emitted only to channels on which the trigger may have been delivered.
6. A post-trigger cooldown prevents immediate recreation after a condition clears; a continuing condition after manual resolution starts a fresh persistence period.
7. Disabling/editing a rule, changing device eligibility, or losing the applicable metric reconciles stale state and cancels obsolete queued notices.

The default Alerts view and navigation badge count all **open** alerts: active plus acknowledged but unresolved.

## Rule options

- Scope: fleet, site, or one device.
- Target role: AP, STA, or both.
- Optional “alertable devices only” policy gate.
- Persistence delay before triggering.
- Post-trigger cooldown after the occurrence.
- Severity: automatic, informational, warning, or critical.
- Optional external channels: email, webhook, Zabbix sender, and sysmon-web.
- Optional external recovery notification.

Automatic severity remains backward compatible for existing rules. The UI explains persistence versus cooldown, acknowledges versus resolves, channel readiness, and per-alert delivery status/errors.

## Ubiquiti alert metrics

- Offline duration
- 5 GHz, 6 GHz, 60 GHz, and LTU signal
- CPU, RAM, and temperature
- Capacity, peer count, and link score
- Radio interference
- RF chain imbalance
- GPS synchronization state

High-noise rules such as fleet-wide peer-count, STA-down, chain-imbalance, and GPS-loss checks remain manual templates rather than one-click recommended defaults.

## sysmon-web alerter delivery

Administrators configure sysmon-web under **Settings → Alert Delivery**:

- Enabled state
- Server host and agent port (default 1347)
- Minted alerter name and bearer token
- Application description
- Pinned sysmon-web certificate/CA PEM
- Connection test and live readiness status

WaveControl maintains a long-lived, pinned-TLS connection, authenticates with `ALERTER`, sends `ALERT CRITICAL`, `ALERT WARNING`, and recovery `ALERT OK`, sends idle `PING` keepalives, and reconnects with exponential backoff capped at one minute. The bearer token is encrypted at rest and never returned through the settings/status APIs.

The durable PostgreSQL outbox remains the delivery authority. sysmon delivery has a dedicated ordered worker so a sysmon outage cannot occupy the workers used by email, webhook, or Zabbix. sysmon notices remain retryable while relevant; other channels retain their bounded retry policy.

See `docs/SYSMON_WEB_ALERTER.md`, `docs/ALERTING.md`, and the supplied protocol reference under `release/reference/ALERTERS.md`.
