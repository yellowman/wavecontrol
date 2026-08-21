# Running WaveControl on a Windows host

WaveControl on Windows is the same Ubiquiti network-management application as on Linux, OpenBSD, or FreeBSD. Windows is only the **host operating system** for the WaveControl server; no Windows endpoint-monitoring capability is added.

## Requirements

- 64-bit Windows 10, Windows 11, or Windows Server 2019 or later
- Go 1.21 or later for source builds
- Windows PowerShell 5.1 or PowerShell 7 for the primary package workflow
- Optional: the .NET SDK or Visual Studio Build Tools for the MSBuild entry point
- PostgreSQL 14 or later, local or reachable over the network
- Network reachability from the Windows host to the Ubiquiti management addresses

The server is pure Go and builds with `CGO_ENABLED=0`. PostgreSQL remains an external service; it is not embedded in the executable.

## Build a complete Windows directory

The primary Windows build entry point is the PowerShell packager. From PowerShell at the repository root:

```powershell
Set-ExecutionPolicy -Scope Process Bypass
.\windows\build.ps1
```

The default output is `dist\windows-amd64`. It contains the executable, web assets, schema, migrations, documentation, empty firmware/backup directories, the run script, an executable SHA-256 file, `wavecontrol.env.example`, and an initial `wavecontrol.env`. The active environment file is therefore ready to edit immediately; no manual copy step is required.

To build ARM64 Windows instead:

```powershell
.\windows\build.ps1 -Architecture arm64
```

`build.ps1` runs `go test ./...` before building. `-SkipTests` exists for packaging diagnostics only and should not be used for a release build. To update an existing package directory while preserving its populated environment file, firmware, and backups:

```powershell
.\windows\build.ps1 -KeepExisting
```

The builder replaces packaged web assets and migrations even with `-KeepExisting`, preventing stale files from surviving an upgrade.

### Optional MSBuild project

`windows\WaveControl.proj` is a normal MSBuild orchestration project for build agents, Visual Studio Build Tools, or developers who prefer a project-builder interface. It deliberately calls the same PowerShell packager so both workflows produce identical layouts.

```powershell
# With the .NET SDK
dotnet msbuild .\windows\WaveControl.proj /t:Package /p:Architecture=amd64

# With Visual Studio Build Tools
msbuild .\windows\WaveControl.proj /t:Package /p:Architecture=arm64
```

Supported properties include:

- `/p:SkipTests=true`
- `/p:KeepExisting=true`
- `/p:OutputDirectory=C:\WaveControlBuild`
- `/p:PowerShellExe=pwsh.exe`

The `Clean` target removes the selected output directory, and the `Test` target runs `go test ./...` directly.

## Initialize PostgreSQL

Create the user and database, then load `schema.sql` with the PostgreSQL `psql.exe` client. One example from a PostgreSQL command prompt is:

```powershell
createuser.exe -h 127.0.0.1 -U postgres --pwprompt wavecontrol
createdb.exe -h 127.0.0.1 -U postgres -O wavecontrol wavecontrol
psql.exe -h 127.0.0.1 -U wavecontrol -d wavecontrol -f .\schema.sql
```

Use a strong database password and place it in the DSN in `wavecontrol.env`. Existing installations are upgraded transactionally by WaveControl's startup schema checks.

## Configure and run

Inside the built directory, edit the automatically installed active environment file and run the launcher:

```powershell
notepad .\wavecontrol.env
.\run-wavecontrol.ps1
```

If the active file is missing but `wavecontrol.env.example` is present, the launcher creates it and stops with an instruction to edit it. The parser accepts the canonical sample's matching single or double quotes and rejects untouched placeholder values.

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

The run script validates the DSN, JWT secret, and exact 256-bit data key before launch. It starts WaveControl in the foreground and explicitly supplies executable-relative paths. This avoids the common Windows behavior where Explorer, Task Scheduler, or a service wrapper starts an executable with `C:\Windows\System32` as its working directory.

## Automatic startup

WaveControl remains a normal foreground console process on Windows. Run `run-wavecontrol.ps1` under a dedicated, non-administrator account using Task Scheduler or a service wrapper that captures standard output/error. Configure automatic restart on failure. The wrapper's working directory is not relied on because the run script passes `-workdir` and an absolute `-webroot`.

## Firewall and reverse proxy

The default UI listener is `127.0.0.1:8080`. Keep it loopback-only when a reverse proxy terminates HTTPS on the same host. To listen on a LAN address, change the `listen_addr` admin setting or pass `-ListenAddress` to the run script, and create a narrowly scoped Windows Firewall rule.

WaveControl needs outbound management access to the Ubiquiti devices, PostgreSQL, and any configured alert destinations. The sysmon-web integration uses outbound TLS to its agent listener, normally TCP 1347.

## Upgrade procedure

For an in-place package directory created by the build script:

1. Stop the scheduled task or service wrapper.
2. Back up PostgreSQL and the complete WaveControl directory.
3. From the updated source tree, rebuild into that directory with `-KeepExisting`:

   ```powershell
   .\windows\build.ps1 -KeepExisting -OutputDirectory C:\WaveControl
   ```

4. Confirm that `wavecontrol.env`, `firmware`, and `backups` are still present and retain their ACLs.
5. Start WaveControl and verify `/health`, login, polling, and alert-delivery readiness.

When deploying a separately built package, replace the executable, web assets, migrations, schema, documentation, and run script while preserving `wavecontrol.env`, `firmware`, `backups`, `WAVECONTROL_JWT_SECRET`, and `WAVECONTROL_DATA_KEY`.
