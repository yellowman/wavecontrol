# Zabbix Integration Guide

waveControl includes a **Zabbix agent protocol** bridge so a Zabbix Server/Proxy can query metrics that are not exposed via Ubiquiti SNMP (especially per-chain signal and per-peer details).

This document covers:
- Enabling the waveControl Zabbix bridge
- Recommended Zabbix host/item configuration
- Useful item keys for **individual radios** (APs and STAs)

---

## 1) Enable the Zabbix bridge in waveControl

You can enable the bridge from the waveControl UI (**Settings**) or directly in the database.

### Database settings

```sql
UPDATE settings SET value = 'true' WHERE key = 'zabbix_enabled';
UPDATE settings SET value = '0.0.0.0:10050' WHERE key = 'zabbix_listen';
UPDATE settings SET value = '10.0.0.5,192.168.1.0/24' WHERE key = 'zabbix_allowed_hosts';
```

- `zabbix_listen` defaults to `127.0.0.1:10050`
- `zabbix_allowed_hosts` should always be set (comma-separated IPs, CIDRs, or hostnames)
- A waveControl restart is required for changes to take effect

### Security note

The Zabbix agent protocol is plaintext. If you need encryption:
- bind to `127.0.0.1` and use SSH tunneling from Zabbix
- or use a VPN between Zabbix and waveControl

---

## 2) Zabbix Server setup (basic)

### Create a Host for waveControl

In Zabbix:
- **Configuration → Hosts → Create host**
- Add an **Agent interface** pointing to the waveControl server address
- Set port to match `zabbix_listen` (default `10050`)

You can now create items on that host that query waveControl.

---

## 3) Discovery (LLD)

waveControl exposes a discovery key:

```
wavecontrol.discovery
```

This returns JSON suitable for Low-Level Discovery. The output contains macros like:
- `{#IP}`
- `{#MAC}`
- `{#HOSTNAME}`
- `{#PLATFORM}` (wave/ltu/airmax/etc)
- `{#ISAP}` (1 for AP, 0 for STA)

### Recommended LLD rule

Create a **Discovery rule** on the waveControl host:
- Type: **Zabbix agent**
- Key: `wavecontrol.discovery`
- Update interval: `1m` (or whatever is appropriate)

Then create Item Prototypes for per-device metrics.

---

## 4) Device metrics (recommended for individual radios)

Device metrics use the key:

```
wavecontrol.device[<DEVICE_KEY>,<METRIC>]
```

### DEVICE_KEY can be IP or MAC

- You may pass **either** the device IP or the device MAC as `<DEVICE_KEY>`.
- **MAC is recommended** so the item stays stable even if an IP is reused or changes.

Examples:

```
# By MAC (preferred)
wavecontrol.device[aa:bb:cc:dd:ee:ff,online]
wavecontrol.device[aa:bb:cc:dd:ee:ff,uptime]

# By IP
wavecontrol.device[192.168.1.10,online]
wavecontrol.device[192.168.1.10,radio60.capacity]
```

### Commonly useful METRIC values

Status/system:
- `online`
- `uptime`
- `last_seen`
- `cpu`
- `ram.usage`
- `ram.free`
- `temp.cpu`
- `temp.radio60`
- `temp.radio5`

Wireless aggregate:
- `peer_count`
- `radio60.capacity`
- `radio60.frequency`
- `radio60.channel_width`
- `radio60.tx_power`
- `radio60.tx_rate`
- `radio60.rx_rate`
- `radio5.capacity`
- `radio5.frequency`
- `radio5.channel_width`
- `radio5.tx_power`
- `radio5.tx_rate`
- `radio5.rx_rate`

GPS:
- `gps.fix`
- `gps.lat`
- `gps.lon`
- `gps.sats`

---

## 5) Peer metrics (advanced)

For per-association (AP→STA) metrics:

```
wavecontrol.peer[<AP_IP>,<STA_MAC>,<METRIC>]
```

Examples:

```
# 60GHz peer signal
wavecontrol.peer[192.168.1.1,aa:bb:cc:dd:ee:ff,signal.60ghz.combined]
wavecontrol.peer[192.168.1.1,aa:bb:cc:dd:ee:ff,signal.60ghz.chain0]

# 5GHz peer signal
wavecontrol.peer[192.168.1.1,aa:bb:cc:dd:ee:ff,signal.5ghz.combined]

# Other peer metrics
wavecontrol.peer[192.168.1.1,aa:bb:cc:dd:ee:ff,cinr.dl]
wavecontrol.peer[192.168.1.1,aa:bb:cc:dd:ee:ff,cinr.ul]
wavecontrol.peer[192.168.1.1,aa:bb:cc:dd:ee:ff,distance]
```

**Note:** Peer items require knowing the AP IP that the STA is associated to. Many installations use custom Zabbix discovery (or external scripts) to generate these items.

---

## 6) Fleet counters

These return counts across the whole in-memory store:

```
wavecontrol.count[online]
wavecontrol.count[offline]
wavecontrol.count[unknown]
wavecontrol.count[total]
```
