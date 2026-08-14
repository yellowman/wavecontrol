# Ubiquiti Firmware Upgrade Tool Specification

## Overview

A command-line tool for batch firmware upgrades on Ubiquiti wireless devices, supporting single-device upgrades and fanout mode (upgrade an AP and all connected stations).

### Goals

1. **Idempotent operation** - Running the tool multiple times is safe; devices already at target version are skipped
2. **Minimal downtime** - Don't wait for reboots during batch operations
3. **Automatic firmware selection** - Given a directory, select correct firmware based on device platform/flavor
4. **Graceful handling** - Continue with other devices if one fails

---

## Supported Platforms

### Wave (60 GHz / 5 GHz)

| Flavor | Platform | Models |
|--------|----------|--------|
| MGMP | ipq807x | Wave-Pro, Wave AP, Wave AP Micro, Wave AP Gen2 |
| GMP | ipq806x | AF60-LR |
| GMC | ipq5018 | Wave Nano, Wave Pico, Wave LR |

**Firmware naming**: `{FLAVOR}.{platform}.{version}.{build}.{date}.{time}.bin`  
**Example**: `GMC.ipq5018.v4.1.0.0edad4ab.251212.0922.bin`

### AirMax (To be implemented)

| Flavor | Platform | Models |
|--------|----------|--------|
| TBD | TBD | Rocket, PowerBeam, NanoStation, etc. |

**Firmware naming**: TBD  
**Example**: TBD

---

## Authentication

### Wave API

- **Endpoint**: `POST /api/v1.0/user/login`
- **Request Body**: `{"username": "<user>", "password": "<pass>"}`
- **Response**: `x-auth-token` header contains session token
- **Usage**: All subsequent requests include `x-auth-token: <token>` header

### AirMax API

- **Endpoint**: TBD (likely `/api/auth` or cookie-based)
- **Request Body**: TBD
- **Response**: TBD

---

## Device Information Retrieval

### Wave API

**Device Info** - `GET /api/v1.0/device`

```json
{
  "identification": {
    "firmware": "GMC.ipq5018.v4.1.0.0edad4ab.251212.0922",
    "firmwareVersion": "4.1.0",
    "model": "Wave-LR",
    "product": "Wave Long-Range",
    "mac": "1c:6a:1b:41:ca:1d"
  },
  "capabilities": {
    "device": {
      "supportedFirmwares": [
        {"flavor": "GMC"}
      ]
    }
  }
}
```

**Key fields**:
- `identification.firmware` - Full firmware string for comparison
- `capabilities.device.supportedFirmwares[0].flavor` - Platform flavor (GMC, GMP, MGMP)

**Connected Stations** - `GET /api/v1.0/statistics`

```json
[{
  "wireless": {
    "peers": [{
      "common": {
        "mgmtIp": "172.24.61.43",
        "hostname": "Station Name",
        "identification": {
          "firmware": "GMC.ipq5018.v4.1.0.0edad4ab.251212.0922",
          "firmwareVersion": "4.1.0",
          "model": "Wave-LR",
          "mac": "1c:6a:1b:41:c5:6b"
        }
      }
    }]
  }
}]
```

**Key fields**:
- `wireless.peers[].common.mgmtIp` - Management IP for connecting to station
- `wireless.peers[].common.identification.firmware` - Full firmware string
- `wireless.peers[].common.hostname` - Display name

### AirMax API

TBD - Document equivalent endpoints for:
- Device info and current firmware
- Platform/flavor detection
- Connected stations list with IPs

---

## Version Comparison Logic

### Algorithm

```
target_firmware = strip_extension(firmware_filename)  # Remove .bin
current_firmware = device.identification.firmware

if lowercase(current_firmware) == lowercase(target_firmware):
    # Already at target version - skip
else:
    # Needs upgrade
```

### Rationale

The full firmware string (e.g., `GMC.ipq5018.v4.1.0.0edad4ab.251212.0922`) includes:
- Platform flavor
- SoC platform  
- Version number
- Git commit hash
- Build date/time

This is unique per build, so exact string match = same firmware. No version parsing required.

---

## Firmware Selection

### Directory Mode

When given a directory instead of a specific file:

1. Query device to get flavor (e.g., `GMC`)
2. List directory for files matching `{FLAVOR}[._]*.bin` (case-insensitive)
3. If multiple matches, select newest by filename (reverse alphabetical sort)
4. Error if no matching firmware found

### Fanout Mode Selection

For each station:
1. Extract flavor from station's `identification.firmware` string
2. If flavor differs from AP, search firmware directory for matching flavor
3. Skip station if no matching firmware available

---

## Upgrade Workflow

### Single Device

```
1. LOGIN
   - Authenticate to device
   - Store session token

2. GET DEVICE INFO
   - Query /device endpoint
   - Extract: current_firmware, flavor, model

3. SELECT FIRMWARE (if directory mode)
   - Find firmware file matching device flavor

4. VERSION CHECK
   - Compare current_firmware vs target filename (minus .bin)
   - If match and not --force: exit "already at target version"

5. CHECK PENDING UPGRADE
   - Query upgrade status endpoint
   - If upgrade already pending with SAME_VERSION and not --force: exit
   - If upgrade pending (different version): proceed to reboot
   - If upgrade in progress: wait for completion

6. UPLOAD FIRMWARE
   - POST multipart form-data with firmware file
   - Poll status until: finished, failed, or timeout

7. VERIFY UPLOAD
   - Check for SAME_VERSION warning
   - If SAME_VERSION and not --force: exit "already at target version"

8. TRIGGER REBOOT (unless --no-reboot)
   - POST reboot command
   - Exit immediately (don't wait for device to come back)

9. POST-REBOOT VERIFICATION (unless --no-verify)
   - Wait for device to respond
   - Re-authenticate
   - Verify firmware version changed
```

### Fanout Mode (AP + Stations)

```
1. LOGIN TO AP
   - Authenticate to AP device

2. GET AP INFO
   - Query device info
   - Query statistics for connected stations

3. SELECT FIRMWARE
   - Determine AP's target firmware

4. VERSION CHECK (AP)
   - Check if AP already at target
   - Set flag but don't exit (need to process stations first)

5. FOR EACH STATION:
   a. Determine station's flavor
   b. Find matching firmware file
   c. Compare station's current firmware vs target
   d. If already at target: skip, increment skip counter
   e. If needs upgrade:
      - Spawn subprocess to upgrade station
      - Use --no-verify (don't wait for reboot)
      - Track success/failure

6. PRINT STATION SUMMARY
   - "X upgraded, Y already current, Z failed"

7. UPGRADE AP (if needed)
   - If AP already at target and not --force: exit
   - Otherwise: upload, reboot (with --no-verify in fanout mode)
```

### Why Stations First

1. Stations can still reach AP during their upgrade
2. AP provides network path for firmware upload to stations
3. After AP reboots, stations may be temporarily unreachable

---

## API Endpoints Summary

### Wave

| Operation | Method | Endpoint |
|-----------|--------|----------|
| Login | POST | `/api/v1.0/user/login` |
| Device Info | GET | `/api/v1.0/device` |
| Statistics (STAs) | GET | `/api/v1.0/statistics` |
| Upgrade Status | GET | `/api/v1.0/system/upgrade` |
| Upload Firmware | POST | `/api/v1.0/system/upgrade/direct` |
| Reboot | POST | `/api/v1.0/system/reboot` |
| Health Check | GET | `/api/v1.0/public/ping` |

### AirMax

| Operation | Method | Endpoint |
|-----------|--------|----------|
| Login | TBD | TBD |
| Device Info | TBD | TBD |
| Statistics (STAs) | TBD | TBD |
| Upgrade Status | TBD | TBD |
| Upload Firmware | TBD | TBD |
| Reboot | TBD | TBD |

---

## Command Line Interface

```
Usage: ubfwupgrade [options] <host> <firmware.bin|firmware_dir>

Arguments:
  host              Device IP address or hostname
  firmware          Specific .bin file OR directory containing firmware files

Options:
  --username=USER   Login username (default: platform-specific)
  --password=PASS   Login password
  --http            Use HTTP instead of HTTPS
  --timeout=SEC     HTTP timeout in seconds (default: 30)
  
  --no-reboot       Upload firmware but don't trigger reboot
  --no-verify       Don't wait for reboot and verify upgrade applied
  --reboot-wait=SEC Max seconds to wait for device after reboot (default: 180)
  
  --fanout          Upgrade all connected stations, then the AP
  --force           Re-upload even if already at target version
  
  --info            Query device info only, don't upgrade
  --verbose         Verbose output
  --help            Show help
```

### Exit Codes

| Code | Meaning |
|------|---------|
| 0 | Success (upgraded or already at target) |
| 1 | Failure (auth, upload, verification, timeout) |

---

## Error Handling

### Authentication Failures
- Try multiple passwords from configured list
- Exit with clear error if all fail

### Network Errors
- Timeout on upload: fail with actionable message
- Connection reset during reboot: treat as success (expected)

### Firmware Errors
- Invalid/corrupt firmware: fail after verification
- Wrong platform: warn but proceed (device will reject)

### Fanout Errors
- Station failure: log and continue with other stations
- Track success/fail counts in summary

---

## Implementation Notes

### Platform Abstraction

To support multiple platforms (Wave, AirMax), abstract:

```
interface DeviceAPI {
    login(host, username, password) -> token
    getDeviceInfo(token) -> {firmware, flavor, model}
    getConnectedStations(token) -> [{ip, firmware, hostname}]
    getUpgradeStatus(token) -> {status, warnings[]}
    uploadFirmware(token, file) -> ok/error
    reboot(token) -> ok/error
    ping(host) -> ok/error
}

class WaveAPI implements DeviceAPI { ... }
class AirMaxAPI implements DeviceAPI { ... }
```

### Firmware Filename Patterns

```
Wave:   {FLAVOR}.{platform}.v{version}.{hash}.{date}.{time}.bin
AirMax: TBD
```

Pattern should be configurable per platform for directory scanning.

---

## Future Enhancements

1. **Parallel fanout** - Upgrade multiple stations concurrently
2. **Dry-run mode** - Show what would be upgraded without doing it
3. **Config file** - Store credentials and default options
4. **Progress display** - Show overall progress during batch upgrades
5. **Rollback support** - If available via API
6. **Firmware download** - Fetch from Ubiquiti servers if not local
