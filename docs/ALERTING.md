# WaveControl alerting

WaveControl alerting evaluates **Ubiquiti AP and STA telemetry only**. Running WaveControl on Windows does not add Windows host monitoring; Windows is simply another supported host operating system for the same radio-management server.

## Alert lifecycle

Each rule is evaluated against every in-scope inventory device on each alert cycle:

1. The rule must be enabled.
2. Scope must match: fleet, site, or one device.
3. Role must match: AP, STA, or both.
4. When `require_alertable` is enabled, the device must be alertable and not currently silenced.
5. The selected metric must be present in the latest live statistics.
6. The operator and threshold must evaluate true.
7. The condition must remain true for `duration_seconds` (the persistence delay).
8. WaveControl creates one active occurrence and persists one outbox item per selected channel.

An occurrence never repeats while it remains active or acknowledged. It resolves when the condition returns to normal, when the metric disappears, or when a rule/device policy change makes the occurrence no longer applicable. A new occurrence can be created only after the condition satisfies the persistence delay again and the prior trigger's `cooldown_seconds` has elapsed.

### Acknowledge versus resolve

- **Acknowledge** means an operator has seen the occurrence. Evaluation continues and the occurrence still resolves automatically when the condition clears.
- **Resolve** closes the current occurrence immediately. It does not disable the rule. If the condition is still bad, WaveControl starts a fresh persistence window and may open another occurrence after both persistence and cooldown permit it.
- **Disable/silence device alerting** resolves current occurrences for that device and clears their evaluation state.
- **Edit, disable, or delete a rule** resolves occurrences created by the old rule definition so stale state cannot leak into the replacement definition.

## Targeting options

| Option | Meaning |
|---|---|
| Scope `all` | Every inventory device with the selected metric |
| Scope `site` | Devices assigned to one site |
| Scope `device` | One inventory device |
| Target `ap` | Ubiquiti access points only |
| Target `sta` | Ubiquiti subscriber/client radios only |
| Target `all` | APs and STAs |
| Alertable only | Honors each device's alertable/silence controls |

A rule is not an inventory query. Devices that do not report the selected metric are skipped rather than treated as zero.

## Metrics

| Metric | Unit | Notes |
|---|---:|---|
| `offline_duration` | seconds | Time since an unreachable device was last seen; for a never-online device, inventory creation time is used |
| `signal_5ghz` | dBm | Reported 5 GHz receive signal |
| `signal_6ghz` | dBm | True 6 GHz MLO signal; the compatibility slot used by a second MLO5 5 GHz radio is excluded |
| `signal_60ghz` | dBm | Reported 60 GHz receive signal |
| `signal_ltu` | dBm | Reported LTU receive signal |
| `cpu` | percent | Current device CPU utilization |
| `temperature` | °C | Reported CPU temperature |
| `ram` | percent | Current device memory utilization |
| `capacity` | Mbps | Combined estimated 60 GHz capacity |
| `peer_count` | peers | AP-reported associated subscriber count |
| `link_score` | score | Reported downlink link score |
| `interference` | percent | Highest reported interference airtime among the device's radios |
| `chain_imbalance` | dB | Largest spread between at least two valid negative-dBm chain readings on any radio; missing/zero chain values are ignored |
| `gps_sync` | 0/1 | `1` synchronized, `0` live state present but unsynchronized; unavailable when the device does not expose a usable state |

Operators are `<`, `<=`, `>`, `>=`, `=`, and `!=`.

`peer_count` needs deliberate scoping. Many legitimate APs can have zero subscribers; the preset is therefore manual and is not installed by the one-click recommended-rule action. Fleet-wide STA-down alerting can also be noisy, so its preset is manual.

## Severity

A rule can set `info`, `warning`, or `critical`, or use `auto` for legacy-compatible metric-based severity.

Automatic severity currently promotes:

- signal below -80 dBm to critical and below -70 dBm to warning;
- CPU/RAM above 95% to critical and above 85% to warning;
- temperature above 85 °C to critical and above 75 °C to warning;
- offline duration above one hour to critical and above five minutes to warning;
- capacity below 25 Mbps to critical and below 100 Mbps to warning;
- link score below 25 to critical and below 50 to warning;
- interference at or above 50% to critical and at or above 25% to warning;
- chain imbalance at or above 12 dB to critical and at or above 6 dB to warning;
- a reported unsynchronized GPS state to critical.

Values not crossing an automatic critical boundary are warning when the rule triggers. Explicit severity is preferable when notification priority is operationally important.

## Delivery channels

A rule may use no external channel and remain in WaveControl's in-application history, or fan out through any combination of:

- **Email:** SMTP endpoint is global; recipients are per rule.
- **Webhook:** destination is per rule. Public HTTP/HTTPS endpoints are accepted after SSRF checks; private, loopback, link-local, credential-bearing, and unsafe redirected destinations are rejected.
- **Zabbix sender:** sends the `wavecontrol.alert` trapper item to the global Zabbix sender endpoint.
- **sysmon-web:** sends `CRITICAL`, `WARNING`, and optional `OK` transitions over the pinned-TLS alerter protocol.

External delivery uses a PostgreSQL outbox. Email, webhook, and Zabbix events are retried with exponential backoff from 30 seconds up to 30 minutes and become terminal after eight attempts. sysmon-web follows its alerter reconnect policy instead: five seconds, doubling to one minute, and continues retrying while the occurrence or matching recovery remains relevant. A process crash leaves a recoverable claim. If an occurrence closes before a pending trigger is delivered, the stale trigger is canceled rather than arriving after recovery. The alert UI shows per-channel trigger/clear status, attempt count, and the latest error.

## Recovery events

`notify_recovery` controls whether a channel that received (or was actively sending) the trigger also receives a clear event. Recovery is not sent to a channel where the trigger was canceled before delivery.

For sysmon-web, recovery is `ALERT OK` with the same stable object key (`device-<device-id>-rule-<rule-id>`), allowing sysmon-web to replace the prior phone notification rather than stack another one.

## Maintenance and device policy

Per-device `alertable`, silence-until, and notes controls are available in inventory workflows. Maintenance windows currently govern scheduled jobs; they are not a blanket suppression mechanism for all radio alerts unless the device is explicitly silenced. Use device/site alert policy intentionally during planned RF or power work.

## Recommended starting policy

The one-click recommended set is intentionally conservative:

- AP down
- weak 5/6/60 GHz and LTU signal
- high CPU, RAM, and temperature
- low 60 GHz capacity
- low link score
- high interference

It does not automatically install STA-down, AP-peer-count, RF-chain-imbalance, or GPS-sync rules. Those depend too heavily on intentional topology and hardware capabilities for a safe fleet-wide default. Review each preset's scope, persistence, threshold, severity, channels, and recovery behavior before enabling external delivery.
