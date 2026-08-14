# AirMAX API Field Mapping

This document maps AirMAX status.cgi fields to WaveControl internal structures.

## Authentication

```
POST /login.cgi
Content-Type: application/x-www-form-urlencoded

username=ubnt&password=ubnt
```

Response: Sets session cookie. Check for "Invalid credentials" in body.

## Status Endpoint

```
GET /status.cgi
Cookie: <session>
```

Returns JSON with full device status.

## Field Mapping

### Host Info

| status.cgi | WaveControl | Type | Notes |
|------------|-------------|------|-------|
| `host.hostname` | `DeviceStats.Hostname`, `devices.hostname` | string | |
| `host.devmodel` | `devices.product` | string | e.g., "Rocket Prism 5AC Gen2" |
| `host.fwversion` | `devices.firmware` | string | e.g., "v8.7.0" |
| `host.uptime` | `DeviceStats.Uptime` | int64 | seconds |
| `host.cpuload` | `DeviceStats.CPU[0].Usage` | float | percentage |
| `host.temperature` | `DeviceStats.Temperature.CPU` | int | Celsius |
| `host.totalram` | `DeviceStats.RAM.Total` | int64 | bytes |
| `host.freeram` | `DeviceStats.RAM.Free` | int64 | bytes |
| `host.netrole` | - | string | "bridge", "router" |

### Wireless Config

| status.cgi | WaveControl | Type | Notes |
|------------|-------------|------|-------|
| `wireless.essid` | `devices.ssid` | string | |
| `wireless.mode` | `devices.role` | string | "ap-ptmp"->"ap", "sta-ptmp"->"sta" |
| `wireless.apmac` | `devices.mac` | string | AP's MAC address |
| `wireless.frequency` | `devices.frequency`, `RadioStats.Frequency` | int | MHz |
| `wireless.chanbw` | `devices.channel_width`, `RadioStats.ChannelBW` | int | MHz |
| `wireless.txpower` | `RadioStats.TxPower` | int | dBm |
| `wireless.noisef` | `RadioStats.NoiseFloor` | int | dBm |
| `wireless.count` | `DeviceStats.PeerCount` | int | Connected stations |
| `wireless.security` | - | string | "WPA2", "open", etc. |
| `wireless.distance` | - | int | Max distance in meters |
| `wireless.dfs` | `RadioStats.DFS.Enabled` | int | 0/1 |
| `wireless.antenna_gain` | `RadioStats.Antenna.Gain` | int | dBi |

### Wireless Signal (STA mode)

| status.cgi | WaveControl | Type | Notes |
|------------|-------------|------|-------|
| `wireless.signal` | `RadioStats.Signal` | int | dBm |
| `wireless.rssi` | `RadioStats.RSSI` | int | |
| `wireless.noisefloor` | `RadioStats.NoiseFloor` | int | dBm |

### Polling/AirMAX Stats

| status.cgi | WaveControl | Type | Notes |
|------------|-------------|------|-------|
| `wireless.polling.dcap` | `Wireless.TxRate` | int | kbps (x1000 for bps) |
| `wireless.polling.ucap` | `Wireless.RxRate` | int | kbps |
| `wireless.polling.use` | - | int | Airtime % |
| `wireless.polling.tx_use` | - | int | TX airtime % |
| `wireless.polling.rx_use` | - | int | RX airtime % |
| `wireless.polling.fixed_frame` | - | bool | Fixed frame enabled |
| `wireless.polling.gps_sync` | - | bool | GPS sync enabled |
| `wireless.polling.ff_frame_dur` | - | int | Frame duration (ms) |
| `wireless.polling.ff_dl_ratio` | - | int | DL/UL ratio % |

### Connected Stations (AP mode)

| status.cgi | WaveControl | Type | Notes |
|------------|-------------|------|-------|
| `wireless.sta[].mac` | `PeerStats.MAC` | string | |
| `wireless.sta[].lastip` | `PeerStats.IP` | string | |
| `wireless.sta[].signal` | `PeerStats.Signal` | int | dBm |
| `wireless.sta[].rssi` | `PeerStats.RSSI` | int | |
| `wireless.sta[].noisefloor` | `PeerStats.NoiseFloor` | int | dBm |
| `wireless.sta[].chainrssi` | - | []int | Per-chain RSSI |
| `wireless.sta[].distance` | `PeerStats.Distance` | int | meters |
| `wireless.sta[].uptime` | `PeerStats.Uptime` | int64 | seconds |
| `wireless.sta[].tx_latency` | - | int | ms |
| `wireless.sta[].stats.rx_bytes` | `PeerStats.RXBytes` | int64 | |
| `wireless.sta[].stats.tx_bytes` | `PeerStats.TXBytes` | int64 | |
| `wireless.sta[].stats.rx_pps` | - | int | packets/sec |
| `wireless.sta[].stats.tx_pps` | - | int | packets/sec |

### Station AirMAX Stats

| status.cgi | WaveControl | Type | Notes |
|------------|-------------|------|-------|
| `wireless.sta[].airmax.downlink_capacity` | `PeerStats.TxRate` | int | kbps |
| `wireless.sta[].airmax.uplink_capacity` | `PeerStats.RxRate` | int | kbps |
| `wireless.sta[].airmax.actual_priority` | - | int | |
| `wireless.sta[].airmax.rx.cinr` | `PeerStats.Radio5GHz.CINR.UL` | int | UL = station -> AP |
| `wireless.sta[].airmax.tx.cinr` | `PeerStats.Radio5GHz.CINR.DL` | int | DL = AP -> station |
| `wireless.sta[].airmax.rx.evm` | `PeerStats.Radio5GHz.EVM.UL` | float | Aggregated (avg of most recent per-chain sample). Higher is generally better. |
| `wireless.sta[].airmax.tx.evm` | `PeerStats.Radio5GHz.EVM.DL` | float | Aggregated (avg of most recent per-chain sample). Higher is generally better. |
| `wireless.sta[].airmax.rx.usage` | - | int | Airtime % |
| `wireless.sta[].airmax.tx.usage` | - | int | Airtime % |

### Station Remote Info

| status.cgi | WaveControl | Type | Notes |
|------------|-------------|------|-------|
| `wireless.sta[].remote.hostname` | `PeerStats.Hostname` | string | |
| `wireless.sta[].remote.platform` | `PeerStats.Model` | string | e.g., "Rocket 5AC Lite" |
| `wireless.sta[].remote.version` | `PeerStats.Firmware` | string | |
| `wireless.sta[].remote.temperature` | `PeerStats.Temperature` | int | Celsius |
| `wireless.sta[].remote.tx_power` | `PeerStats.TXPower` | int | dBm |
| `wireless.sta[].remote.signal` | - | int | STA's view of signal |
| `wireless.sta[].remote.cpuload` | - | float | % |
| `wireless.sta[].remote.netrole` | - | string | |
| `wireless.sta[].remote.ethlist[].speed` | - | int | Mbps |
| `wireless.sta[].remote.ethlist[].plugged` | - | bool | |

### GPS

| status.cgi | WaveControl | Type | Notes |
|------------|-------------|------|-------|
| `gps.lat` | `GPSStats.Lat`, `devices.gps_lat` | float64 | |
| `gps.lon` | `GPSStats.Lon`, `devices.gps_lon` | float64 | |
| `gps.fix` | `GPSStats.Fix` | int | 0=no fix, 1+=fix |
| `gps.sats` | `GPSStats.Sats` | int | Satellite count |
| `gps.alt` | `GPSStats.Alt` | float64 | meters |
| `gps.dop` | - | float64 | Dilution of precision |

### Interfaces

| status.cgi | WaveControl | Type | Notes |
|------------|-------------|------|-------|
| `interfaces[].ifname` | `InterfaceStats.Name` | string | "eth0", "ath0" |
| `interfaces[].hwaddr` | - | string | MAC |
| `interfaces[].status.plugged` | `InterfaceStats.Plugged` | bool | |
| `interfaces[].status.speed` | `InterfaceStats.Speed` | int | Mbps |
| `interfaces[].status.tx_bytes` | `InterfaceStats.TxBytes` | int64 | |
| `interfaces[].status.rx_bytes` | `InterfaceStats.RxBytes` | int64 | |
| `interfaces[].status.cable_len` | - | int | meters |

## Platform Detection

WaveControl detects AirMAX device flavor from `host.devmodel`:

| Contains | Flavor |
|----------|--------|
| "litebeam" | LiteBeam |
| "powerbeam" | PowerBeam |
| "nanobeam" | NanoBeam |
| "rocket" | Rocket |
| "nanostation" | NanoStation |
| "liteap" | LiteAP |
| "prism" | Prism |
| "isostation" | IsoStation |
| "gigabeam" | GigaBeam |
| "airfiber" | airFiber |
| "ltu" | LTU |
| (default) | AirMAX |

## Other Endpoints

### Config Read/Write

```
GET /getcfg.cgi -> plaintext config
POST /writecfg.cgi cfgData=<config>&testmode=yes
POST /apply.cgi apply=yes
POST /discard.cgi d=0
```

### Reboot/Reset

```
POST /reboot.cgi reboot=yes
POST /reset.cgi reset=yes
```

### Site Survey

```
GET /survey.json.cgi
```

### Discovery

```
POST /discovery.cgi discover=y&duration=1000
```

## Firmware Upgrade

### Upload Firmware

```
POST /upgrade.cgi
Content-Type: multipart/form-data

fwfile: <binary firmware data>
```

The firmware file should have the correct platform prefix matching the device.

**Response:**
- Success: HTML page with upgrade progress
- Error: HTML with error message

### Firmware Platform Detection

WaveControl detects the firmware platform from the version string:

```
XC.qca955x.v8.7.0...  -> XC (AirOS 8, AC series)
WA.v8.7.0...          -> WA (AirOS 8, AC variant)
XM.ar7240.v6.3.6...   -> XM (AirOS 5, M series)
XW.v6.3.6...          -> XW (AirOS 5, M variant)
MW.ipq53xx.v2.4.1...  -> MW (Wave MLO)
```

### Firmware Platform Reference

| Prefix | AirOS Version | Devices |
|--------|---------------|---------|
| XC | 8 | Rocket 5AC, PowerBeam 5AC, LiteBeam 5AC, NanoStation 5AC, Prism, IsoStation, LiteAP |
| WA | 8 | AC variant |
| XM | 5 | Rocket M5, NanoStation M5, Bullet M2, NanoBridge M5 |
| XW | 5 | M series variant |
| AF11 | AF | AirFiber 11 (11 GHz) |
| AF5X | AF | AirFiber 5X (5 GHz, full duplex) |
| AF5U | AF | AirFiber 5U (5 GHz, US) |
| AF5 | AF | AirFiber 5 (5 GHz) |
| AF3X | AF | AirFiber 3X (3 GHz) |
| AF2X | AF | AirFiber 2X (2 GHz, uses 2GHz chipset loader) |

### Supported Firmware Transitions

| Current Platform | Can Upgrade To |
|-----------------|----------------|
| XC (AirOS 8) | XC, WA |
| WA (AirOS 8) | XC, WA |
| XM (AirOS 5) | XM, XW |
| XW (AirOS 5) | XM, XW |

Cross-generation upgrades (AirOS 5 <-> AirOS 8) are not supported.

## AirFiber Devices (AF2/AF3/AF5/AF11)

AirFiber devices use the same AirMAX-style API (login.cgi, status.cgi) but include an additional `airfiber` block in the status response.

**Note:** AF-2X and AF-2WA use the 2GHz chipset loader, not a "Gen2" platform.

### AirFiber Status Block

| status.cgi | WaveControl | Type | Notes |
|------------|-------------|------|-------|
| `airfiber.txchanbw` | - | string | TX channel bandwidth |
| `airfiber.rxchanbw` | - | string | RX channel bandwidth |
| `airfiber.txfrequency` | `RadioStats.Frequency` | int | TX frequency MHz |
| `airfiber.rxfrequency` | - | int | RX frequency MHz |
| `airfiber.framelength` | - | string | Frame length |
| `airfiber.duplex` | - | string | "half" or "full" |
| `airfiber.dutycycle` | - | string | Duty cycle percentage |
| `airfiber.gps_sync` | - | bool | GPS sync enabled |
| `airfiber.txmodrate` | - | int | TX modulation rate |
| `airfiber.rxmodrate` | - | int | RX modulation rate |
| `airfiber.txcapacity` | `Wireless.TxRate` | int | TX capacity kbps |
| `airfiber.rxcapacity` | `Wireless.RxRate` | int | RX capacity kbps |
| `airfiber.txpower` | `RadioStats.TxPower` | int | TX power dBm |
| `airfiber.rxgain` | - | int | RX gain dB |
| `airfiber.linkdist` | `RadioStats.Distance` | int | Link distance meters |
| `airfiber.rssi0` | `RadioStats.SignalPerChain[0]` | int | Chain 0 RSSI |
| `airfiber.rssi1` | `RadioStats.SignalPerChain[1]` | int | Chain 1 RSSI |
| `airfiber.signal0dbm` | `RadioStats.SignalPerChain[0]` | int | Chain 0 signal dBm |
| `airfiber.signal1dbm` | `RadioStats.SignalPerChain[1]` | int | Chain 1 signal dBm |
| `airfiber.cinrdb` | - | float | Carrier to interference+noise ratio |
| `airfiber.linkstate` | `RadioStats.LinkState` | string | "operational", "offline" |
| `airfiber.remotemac` | `PeerStats.MAC` | string | Remote device MAC |
| `airfiber.remoteip` | `PeerStats.IP` | string | Remote device IP |

### AirFiber Firmware Patterns

```
AF11.ar934x.v3.2.0...   -> AF11 (11 GHz)
AF5X.ar934x.v3.2.0...   -> AF5X (5 GHz full duplex)
AF5U.ar934x.v3.2.0...   -> AF5U (5 GHz US)
AF5.ar934x.v3.2.0...    -> AF5 (5 GHz)
AF3X.ar934x.v3.2.0...   -> AF3X (3 GHz)
AF2X.ar934x.v3.2.0...   -> AF2X (2 GHz, 2GHz chipset loader)
```
