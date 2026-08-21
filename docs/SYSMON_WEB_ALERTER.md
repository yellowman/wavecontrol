# sysmon-web alerter integration

WaveControl can forward its Ubiquiti alert transitions to sysmon-web using sysmon-web's alerter protocol. WaveControl delegates phone subscription, priority routing, collapse behavior, and push logs to sysmon-web rather than carrying a native mobile-provider subsystem.

## sysmon-web preparation

1. In sysmon-web, open **Admin → Monitoring boxes → Add a box**.
2. Mint a token with the identity WaveControl will use, normally `wavecontrol`.
3. Export/copy the public agent certificate or CA PEM used as `aggregator-ca.pem`.
4. Record the sysmon-web agent listener hostname and port (default 1347).

The token is a credential. WaveControl encrypts it in PostgreSQL with `WAVECONTROL_DATA_KEY` and masks it in API/UI responses.

## WaveControl configuration

As a WaveControl administrator, open **Settings → Alert Delivery → sysmon-web alerter** and configure:

| Setting | Purpose |
|---|---|
| Enable | Allows alert rules to deliver through this channel |
| Server | sysmon-web hostname or IP, without a port |
| Agent port | TLS agent listener, normally 1347 |
| Alerter name | Token identity; letters, digits, `-`, `_`, maximum 64 characters |
| Token | Token minted by sysmon-web |
| Application | Human-readable application label, maximum 128 characters |
| CA certificate PEM | Pinned sysmon-web server certificate or issuing CA |

Save before testing. **Test saved configuration** performs an isolated TLS connection, `ALERTER` authentication, `PING`, and `QUIT`; it does not create an alert or phone notification.

## Protocol implementation

WaveControl:

- opens TLS with TLS 1.2 or later;
- verifies the presented server chain against the configured PEM without disabling certificate verification;
- sends `ALERTER <name> <token> <application>` and requires `333 welcome`;
- maintains one serialized long-lived connection whenever the channel is enabled;
- sends `PING` after an idle interval and reconnects on transport failure;
- sends `ALERT <status> <object> <text>` and requires `333 ok`;
- treats a `444` reply as a refused command without assuming the connection closed;
- sanitizes all protocol text to one line and truncates it to 512 characters.

WaveControl's mapping is:

| WaveControl event | sysmon status | Expected phone priority |
|---|---|---|
| Critical trigger | `CRITICAL` | Loud/high priority |
| Warning trigger | `WARNING` | Quiet |
| Info trigger | `WARNING` | Quiet; the protocol has no INFO verb |
| Clear/recovery | `OK` | Quiet and collapses/replaces the prior object notification |

The object key is stable for the rule/device pair: `device-<device-id>-rule-<rule-id>`. The same key is used for trigger and recovery.

## Failure behavior

A sysmon-web outage does not block alert evaluation or other channels. Both the maintained session and the PostgreSQL notification outbox reconnect/retry after 5 seconds, doubling to a 60-second cap. sysmon work remains retryable while the trigger or matching recovery is relevant; configuration, handshake, and refusal errors stay visible in delivery readiness and on the affected alert.

When a condition recovers before a trigger leaves the outbox, WaveControl cancels the stale trigger. It sends `OK` only for a channel where the trigger was already sent or was concurrently in flight.

## Security boundary

WaveControl does not offer an insecure TLS option. Do not paste a private key into the CA field. Rotating the sysmon token or certificate closes the existing connection and wakes the maintenance loop so it immediately authenticates and verifies using the new settings.
