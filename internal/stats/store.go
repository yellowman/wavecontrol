package stats

import (
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// NormalizeMAC converts a MAC address to lowercase for consistent storage and comparison.
// AirMAX/Wave devices may report MACs in either case, even varying between API calls.
// All MAC addresses should be normalized before storage or comparison.
func NormalizeMAC(mac string) string {
	return strings.ToLower(mac)
}

const ipKeyPrefix = "ip:"

func ipKey(ip string) string {
	return ipKeyPrefix + ip
}

// DeviceStatus is a tri-state status for a device in the in-memory store.
// It allows distinguishing "unknown" (reachable but unpollable) from "offline" (unreachable).
type DeviceStatus string

const (
	StatusOnline    DeviceStatus = "online"
	StatusUnknown   DeviceStatus = "unknown"
	StatusOffline   DeviceStatus = "offline"
	StatusUpgrading DeviceStatus = "upgrading"
)

func normalizeStatus(s DeviceStatus) DeviceStatus {
	switch s {
	case StatusOnline, StatusUnknown, StatusOffline, StatusUpgrading:
		return s
	default:
		return StatusUnknown
	}
}

// Store holds real-time device statistics in memory
type Store struct {
	mu      sync.RWMutex
	devices map[string]*DeviceStats // keyed by MAC (lowercase) when available; devices without MAC are keyed by "ip:<addr>"
	byMAC   map[string]string       // MAC -> IP lookup (effective management IP)
	byIP    map[string]string       // IP -> MAC lookup (for lookups without treating IP as identity)

	lastPoll     time.Time
	staleTimeout time.Duration // How long before a STA is considered stale

	// IP filter for management prefix enforcement
	// If set, only IPs that pass this filter are stored
	ipFilter func(string) bool

	// Throughput history (ring buffer for last 60 samples = 30 min at 30s intervals)
	throughputHistory []ThroughputSample
	throughputIndex   int
	throughputCount   int

	// Stability tracking (flaps and reboots per device)
	stability map[string]*StabilityStats
}

// DeviceStats holds all real-time stats for a device
type DeviceStats struct {
	// Identity
	DeviceID int    `json:"device_id,omitempty"`
	SiteID   int    `json:"site_id,omitempty"`
	MAC      string `json:"mac"`
	IP       string `json:"ip"`
	Hostname string `json:"hostname"`

	// Status
	Status DeviceStatus `json:"status"`
	// DBStatus mirrors Status as a string for UI code paths that expect db_status.
	// This is especially useful for websocket updates where the dashboard wants a
	// single canonical field.
	DBStatus     string `json:"db_status,omitempty"`
	StatusReason string `json:"status_reason,omitempty"`

	// Backwards-compat online flag (true only when Status == "online")
	Online    bool      `json:"online"`
	LastSeen  time.Time `json:"last_seen"`
	LastError string    `json:"last_error,omitempty"`

	// Device info
	Uptime    int64 `json:"uptime"`
	PowerTime int64 `json:"power_time"`

	// System resources
	CPU         []CPUCore `json:"cpu,omitempty"`
	CPUUsage    float64   `json:"cpu_usage"` // Overall CPU usage percentage
	RAM         RAMStats  `json:"ram"`
	MemUsage    float64   `json:"mem_usage"` // Overall memory usage percentage
	Temperature TempStats `json:"temperature"`

	// GPS
	GPS GPSStats `json:"gps,omitempty"`

	// Orientation (60GHz devices)
	Orientation *OrientationStats `json:"orientation,omitempty"`

	// Wireless stats
	Wireless WirelessStats `json:"wireless"`

	// Wireless configuration (from config endpoint, updated periodically)
	Config *WirelessConfig `json:"config,omitempty"`

	// Network configuration (VLAN/IP/DNS/MTU, etc.)
	Network *NetworkConfig `json:"network,omitempty"`

	// Interface stats
	Interfaces []InterfaceStats `json:"interfaces,omitempty"`

	// For STAs: parent AP info
	ParentIP  string `json:"parent_ip,omitempty"`
	ParentMAC string `json:"parent_mac,omitempty"`

	// Peer stats (for APs)
	PeerCount int          `json:"peer_count"`
	Peers     []*PeerStats `json:"peers,omitempty"`
}

// WirelessConfig holds parsed configuration features (from config endpoint)
type WirelessConfig struct {
	// Base mode
	Mode    string `json:"mode"`     // ap, sta
	NetMode string `json:"net_mode"` // ptp, ptmp

	// SSID
	SSID string `json:"ssid,omitempty"`

	// Frame mode
	FrameMode string `json:"frame_mode,omitempty"` // fixed, flex, or empty
	FrameLen  int    `json:"frame_len,omitempty"`  // Frame length (ms)
	DLRatio   int    `json:"dl_ratio,omitempty"`   // DL/UL ratio percentage

	// Feature flags
	GPSSync    bool `json:"gps_sync"`
	WaveAI     bool `json:"wave_ai"`
	AutoFreq60 bool `json:"auto_freq_60"` // Auto frequency on 60GHz
	AutoFreq6  bool `json:"auto_freq_6"`  // Auto frequency on 6GHz
	AutoFreq5  bool `json:"auto_freq_5"`  // Auto frequency on 5GHz
	Compat11N  bool `json:"compat_11n"`   // 802.11n compatibility
}

// NetworkConfig holds parsed network configuration (from config/system endpoints)
type NetworkConfig struct {
	Mode           string   `json:"mode,omitempty"` // bridge, router, etc
	MTU            int      `json:"mtu,omitempty"`
	MgmtVLAN       int      `json:"mgmt_vlan,omitempty"`
	DataIPv4CIDR   string   `json:"data_ipv4_cidr,omitempty"`
	DataIPv4Mode   string   `json:"data_ipv4_mode,omitempty"`
	DefaultGateway string   `json:"default_gateway,omitempty"`
	DNSServers     []string `json:"dns_servers,omitempty"`
}

type CPUCore struct {
	ID    string `json:"id"`
	Usage int    `json:"usage"`
}

type RAMStats struct {
	Total int64 `json:"total"`
	Free  int64 `json:"free"`
	Usage int   `json:"usage"`
}

type TempStats struct {
	CPU     float64 `json:"cpu"`
	Radio60 float64 `json:"radio_60,omitempty"`
	Radio6  float64 `json:"radio_6,omitempty"`
	Radio5  float64 `json:"radio_5,omitempty"`
	Board   float64 `json:"board,omitempty"`
}

type GPSStats struct {
	Fix  bool    `json:"fix"`
	Lat  float64 `json:"lat"`
	Lon  float64 `json:"lon"`
	Alt  float64 `json:"alt"`
	Sats int     `json:"sats"`
}

type OrientationStats struct {
	Tilt   float64 `json:"tilt"`
	Roll   float64 `json:"roll"`
	Tilt24 float64 `json:"tilt24,omitempty"` // 24h average
	Roll24 float64 `json:"roll24,omitempty"` // 24h average
}

type WirelessStats struct {
	ServiceUptime   int64 `json:"service_uptime"`
	ServiceDowntime int64 `json:"service_downtime"`

	// Aggregate link quality
	TxRate    int64      `json:"tx_rate"`
	RxRate    int64      `json:"rx_rate"`
	LinkScore *LinkScore `json:"link_score,omitempty"`

	// Per-radio stats
	Radio60GHz *RadioStats `json:"radio_60ghz,omitempty"` // Wave AP/LR main radio
	Radio5GHz  *RadioStats `json:"radio_5ghz,omitempty"`  // Wave AP/LR backup radio
	Radio6GHz  *RadioStats `json:"radio_6ghz,omitempty"`  // MLO 6GHz backup radio
	RadioLTU   *RadioStats `json:"radio_ltu,omitempty"`   // LTU main radio
	// Radios contains all parsed radios in a stable order (when available).
	//
	// This is primarily used for multi-radio devices like Wave MLO (e.g. MLO5: 2x 5 GHz,
	// MLO6: 5 + 6 GHz) where the fixed slots (radio_5ghz, radio_6ghz, radio_60ghz)
	// are not enough to represent every interface.
	Radios []RadioStats `json:"radios,omitempty"`

	AirViewUtilization []AirViewUtilizationPoint `json:"airview_utilization,omitempty"`
}

type AirViewUtilizationPoint struct {
	FrequencyMHz int64 `json:"frequency_mhz"`
	UsagePct     int64 `json:"usage_pct"`
}

type AFCStats struct {
	Label      string          `json:"label,omitempty"`
	Status     string          `json:"status,omitempty"`
	Type       string          `json:"type,omitempty"`
	Detail     string          `json:"detail,omitempty"`
	Reason     string          `json:"reason,omitempty"`
	ExpiryMs   int64           `json:"expiry_ms,omitempty"`
	Regulatory []AFCRegulatory `json:"regulatory,omitempty"`
}

type AFCRegulatory struct {
	ChannelWidthMHz int64          `json:"channel_width_mhz,omitempty"`
	Channels        []AFCChannel   `json:"channels,omitempty"`
	FreqRanges      []AFCFreqRange `json:"freq_ranges,omitempty"`
}

type AFCChannel struct {
	CenterMHz  int64 `json:"center_mhz,omitempty"`
	MaxEIRPDbm int64 `json:"max_eirp_dbm,omitempty"`
}

type AFCFreqRange struct {
	StartMHz int64 `json:"start_mhz,omitempty"`
	EndMHz   int64 `json:"end_mhz,omitempty"`
}

type RadioStats struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	LinkState string `json:"link_state"`
	// DisplayBandOverride is an optional UI hint. When set, the UI should prefer
	// this label over the implicit band derived from which slot the radio is
	// stored in (e.g. when a Wave MLO5 has two 5GHz radios and the second one is
	// stored into the Radio6GHz slot).
	DisplayBandOverride string `json:"display_band_override,omitempty"`

	Frequency    int `json:"frequency"`
	ChannelWidth int `json:"channel_width"`
	ChannelBW    int `json:"channel_bw,omitempty"` // Alias for AirMAX

	TxPower     int `json:"tx_power"`
	TxPowerEIRP int `json:"tx_power_eirp"`

	// Signal (device-level for STAs, or AirMAX)
	Signal         int    `json:"signal,omitempty"`
	SignalCombined int    `json:"signal_combined,omitempty"` // MRC combined from per-chain
	SignalQuality  string `json:"signal_quality,omitempty"`  // excellent/good/fair/poor
	RSSI           int    `json:"rssi,omitempty"`
	NoiseFloor     int    `json:"noise_floor,omitempty"`
	SignalPerChain []int  `json:"signal_per_chain,omitempty"` // Per-chain dBm

	// Link distance (AirFiber point-to-point)
	Distance float64 `json:"distance,omitempty"` // meters

	// LTU AP-specific
	DLRatio      int     `json:"dl_ratio,omitempty"`      // DL/UL ratio percentage
	FrameLength  float64 `json:"frame_length,omitempty"`  // Frame length in ms
	RxEfficiency float64 `json:"rx_efficiency,omitempty"` // RX efficiency percentage

	Antenna struct {
		Name string `json:"name"`
		Gain int    `json:"gain"`
	} `json:"antenna"`

	Capacity    *CapacityStats `json:"capacity,omitempty"`
	Utilization *Utilization   `json:"utilization,omitempty"`

	// GPS sync (Wave 60GHz)
	GPSSyncState int `json:"gps_sync_state,omitempty"`

	// DFS (5GHz)
	DFS *DFSStats `json:"dfs,omitempty"`
	AFC *AFCStats `json:"afc,omitempty"`
}

type CapacityStats struct {
	DL            int64 `json:"dl"`
	UL            int64 `json:"ul"`
	Combined      int64 `json:"combined"`
	DLIdeal       int64 `json:"dl_ideal"`
	ULIdeal       int64 `json:"ul_ideal"`
	CombinedIdeal int64 `json:"combined_ideal"`
}

type LinkScore struct {
	DL   int `json:"dl"`
	UL   int `json:"ul"`
	DL2  int `json:"dl2"`
	UL2  int `json:"ul2"`
	DL24 int `json:"dl24,omitempty"`
	UL24 int `json:"ul24,omitempty"`
}

type Utilization struct {
	DL           float64 `json:"dl"`
	UL           float64 `json:"ul"`
	Interference float64 `json:"interference"`
}

type DFSStats struct {
	Enabled      bool `json:"enabled"`
	CACDuration  int  `json:"cac_duration"`
	CACRemaining int  `json:"cac_remaining"`
}

type InterfaceStats struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Type            string `json:"type"`
	Enabled         bool   `json:"enabled"`
	Plugged         bool   `json:"plugged,omitempty"`
	Status          string `json:"status,omitempty"`
	Description     string `json:"description,omitempty"`
	CurrentSpeed    string `json:"current_speed,omitempty"`
	ConfiguredSpeed string `json:"configured_speed,omitempty"`
	Speed           int    `json:"speed,omitempty"`

	TxBytes   int64 `json:"tx_bytes"`
	RxBytes   int64 `json:"rx_bytes"`
	TxRate    int64 `json:"tx_rate"`
	RxRate    int64 `json:"rx_rate"`
	TxPackets int64 `json:"tx_packets"`
	RxPackets int64 `json:"rx_packets"`
	TxErrors  int64 `json:"tx_errors"`
	RxErrors  int64 `json:"rx_errors"`
}

// PeerStats holds stats for a connected STA (from AP's perspective)
type PeerStats struct {
	// Identity
	MAC      string `json:"mac"`
	IP       string `json:"ip"`
	Hostname string `json:"hostname"`
	Firmware string `json:"firmware"`
	// FirmwareFull preserves the raw/extended firmware string (often includes flavor/platform)
	// which is useful for flavor detection. For Wave peers this is typically the
	// "identification.firmware" value (e.g. "GMC.ipq5018.v4.1.0....") while the
	// Firmware field may contain the short "firmwareVersion" (e.g. "v4.1.0.00017").
	FirmwareFull string `json:"firmware_full,omitempty"`
	Model        string `json:"model"`

	// Connection
	Distance       float64 `json:"distance"` // meters
	ConnectionTime int64   `json:"connection_time"`
	Uptime         int64   `json:"uptime"`
	PowerTime      int64   `json:"power_time,omitempty"`
	ServiceUptime  int64   `json:"service_uptime,omitempty"`
	NetMode        string  `json:"net_mode,omitempty"` // router, bridge

	// GPS (from peer/STA device)
	GPS GPSStats `json:"gps,omitempty"`

	// Orientation
	Orientation *OrientationStats `json:"orientation,omitempty"`

	// Hardware stats from STA
	CPU         []CPUCore `json:"cpu,omitempty"`
	RAM         RAMStats  `json:"ram,omitempty"`
	Temperature float64   `json:"temperature,omitempty"`
	CarrierDrop bool      `json:"carrier_drop,omitempty"`

	// Signal (aggregate / AirMAX) - AP's RX from this STA
	Signal     int `json:"signal,omitempty"`
	RSSI       int `json:"rssi,omitempty"`
	NoiseFloor int `json:"noise_floor,omitempty"`
	TXPower    int `json:"tx_power,omitempty"`

	// Remote signal (STA's RX from AP) - AirMAX only
	// This is what the STA reports receiving from the AP
	RemoteSignal         int    `json:"remote_signal,omitempty"`
	RemoteSignalCombined int    `json:"remote_signal_combined,omitempty"` // Combined from per-chain
	RemoteSignalQuality  string `json:"remote_signal_quality,omitempty"`  // excellent/good/fair/poor
	RemoteNoiseFloor     int    `json:"remote_noise_floor,omitempty"`
	RemoteSignalPerChain []int  `json:"remote_signal_per_chain,omitempty"`

	// Counters
	TxBytes   int64 `json:"tx_bytes"`
	RxBytes   int64 `json:"rx_bytes"`
	TxRate    int64 `json:"tx_rate"`
	RxRate    int64 `json:"rx_rate"`
	TxPackets int64 `json:"tx_packets"`
	RxPackets int64 `json:"rx_packets"`

	// Traffic shaping (if configured)
	DLRateLimit int64 `json:"dl_rate_limit,omitempty"`
	ULRateLimit int64 `json:"ul_rate_limit,omitempty"`

	// Per-radio signal stats
	Radio60GHz *PeerRadioStats `json:"radio_60ghz,omitempty"`
	Radio5GHz  *PeerRadioStats `json:"radio_5ghz,omitempty"`
	Radio6GHz  *PeerRadioStats `json:"radio_6ghz,omitempty"` // MLO 6GHz radio
	RadioLTU   *PeerRadioStats `json:"radio_ltu,omitempty"`

	// Aggregate link score
	LinkScore *LinkScore `json:"link_score,omitempty"`

	// STA interface stats
	Interfaces []InterfaceStats `json:"interfaces,omitempty"`
}

// PeerRadioStats holds per-radio signal stats for a peer
type PeerRadioStats struct {
	ID        string `json:"id"`
	Active    bool   `json:"active"`
	Connected bool   `json:"connected"`
	LinkState string `json:"link_state"`

	// Signal levels (AP RX - what AP receives from STA)
	Signal         int    `json:"signal"`
	SignalCombined int    `json:"signal_combined,omitempty"` // Combined from per-chain (pre-computed)
	SignalQuality  string `json:"signal_quality,omitempty"`  // excellent/good/fair/poor
	SignalDay      int    `json:"signal_day,omitempty"`
	IdealSignal    int    `json:"ideal_signal,omitempty"`
	NoiseFloor     int    `json:"noise_floor,omitempty"`
	SignalPerChain []int  `json:"signal_per_chain,omitempty"`

	// Remote signal levels (STA RX - what STA receives from AP)
	RemoteSignal         int    `json:"remote_signal,omitempty"`
	RemoteSignalCombined int    `json:"remote_signal_combined,omitempty"` // Combined from per-chain
	RemoteSignalQuality  string `json:"remote_signal_quality,omitempty"`  // excellent/good/fair/poor
	RemoteIdealSignal    int    `json:"remote_ideal_signal,omitempty"`
	RemoteNoiseFloor     int    `json:"remote_noise_floor,omitempty"`
	RemoteSignalPerChain []int  `json:"remote_signal_per_chain,omitempty"`

	// Signal histogram for charts
	SignalHistogram *SignalHistogram `json:"signal_histogram,omitempty"`

	// Calculated
	// Note: for Wave, noise_floor is a long-term average; treat SNR as an estimate.
	SNR       int `json:"snr,omitempty"`        // signal - noise_floor
	RemoteSNR int `json:"remote_snr,omitempty"` // remote_signal - remote_noise_floor

	// Connection time (seconds connected on this radio)
	ConnectionTime int64 `json:"connection_time,omitempty"`

	// Quality metrics (availability varies by platform)
	CINR *CINRStats `json:"cinr,omitempty"`
	EVM  *EVMStats  `json:"evm,omitempty"`

	// Modulation
	MCS *MCSStats `json:"mcs,omitempty"`

	// Airtime
	AirtimeDL float64 `json:"airtime_dl,omitempty"`
	AirtimeUL float64 `json:"airtime_ul,omitempty"`

	// Capacity
	Capacity  *CapacityStats `json:"capacity,omitempty"`
	LinkScore *LinkScore     `json:"link_score,omitempty"`
}

type SignalHistogram struct {
	Histogram []int `json:"histogram"`
	MinSignal int   `json:"min_signal"`
	MaxSignal int   `json:"max_signal"`
	Period    int   `json:"period"` // seconds
}

type CINRStats struct {
	DL int `json:"dl"`
	UL int `json:"ul"`
}

// EVMStats represents Error Vector Magnitude (EVM) measurements.
//
// Directionality:
//   - DL is the AP -> STA direction (what the STA receives / AP transmits).
//   - UL is the STA -> AP direction (what the AP receives / STA transmits).
//
// Interpretation:
//   - Higher values generally indicate better modulation quality.
//   - Availability varies by platform (currently parsed for airMAX; absent for Wave and many LTU payloads).
type EVMStats struct {
	DL float64 `json:"dl"`
	UL float64 `json:"ul"`
}

type MCSStats struct {
	TxIdx      int    `json:"tx_idx"`
	RxIdx      int    `json:"rx_idx"`
	TxLabel    string `json:"tx_label"`
	RxLabel    string `json:"rx_label"`
	TxRate     int    `json:"tx_rate"`
	RxRate     int    `json:"rx_rate"`
	TxIdxIdeal int    `json:"tx_idx_ideal,omitempty"`
	RxIdxIdeal int    `json:"rx_idx_ideal,omitempty"`
}

// ThroughputSample holds a point-in-time network throughput measurement
type ThroughputSample struct {
	Timestamp time.Time `json:"timestamp"`
	TxRate    int64     `json:"tx_rate"`
	RxRate    int64     `json:"rx_rate"`
	Online    int       `json:"online"`
	Offline   int       `json:"offline"`
}

// StabilityStats tracks flapping and reboots for a device
type StabilityStats struct {
	IP          string      `json:"ip"`
	Hostname    string      `json:"hostname"`
	LastUptime  int64       `json:"last_uptime"`
	LastOnline  bool        `json:"last_online"`
	LastSeen    time.Time   `json:"last_seen"`
	Flaps1h     int         `json:"flaps_1h"`    // online->offline transitions in last hour
	Flaps24h    int         `json:"flaps_24h"`   // online->offline transitions in last 24h
	Reboots1h   int         `json:"reboots_1h"`  // detected reboots in last hour
	Reboots24h  int         `json:"reboots_24h"` // detected reboots in last 24h
	FlapTimes   []time.Time `json:"-"`           // timestamps of flaps (for rolling window)
	RebootTimes []time.Time `json:"-"`           // timestamps of reboots (for rolling window)
}

const (
	throughputHistorySize = 60 // 60 samples = 30 min at 30s intervals
)

// NewStore creates a new stats store
func NewStore() *Store {
	return &Store{
		devices:           make(map[string]*DeviceStats),
		byMAC:             make(map[string]string),
		byIP:              make(map[string]string),
		staleTimeout:      5 * time.Minute,
		throughputHistory: make([]ThroughputSample, throughputHistorySize),
		stability:         make(map[string]*StabilityStats),
	}
}

// SetIPFilter sets the IP filter function for management prefix enforcement.
// If set, only IPs that pass the filter will be stored for STAs.
// Pass nil to disable filtering (allow all IPs).
func (s *Store) SetIPFilter(fn func(string) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ipFilter = fn
}

// isIPAllowed checks if an IP passes the filter (or if no filter is set)
func (s *Store) isIPAllowed(ip string) bool {
	if s.ipFilter == nil {
		return true
	}
	return ip != "" && s.ipFilter(ip)
}

// GetIPByMAC returns the current effective IP for a MAC address.
// Returns empty string if MAC not found or has no IP.
func (s *Store) GetIPByMAC(mac string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	mac = NormalizeMAC(mac)
	if ip, ok := s.byMAC[mac]; ok {
		return ip
	}
	return ""
}

// SetIPForMAC sets or updates the IP for a MAC address.
// Used to seed the memory store from the database on restart.
// Only sets the IP if it passes the IP filter (if configured).
// Returns true if the IP was set/changed.
// SetIPForMAC sets or updates the effective management IP for a MAC address.
// Used to seed the memory store from the database on restart and to record learned STA management IPs.
// Only sets the IP if it passes the IP filter (if configured).
// Returns true if the IP mapping was set/changed.
func (s *Store) SetIPForMAC(mac, ip string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	mac = NormalizeMAC(mac)
	if mac == "" {
		return false
	}

	// Check if IP passes filter
	if s.ipFilter != nil && ip != "" && !s.ipFilter(ip) {
		return false
	}

	oldIP := s.byMAC[mac]
	if oldIP == ip {
		return false // No change
	}

	// If this MAC previously mapped to a different IP, clear the reverse lookup
	if oldIP != "" {
		if m, ok := s.byIP[oldIP]; ok && m == mac {
			delete(s.byIP, oldIP)
		}
	}

	// If the new IP is already associated to a different MAC, clear that association.
	// IP is not authoritative; MAC is. This prevents accidental cross-linking on IP reuse.
	if ip != "" {
		if otherMAC, ok := s.byIP[ip]; ok && otherMAC != "" && otherMAC != mac {
			// Clear the other MAC's forward mapping if it pointed at this IP
			if cur, ok := s.byMAC[otherMAC]; ok && cur == ip {
				s.byMAC[otherMAC] = ""
			}
		}
		s.byIP[ip] = mac
	}

	s.byMAC[mac] = ip

	// If we already have a stats entry for this MAC, keep its IP up to date.
	if ds, ok := s.devices[mac]; ok && ds != nil {
		ds.IP = ip
	}

	// If there is an IP-only entry for this IP, remove it to avoid two identities for the same device
	if ip != "" {
		delete(s.devices, ipKey(ip))
	}

	return true
}

// Update updates stats for a device.
// Devices are keyed by MAC when available; if no MAC is present, the entry is keyed by IP ("ip:<addr>").
// Returns true if the device transitioned from offline/unknown -> online.
func (s *Store) Update(ip string, stats *DeviceStats) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	stats.IP = ip
	stats.Status = StatusOnline
	stats.DBStatus = string(stats.Status)
	stats.StatusReason = ""
	stats.Online = true
	stats.LastSeen = now
	stats.LastError = ""

	// Normalize MAC for consistent storage and lookup
	if stats.MAC != "" {
		stats.MAC = NormalizeMAC(stats.MAC)
	}

	key := ipKey(ip)
	if stats.MAC != "" {
		key = stats.MAC
	}

	// Determine previous online state (to detect transitions). If a poll initially
	// bound identity by IP and the successful parser later supplies a MAC, bridge
	// the identity from the IP placeholder before replacing it with the MAC-keyed row.
	wasOnline := false
	existing, existingOK := s.devices[key]
	if (!existingOK || existing == nil) && stats.MAC != "" && ip != "" {
		if byIP, ok := s.devices[ipKey(ip)]; ok && byIP != nil {
			existing = byIP
			existingOK = true
		}
	}
	if existingOK && existing != nil {
		if existing.Status != "" {
			wasOnline = existing.Status == StatusOnline
		} else {
			wasOnline = existing.Online
		}
		// Preserve inventory identity bound by the poller/job. Parsers do not know
		// database ids, but alerting and site-scoped rules require them.
		if stats.DeviceID == 0 && existing.DeviceID != 0 {
			stats.DeviceID = existing.DeviceID
		}
		if stats.SiteID == 0 && existing.SiteID != 0 {
			stats.SiteID = existing.SiteID
		}
		// Preserve parsed config if new poll didn't include it
		if stats.Config == nil && existing.Config != nil {
			stats.Config = existing.Config
		}
	}

	// If we are moving from an IP-only placeholder to a MAC-keyed entry, remove the placeholder
	if stats.MAC != "" {
		delete(s.devices, ipKey(ip))
	}

	// Update MAC/IP lookup maps when MAC is known
	if stats.MAC != "" {
		mac := stats.MAC

		// If this IP was previously mapped to a different MAC, clear that old mapping
		if oldMAC, ok := s.byIP[ip]; ok && oldMAC != "" && oldMAC != mac {
			if cur, ok := s.byMAC[oldMAC]; ok && cur == ip {
				s.byMAC[oldMAC] = ""
			}
		}

		// If this MAC previously had a different IP, clear the reverse lookup for the old IP
		if oldIP, ok := s.byMAC[mac]; ok && oldIP != "" && oldIP != ip {
			if m, ok := s.byIP[oldIP]; ok && m == mac {
				delete(s.byIP, oldIP)
			}
		}

		s.byMAC[mac] = ip
		if ip != "" {
			s.byIP[ip] = mac
		}
	}

	// Normalize/sort interface list to keep UI stable across polls.
	if len(stats.Interfaces) > 1 {
		sort.SliceStable(stats.Interfaces, func(i, j int) bool {
			ai := stats.Interfaces[i]
			aj := stats.Interfaces[j]
			ki := strings.ToLower(ai.ID)
			kj := strings.ToLower(aj.ID)
			if ki == "" {
				ki = strings.ToLower(ai.Name)
			}
			if kj == "" {
				kj = strings.ToLower(aj.Name)
			}
			if ki == kj {
				return strings.ToLower(ai.Name) < strings.ToLower(aj.Name)
			}
			return ki < kj
		})
	}
	s.devices[key] = stats
	return !wasOnline
}

// UpdatePeers updates peer stats for an AP and also creates/updates STA entries.
// Respects the IP filter: only IPs that pass the filter are stored as effective STA management IPs.
// Returns a map of MAC -> newIP for STAs that had their IP updated (for DB sync).
func (s *Store) UpdatePeers(apMAC, apIP string, peers []*PeerStats) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Track which STAs had IP changes (for DB sync)
	ipChanges := make(map[string]string)

	apMAC = NormalizeMAC(apMAC)

	// Update AP's peer list (best-effort)
	if apMAC != "" {
		if ap, ok := s.devices[apMAC]; ok && ap != nil {
			ap.PeerCount = len(peers)
			ap.Peers = peers
		}
		// Keep MAC/IP mapping for the AP up to date
		if apIP != "" {
			// Clear conflicting mapping if this IP was already associated to another MAC
			if otherMAC, ok := s.byIP[apIP]; ok && otherMAC != "" && otherMAC != apMAC {
				if cur, ok := s.byMAC[otherMAC]; ok && cur == apIP {
					s.byMAC[otherMAC] = ""
				}
			}
			if oldIP, ok := s.byMAC[apMAC]; ok && oldIP != "" && oldIP != apIP {
				if m, ok := s.byIP[oldIP]; ok && m == apMAC {
					delete(s.byIP, oldIP)
				}
			}
			s.byMAC[apMAC] = apIP
			s.byIP[apIP] = apMAC
		}
	} else if apIP != "" {
		// Fallback: if caller didn't provide AP MAC, try resolving from IP mapping
		if mapped, ok := s.byIP[apIP]; ok && mapped != "" {
			apMAC = mapped
			if ap, ok := s.devices[apMAC]; ok && ap != nil {
				ap.PeerCount = len(peers)
				ap.Peers = peers
			}
		} else if ap, ok := s.devices[ipKey(apIP)]; ok && ap != nil {
			ap.PeerCount = len(peers)
			ap.Peers = peers
		}
	}

	// Create/update STA entries
	for _, peer := range peers {
		// MAC is authoritative - skip peers without MAC
		if peer == nil || peer.MAC == "" {
			continue
		}

		peer.MAC = NormalizeMAC(peer.MAC)

		// Check if this IP is allowed by the filter
		ipAllowed := s.ipFilter == nil || (peer.IP != "" && s.ipFilter(peer.IP))

		sta, ok := s.devices[peer.MAC]
		if !ok || sta == nil {
			sta = &DeviceStats{MAC: peer.MAC, Status: StatusUnknown}
			s.devices[peer.MAC] = sta
		}

		currentIP := s.byMAC[peer.MAC]
		effectiveIP := currentIP
		if ipAllowed && peer.IP != "" {
			effectiveIP = peer.IP
		}

		// Update IP mappings if the effective IP changed and is allowed
		if effectiveIP != "" && effectiveIP != currentIP {
			// Remove reverse lookup for the old IP (if it pointed to this MAC)
			if currentIP != "" {
				if m, ok := s.byIP[currentIP]; ok && m == peer.MAC {
					delete(s.byIP, currentIP)
				}
			}
			// Clear conflicting mapping if this IP was associated to another MAC
			if otherMAC, ok := s.byIP[effectiveIP]; ok && otherMAC != "" && otherMAC != peer.MAC {
				if cur, ok := s.byMAC[otherMAC]; ok && cur == effectiveIP {
					s.byMAC[otherMAC] = ""
				}
			}
			s.byMAC[peer.MAC] = effectiveIP
			s.byIP[effectiveIP] = peer.MAC
			ipChanges[peer.MAC] = effectiveIP
		} else if _, ok := s.byMAC[peer.MAC]; !ok {
			// Ensure presence is tracked even if no IP is available/allowed
			s.byMAC[peer.MAC] = effectiveIP
		}

		// Update STA stats (always, regardless of whether we have an allowed IP)
		sta.MAC = peer.MAC
		sta.IP = effectiveIP
		sta.Hostname = peer.Hostname
		sta.Status = StatusOnline
		sta.DBStatus = string(sta.Status)
		sta.StatusReason = ""
		sta.Online = true
		sta.LastSeen = time.Now()
		sta.LastError = ""
		sta.ParentIP = apIP
		sta.ParentMAC = apMAC
		sta.Uptime = peer.Uptime

		// Copy throughput rates
		sta.Wireless.TxRate = peer.TxRate
		sta.Wireless.RxRate = peer.RxRate

		// Copy GPS from peer if available
		if peer.GPS.Fix && peer.GPS.Lat != 0 && peer.GPS.Lon != 0 {
			sta.GPS = peer.GPS
		}

		// Copy wireless stats from peer radio
		if peer.Radio60GHz != nil && peer.Radio60GHz.Active {
			sta.Wireless.Radio60GHz = peerRadioToRadioStats(peer.Radio60GHz)
		}
		if peer.Radio5GHz != nil && peer.Radio5GHz.Active {
			sta.Wireless.Radio5GHz = peerRadioToRadioStats(peer.Radio5GHz)
		}
		if peer.Radio6GHz != nil && peer.Radio6GHz.Active {
			sta.Wireless.Radio6GHz = peerRadioToRadioStats(peer.Radio6GHz)
		}
		if peer.RadioLTU != nil {
			sta.Wireless.RadioLTU = peerRadioToRadioStats(peer.RadioLTU)
		}
	}

	return ipChanges
}

// peerRadioToRadioStats converts per-peer radio stats (from an AP's peer list)
// into a simplified RadioStats struct suitable for the STA device entry.
//
// PeerRadioStats contains additional STA-remote fields (what the STA reports
// receiving from the AP) and histogram/diagnostics; RadioStats intentionally
// keeps only the core fields used by the UI and downstream logic.
func peerRadioToRadioStats(pr *PeerRadioStats) *RadioStats {
	if pr == nil {
		return nil
	}

	// Copy the per-chain slice so callers never share backing arrays with the
	// in-memory store.
	var chains []int
	if len(pr.SignalPerChain) > 0 {
		chains = make([]int, len(pr.SignalPerChain))
		copy(chains, pr.SignalPerChain)
	}

	// Capacity is a pointer; copy to avoid sharing.
	var capCopy *CapacityStats
	if pr.Capacity != nil {
		c := *pr.Capacity
		capCopy = &c
	}

	return &RadioStats{
		ID:             pr.ID,
		LinkState:      pr.LinkState,
		Signal:         pr.Signal,
		NoiseFloor:     pr.NoiseFloor,
		SignalPerChain: chains,
		Capacity:       capCopy,
	}
}

// BindIdentityByMAC associates a live stats entry with its inventory identity.
// Poll failure paths often update only status by MAC/IP, but alerting needs the
// database device_id/site_id on the in-memory DeviceStats row so rule state and
// alert rows can be keyed to the real inventory device instead of device_id=0.
func (s *Store) BindIdentityByMAC(mac, ip string, deviceID, siteID int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	mac = NormalizeMAC(mac)
	key := ""
	if mac != "" {
		key = mac
	} else if ip != "" {
		if mapped, ok := s.byIP[ip]; ok && mapped != "" {
			mac = mapped
			key = mapped
		} else {
			key = ipKey(ip)
		}
	}
	if key == "" {
		return
	}

	ds, ok := s.devices[key]
	if !ok || ds == nil {
		ds = &DeviceStats{}
		s.devices[key] = ds
	}
	if deviceID > 0 {
		ds.DeviceID = deviceID
	}
	if siteID > 0 {
		ds.SiteID = siteID
	}
	if mac != "" {
		ds.MAC = mac
	}
	if ip != "" {
		ds.IP = ip
	}

	if mac != "" && ip != "" {
		// If this IP was previously mapped to another MAC, clear the stale reverse link.
		if oldMAC, ok := s.byIP[ip]; ok && oldMAC != "" && oldMAC != mac {
			if cur, ok := s.byMAC[oldMAC]; ok && cur == ip {
				s.byMAC[oldMAC] = ""
			}
		}
		// If this MAC moved, clear the old IP reverse mapping.
		if oldIP, ok := s.byMAC[mac]; ok && oldIP != "" && oldIP != ip {
			if m, ok := s.byIP[oldIP]; ok && m == mac {
				delete(s.byIP, oldIP)
			}
		}
		s.byMAC[mac] = ip
		s.byIP[ip] = mac
		delete(s.devices, ipKey(ip))
	}
}

func (s *Store) SetStatusByMAC(mac, ip string, status DeviceStatus, reason, errMsg string, markSeen bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	status = normalizeStatus(status)
	mac = NormalizeMAC(mac)

	key := ""
	if mac != "" {
		key = mac
	} else {
		// Resolve by IP->MAC mapping if possible (prevents IP reuse from cross-linking devices)
		if ip != "" {
			if mapped, ok := s.byIP[ip]; ok && mapped != "" {
				mac = mapped
				key = mapped
			} else {
				key = ipKey(ip)
			}
		}
	}
	if key == "" {
		return false
	}

	ds, ok := s.devices[key]
	if !ok || ds == nil {
		ds = &DeviceStats{}
		s.devices[key] = ds
	}

	// If this entry is MAC-keyed, keep MAC and lookup maps consistent
	if mac != "" {
		ds.MAC = mac
		if ip != "" {
			// If this IP was previously mapped to another MAC, clear that old mapping
			if oldMAC, ok := s.byIP[ip]; ok && oldMAC != "" && oldMAC != mac {
				if cur, ok := s.byMAC[oldMAC]; ok && cur == ip {
					s.byMAC[oldMAC] = ""
				}
			}
			// If this MAC previously had a different IP, clear the reverse lookup for the old IP
			if oldIP, ok := s.byMAC[mac]; ok && oldIP != "" && oldIP != ip {
				if m, ok := s.byIP[oldIP]; ok && m == mac {
					delete(s.byIP, oldIP)
				}
			}
			s.byMAC[mac] = ip
			s.byIP[ip] = mac
		}
	}

	if ip != "" {
		ds.IP = ip
	}

	prevStatus := ds.Status
	if prevStatus == "" {
		if ds.Online {
			prevStatus = StatusOnline
		} else {
			prevStatus = StatusUnknown
		}
	}

	ds.Status = status
	ds.DBStatus = string(ds.Status)
	ds.StatusReason = reason
	ds.Online = status == StatusOnline
	if errMsg != "" {
		ds.LastError = errMsg
	} else if status == StatusOnline {
		ds.LastError = ""
	}
	if markSeen {
		ds.LastSeen = time.Now()
	}

	return prevStatus == StatusOnline && status != StatusOnline
}

// SetStatus sets a device's status using an IP address (legacy helper).
// Identity will be resolved to MAC when possible.
func (s *Store) SetStatus(ip string, status DeviceStatus, reason, errMsg string, markSeen bool) bool {
	return s.SetStatusByMAC("", ip, status, reason, errMsg, markSeen)
}

func (s *Store) SetUnknown(ip, reason, errMsg string) bool {
	return s.SetStatus(ip, StatusUnknown, reason, errMsg, true)
}

func (s *Store) SetUnknownByMAC(mac, ip, reason, errMsg string) bool {
	return s.SetStatusByMAC(mac, ip, StatusUnknown, reason, errMsg, true)
}

func (s *Store) SetOfflineWithReason(ip, reason, errMsg string) bool {
	return s.SetStatus(ip, StatusOffline, reason, errMsg, false)
}

func (s *Store) SetOfflineWithReasonByMAC(mac, ip, reason, errMsg string) bool {
	return s.SetStatusByMAC(mac, ip, StatusOffline, reason, errMsg, false)
}

func (s *Store) SetOffline(ip string, errMsg string) bool {
	return s.SetOfflineWithReason(ip, "", errMsg)
}

func (s *Store) SetOfflineByMAC(mac, ip, errMsg string) bool {
	return s.SetOfflineWithReasonByMAC(mac, ip, "", errMsg)
}

// copy creates a deep-ish copy of DeviceStats for safe concurrent access.
// We intentionally keep this reasonably lightweight because it is used heavily
// by API/UI paths; only fields that may be mutated concurrently are deep-copied.
func (s *DeviceStats) copy() *DeviceStats {
	if s == nil {
		return nil
	}

	// Shallow copy first, then deep-copy selected sub-structures.
	cp := *s

	// Deep copy WirelessConfig pointer to avoid sharing between goroutines
	// (poller can mutate config fields).
	if s.Config != nil {
		cfg := *s.Config
		cp.Config = &cfg
	}

	// Deep copy LinkScore pointer.
	if s.Wireless.LinkScore != nil {
		ls := *s.Wireless.LinkScore
		cp.Wireless.LinkScore = &ls
	}

	// Deep copy per-radio pointers used by the UI.
	copyRadio := func(r *RadioStats) *RadioStats {
		if r == nil {
			return nil
		}
		rc := *r
		if len(r.SignalPerChain) > 0 {
			rc.SignalPerChain = make([]int, len(r.SignalPerChain))
			copy(rc.SignalPerChain, r.SignalPerChain)
		}
		if r.Capacity != nil {
			c := *r.Capacity
			rc.Capacity = &c
		}
		if r.Utilization != nil {
			u := *r.Utilization
			rc.Utilization = &u
		}
		if r.DFS != nil {
			d := *r.DFS
			rc.DFS = &d
		}
		return &rc
	}
	cp.Wireless.Radio60GHz = copyRadio(s.Wireless.Radio60GHz)
	cp.Wireless.Radio5GHz = copyRadio(s.Wireless.Radio5GHz)
	cp.Wireless.Radio6GHz = copyRadio(s.Wireless.Radio6GHz)
	cp.Wireless.RadioLTU = copyRadio(s.Wireless.RadioLTU)

	// Deep copy slices.
	if len(s.CPU) > 0 {
		cp.CPU = make([]CPUCore, len(s.CPU))
		copy(cp.CPU, s.CPU)
	}
	if len(s.Interfaces) > 0 {
		cp.Interfaces = make([]InterfaceStats, len(s.Interfaces))
		copy(cp.Interfaces, s.Interfaces)
	}

	// Deep copy Orientation pointer.
	if s.Orientation != nil {
		orient := *s.Orientation
		cp.Orientation = &orient
	}

	// Deep copy Peers slice (shallow copy of nested pointers to keep this fast).
	if len(s.Peers) > 0 {
		cp.Peers = make([]*PeerStats, len(s.Peers))
		for i, p := range s.Peers {
			if p != nil {
				peerCopy := *p
				cp.Peers[i] = &peerCopy
			}
		}
	}

	return &cp
}

func (s *Store) Get(ip string) *DeviceStats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if ip == "" {
		return nil
	}
	// Prefer resolving to MAC first (authoritative identity)
	if mac, ok := s.byIP[ip]; ok && mac != "" {
		if ds, ok := s.devices[mac]; ok && ds != nil {
			return ds.copy() // Return copy to prevent data races
		}
	}
	// Fallback: IP-only devices (no MAC known)
	if ds, ok := s.devices[ipKey(ip)]; ok && ds != nil {
		return ds.copy() // Return copy to prevent data races
	}
	return nil
}

// GetByMAC returns stats for a device by MAC (authoritative lookup).
func (s *Store) GetByMAC(mac string) *DeviceStats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	mac = NormalizeMAC(mac)
	if mac == "" {
		return nil
	}
	if ds, ok := s.devices[mac]; ok && ds != nil {
		return ds.copy() // Return copy to prevent data races
	}
	return nil
}

func (s *Store) List() []*DeviceStats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*DeviceStats, 0, len(s.devices))
	for _, stats := range s.devices {
		result = append(result, stats.copy()) // Return copies to prevent data races
	}
	return result
}

// ListAPs returns stats for APs only (devices without parent)
func (s *Store) ListAPs() []*DeviceStats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*DeviceStats, 0)
	for _, stats := range s.devices {
		if stats.ParentIP == "" {
			result = append(result, stats.copy()) // Return copies to prevent data races
		}
	}
	return result
}

// ListSTAs returns stats for STAs of a given AP
func (s *Store) ListSTAs(apIP string) []*DeviceStats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*DeviceStats, 0)
	for _, stats := range s.devices {
		if stats.ParentIP == apIP {
			result = append(result, stats.copy()) // Return copies to prevent data races
		}
	}
	return result
}

// Remove removes a device from the store
// RemoveByMAC removes a device from the in-memory store by MAC (authoritative).
func (s *Store) RemoveByMAC(mac string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	mac = NormalizeMAC(mac)
	if mac == "" {
		return
	}
	if ip, ok := s.byMAC[mac]; ok && ip != "" {
		if m, ok := s.byIP[ip]; ok && m == mac {
			delete(s.byIP, ip)
		}
	}
	delete(s.byMAC, mac)
	delete(s.devices, mac)
}

// Remove removes a device from the in-memory store by IP (legacy helper).
// If the IP maps to a MAC, the MAC-keyed entry will be removed.
func (s *Store) Remove(ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if ip == "" {
		return
	}
	if mac, ok := s.byIP[ip]; ok && mac != "" {
		// Remove mappings and MAC-keyed device
		delete(s.byIP, ip)
		if cur, ok := s.byMAC[mac]; ok && cur == ip {
			delete(s.byMAC, mac)
		}
		delete(s.devices, mac)
		return
	}
	// IP-only device
	delete(s.devices, ipKey(ip))
}

func (s *Store) GetLastPoll() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastPoll
}

// SetLastPoll updates the last poll time
func (s *Store) SetLastPoll(t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastPoll = t
}

// Counts returns online/offline/total counts
func (s *Store) Counts() (online, offline, total int) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, stats := range s.devices {
		total++
		if stats.Online {
			online++
		} else {
			offline++
		}
	}
	return
}

// CountsByStatus returns online/offline/unknown/total counts from memory.
func (s *Store) CountsByStatus() (online, offline, unknown, total int) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, stats := range s.devices {
		total++
		switch stats.Status {
		case StatusOnline:
			online++
		case StatusOffline:
			offline++
		case StatusUnknown:
			unknown++
		default:
			// Backward compatibility: derive from Online bool.
			if stats.Online {
				online++
			} else {
				unknown++
			}
		}
	}
	return
}

// OfflineDevices returns list of offline devices from memory.
func (s *Store) OfflineDevices() []*DeviceStats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []*DeviceStats
	for _, stats := range s.devices {
		if stats.Status == StatusOffline {
			result = append(result, stats.copy())
		}
	}
	return result
}

// UnknownDevices returns list of unknown-status devices from memory.
func (s *Store) UnknownDevices() []*DeviceStats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []*DeviceStats
	for _, stats := range s.devices {
		// Treat empty status + !online as unknown for backward compatibility.
		if stats.Status == StatusUnknown || (stats.Status == "" && !stats.Online) {
			result = append(result, stats.copy())
		}
	}
	return result
}

// LastSeenBatch returns MAC -> LastSeen for all devices (for periodic DB sync)
func (s *Store) LastSeenBatch() map[string]time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make(map[string]time.Time, len(s.devices))
	for _, stats := range s.devices {
		if stats.MAC != "" {
			result[stats.MAC] = stats.LastSeen
		}
	}
	return result
}

// OnlineStatusBatch returns MAC -> Online for all devices (for DB sync)
func (s *Store) OnlineStatusBatch() map[string]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make(map[string]bool, len(s.devices))
	for _, stats := range s.devices {
		if stats.MAC != "" {
			result[stats.MAC] = stats.Online
		}
	}
	return result
}

// CleanStale removes STAs that haven't been seen recently
// Only removes child devices (STAs with ParentIP set), never APs
func (s *Store) CleanStale() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	cutoff := time.Now().Add(-s.staleTimeout)
	removed := 0

	for key, stats := range s.devices {
		if stats == nil {
			continue
		}
		// Only clean STAs (devices with a parent), not APs
		if (stats.ParentMAC != "" || stats.ParentIP != "") && stats.LastSeen.Before(cutoff) {
			mac := NormalizeMAC(stats.MAC)
			if mac != "" {
				// Clear MAC -> IP mapping
				delete(s.byMAC, mac)
				// Clear IP -> MAC mapping if it pointed to this MAC
				if stats.IP != "" {
					if m, ok := s.byIP[stats.IP]; ok && m == mac {
						delete(s.byIP, stats.IP)
					}
				}
			}
			delete(s.devices, key)
			removed++
		}
	}

	return removed
}

func (s *Store) SetStaleTimeout(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.staleTimeout = d
}

// RecordThroughputSample records current network throughput to history
func (s *Store) RecordThroughputSample() {
	s.mu.Lock()
	defer s.mu.Unlock()

	var totalTx, totalRx int64
	var online, offline int

	for _, stats := range s.devices {
		if stats.Online {
			online++
			totalTx += stats.Wireless.TxRate
			totalRx += stats.Wireless.RxRate
		} else {
			offline++
		}
	}

	sample := ThroughputSample{
		Timestamp: time.Now(),
		TxRate:    totalTx,
		RxRate:    totalRx,
		Online:    online,
		Offline:   offline,
	}

	s.throughputHistory[s.throughputIndex] = sample
	s.throughputIndex = (s.throughputIndex + 1) % throughputHistorySize
	if s.throughputCount < throughputHistorySize {
		s.throughputCount++
	}
}

// GetThroughputHistory returns the throughput history (oldest to newest)
func (s *Store) GetThroughputHistory() []ThroughputSample {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.throughputCount == 0 {
		return nil
	}

	result := make([]ThroughputSample, s.throughputCount)

	if s.throughputCount < throughputHistorySize {
		// Buffer not full yet, start from 0
		copy(result, s.throughputHistory[:s.throughputCount])
	} else {
		// Buffer is full, read from oldest to newest
		start := s.throughputIndex // oldest is at current index (will be overwritten next)
		for i := 0; i < throughputHistorySize; i++ {
			idx := (start + i) % throughputHistorySize
			result[i] = s.throughputHistory[idx]
		}
	}

	return result
}

// TrackStability updates flap/reboot tracking for a device (legacy boolean API).
// NOTE: This treats "online=false" as "offline". Call TrackStabilityStatus() for unknown vs offline.
func (s *Store) TrackStability(ip, hostname string, online bool, uptime int64) {
	if online {
		s.TrackStabilityStatus(ip, hostname, StatusOnline, uptime)
		return
	}
	s.TrackStabilityStatus(ip, hostname, StatusOffline, uptime)
}

// TrackStabilityStatus updates flap/reboot tracking for a device with tri-state status.
// Flaps are counted only for online -> offline transitions (NOT online -> unknown).
func (s *Store) TrackStabilityStatus(ip, hostname string, status DeviceStatus, uptime int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	hour := now.Add(-time.Hour)
	day := now.Add(-24 * time.Hour)

	status = normalizeStatus(status)
	online := status == StatusOnline

	// Key stability by MAC when possible to avoid mixing devices on IP reuse.
	key := ip
	if ip != "" {
		if mac, ok := s.byIP[ip]; ok && mac != "" {
			key = mac
		}
	}

	st, ok := s.stability[key]
	if !ok {
		st = &StabilityStats{
			IP:         ip,
			Hostname:   hostname,
			LastOnline: online,
			LastUptime: uptime,
			LastSeen:   now,
		}
		s.stability[key] = st
		return
	}

	// Always keep latest presentation fields up to date
	st.IP = ip
	st.Hostname = hostname
	st.LastSeen = now

	// Track flaps (online -> offline transitions)
	if st.LastOnline && !online {
		st.FlapTimes = append(st.FlapTimes, now)
	}

	// Track reboots (uptime decreases while still online)
	if online && st.LastOnline && uptime > 0 && st.LastUptime > 0 && uptime < st.LastUptime {
		st.RebootTimes = append(st.RebootTimes, now)
	}

	st.LastOnline = online
	st.LastUptime = uptime

	// Prune to rolling windows
	newFlaps := make([]time.Time, 0, len(st.FlapTimes))
	for _, t := range st.FlapTimes {
		if t.After(day) {
			newFlaps = append(newFlaps, t)
		}
	}
	st.FlapTimes = newFlaps

	newReboots := make([]time.Time, 0, len(st.RebootTimes))
	for _, t := range st.RebootTimes {
		if t.After(day) {
			newReboots = append(newReboots, t)
		}
	}
	st.RebootTimes = newReboots

	// Count 1h and 24h
	st.Flaps1h = 0
	st.Flaps24h = len(st.FlapTimes)
	for _, t := range st.FlapTimes {
		if t.After(hour) {
			st.Flaps1h++
		}
	}

	st.Reboots1h = 0
	st.Reboots24h = len(st.RebootTimes)
	for _, t := range st.RebootTimes {
		if t.After(hour) {
			st.Reboots1h++
		}
	}
}

func (s *Store) GetStabilityStats() []*StabilityStats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*StabilityStats, 0, len(s.stability))
	for _, st := range s.stability {
		// Copy to avoid race
		cp := *st
		result = append(result, &cp)
	}
	return result
}

// GetFlappingDevices returns devices with flaps in the last hour
func (s *Store) GetFlappingDevices(minFlaps int) []*StabilityStats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*StabilityStats, 0)
	for _, st := range s.stability {
		if st.Flaps1h >= minFlaps || st.Reboots1h > 0 {
			cp := *st
			result = append(result, &cp)
		}
	}
	return result
}

// Signal thresholds for quality classification
// MUST MATCH web/js/app.js SIGNAL_THRESHOLDS
const (
	Signal5GHzGood  = -62 // > -62 dBm = good
	Signal5GHzFair  = -70 // -62 to -70 dBm = fair, < -70 = poor
	Signal60GHzGood = -55 // > -55 dBm = good
	Signal60GHzFair = -65 // -55 to -65 dBm = fair, < -65 = poor
)

// CombineSignals computes combined signal from per-chain values using MRC
// (Maximum Ratio Combining) power addition formula.
//
// IMPORTANT: Input values must already be in dBm (negative values).
// For AirMAX devices, RSSI-to-dBm conversion must happen first via
// airmax.rssiToDbm() before calling this function.
//
// Formula: P_combined = 10 * log10(Σ 10^(P_chain/10))
//
// Example: Two -63 dBm signals combine to -60 dBm (3 dB gain from diversity)
func CombineSignals(chains []int) int {
	if len(chains) == 0 {
		return 0
	}
	if len(chains) == 1 {
		return chains[0]
	}

	// Filter out zero/invalid values
	var valid []int
	for _, c := range chains {
		if c < 0 {
			valid = append(valid, c)
		}
	}
	if len(valid) == 0 {
		return 0
	}
	if len(valid) == 1 {
		return valid[0]
	}

	// Power addition: P_total = P1 + P2 in linear scale
	// dBm to linear: 10^(dBm/10)
	// Linear to dBm: 10 * log10(linear)
	var linearSum float64
	for _, dBm := range valid {
		linearSum += math.Pow(10, float64(dBm)/10)
	}
	return int(math.Round(10 * math.Log10(linearSum)))
}

// SignalQuality5GHz returns signal quality string for 5GHz/LTU signals
func SignalQuality5GHz(signal int) string {
	if signal == 0 {
		return ""
	}
	if signal >= Signal5GHzGood+5 {
		return "excellent"
	}
	if signal > Signal5GHzGood {
		return "good"
	}
	if signal > Signal5GHzFair {
		return "fair"
	}
	return "poor"
}

// SignalQuality60GHz returns signal quality string for 60GHz signals
func SignalQuality60GHz(signal int) string {
	if signal == 0 {
		return ""
	}
	if signal >= Signal60GHzGood+5 {
		return "excellent"
	}
	if signal > Signal60GHzGood {
		return "good"
	}
	if signal > Signal60GHzFair {
		return "fair"
	}
	return "poor"
}
