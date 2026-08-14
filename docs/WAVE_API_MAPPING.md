# Wave API Stats Field Mapping

## API Endpoints

| Endpoint | Purpose | Poll Frequency |
|----------|---------|----------------|
| `GET /api/v1.0/device` | Static device info | On discovery, on firmware change |
| `GET /api/v1.0/statistics` | Real-time stats + peer discovery | Every 30s |
| `GET /api/v1.0/system/upgrade` | Firmware upgrade status | During upgrades |
| `POST /api/v1.0/system/upgrade/direct` | Upload firmware | On demand |
| `POST /api/v1.0/system/reboot` | Reboot device | After firmware upload |

---

## Device Info (`/api/v1.0/device`)

### Stored in Database (Static)

```
identification.mac             -> devices.mac (PK)
identification.firmware        -> devices.firmware
identification.firmwareVersion -> devices.firmware_version
identification.model           -> devices.model
identification.product         -> devices.product
identification.family          -> devices.platform ("wave")
capabilities.device.supportedFirmwares[0].flavor -> devices.flavor

# Derived from firmware string if flavor not in capabilities:
# "GMC.ipq5018.v4.1.0..." -> "GMC"
# "MGMP.ipq807x.v4.1.0..." -> "MGMP"
```

### Radio Capabilities (Reference Only)

```
capabilities.radios[]:
  - id: "main"        -> 60 GHz radio
    name: "60 GHz"
    peer_limit: 31
    channelWidths.ptmp: [540, 1080, 2160]
    
  - id: "backup"      -> 5 GHz radio
    name: "5 GHz Backup"
    channelWidths.ptmp: [20, 40, 80]
```

---

## Statistics (`/api/v1.0/statistics`)

Returns array with single element: `stats[0]`

### Device Stats -> In-Memory

```go
// Path: stats[0].device
type DeviceStats struct {
    Uptime      int64    // device.uptime (seconds)
    PowerTime   int64    // device.powerTime (total powered time)
    
    CPU         []CPUCore // device.cpu[]
    RAM         RAMStats  // device.ram
    
    Temperatures struct {
        CPU       float64  // device.temperatures[name="cpu"].value
        Radio60   float64  // device.temperatures[name="main"].value
        Radio5    float64  // device.temperatures[name="backup"].value
    }
    
    GPS struct {
        Fix       bool     // device.gps.fix
        Lat       float64  // device.gps.lat
        Lon       float64  // device.gps.lon
        Alt       float64  // device.gps.alt
        Sats      int      // device.gps.sats
    }
    
    Orientation struct {
        Tilt      float64  // device.orientation.tilt
        Roll      float64  // device.orientation.roll
    }
}

type CPUCore struct {
    ID        string   // device.cpu[].identifier
    Usage     int      // device.cpu[].usage (percent)
}

type RAMStats struct {
    Total     int64    // device.ram.total
    Free      int64    // device.ram.free
    Usage     int      // device.ram.usage (percent)
}
```

### Wireless Stats -> In-Memory

```go
// Path: stats[0].wireless
type WirelessStats struct {
    ServiceUptime   int64    // wireless.serviceUptime
    ServiceDowntime int64    // wireless.serviceDowntime
    
    // Aggregate link quality
    LinkQuality struct {
        RxRate    int64    // wireless.linkQuality.counters.rxRate (bps)
        TxRate    int64    // wireless.linkQuality.counters.txRate (bps)
        
        Capacity  CapacityStats  // wireless.linkQuality.capacity
        LinkScore LinkScore      // wireless.linkQuality.linkScore
    }
    
    // Per-radio stats
    Radio60GHz RadioStats  // wireless.radios[id="main"]
    Radio5GHz  RadioStats  // wireless.radios[id="backup"]
    
    // Connected peers (for APs)
    PeerCount int
    Peers     []PeerStats
}

type RadioStats struct {
    ID              string   // radios[].id ("main" or "backup")
    Name            string   // "60 GHz" or "5 GHz Backup"
    LinkState       string   // radios[].linkState ("connected", "disconnected")
    
    Frequency       int      // radios[].frequency.center (MHz)
    ChannelWidth    int      // radios[].channelWidth.tx (MHz)
    
    OutputPower struct {
        EIRP      int      // radios[].outputPower.eirp (dBm)
        Conducted int      // radios[].outputPower.conducted (dBm)
    }
    
    Antenna struct {
        Name      string   // radios[].antenna.name
        Gain      int      // radios[].antenna.gain (dBi)
    }
    
    Capacity      CapacityStats  // radios[].linkQuality.capacity
    
    // Channel utilization
    Utilization struct {
        DL          float64  // radios[].channelUtilization.dl
        UL          float64  // radios[].channelUtilization.ul
        Interference float64 // radios[].channelUtilization.common.interference
    }
    
    ServiceUptime   int64    // radios[].serviceUptime
    ServiceActiveTime int64  // radios[].serviceActiveTime
    
    // GPS sync (60GHz only)
    GPSSyncState    int      // radios[].gpsSyncState
    
    // DFS (5GHz only)
    DFS struct {
        Enabled       bool   // radios[].dfs.enabled
        CACDuration   int    // radios[].dfs.cacDuration
        CACRemaining  int    // radios[].dfs.cacRemaining
    }
}

type CapacityStats struct {
    DL            int64    // capacity.dl (bps)
    UL            int64    // capacity.ul (bps)
    Combined      int64    // capacity.combined (bps)
    DLIdeal       int64    // capacity.dlIdeal (bps)
    ULIdeal       int64    // capacity.ulIdeal (bps)
    CombinedIdeal int64    // capacity.combinedIdeal (bps)
}

type LinkScore struct {
    DL    int    // linkScore.dl (0-100)
    UL    int    // linkScore.ul (0-100)
    DL2   int    // linkScore.dl2 (0-100)
    UL2   int    // linkScore.ul2 (0-100)
    DL24  int    // linkScore.dl24 (24-hour average)
    UL24  int    // linkScore.ul24 (24-hour average)
}
```

### Peer Stats (from AP's perspective) -> In-Memory

```go
// Path: stats[0].wireless.peers[]
type PeerStats struct {
    // Identity (from peer.common)
    MAC           string   // common.identification.mac
    IP            string   // common.mgmtIp
    Hostname      string   // common.hostname
    Firmware      string   // common.identification.firmware
    Model         string   // common.identification.model
    
    // Physical
    Distance      float64  // common.distance (meters)
    
    // Aggregate counters
    Counters struct {
        TxBytes   int64    // common.counters.txBytes
        RxBytes   int64    // common.counters.rxBytes
        TxRate    int64    // common.counters.txRate (bps)
        RxRate    int64    // common.counters.rxRate (bps)
        TxPackets int64    // common.counters.txPackets
        RxPackets int64    // common.counters.rxPackets
        TxPPS     int      // common.counters.txPPS
        RxPPS     int      // common.counters.rxPPS
    }
    
    // Aggregate link quality
    LinkScore     LinkScore  // common.linkQuality.linkScore
    
    // Traffic shaping (if configured)
    TrafficShaping struct {
        DLRate    int64    // common.trafficShaping.dlRate (kbps)
        ULRate    int64    // common.trafficShaping.ulRate (kbps)
    }
    
    // Per-radio signal (from peer.local[])
    Radio60GHz    PeerRadioStats  // local[id="main"]
    Radio5GHz     PeerRadioStats  // local[id="backup"]
    
    // Connection state
    Uptime        int64    // common.uptime
    ServiceUptime int64    // common.serviceUptime
}

type PeerRadioStats struct {
    ID              string   // local[].id
    Active          bool     // local[].active (true = currently carrying traffic)
    Connected       bool     // local[].connected
    LinkState       string   // local[].linkState ("active", "connected", "disconnected")
    ConnectionTime  int64    // local[].connectionTime (seconds)
    
    // Signal levels
    Signal          int      // local[].linkQuality.signal (dBm)
    SignalDay       int      // local[].linkQuality.signalDay (24h avg dBm)
    IdealSignal     int      // local[].linkQuality.idealSignal (dBm)
    NoiseFloor      int      // local[].linkQuality.noiseFloor (dBm, 5GHz only)

	// Remote signal levels (as reported by the peer / STA)
	// These are useful to distinguish AP TX issues (DL quality) from AP RX issues (UL quality).
	RemoteSignal     int      // remote[].linkQuality.signal (dBm)
	RemoteNoiseFloor int      // remote[].linkQuality.noiseFloor (dBm, 5GHz only)
    
	// Per-chain signals (5GHz only - 60GHz is single beam)
	SignalPerChain      []int // local[].linkQuality.signalPerChain (dBm per chain)
	SignalCombined      int   // computed from SignalPerChain when multiple chains exist
	IdealSignalPerChain []int // local[].linkQuality.idealSignalPerChain
	RemoteSignalPerChain []int // remote[].linkQuality.signalPerChain
	RemoteSignalCombined int   // computed from RemoteSignalPerChain when multiple chains exist

	// Calculated SNR (estimated, 5GHz only)
	// Wave reports noiseFloor as a long-term average, so treat this as an approximation.
	SNR       int // (signalCombined || signal) - noiseFloor
	RemoteSNR int // (remoteSignalCombined || remoteSignal) - remoteNoiseFloor
    
    // Modulation
    MCS struct {
        TxIdx       int      // local[].linkQuality.mcs.txIdx
        RxIdx       int      // local[].linkQuality.mcs.rxIdx
        TxRate      int      // local[].linkQuality.mcs.txRate (streams)
        RxRate      int      // local[].linkQuality.mcs.rxRate (streams)
        TxLabel     string   // local[].linkQuality.mcs.txLabel ("16QAM", "64QAM", etc)
        RxLabel     string   // local[].linkQuality.mcs.rxLabel
        TxIdxIdeal  int      // local[].linkQuality.mcs.txIdxIdeal
        RxIdxIdeal  int      // local[].linkQuality.mcs.rxIdxIdeal
    }
    
    // Airtime utilization
    Airtime struct {
        DL          float64  // local[].linkQuality.airtime.dl (percent)
        UL          float64  // local[].linkQuality.airtime.ul (percent)
    }
    
    // Link quality scores
    LinkScore       LinkScore  // local[].linkQuality.linkScore
    Capacity        CapacityStats  // local[].linkQuality.capacity
}
```

### Interface Stats -> In-Memory

```go
// Path: stats[0].interfaces[]
type InterfaceStats struct {
    ID        string   // interfaces[].id ("wlan0", "ath0", "eth0", "eth1", "br0")
    Name      string   // interfaces[].name
    Type      string   // interfaces[].status.type ("wireless", "ethernet", "bridge")
    
    Enabled   bool     // interfaces[].status.enabled
    Plugged   bool     // interfaces[].status.plugged (ethernet only)
    Speed     string   // interfaces[].status.speed
    MTU       int      // interfaces[].status.mtu
    
    Stats struct {
        TxBytes   int64    // interfaces[].statistics.txBytes
        RxBytes   int64    // interfaces[].statistics.rxBytes
        TxRate    int64    // interfaces[].statistics.txRate (bps)
        RxRate    int64    // interfaces[].statistics.rxRate (bps)
        TxPackets int64    // interfaces[].statistics.txPackets
        RxPackets int64    // interfaces[].statistics.rxPackets
        TxPPS     int      // interfaces[].statistics.txPPS
        RxPPS     int      // interfaces[].statistics.rxPPS
        TxErrors  int64    // interfaces[].statistics.txErrors
        RxErrors  int64    // interfaces[].statistics.rxErrors
        TxDropped int64    // interfaces[].statistics.txDropped
        RxDropped int64    // interfaces[].statistics.rxDropped
    }
}
```

---

## Interface ID Reference

| ID | Name | Type | Notes |
|----|------|------|-------|
| `wlan0` | 60 GHz | wireless | Main radio interface |
| `ath0` | 5 GHz Backup | wireless | Backup radio interface |
| `eth0` | Ethernet Port | ethernet | GigE with PoE input |
| `eth1` | SFP+ Port | ethernet | SFP+ (on some models) |
| `br0` | Data | bridge | Data traffic bridge |
| `br0.X` | VLAN X | vlan | Management VLAN |

---

## Signal Level Interpretation

### 60 GHz Radio
- Excellent: > -55 dBm
- Good: -55 to -60 dBm
- Fair: -60 to -65 dBm
- Poor: -65 to -70 dBm
- Bad: < -70 dBm

### 5 GHz Radio
- Excellent: > -55 dBm
- Good: -55 to -65 dBm
- Fair: -65 to -75 dBm
- Poor: -75 to -85 dBm
- Bad: < -85 dBm

---

## Key Stats for Monitoring

### Critical (Alert on)
- `status` = offline (device unreachable)
- `signal` < threshold (link degradation)
- `linkScore.dl` or `linkScore.ul` < 30 (poor link quality)
- `airtime.dl` or `airtime.ul` > 80% (congestion)
- `channelUtilization.common.interference` > 50% (interference)

### Important (Dashboard)
- Per-chain signal variance > 6 dB (antenna issue)
- MCS index significantly below ideal (environmental interference)
- Capacity significantly below ideal (link quality issue)
- `serviceDowntime` increasing (instability)

### Informational
- Uptime
- Temperature
- GPS coordinates
- Traffic counters

---

## Example Stats Extraction (Go)

```go
func extractStats(data []byte) (*DeviceStats, error) {
    var resp []map[string]any
    if err := json.Unmarshal(data, &resp); err != nil {
        return nil, err
    }
    if len(resp) == 0 {
        return nil, fmt.Errorf("empty response")
    }
    
    raw := resp[0]
    stats := &DeviceStats{}
    
    // Device stats
    if device, ok := raw["device"].(map[string]any); ok {
        stats.Uptime = int64(device["uptime"].(float64))
        stats.PowerTime = int64(device["powerTime"].(float64))
        
        if ram, ok := device["ram"].(map[string]any); ok {
            stats.RAM.Total = int64(ram["total"].(float64))
            stats.RAM.Free = int64(ram["free"].(float64))
            stats.RAM.Usage = int(ram["usage"].(float64))
        }
        
        // ... etc
    }
    
    // Wireless stats
    if wireless, ok := raw["wireless"].(map[string]any); ok {
        if radios, ok := wireless["radios"].([]any); ok {
            for _, r := range radios {
                radio := r.(map[string]any)
                id := radio["id"].(string)
                
                if id == "main" {
                    stats.Radio60GHz = extractRadioStats(radio)
                } else if id == "backup" {
                    stats.Radio5GHz = extractRadioStats(radio)
                }
            }
        }
        
        if peers, ok := wireless["peers"].([]any); ok {
            stats.PeerCount = len(peers)
            for _, p := range peers {
                peer := p.(map[string]any)
                stats.Peers = append(stats.Peers, extractPeerStats(peer))
            }
        }
    }
    
    return stats, nil
}
```

---

## LTU Device Support (TODO)

LTU devices (LTU-LR, LTU-Rocket, etc.) have a hybrid API:
- Wave-style endpoints: `/api/v1.0/device`, `/api/v1.0/statistics`
- AirMax-style endpoints: `/status.cgi`, `/stats.cgi`

Field mapping to be documented after API samples collected.

---

## LTU Device Support

LTU devices use the same Wave-style API (`/api/v1.0/device`, `/api/v1.0/statistics`).

### LTU Identification

```
identification.mac             -> devices.mac
identification.firmware        -> "aflturocket.amesoc3.v2.3.2.00015.240130.0801"
identification.firmwareVersion -> "2.3.2"
identification.model           -> "LTU-Rocket"
identification.product         -> "LTU-Rocket"
identification.family          -> "ltu"
```

### LTU Radio Stats

LTU has a single radio (`id: "main"`) with these key differences from Wave:

```go
// Path: stats[0].wireless.radios[id="main"]
type LTURadioStats struct {
    // Same as Wave RadioStats, but:
    // - No backup 5GHz radio
    // - Channel widths: 10, 20, 30, 40, 50, 60, 80, 100 MHz
    // - Frequency range: 5150-5925 MHz
}
```

### LTU Peer Stats (CINR instead of pure signal)

```go
// Path: stats[0].wireless.peers[].local[id="main"].linkQuality
type LTULinkQuality struct {
    Signal         int      // dBm
    SignalPerChain []int    // Per-chain dBm values
    
    // LTU-specific: Carrier-to-Interference-plus-Noise Ratio
    CINR struct {
        DL int  // Downlink CINR (dB)
        UL int  // Uplink CINR (dB)
    }
    
    // No noise floor in LTU - use CINR instead
}
```

### LTU Flavor Extraction

```go
func extractLTUFlavor(firmware string) string {
    // LTU AP: "aflturocket.amesoc3.v2.3.2..." -> "AFLTUROCKET"
    // LTU STA: "afltu.amesoc3.v2.4.0..." -> "AFLTU"
    // AF 5XHD: "af5xhd.amesoc3.v2.4.0..." -> "AF5XHD"
    parts := strings.Split(firmware, ".")
    if len(parts) > 0 {
        prefix := strings.ToLower(parts[0])
        if strings.HasPrefix(prefix, "afltu") || strings.HasPrefix(prefix, "af5x") || strings.HasPrefix(prefix, "af60") {
            return strings.ToUpper(parts[0])
        }
    }
    return ""
}
```

### Known LTU/AirFiber Flavors

| Flavor | Device Type |
|--------|-------------|
| `AFLTUROCKET` | LTU-Rocket AP |
| `AFLTU` | LTU, LTU-LR (AP/STA) |
| `AF5XHD` | airFiber 5XHD |

### Platform Detection

```go
func detectPlatform(firmware string) string {
    lower := strings.ToLower(firmware)
    switch {
    case strings.HasPrefix(lower, "gmc.") || strings.HasPrefix(lower, "gmp.") || strings.HasPrefix(lower, "mgmp."):
        return "wave"
    case strings.HasPrefix(lower, "afltu"):
        return "ltu"
    case strings.HasPrefix(lower, "wa.") || strings.HasPrefix(lower, "xc.") || strings.HasPrefix(lower, "xw."):
        return "airmax"
    default:
        return "unknown"
    }
}
```
