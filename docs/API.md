# waveControl API Reference

## Authentication

All API endpoints (except `/auth/login` and `/ping`) require a Bearer token.

```
Authorization: Bearer <token>
```

### POST /api/wavecontrol/auth/login

Login and receive JWT token.

**Request:**
```json
{
  "username": "admin",
  "password": "password"
}
```

**Response:**
```json
{
  "token": "eyJhbG...",
  "user": { "id": 1, "username": "admin", "roles": ["administrator"] }
}
```

---

## Devices

### GET /api/wavecontrol/devices

List all devices with live stats merged.

**Response:**
```json
[
  {
    "id": 1,
    "mac": "AA:BB:CC:DD:EE:FF",
    "ip_address": "192.168.1.100",
    "hostname": "Wave-AP-1",
    "product": "Wave AP",
    "model": "Wave-AP",
    "platform": "wave",
    "flavor": "MGMP",
    "firmware": "2.0.0-rc2+12345.abcdef",
    "firmware_version": "2.0.0-rc2",
    "site_id": 1,
    "site_name": "Tower Alpha",
    "region_name": "Portland",
    "online": true,
    "uptime": 86400,
    "peer_count": 5,
    "signal_60ghz": -58,
    "signal_5ghz": -65,
    "capacity_60ghz": 1200000000
  }
]
```

### POST /api/wavecontrol/devices

Add a single device.

**Request:**
```json
{
  "ip": "192.168.1.100",
  "username": "ubnt",
  "password": "ubnt",
  "site_id": 1
}
```

### POST /api/wavecontrol/devices/bulk-add

Add multiple devices.

**Request:**
```json
{
  "ips": ["192.168.1.100", "192.168.1.101", "192.168.1.102"],
  "username": "ubnt",
  "password": "ubnt",
  "site_id": 1
}
```

### DELETE /api/wavecontrol/devices/{id}

Delete a device.

### POST /api/wavecontrol/devices/{id}/refresh

Force poll a device immediately.

### POST /api/wavecontrol/devices/{id}/reboot

Immediately send a reboot command to the remote radio. Requires editor/admin permission. Unlike scheduled reboot jobs, this endpoint is a direct operator action used by the device detail pane.

The server dispatches by the device's stored platform/flavor:

| Platform / flavor | Reboot API |
|---|---|
| Wave, Wave MLO, AirFiber 60 (`wave`, `GMC`, `GMP`, `MGMP`, `MW`, `GP`) | `POST /api/v1.0/system/reboot` with `x-auth-token` |
| LTU / AirFiber 5XHD (`ltu`, `AFLTUROCKET`, `AFLTU`, `AF5XHD`) | `POST /api/v1.0/system/reboot` with airMAX `/reboot.cgi` fallback |
| airMAX AC/M and legacy AirFiber (`airmax`, `airfiber`, `XC`, `WA`, `XM`, `XW`, `AF11`, etc.) | `POST /reboot.cgi` with `reboot=yes` |

**Response:**
```json
{
  "device_id": 1,
  "ip_address": "192.168.1.100",
  "mac": "aa:bb:cc:dd:ee:ff",
  "hostname": "Tower AP",
  "platform": "airmax",
  "flavor": "XC",
  "api": "airmax_cgi",
  "status": "rebooting",
  "message": "reboot initiated"
}
```

### GET /api/wavecontrol/devices/{id}/identity-mismatch

Return the persisted identity mismatch for a device whose current poll result reported a different MAC address than inventory expected. This is used by the device detail pane before adopting replacement AP or directly managed STA hardware.

**Response:**
```json
{
  "device_id": 1,
  "expected_mac": "aa:bb:cc:dd:ee:ff",
  "observed_macs": ["11:22:33:44:55:66"],
  "observed_ip": "192.168.1.100",
  "source": "wave",
  "observed_at": "2026-05-20T16:30:00Z",
  "last_error": "wave mac mismatch: expected=aa:bb:cc:dd:ee:ff observed=11:22:33:44:55:66"
}
```

### POST /api/wavecontrol/devices/{id}/learn-mac

Explicitly adopt a replacement AP or directly managed STA MAC after `status_reason=mac_mismatch`. Requires editor/admin permission. The server verifies that the requested MAC was recently observed at the device IP and is not already another device row. AP replacements update child `parent_mac` references; STA replacements keep the AP association unchanged. The server then clears the mismatch record, writes changelog, and queues a refresh.

**Request:**
```json
{
  "new_mac": "11:22:33:44:55:66",
  "reason": "ap_replaced"
}
```

For directly managed STA replacements, send `"reason": "sta_replaced"`; if omitted, the server defaults the reason from the device role.

**Response:**
```json
{
  "ok": true,
  "device_id": 1,
  "old_mac": "aa:bb:cc:dd:ee:ff",
  "new_mac": "11:22:33:44:55:66",
  "ip_address": "192.168.1.100",
  "hostname": "Wave-AP-1",
  "role": "ap",
  "child_parent_refs_updated": 5
}
```

Conflict responses include `identity_mismatch_not_recorded`, `device_not_in_mac_mismatch`, `identity_mismatch_stale`, `identity_mismatch_ip_changed`, and `observed_mac_already_exists`.

---

## Sites

### GET /api/wavecontrol/sites

List all sites with device counts.

**Response:**
```json
[
  {
    "id": 1,
    "name": "Tower Alpha",
    "region_id": 1,
    "region_name": "Portland",
    "address": "123 Tower Rd",
    "gps_lat": 45.5231,
    "gps_lon": -122.6765,
    "device_count": 15
  }
]
```

### POST /api/wavecontrol/sites

Create a site.

**Request:**
```json
{
  "name": "Tower Alpha",
  "region_id": 1,
  "address": "123 Tower Rd",
  "gps_lat": 45.5231,
  "gps_lon": -122.6765,
  "notes": "Main downtown tower"
}
```

### PATCH /api/wavecontrol/sites/{id}

Update a site.

### DELETE /api/wavecontrol/sites/{id}

Delete a site (devices set to null site).

---

## Regions

### GET /api/wavecontrol/regions

List all regions with site counts.

**Response:**
```json
[
  {
    "id": 1,
    "name": "Portland",
    "parent_id": 2,
    "parent_name": "Oregon",
    "site_count": 5
  }
]
```

### POST /api/wavecontrol/regions

Create a region.

**Request:**
```json
{
  "name": "Portland",
  "parent_id": 2
}
```

### PATCH /api/wavecontrol/regions/{id}

Update a region.

### DELETE /api/wavecontrol/regions/{id}

Delete a region (sites set to null region).

---

## Firmware

### GET /api/wavecontrol/firmware

List available firmware files.

**Response:**
```json
[
  {
    "filename": "WA.v2.0.0-rc2.bin",
    "platform": "wave",
    "version": "2.0.0-rc2",
    "size": 15728640,
    "modified": "2024-01-15T10:30:00Z"
  }
]
```

### POST /api/wavecontrol/devices/{id}/upgrade

Upgrade device firmware.

**Request:**
```json
{
  "firmware": "WA.v2.0.0-rc2.bin",
  "force": false
}
```

### POST /api/wavecontrol/devices/{id}/upgrade-fanout

Upgrade AP and all connected STAs (Wave only).

### POST /api/wavecontrol/devices/bulk-upgrade

Bulk upgrade multiple devices.

**Request:**
```json
{
  "device_ids": [1, 2, 3],
  "firmware": "WA.v2.0.0-rc2.bin",
  "force": false
}
```

---

## Stats (Real-time)

### GET /api/wavecontrol/stats

Get all device stats from memory.

### GET /api/wavecontrol/stats/{ip}

Get stats for specific device by IP.

---

## Scheduled Jobs

### GET /api/wavecontrol/jobs

List scheduled jobs.

### POST /api/wavecontrol/jobs

Create a scheduled job.

**Request:**
```json
{
  "job_type": "upgrade",
  "device_ids": [1, 2, 3],
  "parameters": { "firmware": "WA.v2.0.0-rc2.bin" },
  "scheduled_at": "2024-01-20T02:00:00Z",
  "repeat_cron": ""
}
```

### DELETE /api/wavecontrol/jobs/{id}

Cancel a job.

---

## Config Backup

### POST /api/wavecontrol/devices/{id}/backup

Backup device configuration.

### POST /api/wavecontrol/devices/{id}/restore

Restore configuration.

**Request:**
```json
{
  "config_id": 5
}
```

### GET /api/wavecontrol/devices/{id}/configs

List saved configurations for device.

---

## Users (Admin only)

### GET /api/wavecontrol/users

List users.

### POST /api/wavecontrol/users

Create user.

**Request:**
```json
{
  "username": "operator",
  "password": "securepass",
  "roles": ["editor"]
}
```

### PATCH /api/wavecontrol/users/{id}

Update user.

### DELETE /api/wavecontrol/users/{id}

Delete user.

---

## Settings

### GET /api/wavecontrol/settings

List all settings.

### PATCH /api/wavecontrol/settings/{key}

Update a setting.

**Request:**
```json
{
  "value": "60"
}
```

---

## WebSocket

### GET /api/wavecontrol/ws

WebSocket endpoint for real-time updates.

**Messages sent by server:**
```json
{"type": "stats", "data": {...}}
{"type": "device_online", "device_id": 1}
{"type": "device_offline", "device_id": 1}
{"type": "upgrade_progress", "device_id": 1, "progress": 50}
```

---

## Mobile Clients / Push Notifications

Native Android and iOS clients use the server as the always-on monitor. Mobile clients register OS push tokens, receive alerts through FCM/APNs, and use REST/WebSocket only for state reconciliation and foreground live views.

All mobile endpoints require the same Bearer token authentication as the rest of the protected API.

### POST /api/wavecontrol/mobile/register

Register or refresh a mobile push token for the authenticated user.

**Request:**
```json
{
  "platform": "android",
  "provider": "fcm",
  "token": "device-push-token",
  "device_name": "Pixel NOC phone",
  "app_version": "1.0.0",
  "os_version": "Android 15"
}
```

For iOS, use either `{ "platform": "ios", "provider": "apns" }` for direct APNs or `{ "platform": "ios", "provider": "fcm" }` when using Firebase as the APNs bridge.

**Response:**
```json
{
  "ok": true,
  "device": {
    "id": "uuid",
    "platform": "android",
    "provider": "fcm",
    "device_name": "Pixel NOC phone",
    "enabled": true
  }
}
```

### DELETE /api/wavecontrol/mobile/register

Disable a mobile token for the authenticated user.

**Request:**
```json
{
  "platform": "android",
  "provider": "fcm",
  "token": "device-push-token"
}
```

If `token` is omitted, all enabled tokens for that platform/provider are disabled for the user.

### GET /api/wavecontrol/mobile/devices

List the authenticated user's registered mobile clients. Tokens are never returned.

### GET /api/wavecontrol/mobile/preferences

Get mobile alert preferences for the authenticated user.

### PATCH /api/wavecontrol/mobile/preferences

Update mobile alert preferences.

**Request:**
```json
{
  "push_enabled": true,
  "notify_critical": true,
  "notify_warning": true,
  "notify_info": false,
  "quiet_hours_start": "22:00:00",
  "quiet_hours_end": "06:00:00",
  "timezone": "America/Los_Angeles"
}
```

### GET /api/wavecontrol/mobile/bootstrap

Bootstrap a mobile app after login or notification tap.

Query parameters:
- `since_alert_id`: only return alerts newer than this alert id.
- `limit`: max alerts, default 100, max 500.

Response includes server time, registered mobile devices, push preferences, recent alerts, current live stats, and the WebSocket path.

### GET /api/wavecontrol/mobile/alerts

Reconcile alert history by cursor.

Query parameters:
- `since`: return alerts with id greater than this value.
- `status`: optional status filter.
- `limit`: default 100, max 500.

### POST /api/wavecontrol/mobile/test-push

Queue a test notification to all enabled mobile devices for the authenticated user.

### Alert rule mobile channel

Add `mobile` to an alert rule's `notify_channels` to send native push notifications:

```json
{
  "name": "Down host",
  "enabled": true,
  "scope": "all",
  "metric": "offline_duration",
  "operator": "gte",
  "threshold": 180,
  "duration_seconds": 0,
  "notify_channels": ["mobile", "email"],
  "cooldown_seconds": 900
}
```

### Push provider settings

Provider configuration is stored in `/api/wavecontrol/settings`:

| Key | Purpose |
|---|---|
| `mobile_push_enabled` | Global mobile push switch |
| `fcm_enabled` | Enable Firebase Cloud Messaging |
| `fcm_project_id` | Firebase project id; optional if service account JSON includes it |
| `fcm_service_account_json` | Firebase service account JSON for HTTP v1 send |
| `apns_enabled` | Enable direct Apple Push Notification service |
| `apns_team_id` | Apple Developer Team ID |
| `apns_key_id` | APNs auth key id |
| `apns_bundle_id` | iOS app bundle id / APNs topic |
| `apns_private_key_p8` | APNs `.p8` private key contents |
| `apns_production` | `true` for production APNs, `false` for sandbox |

Push tokens are AES-GCM encrypted at rest using the server JWT secret as the local encryption secret. The durable `notification_outbox` table retries transient FCM/APNs failures and disables a mobile token on terminal provider errors.

---

## Alert Rules UI and Operator Workflow

The web client now exposes a top-level **Alerts** page for operators.

### Operator UI

The Alerts page provides:

- Active/recent alert list with status filtering.
- Acknowledge and resolve actions for editor/admin users.
- Alert rule list with enabled/disabled state, scope, condition, delay, channels, and cooldown.
- Presets for common NOC rules:
  - Host down
  - Weak 5 GHz signal
  - Weak 60 GHz signal
  - Weak LTU signal
  - High CPU
  - High temperature
  - Low 60 GHz capacity
  - Peer count dropped
  - Low link score
- Rule form with scope selection for all devices, site, or single device.
- Notification channels: mobile, email, webhook, and zabbix.
- Mobile test push button.

### Backend endpoints used by the UI

```http
GET    /api/wavecontrol/alerts?status=active&limit=100
POST   /api/wavecontrol/alerts/{id}/acknowledge
POST   /api/wavecontrol/alerts/{id}/resolve

GET    /api/wavecontrol/alerts/rules
POST   /api/wavecontrol/alerts/rules
PATCH  /api/wavecontrol/alerts/rules/{id}
DELETE /api/wavecontrol/alerts/rules/{id}

POST   /api/wavecontrol/mobile/test-push
```

### Rule payload

```json
{
  "name": "Device down",
  "enabled": true,
  "scope": "all",
  "scope_id": null,
  "metric": "offline_duration",
  "operator": "gte",
  "threshold": 180,
  "duration_seconds": 0,
  "notify_channels": ["mobile"],
  "notify_emails": [],
  "webhook_url": "",
  "cooldown_seconds": 900
}
```

`POST /alerts/rules` now respects an explicit `enabled: false` value. If the field is omitted by an older client, the rule defaults to enabled.


### Recommended alert rule installer

The web **Alerts** page includes an **Install recommended rules** action. It creates the built-in presets that are not already present, using duplicate detection by name, metric, operator, threshold, scope, and scope ID. Installed presets default to the `mobile` notification channel and remain editable afterward.

---

## Alert targeting and device alert policy

Alert rules now support a uniform target filter and per-device eligibility gate.

### Alert rule fields

`POST /api/wavecontrol/alerts/rules` and `PATCH /api/wavecontrol/alerts/rules/{id}` accept:

```json
{
  "target_role": "all",
  "require_alertable": true
}
```

`target_role` may be `all`, `ap`, or `sta`. When `require_alertable` is true, the rule only evaluates devices whose inventory row is marked `alertable=true` and whose temporary silence has expired.

### Device alert policy

```http
PATCH /api/wavecontrol/devices/{id}/alerting
```

```json
{
  "alertable": true,
  "silence_seconds": 3600,
  "alert_notes": "seasonal customer"
}
```

Other forms:

```json
{ "clear_silence": true }
```

```json
{ "alert_silenced_until": "2026-05-23T04:00:00Z" }
```

Bulk update:

```http
POST /api/wavecontrol/devices/bulk-alerting
```

```json
{
  "device_ids": [1, 2, 3],
  "alertable": false
}
```

Device list/detail responses include `alertable`, `alert_silenced_until`, and `alert_notes`.
