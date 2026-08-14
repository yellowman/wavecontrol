# Native mobile clients

waveControl's mobile architecture is server-authoritative:

```text
Poller -> Stats Store -> Alert Manager -> notification_outbox -> FCM/APNs -> Android/iOS
                                      \-> WebSocket Hub -> foreground dashboards
```

The phone is not the always-on monitor. The server is. Android and iOS clients are full-time reachable through OS push services, then reconcile state through REST when opened.

## Android client shape

Recommended stack:

```text
Kotlin
Jetpack Compose
OkHttp or Ktor client
Firebase Cloud Messaging
Room for local cache
DataStore for settings
Android Keystore for auth token material
WorkManager for low-frequency reconciliation
```

Runtime behavior:

1. Login with `/auth/login`.
2. Store the Bearer token in encrypted storage.
3. Ask for notification permission on Android 13+.
4. Obtain the FCM registration token.
5. `POST /api/wavecontrol/mobile/register` with platform `android`, provider `fcm`.
6. Show OS notifications from FCM payloads.
7. On notification tap, call `/mobile/bootstrap` and navigate to the alert/device.
8. Keep `/ws` open only while the app is foregrounded or while an explicit visible NOC foreground service is enabled.

Do not rely on a silent forever-WebSocket in the background. Use FCM for outage alerts and WorkManager only as a reconciliation fallback.

## iOS client shape

Recommended stack:

```text
Swift
SwiftUI
URLSession / URLSessionWebSocketTask
APNs directly or Firebase Messaging as APNs bridge
Keychain for auth token material
SwiftData/CoreData/SQLite for local cache
```

Runtime behavior:

1. Login with `/auth/login`.
2. Store the Bearer token in Keychain.
3. Request user notification authorization.
4. Register with APNs directly, or Firebase Messaging if using FCM for iOS.
5. `POST /api/wavecontrol/mobile/register` with platform `ios` and provider `apns` or `fcm`.
6. Use visible APNs notifications for down-host alarms.
7. On tap, call `/mobile/bootstrap` and open the alert/device view.
8. Use `URLSessionWebSocketTask` only in foreground dashboard/NOC views.

Silent background pushes are not the primary alarm path. Treat them as opportunistic refresh hints only.

## Push payload contract

The outbox stores provider-neutral payloads with:

```json
{
  "event_type": "alert.created",
  "title": "CRITICAL: Wave-AP-1",
  "body": "Down host: offline_duration is at or above threshold",
  "severity": "critical",
  "collapse_key": "device:55:alert",
  "deep_link": "wavecontrol://alerts/1234",
  "data": {
    "alert_id": "1234",
    "device_id": "55",
    "site_id": "1",
    "metric": "offline_duration"
  }
}
```

Client code should not assume push delivery is exactly-once. Store the highest seen alert id and call `/mobile/alerts?since=<id>` or `/mobile/bootstrap?since_alert_id=<id>` after launch/resume.

## Notification channels

Android should create these channels:

```text
Down hosts      high importance, sound/vibration
Warnings        default importance
General         default/low importance
NOC live mode   persistent foreground-service notification
```

iOS should map critical operational alarms to Time Sensitive notifications where the app entitlement/configuration permits it. Critical Alerts are a later entitlement-driven option, not a v1 dependency.
