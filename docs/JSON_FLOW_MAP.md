# JSON Flow Map

Complete mapping from device API responses -> Go structs -> Stats Store -> waveControl API -> Frontend.

---

## Platform Detection

| Firmware Prefix | Platform | API Style | Radio Slots |
|-----------------|----------|-----------|-------------|
| `XC.`, `WA.`, `XW.`, `XM.` | airmax | `/status.cgi` | Radio5GHz only |
| `AFLTU.`, `AFLTUROCKET.`, `AF5XHD.` | ltu | `/api/v1.0/statistics` | RadioLTU (main) |
| `GMC.`, `GMP.`, `MGMP.`, `MW.` | wave | `/api/v1.0/statistics` | Radio60GHz (main), Radio5GHz (backup) |
| `GP.` | wave | `/api/v1.0/statistics` | AirFiber 60 - Radio60GHz (main), Radio5GHz (backup) |

### Firmware Prefix Reference

| Prefix | Product Line | Notes |
|--------|--------------|-------|
| `XC.` | AirMAX AC | Rocket 5AC, NanoStation 5AC, etc. |
| `WA.` | AirMAX AC | Alternative AC prefix |
| `XW.` | AirMAX M | Rocket M5, NanoStation M5, etc. |
| `XM.` | AirMAX M | Legacy M series |
| `AFLTU.` | LTU | LTU-Lite, LTU-LR stations |
| `AFLTUROCKET.` | LTU | LTU-Rocket APs |
| `AF5XHD.` | LTU | AirFiber 5XHD (uses LTU API) |
| `GMC.` | Wave | Wave AP/Pico |
| `GMP.` | Wave | Wave Pro |
| `MGMP.` | Wave | Wave Mega |
| `MW.` | Wave | Wave Nano |
| `GP.` | AirFiber 60 | AF60/AF60-LR (uses Wave API) |

**Note:** Wave MLO (MLO5/MLO6) models use multiple 5GHz or 6GHz radios instead of 60GHz+5GHz. API structure TBD.

---

## AirMAX Platform

### Endpoint: `GET /status.cgi`

### Device Stats Flow

```
/status.cgi JSON                    -> airmax.Status struct           -> stats.DeviceStats              -> API /devices
---------------------------------------------------------------------------------------------------------------------

host.hostname                       -> Status.Host.Hostname           -> DeviceStats.Hostname           -> hostname
host.uptime                         -> Status.Host.Uptime             -> DeviceStats.Uptime             -> uptime
host.totalram                       -> Status.Host.TotalRAM           -> DeviceStats.RAM.Total          -> ram.total
host.freeram                        -> Status.Host.FreeRAM            -> DeviceStats.RAM.Free           -> ram.free
(calculated)                        -> -                              -> DeviceStats.RAM.Usage          -> ram.usage
host.cpuload                        -> Status.Host.CPULoad            -> DeviceStats.CPU[0].Usage       -> cpu[0].usage
host.temperature                    -> Status.Host.Temperature        -> DeviceStats.Temperature.CPU    -> temperature

wireless.frequency                  -> Status.Wireless.Frequency      -> Radio5GHz.Frequency            -> radio_5ghz.frequency
wireless.chanbw                     -> Status.Wireless.ChanBW         -> Radio5GHz.ChannelBW            -> radio_5ghz.channel_bw
wireless.txpower                    -> Status.Wireless.TXPower        -> Radio5GHz.TxPower              -> radio_5ghz.tx_power
wireless.noisef                     -> Status.Wireless.NoiseF         -> Radio5GHz.NoiseFloor           -> radio_5ghz.noise_floor
wireless.signal (STA only)          -> Status.Wireless.Signal         -> Radio5GHz.Signal               -> radio_5ghz.signal
wireless.rssi (STA only)            -> Status.Wireless.RSSI           -> Radio5GHz.RSSI                 -> radio_5ghz.rssi

wireless.polling.dcap               -> Polling.DCap                   -> Wireless.TxRate (x1000)        -> (via peer)
wireless.polling.ucap               -> Polling.UCap                   -> Wireless.RxRate (x1000)        -> (via peer)

gps.fix                             -> Status.GPS.Fix                 -> DeviceStats.GPS.Fix            -> gps_lat/gps_lon presence
gps.lat                             -> Status.GPS.Lat                 -> DeviceStats.GPS.Lat            -> gps_lat
gps.lon                             -> Status.GPS.Lon                 -> DeviceStats.GPS.Lon            -> gps_lon
```

### Station/Peer Stats Flow (AP reporting connected STAs)

```
/status.cgi stations[]              -> airmax.Station struct          -> stats.PeerStats                -> API /devices (child)
---------------------------------------------------------------------------------------------------------------------

stations[].mac                      -> Station.MAC                    -> PeerStats.MAC                  -> mac
stations[].name                     -> Station.Name                   -> PeerStats.Hostname             -> hostname
stations[].lastip                   -> Station.LastIP                 -> PeerStats.IP                   -> ip_address
stations[].signal                   -> Station.Signal                 -> PeerStats.Signal               -> signal_level, signal_airmax
                                    -> rssiToDbm(Signal)              -> Radio5GHz.Signal               -> radio_5ghz.signal
stations[].rssi                     -> Station.RSSI                   -> PeerStats.RSSI                 -> (internal)
stations[].noisefloor               -> Station.NoiseFloor             -> PeerStats.NoiseFloor           -> (internal)
                                    -> rssiToDbm(NoiseFloor)          -> Radio5GHz.NoiseFloor           -> radio_5ghz.noise_floor
stations[].chainrssi[]              -> Station.ChainRSSI[]            -> (via GetChainSignals)          -> signal_per_chain
                                    -> rssiToDbm(each)                -> Radio5GHz.SignalPerChain       -> radio_5ghz.signal_per_chain
                                    -> drops only 0 placeholders; keeps negative dBm chains even near noisefloor
stations[].distance                 -> Station.Distance               -> PeerStats.Distance             -> distance
stations[].uptime                   -> Station.Uptime                 -> PeerStats.Uptime               -> uptime
stations[].txbytes                  -> Station.TXBytes                -> PeerStats.TxBytes              -> tx_bytes
stations[].rxbytes                  -> Station.RXBytes                -> PeerStats.RxBytes              -> rx_bytes
stations[].txpackets                -> Station.TXPackets              -> PeerStats.TxPackets            -> tx_packets
stations[].rxpackets                -> Station.RXPackets              -> PeerStats.RxPackets            -> rx_packets
stations[].stats.rx_bytes           -> Station.Stats.RXBytes          -> PeerStats.RxBytes (alt)        -> rx_bytes
stations[].stats.tx_bytes           -> Station.Stats.TXBytes          -> PeerStats.TxBytes (alt)        -> tx_bytes

stations[].airmax.downlink_capacity -> Station.AirMax.DownlinkCapacity -> Radio5GHz.Capacity.DL (x1000) -> radio_5ghz.capacity.dl, capacity_5ghz
stations[].airmax.uplink_capacity   -> Station.AirMax.UplinkCapacity   -> Radio5GHz.Capacity.UL (x1000) -> radio_5ghz.capacity.ul
stations[].airmax.rx.cinr           -> Station.AirMax.RX.CINR          -> Radio5GHz.CINR.DL             -> radio_5ghz.cinr.dl
stations[].airmax.tx.cinr           -> Station.AirMax.TX.CINR          -> Radio5GHz.CINR.UL             -> radio_5ghz.cinr.ul

stations[].remote.hostname          -> Station.Remote.Hostname        -> PeerStats.Hostname (override)  -> hostname
stations[].remote.platform          -> Station.Remote.Platform        -> PeerStats.Model                -> model
stations[].remote.version           -> Station.Remote.Version         -> PeerStats.Firmware             -> firmware
stations[].remote.temperature       -> Station.Remote.Temperature     -> PeerStats.Temperature          -> temperature
stations[].remote.signal            -> Station.Remote.Signal          -> PeerStats.RemoteSignal         -> remote_signal
stations[].remote.noisefloor        -> Station.Remote.NoiseFloor      -> PeerStats.RemoteNoiseFloor     -> remote_noise_floor
stations[].remote.chainrssi[]       -> Station.Remote.ChainRSSI[]     -> PeerStats.RemoteSignalPerChain -> remote_signal_per_chain
                                    -> mirrored to Radio5GHz.RemoteSignalPerChain for reports
                                    -> drops only 0 placeholders; keeps negative dBm chains even near remote.noisefloor
```

### Signal Direction Clarification (AirMAX)

For AirMAX devices, there are TWO signal measurements for each STA:

1. **Signal at AP** (`radio_5ghz`): What the AP receives from the STA
   - Source: `stations[].signal`, `stations[].chainrssi[]`
   - This is the AP's RX signal strength

2. **Signal at STA** (`remote_signal`): What the STA receives from the AP  
   - Source: `stations[].remote.signal`, `stations[].remote.chainrssi[]`
   - This is the STA's RX signal strength (reported back to AP)

### RSSI to dBm Conversion

AirMAX devices may return signal values in two formats:
- **Negative values** (-65, -70): Already in dBm, used as-is
- **Positive values** (30, 25): RSSI scale (0-95), converted via `dBm = RSSI - 95`

Note: The example `airmax_ap.json` shows chainrssi as `[-66, -67]` (already dBm), but some real devices return positive RSSI values like `[32, 33]` which need conversion.

```go
func rssiToDbm(val int) int {
    if val <= 0 {
        return val  // Already dBm
    }
    return val - 95  // Convert RSSI to dBm
}
```

---

## LTU Platform

### Endpoint: `GET /api/v1.0/statistics`

### Device Stats Flow

```
/api/v1.0/statistics JSON           -> parseStats()                   -> stats.DeviceStats              -> API /devices
---------------------------------------------------------------------------------------------------------------------

[0].device.uptime                   -> -                              -> DeviceStats.Uptime             -> uptime
[0].device.powerTime                -> -                              -> DeviceStats.PowerTime          -> power_time
[0].device.ram.total                -> -                              -> DeviceStats.RAM.Total          -> ram.total
[0].device.ram.free                 -> -                              -> DeviceStats.RAM.Free           -> ram.free
[0].device.ram.usage                -> -                              -> DeviceStats.RAM.Usage          -> ram.usage
[0].device.cpu[].identifier         -> -                              -> DeviceStats.CPU[].ID           -> cpu[].id
[0].device.cpu[].usage              -> -                              -> DeviceStats.CPU[].Usage        -> cpu[].usage
[0].device.temperatures[name=cpu]   -> -                              -> Temperature.CPU                -> temperature
[0].device.gps.fix                  -> -                              -> DeviceStats.GPS.Fix            -> (presence)
[0].device.gps.lat                  -> -                              -> DeviceStats.GPS.Lat            -> gps_lat
[0].device.gps.lon                  -> -                              -> DeviceStats.GPS.Lon            -> gps_lon

[0].wireless.radios[id=main]        -> parseRadioStats()              -> RadioLTU (when platform=ltu)   -> radio_ltu
  .frequency.center                 -> -                              -> RadioLTU.Frequency             -> radio_ltu.frequency
  .channelWidth.tx                  -> -                              -> RadioLTU.ChannelWidth          -> radio_ltu.channel_width
  .outputPower.conducted            -> -                              -> RadioLTU.TxPower               -> radio_ltu.tx_power
  .outputPower.eirp                 -> -                              -> RadioLTU.TxPowerEIRP           -> radio_ltu.tx_power_eirp
  .linkQuality.capacity.combined    -> -                              -> RadioLTU.Capacity.Combined     -> capacity_ltu
  .channelUtilization.dl            -> -                              -> RadioLTU.Utilization.DL        -> radio_ltu.utilization.dl
  .channelUtilization.ul            -> -                              -> RadioLTU.Utilization.UL        -> radio_ltu.utilization.ul
```

### Peer Stats Flow (LTU AP reporting connected STAs)

```
[0].wireless.peers[]                -> parsePeer()                    -> stats.PeerStats                -> API /devices (child)
---------------------------------------------------------------------------------------------------------------------

peers[].common.identification.mac   -> -                              -> PeerStats.MAC                  -> mac
peers[].common.hostname             -> -                              -> PeerStats.Hostname             -> hostname
peers[].common.mgmtIp               -> -                              -> PeerStats.IP                   -> ip_address
peers[].common.distance             -> -                              -> PeerStats.Distance             -> distance
peers[].common.counters.txBytes     -> -                              -> PeerStats.TxBytes              -> tx_bytes
peers[].common.counters.rxBytes     -> -                              -> PeerStats.RxBytes              -> rx_bytes
peers[].common.counters.txRate      -> -                              -> PeerStats.TxRate               -> tx_rate
peers[].common.counters.rxRate      -> -                              -> PeerStats.RxRate               -> rx_rate

peers[].local[id=main]              -> parsePeerRadioStats()          -> PeerStats.RadioLTU             -> radio_ltu
  .linkQuality.signal               -> -                              -> RadioLTU.Signal                -> radio_ltu.signal, signal_ltu
  .linkQuality.signalDay            -> -                              -> RadioLTU.SignalDay             -> radio_ltu.signal_day
  .linkQuality.signalPerChain[]     -> -                              -> RadioLTU.SignalPerChain        -> radio_ltu.signal_per_chain
  .linkQuality.noiseFloor           -> -                              -> RadioLTU.NoiseFloor            -> radio_ltu.noise_floor
  .linkQuality.cinr.dl              -> -                              -> RadioLTU.CINR.DL               -> radio_ltu.cinr.dl
  .linkQuality.cinr.ul              -> -                              -> RadioLTU.CINR.UL               -> radio_ltu.cinr.ul
  .linkQuality.mcs.txIdx            -> -                              -> RadioLTU.MCS.TxIdx             -> radio_ltu.mcs.tx_idx
  .linkQuality.mcs.rxIdx            -> -                              -> RadioLTU.MCS.RxIdx             -> radio_ltu.mcs.rx_idx
  .linkQuality.capacity.combined    -> -                              -> RadioLTU.Capacity.Combined     -> radio_ltu.capacity.combined
  .linkQuality.airtime.dl           -> -                              -> RadioLTU.AirtimeDL             -> radio_ltu.airtime_dl
  .linkQuality.airtime.ul           -> -                              -> RadioLTU.AirtimeUL             -> radio_ltu.airtime_ul
```

---

## Wave Platform

### Endpoint: `GET /api/v1.0/statistics`

### Device Stats Flow

```
/api/v1.0/statistics JSON           -> parseStats()                   -> stats.DeviceStats              -> API /devices
---------------------------------------------------------------------------------------------------------------------

[0].device.uptime                   -> -                              -> DeviceStats.Uptime             -> uptime
[0].device.powerTime                -> -                              -> DeviceStats.PowerTime          -> power_time
[0].device.ram.total/free/usage     -> -                              -> DeviceStats.RAM.*              -> ram.*
[0].device.cpu[].identifier/usage   -> -                              -> DeviceStats.CPU[].*            -> cpu[].*
[0].device.temperatures[name=cpu]   -> -                              -> Temperature.CPU                -> temperature
[0].device.temperatures[name=main]  -> -                              -> Temperature.Radio60            -> (internal)
[0].device.temperatures[name=backup]-> -                              -> Temperature.Radio5             -> (internal)
[0].device.gps.*                    -> -                              -> DeviceStats.GPS.*              -> gps_lat, gps_lon
[0].device.orientation.tilt         -> -                              -> DeviceStats.Orientation.Tilt   -> orientation.tilt
[0].device.orientation.roll         -> -                              -> DeviceStats.Orientation.Roll   -> orientation.roll
[0].device.orientation.tilt24       -> -                              -> DeviceStats.Orientation.Tilt24 -> orientation.tilt24
[0].device.orientation.roll24       -> -                              -> DeviceStats.Orientation.Roll24 -> orientation.roll24

[0].wireless.radios[id=main]        -> parseRadioStats()              -> Radio60GHz                     -> radio_60ghz
  .frequency.center                 -> -                              -> Radio60GHz.Frequency           -> radio_60ghz.frequency
  .channelWidth.tx                  -> -                              -> Radio60GHz.ChannelWidth        -> radio_60ghz.channel_width
  .outputPower.conducted            -> -                              -> Radio60GHz.TxPower             -> radio_60ghz.tx_power
  .linkQuality.capacity.combined    -> -                              -> Radio60GHz.Capacity.Combined   -> capacity_60ghz
  .gpsSyncState                     -> -                              -> Radio60GHz.GPSSyncState        -> radio_60ghz.gps_sync_state

[0].wireless.radios[id=backup]      -> parseRadioStats()              -> Radio5GHz                      -> radio_5ghz
  .frequency.center                 -> -                              -> Radio5GHz.Frequency            -> radio_5ghz.frequency
  .channelWidth.tx                  -> -                              -> Radio5GHz.ChannelWidth         -> radio_5ghz.channel_width
  .outputPower.conducted            -> -                              -> Radio5GHz.TxPower              -> radio_5ghz.tx_power
  .linkQuality.capacity.combined    -> -                              -> Radio5GHz.Capacity.Combined    -> capacity_5ghz
  .dfs.enabled                      -> -                              -> Radio5GHz.DFS.Enabled          -> radio_5ghz.dfs.enabled
```

### Peer Stats Flow (Wave AP reporting connected STAs)

```
[0].wireless.peers[]                -> parsePeer()                    -> stats.PeerStats                -> API /devices (child)
---------------------------------------------------------------------------------------------------------------------

peers[].common.identification.mac   -> -                              -> PeerStats.MAC                  -> mac
peers[].common.hostname             -> -                              -> PeerStats.Hostname             -> hostname
peers[].common.mgmtIp               -> -                              -> PeerStats.IP                   -> ip_address
peers[].common.distance             -> -                              -> PeerStats.Distance             -> distance
peers[].common.counters.*           -> -                              -> PeerStats.Tx/RxBytes/Rate      -> tx_bytes, rx_bytes, etc.

peers[].local[id=main]              -> parsePeerRadioStats()          -> PeerStats.Radio60GHz           -> radio_60ghz
  .active                           -> -                              -> Radio60GHz.Active              -> radio_60ghz.active
  .linkQuality.signal               -> -                              -> Radio60GHz.Signal              -> radio_60ghz.signal, signal_60ghz
  .linkQuality.signalDay            -> -                              -> Radio60GHz.SignalDay           -> radio_60ghz.signal_day
  .linkQuality.idealSignal          -> -                              -> Radio60GHz.IdealSignal         -> radio_60ghz.ideal_signal
  .linkQuality.capacity.combined    -> -                              -> Radio60GHz.Capacity.Combined   -> radio_60ghz.capacity.combined
  .linkQuality.linkScore.dl/ul      -> -                              -> Radio60GHz.LinkScore.DL/UL     -> radio_60ghz.link_score.*

peers[].local[id=backup]            -> parsePeerRadioStats()          -> PeerStats.Radio5GHz            -> radio_5ghz
  .active                           -> -                              -> Radio5GHz.Active               -> radio_5ghz.active
  .linkQuality.signal               -> -                              -> Radio5GHz.Signal               -> radio_5ghz.signal, signal_5ghz
  .linkQuality.signalPerChain[]     -> -                              -> Radio5GHz.SignalPerChain       -> radio_5ghz.signal_per_chain
  .linkQuality.noiseFloor           -> -                              -> Radio5GHz.NoiseFloor           -> radio_5ghz.noise_floor
  .linkQuality.capacity.combined    -> -                              -> Radio5GHz.Capacity.Combined    -> radio_5ghz.capacity.combined

peers[].remote[id=main]             -> parse in poller                -> Radio60GHz/RadioLTU.Remote*    -> radio_60ghz/radio_ltu.remote_*
  .linkQuality.signal               -> -                              -> Radio*.RemoteSignal            -> radio_*.remote_signal
  .linkQuality.signalPerChain[]     -> -                              -> Radio*.RemoteSignalPerChain    -> radio_*.remote_signal_per_chain
  .linkQuality.noiseFloor           -> -                              -> Radio*.RemoteNoiseFloor        -> radio_*.remote_noise_floor

peers[].remote[id=backup]           -> parse in poller                -> Radio5GHz.Remote*              -> radio_5ghz.remote_*
  .linkQuality.signal               -> -                              -> Radio5GHz.RemoteSignal         -> radio_5ghz.remote_signal
  .linkQuality.signalPerChain[]     -> -                              -> Radio5GHz.RemoteSignalPerChain -> radio_5ghz.remote_signal_per_chain
  .linkQuality.noiseFloor           -> -                              -> Radio5GHz.RemoteNoiseFloor     -> radio_5ghz.remote_noise_floor
```

### Signal Direction Clarification (LTU/Wave)

For LTU and Wave devices querying an AP, the peer stats contain two perspectives:

1. **`local`** (AP RX): What the AP receives from the STA
   - Source: `peers[].local[id=main|backup].linkQuality.signal`
   - Stored in: `Radio60GHz` / `Radio5GHz` (Wave) or `RadioLTU` (LTU)
   - This is the AP's RX signal strength from this STA

2. **`remote`** (STA RX): What the STA receives from the AP
   - Source: `peers[].remote[id=main|backup].linkQuality.signal`
   - Stored in: `Radio*.RemoteSignal`, `Radio*.RemoteSignalPerChain`, `Radio*.RemoteNoiseFloor`
   - This is the STA's RX signal strength (reported back to AP)

**Chain counts:**
- 60GHz (main): 1 chain (no per-chain array)
- 5GHz (backup): 2 chains

---

## Frontend Signal Display Logic

### Signal Column Selection

```javascript
// Frontend: components.js - getSignal5GHzChains()

// Priority order for per-chain signals:
1. device.radio_5ghz.signal_per_chain[]     // Wave 5GHz backup, AirMAX
2. device.radio_ltu.signal_per_chain[]      // LTU
3. device.signal_per_chain[]                // Direct from API (AirMAX legacy)
4. [device.signal_5ghz || signal_ltu || signal_airmax]  // Fallback to single value

// Combined signal calculation:
combineSignals(chains) -> power-sum of chain values
```

### Primary Signal for Dashboard

```javascript
// Frontend: getPeerSignal() equivalent logic

// Priority order:
1. radio_60ghz.signal (if active)    -> Wave 60GHz link
2. radio_ltu.signal                  -> LTU link  
3. radio_5ghz.signal                 -> Wave 5GHz backup, AirMAX
4. signal (direct field)             -> AirMAX fallback
```

---

## Summary: Radio Slot Usage by Platform

| Platform | Radio60GHz | Radio5GHz | RadioLTU |
|----------|------------|-----------|----------|
| Wave | AP RX + STA RX (1 chain) | AP RX + STA RX (2 chains) | - |
| AirFiber 60 (GP.) | AP RX + STA RX (1 chain) | AP RX + STA RX (2 chains) | - |
| LTU (incl. AF5XHD) | - | - | AP RX + STA RX (2 chains) |
| AirMAX | - | AP RX + Capacity + CINR (2 chains) | STA RX via remote_signal |

**Signal Direction:**
- **AP RX**: Signal the AP receives from this STA (from `local` section)
- **STA RX**: Signal the STA receives from the AP (from `remote` section, reported back)

**Per-Radio Remote Signal Fields:**
- `radio_60ghz.remote_signal` / `radio_60ghz.remote_signal_per_chain`
- `radio_5ghz.remote_signal` / `radio_5ghz.remote_signal_per_chain`
- `radio_ltu.remote_signal` / `radio_ltu.remote_signal_per_chain`

---

## Key Differences

### Signal Encoding
- **Wave/LTU/AirFiber 60**: Always dBm (negative values)
- **AirMAX**: May be RSSI (0-95) or dBm (negative), converted via `rssiToDbm()`

### Radio Identification
- **Wave/AirFiber 60**: `id: "main"` = 60GHz, `id: "backup"` = 5GHz
- **LTU**: `id: "main"` = 5GHz (only radio, stored in RadioLTU)
- **AirMAX**: Single 5GHz radio, no backup; remote signal is separate field

### Peer Discovery
- **Wave/LTU/AirFiber 60**: `peers[]` array in statistics response
- **AirMAX**: `stations[]` array in status.cgi response

### Capacity Units
- **Wave/LTU/AirFiber 60**: In kbps, multiplied by 1000 in parseCapacity()
- **AirMAX**: In kbps, multiplied by 1000

### Capacity Storage
- **Wave/LTU**: `radio_60ghz.capacity`, `radio_5ghz.capacity`, `radio_ltu.capacity` structs with dl/ul/combined
- **AirMAX**: `radio_5ghz.capacity` struct with dl/ul/combined (from airmax.downlink_capacity/uplink_capacity)

### tx_rate/rx_rate vs Capacity
- **Wave/LTU**: `tx_rate`/`rx_rate` = actual throughput (from counters.txRate/rxRate)
- **AirMAX**: Does NOT use tx_rate/rx_rate for capacity; uses `radio_5ghz.capacity` struct instead

### Interfaces Format
- **Wave/LTU**: Map format `{ "eth0": {...} }`
- **AirMAX**: Can be map OR array `[{ "ifname": "eth0", ...}]` - parser handles both
