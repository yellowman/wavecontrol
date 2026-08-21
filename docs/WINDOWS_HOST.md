# Running WaveControl on a Windows host

WaveControl on Windows is the same Ubiquiti network-management application as on Linux, OpenBSD, or FreeBSD. Windows is only the **host operating system** for the WaveControl server; no Windows endpoint-monitoring capability is added.

## Requirements

- 64-bit Windows 10, Windows 11, or Windows Server 2019 or later
- Go 1.21 or later for source builds
- PostgreSQL 14 or later, local or reachable over the network
- Network reachability from the Windows host to the Ubiquiti management addresses

The server is pure Go and builds with `CGO_ENABLED=0`. PostgreSQL remains an external service; it is not embedded in the executable.

## Build a complete Windows directory

From PowerShell at the repository root:

```powershell
Set-ExecutionPolicy -Scope Process Bypass
.\windows\build.ps1
```

The default output is `dist\windows-amd64`. It contains the executable, `web` assets, schema, migrations, documentation, empty firmware/backup directories, the run script, and an executable SHA-256 file.

To cross-build ARM64 Windows instead:

```powershell
.\windows\build.ps1 -Architecture arm64
```

`build.ps1` runs `go test ./...` before building. `-SkipTests` exists for packaging only; it should not be used for a release build.

## Initialize PostgreSQL

Create the user and database, then load `schema.sql` with the PostgreSQL `psql.exe` client. One example from a PostgreSQL command prompt is:

```powershell
createuser.exe -h 127.0.0.1 -U postgres --pwprompt wavecontrol
createdb.exe -h 127.0.0.1 -U postgres -O wavecontrol wavecontrol
psql.exe -h 127.0.0.1 -U wavecontrol -d wavecontrol -f .\schema.sql
```

Use a strong database password and place it in the DSN in `wavecontrol.env`. Existing installations are upgraded transactionally by WaveControl's startup schema checks.

## Configure and run

Inside the built directory:

```powershell
Copy-Item .\wavecontrol.env.example .\wavecontrol.env
notepad .\wavecontrol.env
.\run-wavecontrol.ps1
```

Generate the two persistent secrets once and keep them unchanged across restarts:

```powershell
$rng = [Security.Cryptography.RandomNumberGenerator]::Create()
$bytes = New-Object byte[] 48
$rng.GetBytes($bytes)
[Convert]::ToBase64String($bytes)  # WAVECONTROL_JWT_SECRET

$bytes = New-Object byte[] 32
$rng.GetBytes($bytes)
[Convert]::ToBase64String($bytes)  # WAVECONTROL_DATA_KEY
$rng.Dispose()
```

Protect the populated environment file so only the service account and administrators can read it. For a dedicated local account named `wavecontrol-svc`:

```powershell
icacls .\wavecontrol.env /inheritance:r
icacls .\wavecontrol.env /grant:r 'Administrators:F' 'wavecontrol-svc:R'
```

The run script always starts WaveControl in the foreground and explicitly supplies executable-relative paths. This avoids the common Windows behavior where Explorer, Task Scheduler, or a service wrapper starts an executable with `C:\Windows\System32` as its working directory.

## Automatic startup

WaveControl remains a normal foreground console process on Windows. Run `run-wavecontrol.ps1` under a dedicated, non-administrator account using Task Scheduler or a service wrapper that captures standard output/error. Configure automatic restart on failure. The wrapper's working directory is not relied on because the run script passes `-workdir` and an absolute `-webroot`.

## Firewall and reverse proxy

The default UI listener is `127.0.0.1:8080`. Keep it loopback-only when a reverse proxy terminates HTTPS on the same host. To listen on a LAN address, change the `listen_addr` admin setting or pass `-ListenAddress` to the run script, and create a narrowly scoped Windows Firewall rule.

WaveControl needs outbound management access to the Ubiquiti devices, PostgreSQL, and any configured alert destinations. The sysmon-web integration uses outbound TLS to its agent listener, normally TCP 1347.

## Upgrade procedure

1. Stop the scheduled task/service wrapper.
2. Back up PostgreSQL and the WaveControl directory.
3. Replace `wavecontrol.exe` and `web` with the new package.
4. Preserve `wavecontrol.env`, `firmware`, `backups`, `WAVECONTROL_JWT_SECRET`, and `WAVECONTROL_DATA_KEY`.
5. Start WaveControl and verify `/health`, login, polling, and alert delivery readiness.
