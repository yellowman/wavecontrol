package airmax

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Client for AirMAX device API
type Client struct {
	httpClient *http.Client
	host       string
	scheme     string
	username   string
	password   string
	csrfToken  string // X-CSRF-ID token from login response
	csrfError  error  // Error from CSRF fetch attempt (for diagnostics)
}

// Status represents the full status.cgi response
type Status struct {
	Host          HostInfo                 `json:"host"`
	MCAStatus     *MCAStatus               `json:"mca_status,omitempty"`
	Wireless      WirelessInfo             `json:"wireless"`
	InterfacesRaw json.RawMessage          `json:"interfaces,omitempty"` // Can be map or array
	Interfaces    map[string]InterfaceInfo `json:"-"`                    // Parsed result
	Stations      []Station                `json:"stations,omitempty"`
	GPS           *GPSInfo                 `json:"gps,omitempty"`
	Airfiber      *AirfiberInfo            `json:"airfiber,omitempty"`
	Services      map[string]any           `json:"services,omitempty"`
}

type MCAStatus struct {
	Platform     string `json:"platform"`
	Firmware     string `json:"firmware"`
	ShortVersion string `json:"shortVersion"`
	DeviceModel  string `json:"deviceModel"`
	DeviceName   string `json:"deviceName"`
}

type HostInfo struct {
	Hostname    string  `json:"hostname"`
	DevModel    string  `json:"devmodel"`
	FWVersion   string  `json:"fwversion"`
	Uptime      int64   `json:"uptime"`
	Time        string  `json:"time"`
	Timestamp   int64   `json:"timestamp"`
	NetRole     string  `json:"netrole"`
	LoadAvg     float64 `json:"loadavg"`
	TotalRAM    int64   `json:"totalram"`
	FreeRAM     int64   `json:"freeram"`
	Temperature float64 `json:"temperature"`
	CPULoad     float64 `json:"cpuload"`
}

type InterfaceInfo struct {
	MAC     string `json:"mac"`
	IP      string `json:"ip"`
	Netmask string `json:"netmask"`
	Status  string `json:"status"`
	Speed   string `json:"speed"`
	Duplex  string `json:"duplex"`

	// Extended fields (present on some AirOS builds where interfaces are returned as an array with a status object)
	CurrentSpeed string `json:"current_speed,omitempty"`
	CableLength  int    `json:"cable_len,omitempty"`
	RxBytes      int64  `json:"rx_bytes,omitempty"`
	TxBytes      int64  `json:"tx_bytes,omitempty"`
	RxErrors     int64  `json:"rx_errors,omitempty"`
	TxErrors     int64  `json:"tx_errors,omitempty"`
	SNR          int    `json:"snr,omitempty"`
}

type WirelessInfo struct {
	ESSID string `json:"essid"`
	Mode  string `json:"mode"` // ap-ptmp, sta-ptmp, ap-ptp, sta-ptp (AirFiber uses "airfiber")
	// OpMode is used by AirFiber (e.g. "master" / "slave").
	OpMode      string          `json:"opmode,omitempty"`
	IEEEMode    string          `json:"ieeemode"`
	APMAC       string          `json:"apmac"`
	Frequency   int             `json:"frequency"`
	Center1Freq int             `json:"center1_freq"`
	ChanBW      int             `json:"chanbw"`
	TXPower     int             `json:"txpower"`
	NoiseF      int             `json:"noisef"`
	Security    string          `json:"security"`
	Distance    int             `json:"distance"`
	DFS         int             `json:"dfs"`
	Count       int             `json:"count"` // Number of connected stations
	AntennaGain int             `json:"antenna_gain"`
	Compat11N   int             `json:"compat_11n"`
	RXChainmask int             `json:"rx_chainmask"`
	TXChainmask int             `json:"tx_chainmask"`
	RXNSS       int             `json:"rx_nss"`
	TXNSS       int             `json:"tx_nss"`
	Polling     json.RawMessage `json:"polling,omitempty"` // Can be string "enabled" or object
	Stations    []Station       `json:"sta,omitempty"`

	// For STA mode - signal to AP
	Signal     int `json:"signal,omitempty"`
	RSSI       int `json:"rssi,omitempty"`
	NoiseFloor int `json:"noisefloor,omitempty"`

	// LTU specific
	SyncMode    int `json:"sync_mode,omitempty"`
	FrameLength int `json:"frame_length,omitempty"`
	DutyCycle   int `json:"duty_cycle,omitempty"`
	GPSState    int `json:"gps_state,omitempty"`
}

// WirelessInfo helper methods
func (w *WirelessInfo) GetFrequency() int {
	return w.Frequency
}

func (w *WirelessInfo) GetChanBW() int {
	return w.ChanBW
}

func (w *WirelessInfo) GetTXPower() int {
	return w.TXPower
}

func (w *WirelessInfo) GetNoiseF() int {
	return rssiToDbm(w.NoiseF)
}

func (w *WirelessInfo) GetSignal() int {
	return rssiToDbm(w.Signal)
}

func (w *WirelessInfo) GetRSSI() int {
	return rssiToDbm(w.RSSI)
}

func (w *WirelessInfo) GetNoiseFloor() int {
	return rssiToDbm(w.NoiseFloor)
}

func (w *WirelessInfo) GetCount() int {
	return w.Count
}

// GetPollingInfo parses the polling field which can be string or object
func (w *WirelessInfo) GetPollingInfo() *PollingInfo {
	if len(w.Polling) == 0 {
		return nil
	}
	// Check if it's a string like "enabled"
	if w.Polling[0] == '"' {
		return nil // It's just a string, not an object
	}
	var info PollingInfo
	if json.Unmarshal(w.Polling, &info) == nil {
		return &info
	}
	return nil
}

type PollingInfo struct {
	DCap       int  `json:"dcap"`
	UCap       int  `json:"ucap"`
	Use        int  `json:"use"`
	TXUse      int  `json:"tx_use"`
	RXUse      int  `json:"rx_use"`
	FixedFrame bool `json:"fixed_frame"`
	GPSSync    bool `json:"gps_sync"`
	FFFrameDur int  `json:"ff_frame_dur"`
	FFDLRatio  int  `json:"ff_dl_ratio"`
	FlexMode   bool `json:"flex_mode"`
}

type Station struct {
	MAC        string `json:"mac"`
	Name       string `json:"name"`
	LastIP     string `json:"lastip"`
	AssocID    int    `json:"associd"`
	APTS       int64  `json:"apts"`
	Signal     int    `json:"signal"`
	RSSI       int    `json:"rssi"`
	NoiseFloor int    `json:"noisefloor"`
	ChainRSSI  []int  `json:"chainrssi"`
	CCQ        int    `json:"ccq"`
	TX         int    `json:"tx"`
	RX         int    `json:"rx"`
	TXIdx      int    `json:"tx_idx,omitempty"`
	RXIdx      int    `json:"rx_idx,omitempty"`
	TXNSS      int    `json:"tx_nss,omitempty"`
	RXNSS      int    `json:"rx_nss,omitempty"`
	TXPower    int    `json:"txpower"`
	Distance   int    `json:"distance"`
	// Frequencies (if reported by the platform / firmware).
	Frequency   int   `json:"frequency,omitempty"`
	TxFrequency int   `json:"tx_frequency,omitempty"`
	RxFrequency int   `json:"rx_frequency,omitempty"`
	Uptime      int64 `json:"uptime"`
	TXBytes     int64 `json:"txbytes"`
	RXBytes     int64 `json:"rxbytes"`
	TXPackets   int64 `json:"txpackets"`
	RXPackets   int64 `json:"rxpackets"`

	// Nested stats format (some devices)
	Stats  *StationStats `json:"stats,omitempty"`
	AirMax *AirMaxStats  `json:"airmax,omitempty"`
	Remote *RemoteInfo   `json:"remote,omitempty"`
}

var stationNumberRe = regexp.MustCompile(`[-+]?\d+(?:\.\d+)?`)

func mapGetFirst(m map[string]any, keys ...string) (any, bool) {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			return v, true
		}
	}
	return nil, false
}

func anyToString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	case json.Number:
		return t.String()
	case float64:
		// Common for map[string]any numbers
		// Render without trailing decimals when possible.
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	}
	return ""
}

func anyToInt(v any) (int, bool) {
	switch t := v.(type) {
	case float64:
		return int(t), true
	case int:
		return t, true
	case int64:
		return int(t), true
	case json.Number:
		if i64, err := t.Int64(); err == nil {
			return int(i64), true
		}
		if f64, err := t.Float64(); err == nil {
			return int(f64), true
		}
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0, false
		}
		match := stationNumberRe.FindString(s)
		if match == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(match, 64)
		if err != nil {
			return 0, false
		}
		return int(f), true
	case map[string]any:
		// Some firmwares embed numeric values inside objects, e.g. {"rate":150}
		for _, k := range []string{"rate", "value", "val", "mbps", "Mbps", "bps", "rate_mbps", "tx_rate", "rx_rate"} {
			if inner, ok := t[k]; ok {
				if n, ok := anyToInt(inner); ok {
					return n, true
				}
			}
		}

	}
	return 0, false
}

func anyToInt64(v any) (int64, bool) {
	switch t := v.(type) {
	case float64:
		return int64(t), true
	case int:
		return int64(t), true
	case int64:
		return t, true
	case json.Number:
		if i64, err := t.Int64(); err == nil {
			return i64, true
		}
		if f64, err := t.Float64(); err == nil {
			return int64(f64), true
		}
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0, false
		}
		match := stationNumberRe.FindString(s)
		if match == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(match, 64)
		if err != nil {
			return 0, false
		}
		return int64(f), true
	case map[string]any:
		// Some firmwares embed numeric values inside objects, e.g. {"rate":150}
		for _, k := range []string{"rate", "value", "val", "mbps", "Mbps", "bps", "rate_mbps", "tx_rate", "rx_rate"} {
			if inner, ok := t[k]; ok {
				if n, ok := anyToInt64(inner); ok {
					return n, true
				}
			}
		}

	}
	return 0, false
}

func anyToIntSlice(v any) []int {
	switch t := v.(type) {
	case []any:
		out := make([]int, 0, len(t))
		for _, it := range t {
			if n, ok := anyToInt(it); ok {
				out = append(out, n)
			}
		}
		return out
	case []int:
		return append([]int(nil), t...)
	case []float64:
		out := make([]int, 0, len(t))
		for _, f := range t {
			out = append(out, int(f))
		}
		return out
	}
	return nil
}

func anyToBool(v any) (bool, bool) {
	switch t := v.(type) {
	case bool:
		return t, true
	case string:
		s := strings.TrimSpace(strings.ToLower(t))
		if s == "" {
			return false, false
		}
		switch s {
		case "1", "true", "yes", "y", "on", "enabled", "enable":
			return true, true
		case "0", "false", "no", "n", "off", "disabled", "disable":
			return false, true
		}
		b, err := strconv.ParseBool(s)
		if err == nil {
			return b, true
		}
		return false, false
	case float64:
		return t != 0, true
	case float32:
		return t != 0, true
	case int:
		return t != 0, true
	case int64:
		return t != 0, true
	case int32:
		return t != 0, true
	case uint:
		return t != 0, true
	case uint64:
		return t != 0, true
	case uint32:
		return t != 0, true
	default:
		return false, false
	}
}

func mapInt(m map[string]any, keys ...string) int {
	// NOTE: We intentionally do NOT use mapGetFirst() here.
	// Some AirOS firmwares include keys like "tx"/"rx" but the values are objects
	// or non-numeric, while the numeric rate is available under a different key
	// (e.g. "tx_rate"). If we stop on the first present key, we incorrectly return 0.
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		if n, ok := anyToInt(v); ok {
			return n
		}
	}
	return 0
}

func mapIntOK(m map[string]any, keys ...string) (int, bool) {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		if n, ok := anyToInt(v); ok {
			return n, true
		}
	}
	return 0, false
}

func mapInt64(m map[string]any, keys ...string) int64 {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		if n, ok := anyToInt64(v); ok {
			return n
		}
	}
	return 0
}

func anyToFloat64(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		if err == nil {
			return f, true
		}
		i, err := x.Int64()
		if err == nil {
			return float64(i), true
		}
	case string:
		// Extract a number from strings like "2.0ms" or "56MHz".
		s := strings.TrimSpace(x)
		if s == "" {
			return 0, false
		}
		// Try direct parse first.
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f, true
		}
		// Fallback: find the first number (supports decimals).
		re := regexp.MustCompile(`[-+]?\d+(?:\.\d+)?`)
		if match := re.FindString(s); match != "" {
			if f, err := strconv.ParseFloat(match, 64); err == nil {
				return f, true
			}
		}
	}
	return 0, false
}

func mapFloat64(m map[string]any, keys ...string) float64 {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		f, ok := anyToFloat64(v)
		if ok {
			return f
		}
	}
	return 0
}

func mapString(m map[string]any, keys ...string) string {
	if v, ok := mapGetFirst(m, keys...); ok {
		return strings.TrimSpace(anyToString(v))
	}
	return ""
}

func mapBool(m map[string]any, keys ...string) bool {
	for _, key := range keys {
		v, ok := mapGetFirst(m, key)
		if !ok {
			continue
		}
		b, ok := anyToBool(v)
		if ok {
			return b
		}
	}
	return false
}

func mapIntSlice(m map[string]any, key string) []int {
	if v, ok := m[key]; ok {
		return anyToIntSlice(v)
	}
	return nil
}

// UnmarshalJSON implements tolerant parsing for station list entries.
// AirOS station list fields (especially tx/rx rates) are not consistent across
// firmware versions: values may be ints, floats, or strings, and the key names
// can vary (tx/rx vs tx_rate/rx_rate, etc.).
func (s *Station) UnmarshalJSON(data []byte) error {
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}

	s.MAC = mapString(m, "mac")
	s.Name = mapString(m, "name")
	s.LastIP = mapString(m, "lastip", "last_ip", "ip")
	s.AssocID = mapInt(m, "associd")
	s.APTS = mapInt64(m, "apts")
	s.Signal = mapInt(m, "signal")
	s.RSSI = mapInt(m, "rssi")
	s.NoiseFloor = mapInt(m, "noisefloor", "noisef")
	s.ChainRSSI = mapIntSlice(m, "chainrssi")
	s.CCQ = mapInt(m, "ccq")

	// tx/rx modulation / PHY rate fields: try multiple keys.
	// Prefer *_rate over bare tx/rx because bare tx/rx is sometimes used for
	// traffic throughput in some firmwares, whereas *_rate is PHY/link rate.
	s.TX = mapInt(m, "tx_rate", "txrate", "txRate", "txrate_mbps", "tx_rate_mbps", "tx", "TX")
	s.RX = mapInt(m, "rx_rate", "rxrate", "rxRate", "rxrate_mbps", "rx_rate_mbps", "rx", "RX")

	// MCS / index fields
	s.TXIdx = -1
	s.RXIdx = -1
	if v, ok := mapIntOK(m, "tx_idx", "txidx", "tx_mcs", "txmcs", "txMcs"); ok {
		s.TXIdx = v
	}
	if v, ok := mapIntOK(m, "rx_idx", "rxidx", "rx_mcs", "rxmcs", "rxMcs"); ok {
		s.RXIdx = v
	}
	// Some firmware nests it.
	if s.TXIdx == -1 || s.RXIdx == -1 {
		if v, ok := m["mcs"]; ok {
			if mm, ok := v.(map[string]any); ok {
				if s.TXIdx == -1 {
					if iv, ok := mapIntOK(mm, "tx_idx", "tx", "tx_mcs", "txmcs"); ok {
						s.TXIdx = iv
					}
				}
				if s.RXIdx == -1 {
					if iv, ok := mapIntOK(mm, "rx_idx", "rx", "rx_mcs", "rxmcs"); ok {
						s.RXIdx = iv
					}
				}
			}
		}
	}

	// Number of spatial streams (NSS). Some firmware exposes per-direction
	// values, some only exposes a single "nss".
	s.TXNSS = mapInt(m, "tx_nss", "txnss", "txNSS", "nss")
	s.RXNSS = mapInt(m, "rx_nss", "rxnss", "rxNSS", "nss")

	s.TXPower = mapInt(m, "txpower", "tx_power")
	s.Distance = mapInt(m, "distance")
	s.Uptime = mapInt64(m, "uptime")
	// Counters: multiple naming variants exist.
	s.TXBytes = mapInt64(m, "txbytes", "tx_bytes")
	s.RXBytes = mapInt64(m, "rxbytes", "rx_bytes")
	s.TXPackets = mapInt64(m, "txpackets", "tx_packets")
	s.RXPackets = mapInt64(m, "rxpackets", "rx_packets")

	// Nested objects - best effort.
	if v, ok := m["stats"]; ok {
		if raw, err := json.Marshal(v); err == nil {
			var st StationStats
			if err := json.Unmarshal(raw, &st); err == nil {
				s.Stats = &st
			}
		}
	}
	if v, ok := m["airmax"]; ok {
		if raw, err := json.Marshal(v); err == nil {
			var am AirMaxStats
			if err := json.Unmarshal(raw, &am); err == nil {
				s.AirMax = &am
			}
		}
	}
	if v, ok := m["remote"]; ok {
		if raw, err := json.Marshal(v); err == nil {
			var r RemoteInfo
			if err := json.Unmarshal(raw, &r); err == nil {
				s.Remote = &r
			}
		}
	}

	return nil
}

// Helper methods for Station

// ============================================================================
// RSSI to dBm Conversion for AirMAX Devices
// ============================================================================
//
// AirMAX devices (AirOS) report signal strength in two different formats:
//
// 1. dBm (negative values like -65): Already in standard decibel-milliwatts
//    format. These values are used directly without conversion.
//
// 2. RSSI (positive values like 30, 40): These are "Received Signal Strength
//    Indicator" values that are RELATIVE measurements. AirMAX uses an offset
//    of 95 to convert to dBm.
//
// CONVERSION FORMULA (for positive RSSI only):
//
//     dBm = RSSI - 95
//
// EXAMPLES:
//     RSSI 40 → -55 dBm (excellent signal)
//     RSSI 30 → -65 dBm (good signal)
//     RSSI 20 → -75 dBm (fair signal)
//     RSSI 10 → -85 dBm (poor signal)
//
// WHERE THIS APPLIES:
//   - wireless.sta[].chainrssi[] - Per-chain RSSI values from station list
//   - wireless.sta[].rssi - Overall RSSI from station (when positive)
//   - wireless.sta[].remote.chainrssi[] - Remote station per-chain values
//
// WHERE THIS DOES NOT APPLY:
//   - Values already negative (they're already dBm)
//   - wireless.noisef / noisefloor - These are already dBm (typically -94 to -90)
//   - SNR calculations (SNR = signal_dBm - noise_floor_dBm, both in dBm)
//
// COMMON MISCONCEPTION:
//   DO NOT use "noise_floor - RSSI" for conversion. The conversion is simply
//   "RSSI - 95" using a fixed offset. The noise floor is only used for SNR
//   calculations AFTER both signal and noise are in dBm.
//
// ============================================================================

// rssiToDbm converts a positive RSSI value to dBm using the AirMAX offset.
// If the value is already negative (already dBm), it is returned unchanged.
//
// This function should ONLY be used for signal strength values from AirMAX
// station lists where ChainRSSI or RSSI may be reported as positive integers.
func rssiToDbm(val int) int {
	if val <= 0 {
		// Already in dBm format (negative value)
		return val
	}
	// Positive RSSI: convert using fixed 95 offset
	// This is the standard AirMAX/Ubiquiti conversion factor
	return val - 95
}

func cleanAirMAXChainSignals(raw []int, noiseFloor int, limit int) []int {
	if len(raw) == 0 || limit <= 0 {
		return nil
	}
	if len(raw) < limit {
		limit = len(raw)
	}
	result := make([]int, 0, limit)
	for i := 0; i < limit; i++ {
		v := raw[i]
		if v == 0 {
			continue
		}
		// Positive AirMAX chain RSSI values are offset-encoded and must be
		// converted to dBm. Negative values are already dBm and can be valid
		// chain readings even when they are close to the reported noise floor.
		// Do not drop negative values here; only zero denotes a disabled/empty
		// chain placeholder in the AP station-list shape.
		result = append(result, rssiToDbm(v))
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func (s *Station) GetSignal() int {
	return rssiToDbm(s.Signal)
}

func (s *Station) GetRSSI() int {
	return rssiToDbm(s.RSSI)
}

func (s *Station) GetNoiseFloor() int {
	return rssiToDbm(s.NoiseFloor)
}

// GetChainSignals returns per-chain signals converted to dBm.
// AirMAX devices report ChainRSSI as positive integers (e.g., [35, 33])
// which must be converted to dBm using rssiToDbm().
//
// Example: ChainRSSI [35, 33] → [-60, -62] dBm
//
// AirMAX radios are 2x2 MIMO maximum, so we limit to 2 chains. Some
// AirOS builds include a trailing zero placeholder; that is not a usable
// chain signal. Negative values are already dBm and must be preserved even
// if they are near the reported noise floor.
func (s *Station) GetChainSignals() []int {
	return cleanAirMAXChainSignals(s.ChainRSSI, s.NoiseFloor, 2)
}

func (s *Station) GetDistance() int {
	return s.Distance
}

func (s *Station) GetUptime() int64 {
	return s.Uptime
}

func (s *Station) GetTXBytes() int64 {
	if s.Stats != nil {
		return s.Stats.TXBytes
	}
	return s.TXBytes
}

func (s *Station) GetRXBytes() int64 {
	if s.Stats != nil {
		return s.Stats.RXBytes
	}
	return s.RXBytes
}

func (s *Station) GetTXPackets() int64 {
	if s.Stats != nil {
		return s.Stats.TXPackets
	}
	return s.TXPackets
}

func (s *Station) GetRXPackets() int64 {
	if s.Stats != nil {
		return s.Stats.RXPackets
	}
	return s.RXPackets
}

type StationStats struct {
	RXBytes   int64 `json:"rx_bytes"`
	RXPackets int64 `json:"rx_packets"`
	RXPPS     int   `json:"rx_pps"`
	TXBytes   int64 `json:"tx_bytes"`
	TXPackets int64 `json:"tx_packets"`
	TXPPS     int   `json:"tx_pps"`
}

type AirMaxStats struct {
	ActualPriority   int `json:"actual_priority"`
	DesiredPriority  int `json:"desired_priority"`
	DownlinkCapacity int `json:"downlink_capacity"`
	UplinkCapacity   int `json:"uplink_capacity"`
	Beam             int `json:"beam"`
	ATCPStatus       int `json:"atpc_status"`
	RX               struct {
		Usage int `json:"usage"`
		CINR  int `json:"cinr"`
		// EVM is returned as a per-chain series (historical samples).
		// Example: [[26,24,24,...],[35,32,...]]
		EVM [][]float64 `json:"evm,omitempty"`
	} `json:"rx"`
	TX struct {
		Usage int `json:"usage"`
		CINR  int `json:"cinr"`
		// EVM is returned as a per-chain series (historical samples).
		// Example: [[26,24,24,...],[35,32,...]]
		EVM [][]float64 `json:"evm,omitempty"`
	} `json:"tx"`
}

type RemoteInfo struct {
	Hostname    string    `json:"hostname"`
	Platform    string    `json:"platform"`
	Version     string    `json:"version"`
	Time        string    `json:"time"`
	CPULoad     float64   `json:"cpuload"`
	Temperature int       `json:"temperature"`
	TotalRAM    int64     `json:"totalram"`
	FreeRAM     int64     `json:"freeram"`
	NetRole     string    `json:"netrole"`
	Mode        string    `json:"mode"`
	Compat11N   int       `json:"compat_11n"`
	Signal      int       `json:"signal"`
	RSSI        int       `json:"rssi"`
	NoiseFloor  int       `json:"noisefloor"`
	TXPower     int       `json:"tx_power"`
	Distance    int       `json:"distance"`
	RXChainmask int       `json:"rx_chainmask"`
	ChainRSSI   []int     `json:"chainrssi"`
	TXBytes     int64     `json:"tx_bytes"`
	RXBytes     int64     `json:"rx_bytes"`
	Uptime      int64     `json:"uptime"`
	AntennaGain int       `json:"antenna_gain"`
	EthList     []EthInfo `json:"ethlist"`
	IPAddr      []string  `json:"ipaddr"`
}

type EthInfo struct {
	IFName   string `json:"ifname"`
	Enabled  bool   `json:"enabled"`
	Plugged  bool   `json:"plugged"`
	Duplex   bool   `json:"duplex"`
	Speed    int    `json:"speed"`
	CableLen int    `json:"cable_len"`
}

// RemoteInfo helper methods

func (r *RemoteInfo) GetSignal() int {
	return rssiToDbm(r.Signal)
}

func (r *RemoteInfo) GetNoiseFloor() int {
	// Note: NoiseFloor is typically already in dBm (negative), but
	// rssiToDbm handles this by returning negative values unchanged.
	return rssiToDbm(r.NoiseFloor)
}

// GetChainSignals returns per-chain signals converted to dBm.
// Remote ChainRSSI values from the STA's perspective need the same
// RSSI-to-dBm conversion as local values.
func (r *RemoteInfo) GetChainSignals() []int {
	return cleanAirMAXChainSignals(r.ChainRSSI, r.NoiseFloor, 2)
}

type GPSInfo struct {
	Lat             float64 `json:"lat"`
	Lon             float64 `json:"lon"`
	Fix             int     `json:"fix"`
	Sats            int     `json:"sats"`
	Dim             int     `json:"dim"`
	DOP             float64 `json:"dop"`
	Alt             float64 `json:"alt"`
	TimeSyncEnabled int     `json:"time_sync_enabled"`
}

// GPSInfo helper methods
func (g *GPSInfo) GetLat() float64 {
	return g.Lat
}

func (g *GPSInfo) GetLon() float64 {
	return g.Lon
}

func (g *GPSInfo) GetFix() int {
	return g.Fix
}

// AirfiberInfo holds AirFiber-specific status data
// Present in status.cgi for AF2, AF3, AF5, AF11 devices
type AirfiberInfo struct {
	// Channel configuration
	TXChanBW    string `json:"txchanbw"`
	RXChanBW    string `json:"rxchanbw"`
	TXFrequency int    `json:"txfrequency"`
	RXFrequency int    `json:"rxfrequency"`
	Frequency   int    `json:"frequency"` // For half-duplex

	// Frame/timing
	FrameLength string `json:"framelength"`
	Duplex      string `json:"duplex"` // "half", "full"
	DutyCycle   string `json:"dutycycle"`
	GPSSync     bool   `json:"gps_sync"`

	// Link stats
	TXModRate int `json:"txmodrate"` // TX modulation rate
	RXModRate int `json:"rxmodrate"` // RX modulation rate
	// Remote TX/RX modulation rate (peer-reported). Present on some airFiber firmwares.
	// Example values in status: "4x", "7x" which we normalize to ints (4, 7).
	RemoteTXModRate  int    `json:"remote_txmodrate"`
	RemoteRXModRate  int    `json:"remote_rxmodrate"`
	TXCapacity       int    `json:"txcapacity"`      // TX capacity kbps
	RXCapacity       int    `json:"rxcapacity"`      // RX capacity kbps
	TXPower          int    `json:"txpower"`         // TX power dBm
	RXGain           int    `json:"rxgain"`          // RX gain dB
	LinkDistance     int    `json:"linkdist"`        // Link distance meters
	RSSI0            int    `json:"rssi0"`           // Chain 0 RSSI
	RSSI1            int    `json:"rssi1"`           // Chain 1 RSSI
	Signal0dBm       int    `json:"signal0dbm"`      // Chain 0 signal dBm
	Signal1dBm       int    `json:"signal1dbm"`      // Chain 1 signal dBm
	RemoteSignal0dBm int    `json:"remote_rxpower0"` // Remote chain 0 signal dBm (peer-reported)
	RemoteSignal1dBm int    `json:"remote_rxpower1"` // Remote chain 1 signal dBm (peer-reported)
	IdealRSSI0       int    `json:"idealrssi0"`      // Ideal RSSI chain 0
	IdealRSSI1       int    `json:"idealrssi1"`      // Ideal RSSI chain 1
	LinkState        string `json:"linkstate"`       // "operational", "offline", etc.
	RemoteMAC        string `json:"remotemac"`       // Remote device MAC
	RemoteIP         string `json:"remoteip"`        // Remote device IP
	// Optional remote identification fields. Not all firmwares provide these.
	RemoteHostname    string  `json:"remotehostname"`    // Remote device hostname (optional)
	RemoteDeviceName  string  `json:"remotedevicename"`  // Remote device name (optional)
	RemoteDeviceModel string  `json:"remotedevicemodel"` // Remote device model (optional)
	LocalMAC          string  `json:"localmac"`          // Local RF MAC
	CINRdB            float64 `json:"cinrdb"`            // Carrier to interference+noise ratio
	RemoteCINRdB      float64 `json:"remote_cinrdb"`     // Remote carrier to interference+noise ratio (if provided)

	// Error counters
	TXErrors  int64 `json:"txerrors"`
	RXErrors  int64 `json:"rxerrors"`
	TXDropped int64 `json:"txdropped"`
	RXDropped int64 `json:"rxdropped"`
}

// UnmarshalJSON is lenient across AirFiber firmware variants.
// AirFiber status payloads frequently encode numeric fields as strings (e.g. "56MHz", "2.0ms").
// We accept both legacy and snake_case keys and coerce into strongly-typed fields.
func (a *AirfiberInfo) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		return nil
	}

	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}

	// Channel configuration / frequencies (support legacy + snake_case)
	a.TXChanBW = mapString(m, "txchanbw", "tx_chanbw")
	a.RXChanBW = mapString(m, "rxchanbw", "rx_chanbw")
	a.TXFrequency = mapInt(m, "txfrequency", "tx_frequency", "txfreq")
	a.RXFrequency = mapInt(m, "rxfrequency", "rx_frequency", "rxfreq")
	a.Frequency = mapInt(m, "frequency")

	// Frame/timing
	a.FrameLength = mapString(m, "framelength", "frame_length")
	a.Duplex = mapString(m, "duplex")
	a.DutyCycle = mapString(m, "dutycycle")
	a.GPSSync = mapBool(m, "gps_sync", "gpsSync", "gpssync")

	// Link stats
	a.TXModRate = mapInt(m, "txmodrate")
	a.RXModRate = mapInt(m, "rxmodrate")
	a.RemoteTXModRate = mapInt(m, "remote_txmodrate", "remoteTxModRate", "remotetxmodrate")
	a.RemoteRXModRate = mapInt(m, "remote_rxmodrate", "remoteRxModRate", "remoterxmodrate")
	a.TXCapacity = mapInt(m, "txcapacity")
	a.RXCapacity = mapInt(m, "rxcapacity")
	a.TXPower = mapInt(m, "txpower")
	a.RXGain = mapInt(m, "rxgain")
	a.LinkDistance = mapInt(m, "linkdist", "distance")
	a.RSSI0 = mapInt(m, "rssi0")
	a.RSSI1 = mapInt(m, "rssi1")

	// Some firmwares report per-chain RX power instead of signal*dBm keys (AirFiber 11 Ultra does this).
	a.Signal0dBm = mapInt(m, "signal0dbm", "signal0_dbm", "signal0", "rxpower0", "rxpower0_dbm", "rxpower0dbm")
	a.Signal1dBm = mapInt(m, "signal1dbm", "signal1_dbm", "signal1", "rxpower1", "rxpower1_dbm", "rxpower1dbm")
	a.RemoteSignal0dBm = mapInt(m, "remote_rxpower0", "remote_signal0dbm", "remote_signal0_dbm", "remote_signal0")
	a.RemoteSignal1dBm = mapInt(m, "remote_rxpower1", "remote_signal1dbm", "remote_signal1_dbm", "remote_signal1")

	a.IdealRSSI0 = mapInt(m, "idealrssi0", "ideal_rssi0")
	a.IdealRSSI1 = mapInt(m, "idealrssi1", "ideal_rssi1")
	a.LinkState = mapString(m, "linkstate", "link_state")

	// Peer identification (support both legacy and snake_case keys)
	a.RemoteMAC = mapString(m, "remotemac", "remote_mac")
	a.RemoteIP = mapString(m, "remoteip", "remote_ip")
	a.RemoteHostname = mapString(m, "remotehostname", "remote_hostname", "remote_host", "remotehost")
	a.RemoteDeviceName = mapString(m, "remotedevicename", "remote_device_name", "remote_device", "remote_name", "remotename")
	a.RemoteDeviceModel = mapString(m, "remotedevicemodel", "remote_device_model", "remote_model", "remotemodel")
	a.LocalMAC = mapString(m, "localmac", "local_mac")

	a.CINRdB = mapFloat64(m, "cinrdb", "cinr", "cinr_db")
	a.RemoteCINRdB = mapFloat64(m, "remote_cinrdb", "remote_cinr", "remote_cinr_db")

	// Error counters
	a.TXErrors = mapInt64(m, "txerrors", "tx_errors")
	a.RXErrors = mapInt64(m, "rxerrors", "rx_errors")
	a.TXDropped = mapInt64(m, "txdropped", "tx_dropped")
	a.RXDropped = mapInt64(m, "rxdropped", "rx_dropped")

	return nil
}

// GetSignal returns the best signal value in dBm
func (a *AirfiberInfo) GetSignal() int {
	// Normalize candidates to dBm and return the best (closest to 0).
	sig := 0
	for _, v := range []int{a.Signal0dBm, a.Signal1dBm, a.RSSI0, a.RSSI1} {
		if v == 0 {
			continue
		}
		dbm := rssiToDbm(v)
		if sig == 0 || dbm > sig {
			sig = dbm
		}
	}
	return sig
}

func (a *AirfiberInfo) GetRemoteSignal() int {
	// Remote chain powers are already reported as RSSI/dBm style values.
	sig := 0
	for _, v := range []int{a.RemoteSignal0dBm, a.RemoteSignal1dBm} {
		if v == 0 {
			continue
		}
		dbm := rssiToDbm(v)
		if sig == 0 || dbm > sig {
			sig = dbm
		}
	}
	return sig
}

// GetCapacity returns combined TX+RX capacity in bps
func (a *AirfiberInfo) GetCapacity() int64 {
	return int64(a.TXCapacity+a.RXCapacity) * 1000
}

// GetChainSignals returns per-chain signals in dBm
func (a *AirfiberInfo) GetChainSignals() []int {
	if a.Signal0dBm != 0 || a.Signal1dBm != 0 {
		// Some firmwares return RSSI as a positive integer; normalize to dBm.
		s0, s1 := rssiToDbm(a.Signal0dBm), rssiToDbm(a.Signal1dBm)
		// Preserve zero when the chain value is missing.
		if a.Signal0dBm == 0 {
			s0 = 0
		}
		if a.Signal1dBm == 0 {
			s1 = 0
		}
		return []int{s0, s1}
	}
	if a.RSSI0 != 0 || a.RSSI1 != 0 {
		rssi0, rssi1 := a.RSSI0, a.RSSI1
		if rssi0 > 0 {
			rssi0 -= 95
		}
		if rssi1 > 0 {
			rssi1 -= 95
		}
		return []int{rssi0, rssi1}
	}
	return nil
}

// GetRemoteChainSignals returns the remote per-chain RX power (dBm) if available.
// Not all firmwares expose this.
func (a *AirfiberInfo) GetRemoteChainSignals() []int {
	if a.RemoteSignal0dBm != 0 || a.RemoteSignal1dBm != 0 {
		s0, s1 := rssiToDbm(a.RemoteSignal0dBm), rssiToDbm(a.RemoteSignal1dBm)
		if a.RemoteSignal0dBm == 0 {
			s0 = 0
		}
		if a.RemoteSignal1dBm == 0 {
			s1 = 0
		}
		return []int{s0, s1}
	}
	return nil
}

// NewClient creates a new AirMAX client
func NewClient(host string, timeout time.Duration) *Client {
	return NewClientWithTransport(host, timeout, nil)
}

// NewClientWithTransport creates a new AirMAX client with custom transport
func NewClientWithTransport(host string, timeout time.Duration, transport http.RoundTripper) *Client {
	jar, _ := cookiejar.New(nil)

	if transport == nil {
		transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}

	return &Client{
		httpClient: &http.Client{
			Timeout:   timeout,
			Jar:       jar,
			Transport: transport,
		},
		host:   host,
		scheme: "https", // Will try http if https fails
	}
}

// Login authenticates with the device
func (c *Client) Login(username, password string) error {
	c.username = username
	c.password = password

	var lastErr error

	// Try AirOS 8+ API auth first (HTTPS)
	err := c.tryAPIAuth("https")
	if err == nil {
		c.scheme = "https"
		log.Printf("AirMAX %s: login via HTTPS /api/auth (csrf=%v)", c.host, c.csrfToken != "")
		return nil
	}
	lastErr = err

	// Try AirOS 8+ API auth (HTTP)
	err = c.tryAPIAuth("http")
	if err == nil {
		c.scheme = "http"
		log.Printf("AirMAX %s: login via HTTP /api/auth (csrf=%v)", c.host, c.csrfToken != "")
		return nil
	}
	lastErr = err

	// Fall back to legacy login.cgi for AirOS 5/6/7 (HTTPS)
	err = c.tryLogin("https")
	if err == nil {
		c.scheme = "https"
		log.Printf("AirMAX %s: login via HTTPS login.cgi (csrf=%v)", c.host, c.csrfToken != "")
		return nil
	}
	lastErr = err

	// Fall back to legacy login.cgi (HTTP)
	err = c.tryLogin("http")
	if err == nil {
		c.scheme = "http"
		log.Printf("AirMAX %s: login via HTTP login.cgi (csrf=%v)", c.host, c.csrfToken != "")
		return nil
	}
	lastErr = err

	return fmt.Errorf("all auth methods failed: %w", lastErr)
}

// isConnectionError checks if an error is a network/connection error vs auth error
func isConnectionError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "connection refused") ||
		strings.Contains(s, "no such host") ||
		strings.Contains(s, "timeout") ||
		strings.Contains(s, "i/o timeout") ||
		strings.Contains(s, "network is unreachable") ||
		strings.Contains(s, "no route to host") ||
		strings.Contains(s, "connection reset") ||
		strings.Contains(s, "request failed:")
}

// tryAPIAuth tries the /api/auth endpoint (AirOS 8+)
func (c *Client) tryAPIAuth(scheme string) error {
	authURL := fmt.Sprintf("%s://%s/api/auth", scheme, c.host)

	body := fmt.Sprintf(`{"username":"%s","password":"%s"}`, c.username, c.password)
	req, _ := http.NewRequest("POST", authURL, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s /api/auth request failed: %w", scheme, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s /api/auth status %d", scheme, resp.StatusCode)
	}

	// Capture CSRF token from response header (required for AirOS 8+)
	if csrf := resp.Header.Get("X-CSRF-ID"); csrf != "" {
		c.csrfToken = csrf
		log.Printf("AirMAX %s: got CSRF from %s /api/auth", c.host, scheme)
	}

	// Check for auth token in response or cookie
	cookies := c.httpClient.Jar.Cookies(resp.Request.URL)
	for _, cookie := range cookies {
		if strings.Contains(cookie.Name, "AIROS") || strings.Contains(cookie.Name, "X-") {
			return nil // Got session
		}
	}

	// Check response body for token
	respBody, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(respBody), "token") || len(cookies) > 0 {
		return nil
	}

	return fmt.Errorf("%s /api/auth: no session established", scheme)
}

func (c *Client) tryLogin(scheme string) error {
	loginURL := fmt.Sprintf("%s://%s/login.cgi", scheme, c.host)

	// First GET login.cgi to establish session cookie
	resp, err := c.httpClient.Get(loginURL)
	if err != nil {
		return fmt.Errorf("%s login page request failed: %w", scheme, err)
	}
	resp.Body.Close()

	// POST credentials as form-urlencoded (AirOS requires this; multipart is unreliable)
	form := url.Values{}
	form.Set("username", c.username)
	form.Set("password", c.password)
	req, err := http.NewRequest("POST", loginURL, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("%s create login request failed: %w", scheme, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")

	resp, err = c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s login request failed: %w", scheme, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)

	bodyLower := strings.ToLower(bodyStr)

	// Check for invalid credentials
	if strings.Contains(bodyStr, "Invalid credentials") || strings.Contains(bodyLower, "invalid credentials") {
		return fmt.Errorf("%s invalid credentials", scheme)
	}

	// If we still got the login form back, treat it as a login failure.
	// This guards against false-positives when the device ignores the POST body.
	if strings.Contains(bodyLower, "name=\"username\"") && strings.Contains(bodyLower, "name=\"password\"") {
		return fmt.Errorf("%s login.cgi did not authenticate (still on login page)", scheme)
	}

	// Capture CSRF token from response header (AirOS 8+)
	if csrf := resp.Header.Get("X-CSRF-ID"); csrf != "" {
		c.csrfToken = csrf
	}

	// Must have an AIROS session cookie to be a real AirMAX device
	cookies := c.httpClient.Jar.Cookies(resp.Request.URL)
	for _, cookie := range cookies {
		if strings.HasPrefix(cookie.Name, "AIROS_") {
			// Got session via login.cgi
			// For AirOS 8+ devices, fetch CSRF token via /api/auth using the established session.
			if c.csrfToken == "" {
				_ = c.fetchCSRFToken(scheme)
			}
			return nil
		}
	}

	// No AIROS cookie - not a real AirMAX device
	return fmt.Errorf("%s no AIROS session cookie (got %d cookies)", scheme, len(cookies))
}

// fetchCSRFWithSession tries to get CSRF token using existing session (no re-auth)
func (c *Client) fetchCSRFWithSession(scheme string) {
	// GET /api/auth with existing session cookies to get CSRF token
	authURL := fmt.Sprintf("%s://%s/api/auth", scheme, c.host)

	resp, err := c.httpClient.Get(authURL)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	// Check for CSRF token in response header
	if csrf := resp.Header.Get("X-CSRF-ID"); csrf != "" {
		c.csrfToken = csrf
	}
}

// fetchCSRFToken tries to get CSRF token from /api/auth using existing session
func (c *Client) fetchCSRFToken(scheme string) error {
	authURL := fmt.Sprintf("%s://%s/api/auth", scheme, c.host)
	reqDesc := fmt.Sprintf("POST %s [user=%s]", authURL, c.username)

	// Clear any previous CSRF error so diagnostics reflect the latest attempt.
	c.csrfError = nil

	body := fmt.Sprintf(`{"username":"%s","password":"%s"}`, c.username, c.password)

	req, _ := http.NewRequest("POST", authURL, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// Some AirOS builds are picky and behave more consistently when the request
	// looks like an AJAX call from the UI.
	req.Header.Set("X-Requested-With", "XMLHttpRequest")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		e := fmt.Errorf("CSRF fetch failed: %s -> %w", reqDesc, err)
		c.csrfError = e
		return e
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		e := fmt.Errorf("CSRF fetch failed: %s -> status %d: %s", reqDesc, resp.StatusCode, string(respBody))
		c.csrfError = e
		return e
	}

	if csrf := resp.Header.Get("X-CSRF-ID"); csrf != "" {
		c.csrfToken = csrf
		return nil
	}

	e := fmt.Errorf("CSRF fetch failed: %s -> status 200 but no X-CSRF-ID header", reqDesc)
	c.csrfError = e
	return e
}

// LoginWithPasswords tries multiple passwords
func (c *Client) LoginWithPasswords(username string, passwords []string) error {
	var lastErr error
	for _, pw := range passwords {
		err := c.Login(username, pw)
		if err == nil {
			return nil
		}
		lastErr = err

		// Stop early on connection errors - no point trying more passwords
		if isConnectionError(err) {
			return err
		}
	}
	return lastErr
}

// Credential holds a username/password pair
type Credential struct {
	Username string
	Password string
}

// LoginWithCredentials tries multiple username/password pairs until one succeeds
func (c *Client) LoginWithCredentials(creds []Credential) error {
	var lastErr error
	for _, cred := range creds {
		err := c.Login(cred.Username, cred.Password)
		if err == nil {
			return nil
		}
		lastErr = err

		// Stop early on connection errors - no point trying more credentials
		if isConnectionError(err) {
			return err
		}
	}
	return lastErr
}

// GetStatus fetches the full device status
func (c *Client) GetStatus() (*Status, error) {
	statusURL := fmt.Sprintf("%s://%s/status.cgi", c.scheme, c.host)

	resp, err := c.httpClient.Get(statusURL)
	if err != nil {
		return nil, fmt.Errorf("status.cgi request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status.cgi returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("status.cgi read failed: %w", err)
	}

	// First try strict unmarshal
	var status Status
	if err := json.Unmarshal(body, &status); err == nil {
		// Parse interfaces (can be map or array)
		parseInterfaces(&status, c.host)
		return &status, nil
	}

	// Strict failed - try lenient parsing via map
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("status.cgi JSON invalid: %w", err)
	}

	// Parse each section, log errors but continue
	if hostRaw, ok := raw["host"]; ok {
		if err := json.Unmarshal(hostRaw, &status.Host); err != nil {
			log.Printf("AirMAX %s: host parse error: %v", c.host, err)
			// Try parsing into map and extract manually
			parseHostLenient(hostRaw, &status.Host, c.host)
		}
	}

	if mcaRaw, ok := raw["mca_status"]; ok {
		var mca MCAStatus
		if err := json.Unmarshal(mcaRaw, &mca); err != nil {
			log.Printf("AirMAX %s: mca_status parse error: %v", c.host, err)
		} else {
			status.MCAStatus = &mca
		}
	}

	if wirelessRaw, ok := raw["wireless"]; ok {
		// Use lenient parsing directly - AirMAX devices have inconsistent types
		// (e.g., frame_length can be int or float depending on firmware)
		parseWirelessLenient(wirelessRaw, &status.Wireless, c.host)
	}

	if ifacesRaw, ok := raw["interfaces"]; ok {
		status.InterfacesRaw = ifacesRaw
		parseInterfaces(&status, c.host)
	}

	if stationsRaw, ok := raw["stations"]; ok {
		if err := json.Unmarshal(stationsRaw, &status.Stations); err != nil {
			log.Printf("AirMAX %s: stations parse error: %v", c.host, err)
		}
	}

	if gpsRaw, ok := raw["gps"]; ok {
		var gps GPSInfo
		if err := json.Unmarshal(gpsRaw, &gps); err != nil {
			log.Printf("AirMAX %s: gps parse error: %v", c.host, err)
		} else {
			status.GPS = &gps
		}
	}

	if afRaw, ok := raw["airfiber"]; ok {
		var af AirfiberInfo
		if err := json.Unmarshal(afRaw, &af); err != nil {
			log.Printf("AirMAX %s: airfiber parse error: %v", c.host, err)
		} else {
			status.Airfiber = &af
		}
	}

	return &status, nil
}

// parseHostLenient extracts host fields from raw JSON map
func parseHostLenient(raw json.RawMessage, host *HostInfo, logHost string) {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	if v, ok := m["hostname"].(string); ok {
		host.Hostname = v
	}
	if v, ok := m["devmodel"].(string); ok {
		host.DevModel = v
	}
	if v, ok := m["fwversion"].(string); ok {
		host.FWVersion = v
	}
	if v, ok := m["netrole"].(string); ok {
		host.NetRole = v
	}
	if v, ok := m["time"].(string); ok {
		host.Time = v
	}
	if v, ok := m["uptime"].(float64); ok {
		host.Uptime = int64(v)
	}
	if v, ok := m["totalram"].(float64); ok {
		host.TotalRAM = int64(v)
	}
	if v, ok := m["freeram"].(float64); ok {
		host.FreeRAM = int64(v)
	}
	if v, ok := m["temperature"].(float64); ok {
		host.Temperature = v
	}
	if v, ok := m["cpuload"].(float64); ok {
		host.CPULoad = v
	}
	if v, ok := m["loadavg"].(float64); ok {
		host.LoadAvg = v
	}
}

// parseWirelessLenient extracts wireless fields from raw JSON map
func parseWirelessLenient(raw json.RawMessage, w *WirelessInfo, logHost string) {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	// String fields
	if v, ok := m["essid"].(string); ok {
		w.ESSID = v
	}
	if v, ok := m["mode"].(string); ok {
		w.Mode = v
	}
	if v, ok := m["opmode"].(string); ok {
		w.OpMode = v
	}
	if v, ok := m["op_mode"].(string); ok && w.OpMode == "" {
		w.OpMode = v
	}
	if v, ok := m["ieeemode"].(string); ok {
		w.IEEEMode = v
	}
	if v, ok := m["apmac"].(string); ok {
		w.APMAC = v
	}
	if v, ok := m["security"].(string); ok {
		w.Security = v
	}
	// Numeric fields (JSON numbers are float64)
	if v, ok := m["frequency"].(float64); ok {
		w.Frequency = int(v)
	}
	if v, ok := m["center1_freq"].(float64); ok {
		w.Center1Freq = int(v)
	}
	if v, ok := m["chanbw"].(float64); ok {
		w.ChanBW = int(v)
	}
	if v, ok := m["txpower"].(float64); ok {
		w.TXPower = int(v)
	}
	if v, ok := m["noisef"].(float64); ok {
		w.NoiseF = int(v)
	}
	if v, ok := m["signal"].(float64); ok {
		w.Signal = int(v)
	}
	if v, ok := m["rssi"].(float64); ok {
		w.RSSI = int(v)
	}
	if v, ok := m["noisefloor"].(float64); ok {
		w.NoiseFloor = int(v)
	}
	if v, ok := m["distance"].(float64); ok {
		w.Distance = int(v)
	}
	if v, ok := m["count"].(float64); ok {
		w.Count = int(v)
	}
	if v, ok := m["dfs"].(float64); ok {
		w.DFS = int(v)
	}
	if v, ok := m["antenna_gain"].(float64); ok {
		w.AntennaGain = int(v)
	}
	if v, ok := m["compat_11n"].(float64); ok {
		w.Compat11N = int(v)
	}
	if v, ok := m["rx_chainmask"].(float64); ok {
		w.RXChainmask = int(v)
	}
	if v, ok := m["tx_chainmask"].(float64); ok {
		w.TXChainmask = int(v)
	}
	if v, ok := m["rx_nss"].(float64); ok {
		w.RXNSS = int(v)
	}
	if v, ok := m["tx_nss"].(float64); ok {
		w.TXNSS = int(v)
	}
	// LTU specific fields
	if v, ok := m["frame_length"].(float64); ok {
		w.FrameLength = int(v)
	}
	if v, ok := m["duty_cycle"].(float64); ok {
		w.DutyCycle = int(v)
	}
	if v, ok := m["sync_mode"].(float64); ok {
		w.SyncMode = int(v)
	}
	if v, ok := m["gps_state"].(float64); ok {
		w.GPSState = int(v)
	}
	// Polling - keep as raw JSON
	if v, ok := m["polling"]; ok {
		if raw, err := json.Marshal(v); err == nil {
			w.Polling = raw
		}
	}
	// Stations array - parse separately
	if v, ok := m["sta"]; ok {
		if raw, err := json.Marshal(v); err == nil {
			json.Unmarshal(raw, &w.Stations)
		}
	}
}

// parseInterfaces handles interfaces in both map and array format
func parseInterfaces(status *Status, logHost string) {
	if len(status.InterfacesRaw) == 0 {
		return
	}

	// Initialize map
	status.Interfaces = make(map[string]InterfaceInfo)

	// Try as map first (standard format)
	var ifaceMap map[string]InterfaceInfo
	if json.Unmarshal(status.InterfacesRaw, &ifaceMap) == nil {
		status.Interfaces = ifaceMap
		return
	}

	// Try as array of objects with ifname field
	var arr []map[string]any
	if json.Unmarshal(status.InterfacesRaw, &arr) == nil {
		for _, item := range arr {
			ifname, _ := item["ifname"].(string)
			if ifname == "" {
				continue
			}

			var iface InterfaceInfo

			// Top-level fields (some firmwares expose these directly)
			if mac, ok := item["hwaddr"].(string); ok && mac != "" {
				iface.MAC = mac
			}
			if mac, ok := item["mac"].(string); ok && mac != "" {
				iface.MAC = mac
			}
			if ip4, ok := item["ipv4"].(map[string]any); ok {
				if addr, ok := ip4["addr"].(string); ok {
					iface.IP = addr
				}
				if nm, ok := ip4["netmask"].(string); ok {
					iface.Netmask = nm
				}
			}
			if ip, ok := item["ip"].(string); ok && ip != "" {
				iface.IP = ip
			}
			if netmask, ok := item["netmask"].(string); ok && netmask != "" {
				iface.Netmask = netmask
			}

			// Status can be a string OR a rich object depending on firmware.
			if st, ok := item["status"].(map[string]any); ok {
				// Some firmwares nest mac/speed/counters under the status object.
				if mac, ok := st["mac"].(string); ok && iface.MAC == "" {
					iface.MAC = mac
				}
				if cs, ok := st["current_speed"].(string); ok {
					iface.CurrentSpeed = cs
				}
				if sp, ok := st["speed"].(string); ok {
					iface.Speed = sp
				}
				if dp, ok := st["duplex"].(string); ok {
					iface.Duplex = dp
				}
				if v, ok := st["cable_len"]; ok {
					switch t := v.(type) {
					case float64:
						iface.CableLength = int(t)
					case int:
						iface.CableLength = t
					case int64:
						iface.CableLength = int(t)
					}
				}
				if v, ok := st["rx_bytes"]; ok {
					switch t := v.(type) {
					case float64:
						iface.RxBytes = int64(t)
					case int64:
						iface.RxBytes = t
					case int:
						iface.RxBytes = int64(t)
					}
				}
				if v, ok := st["tx_bytes"]; ok {
					switch t := v.(type) {
					case float64:
						iface.TxBytes = int64(t)
					case int64:
						iface.TxBytes = t
					case int:
						iface.TxBytes = int64(t)
					}
				}
				if v, ok := st["rx_errors"]; ok {
					switch t := v.(type) {
					case float64:
						iface.RxErrors = int64(t)
					case int64:
						iface.RxErrors = t
					case int:
						iface.RxErrors = int64(t)
					}
				}
				if v, ok := st["tx_errors"]; ok {
					switch t := v.(type) {
					case float64:
						iface.TxErrors = int64(t)
					case int64:
						iface.TxErrors = t
					case int:
						iface.TxErrors = int64(t)
					}
				}
				if v, ok := st["snr"]; ok {
					switch t := v.(type) {
					case float64:
						iface.SNR = int(t)
					case int:
						iface.SNR = t
					case int64:
						iface.SNR = int(t)
					}
				}
			} else if s, ok := item["status"].(string); ok {
				iface.Status = s
			}

			// If "enabled" exists, convert to string status (this is the best general signal)
			if enabled, ok := item["enabled"].(bool); ok {
				if enabled {
					iface.Status = "enabled"
				} else {
					iface.Status = "disabled"
				}
			}

			status.Interfaces[ifname] = iface
		}
		return
	}

	log.Printf("AirMAX %s: interfaces parse error: unknown format", logHost)
}

// IsAP returns true if the device is operating as an access point
func (s *Status) IsAP() bool {
	// AirFiber links report mode="airfiber" and use opmode=master/slave.
	if s.IsAirFiber() {
		op := strings.ToLower(strings.TrimSpace(s.Wireless.OpMode))
		return op == "master" || op == "ap"
	}
	mode := strings.ToLower(s.Wireless.Mode)
	return strings.HasPrefix(mode, "ap-") || mode == "ap" || mode == "master"
}

// IsSTA returns true if the device is operating as a station
func (s *Status) IsSTA() bool {
	if s.IsAirFiber() {
		op := strings.ToLower(strings.TrimSpace(s.Wireless.OpMode))
		return op == "slave" || op == "sta"
	}
	mode := strings.ToLower(s.Wireless.Mode)
	return strings.HasPrefix(mode, "sta-") || mode == "sta" || mode == "slave"
}

// GetEth0 returns the eth0 interface info
func (s *Status) GetEth0() *InterfaceInfo {
	if iface, ok := s.Interfaces["eth0"]; ok {
		return &iface
	}
	return nil
}

// GetMAC returns the device MAC address
func (s *Status) GetMAC() string {
	// Try eth0 first
	if eth0 := s.GetEth0(); eth0 != nil && eth0.MAC != "" {
		return eth0.MAC
	}
	// Fall back to wireless APMAC
	return s.Wireless.APMAC
}

// GetModel returns the device model
func (s *Status) GetModel() string {
	// Try mca_status first (more reliable)
	if s.MCAStatus != nil && s.MCAStatus.DeviceModel != "" {
		return s.MCAStatus.DeviceModel
	}
	return s.Host.DevModel
}

// GetNetMode builds the network mode features string from status.cgi data
// Format examples: "ap-ptmp+fixed=5/67+sync", "sta-ptp", "ap+flex+sync"
func (s *Status) GetNetMode() string {
	if s == nil || s.Wireless.Mode == "" {
		return ""
	}

	mode := s.Wireless.Mode

	// Get polling info for feature flags
	polling := s.Wireless.GetPollingInfo()
	features := []string{}

	// Fixed frame mode with parameters
	if polling != nil && polling.FixedFrame {
		if polling.FFFrameDur > 0 && polling.FFDLRatio > 0 {
			features = append(features, fmt.Sprintf("fixed=%d/%d", polling.FFFrameDur, polling.FFDLRatio))
		} else {
			features = append(features, "fixed")
		}
	}

	// Flex mode
	if polling != nil && polling.FlexMode {
		features = append(features, "flex")
	}

	// GPS sync (from polling or LTU sync_mode)
	if polling != nil && polling.GPSSync {
		features = append(features, "sync")
	} else if s.Wireless.SyncMode > 0 {
		features = append(features, "sync")
	}

	// 802.11n compatibility
	if s.Wireless.Compat11N != 0 {
		features = append(features, "11n")
	}

	// LTU specific: frame_length and duty_cycle from wireless
	if s.Wireless.FrameLength > 0 && s.Wireless.DutyCycle > 0 {
		features = append(features, fmt.Sprintf("fixed=%d/%d", int(s.Wireless.FrameLength), s.Wireless.DutyCycle))
	}

	// Build final string
	if len(features) > 0 {
		mode = mode + "+" + strings.Join(features, "+")
	}

	return mode
}

// GetFirmware returns the firmware version
func (s *Status) GetFirmware() string {
	if s.MCAStatus != nil && s.MCAStatus.ShortVersion != "" {
		return s.MCAStatus.ShortVersion
	}
	return s.Host.FWVersion
}

// GetFullFirmware returns the full firmware string (e.g., "XC.qca955x.v8.7.0")
func (s *Status) GetFullFirmware() string {
	// Try MCAStatus.Firmware first (most complete)
	if s.MCAStatus != nil && s.MCAStatus.Firmware != "" {
		return s.MCAStatus.Firmware
	}
	// Fall back to short version
	return s.Host.FWVersion
}

// DeviceInfo contains device identification info from /api/v1.0/device endpoint
type DeviceInfo struct {
	Firmware        string `json:"firmware"`
	FirmwareVersion string `json:"firmwareVersion"`
	Product         string `json:"product"`
	Model           string `json:"model"`
	MAC             string `json:"mac"`
	Hostname        string `json:"hostname"`
}

// GetDeviceInfo fetches device info from /api/v1.0/device endpoint (AirOS 8+)
func (c *Client) GetDeviceInfo() (*DeviceInfo, error) {
	url := fmt.Sprintf("%s://%s/api/v1.0/device", c.scheme, c.host)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	// httpClient has cookie jar with auth cookies from login
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	var data map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}

	ident, ok := data["identification"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("no identification block")
	}

	info := &DeviceInfo{}
	if v, ok := ident["firmware"].(string); ok {
		info.Firmware = v
	}
	if v, ok := ident["firmwareVersion"].(string); ok {
		info.FirmwareVersion = v
	}
	if v, ok := ident["product"].(string); ok {
		info.Product = v
	}
	if v, ok := ident["model"].(string); ok {
		info.Model = v
	}
	if v, ok := ident["mac"].(string); ok {
		info.MAC = strings.ToLower(v)
	}
	if v, ok := ident["hostname"].(string); ok {
		info.Hostname = v
	} else if v, ok := ident["name"].(string); ok {
		info.Hostname = v
	}

	return info, nil
}

// GetStations returns the station list from either root or wireless.sta
func (s *Status) GetStations() []Station {
	var stas []Station

	// Check root level first (newer format)
	if len(s.Stations) > 0 {
		stas = append(stas, s.Stations...)
	} else if len(s.Wireless.Stations) > 0 {
		// Fall back to wireless.sta (older format)
		stas = append(stas, s.Wireless.Stations...)
	}

	// AirFiber master side is a PTP AP with a single remote. It does not always expose a standard
	// station list, so we synthesize a Station entry from the airfiber block so the rest of the
	// pipeline (peer parsing, topology, UI) can treat it like any other peer.
	if s.IsAirFiber() {
		op := strings.ToLower(strings.TrimSpace(s.Wireless.OpMode))
		if op == "master" && s.Airfiber != nil {
			af := s.Airfiber
			// Only synthesize a peer when the link is actually up.
			if strings.ToLower(strings.TrimSpace(af.LinkState)) != "operational" {
				return stas
			}

			remoteMAC := strings.ToLower(strings.TrimSpace(af.RemoteMAC))
			if remoteMAC != "" && remoteMAC != "00:00:00:00:00:00" {
				// Avoid duplicates if firmware already provided a station list.
				for _, sta := range stas {
					if strings.ToLower(strings.TrimSpace(sta.MAC)) == remoteMAC {
						return stas
					}
				}

				// Normalize capacity to AirMaxStats units (kbps).
				normalizeKbps := func(v int) int {
					if v <= 0 {
						return 0
					}
					// AirFiber firmwares vary: some report bps, some kbps, some Mbps.
					// - if very large, assume bps and convert to kbps
					if v >= 1_000_000 {
						return v / 1_000
					}
					// - if small, assume Mbps and convert to kbps
					if v < 1_000 {
						return v * 1_000
					}
					// - otherwise assume already kbps
					return v
				}
				// AirFiber tx/rx frequency is commonly reported in kHz (e.g. 11645000 -> 11645 MHz).
				normalizeMHz := func(v int) int {
					if v <= 0 {
						return 0
					}
					if v > 100_000 {
						return v / 1_000
					}
					return v
				}
				avg := func(vals []int) int {
					if len(vals) == 0 {
						return 0
					}
					sum := 0
					for _, x := range vals {
						sum += x
					}
					return sum / len(vals)
				}

				name := strings.TrimSpace(af.RemoteHostname)
				if name == "" {
					name = strings.TrimSpace(af.RemoteDeviceName)
				}
				if name == "" {
					name = strings.TrimSpace(af.RemoteDeviceModel)
				}
				if name == "" {
					name = remoteMAC
				}

				chain := af.GetChainSignals()
				remoteChain := af.GetRemoteChainSignals()

				signal := s.Wireless.Signal
				rssi := s.Wireless.RSSI
				if signal == 0 && len(chain) > 0 {
					signal = avg(chain)
				}
				if rssi == 0 {
					rssi = signal
				}
				remoteSignal := af.GetRemoteSignal()
				if remoteSignal == 0 && len(remoteChain) > 0 {
					remoteSignal = avg(remoteChain)
				}

				freq := s.Wireless.Frequency
				txFreq := normalizeMHz(af.TXFrequency)
				rxFreq := normalizeMHz(af.RXFrequency)
				if freq == 0 {
					if txFreq != 0 {
						freq = txFreq
					} else if rxFreq != 0 {
						freq = rxFreq
					}
				}
				if txFreq == 0 {
					txFreq = freq
				}
				if rxFreq == 0 {
					rxFreq = freq
				}

				nss := len(chain)

				distance := s.Wireless.Distance
				if af.LinkDistance > 0 {
					distance = af.LinkDistance
				}

				sta := Station{
					Name:        name,
					MAC:         remoteMAC,
					LastIP:      strings.TrimSpace(af.RemoteIP),
					Signal:      signal,
					RSSI:        rssi,
					NoiseFloor:  s.Wireless.NoiseFloor,
					Distance:    distance,
					Frequency:   freq,
					TxFrequency: txFreq,
					RxFrequency: rxFreq,
					TXPower:     af.TXPower,
					TXIdx:       af.TXModRate,
					RXIdx:       af.RXModRate,
				}
				if nss > 0 {
					sta.TXNSS = nss
					sta.RXNSS = nss
				}
				if len(chain) > 0 {
					sta.ChainRSSI = chain
				} else if af.Signal0dBm != 0 || af.Signal1dBm != 0 {
					sta.ChainRSSI = []int{af.Signal0dBm, af.Signal1dBm}
				}
				// Feed link capacity/CINR into the existing AirMaxStats-derived peer rendering.
				dlKbps := normalizeKbps(af.TXCapacity)
				ulKbps := normalizeKbps(af.RXCapacity)
				if dlKbps > 0 || ulKbps > 0 || af.CINRdB != 0 || af.RemoteCINRdB != 0 {
					am := &AirMaxStats{
						DownlinkCapacity: dlKbps,
						UplinkCapacity:   ulKbps,
					}
					// Poller conversion treats TX.CINR as DL and RX.CINR as UL.
					if af.CINRdB != 0 {
						if af.CINRdB > 0 {
							am.RX.CINR = int(af.CINRdB + 0.5)
						} else {
							am.RX.CINR = int(af.CINRdB - 0.5)
						}
					}
					if af.RemoteCINRdB != 0 {
						if af.RemoteCINRdB > 0 {
							am.TX.CINR = int(af.RemoteCINRdB + 0.5)
						} else {
							am.TX.CINR = int(af.RemoteCINRdB - 0.5)
						}
					}

					// Some airFiber firmwares only report CINR from one side.
					// Mirror the available value so we don't display a misleading 0.
					if am.TX.CINR == 0 && am.RX.CINR != 0 {
						am.TX.CINR = am.RX.CINR
					} else if am.RX.CINR == 0 && am.TX.CINR != 0 {
						am.RX.CINR = am.TX.CINR
					}

					sta.AirMax = am
				}

				remote := &RemoteInfo{
					Hostname:   name,
					Platform:   strings.TrimSpace(af.RemoteDeviceModel),
					Signal:     remoteSignal,
					RSSI:       remoteSignal,
					NoiseFloor: s.Wireless.NoiseFloor,
					TXPower:    af.TXPower,
					Distance:   distance,
				}
				if len(remoteChain) > 0 {
					remote.ChainRSSI = remoteChain
				} else if af.RemoteSignal0dBm != 0 || af.RemoteSignal1dBm != 0 {
					remote.ChainRSSI = []int{af.RemoteSignal0dBm, af.RemoteSignal1dBm}
				}
				sta.Remote = remote

				stas = append(stas, sta)
			}
		}
	}

	return stas
}

// DetectFlavor determines the device flavor/platform
func (s *Status) DetectFlavor() string {
	model := strings.ToLower(s.GetModel())

	switch {
	case strings.Contains(model, "litebeam"):
		return "LiteBeam"
	case strings.Contains(model, "powerbeam"):
		return "PowerBeam"
	case strings.Contains(model, "nanobeam"):
		return "NanoBeam"
	case strings.Contains(model, "rocket"), strings.Contains(model, "r5ac"):
		return "Rocket"
	case strings.Contains(model, "nanostation"):
		return "NanoStation"
	case strings.Contains(model, "liteap"):
		return "LiteAP"
	case strings.Contains(model, "prism"):
		return "Prism"
	case strings.Contains(model, "isostation"):
		return "IsoStation"
	case strings.Contains(model, "gigabeam"):
		return "GigaBeam"
	case strings.Contains(model, "ltu"):
		return "LTU"
	// AirFiber models - more specific detection
	case strings.Contains(model, "af-11"), strings.Contains(model, "af11"):
		return "AF11"
	case strings.Contains(model, "af-5"), strings.Contains(model, "af5"):
		// Check for AF5X vs AF5/AF5U
		if strings.Contains(model, "af5x") || strings.Contains(model, "af-5x") {
			return "AF5X"
		}
		return "AF5"
	case strings.Contains(model, "af-3"), strings.Contains(model, "af3"):
		return "AF3X"
	case strings.Contains(model, "af-2"), strings.Contains(model, "af2"):
		// AF2X uses 2GHz loader, not Gen2
		return "AF2X"
	case strings.Contains(model, "airfiber"):
		// Generic airfiber fallback
		return "airFiber"
	default:
		return "AirMAX"
	}
}

// DetectFirmwarePlatform extracts the firmware platform prefix (XM, XW, XC, WA, TI, AF, etc.)
func (s *Status) DetectFirmwarePlatform() string {
	fw := s.Host.FWVersion

	// AirFiber firmware prefixes
	// AF11: AF11.ar934x.vX.X.X
	// AF5/AF5U: AF5.ar934x.vX.X.X or AF5U.ar934x.vX.X.X
	// AF5X: AF5X.ar934x.vX.X.X
	// AF3X: AF3X.ar934x.vX.X.X
	// AF2X: AF2X.ar934x.vX.X.X (2GHz loader, not Gen2)
	if strings.HasPrefix(fw, "AF11.") {
		return "AF11"
	}
	if strings.HasPrefix(fw, "AF5X.") {
		return "AF5X"
	}
	if strings.HasPrefix(fw, "AF5U.") {
		return "AF5U"
	}
	if strings.HasPrefix(fw, "AF5.") {
		return "AF5"
	}
	if strings.HasPrefix(fw, "AF3X.") {
		return "AF3X"
	}
	if strings.HasPrefix(fw, "AF2X.") {
		return "AF2X" // 2GHz chipset loader, not Gen2
	}

	// Format: "XC.qca955x.v8.7.0..." or "v8.7.0" or "XM.ar7240.v6.3.0"
	// AirOS 8 (AC series) - XC/2XC/WA/2WA
	if strings.HasPrefix(fw, "XC.") || strings.HasPrefix(fw, "2XC.") {
		return "XC" // AC series (QCA chipset) - AirOS 8
	}
	if strings.HasPrefix(fw, "WA.") || strings.HasPrefix(fw, "2WA.") {
		return "WA" // AC series variant - AirOS 8
	}
	// AirOS 5 (M series) - XM/XW/TI
	if strings.HasPrefix(fw, "XM.") {
		return "XM" // M series (Atheros AR7240/AR9342) - AirOS 5
	}
	if strings.HasPrefix(fw, "XW.") {
		return "XW" // M series variant - AirOS 5
	}
	if strings.HasPrefix(fw, "TI.") {
		return "TI" // M series TI chipset variant - AirOS 5
	}

	// Try to detect from model name if not in version
	model := strings.ToLower(s.Host.DevModel)

	// LTU devices (have "ltu" in model name, firmware is just version like "2.3.2")
	// Only Rocket uses AFLTUROCKET, all others (LTU-Lite, LTU-LR) use AFLTU
	if strings.Contains(model, "ltu") {
		if strings.Contains(model, "rocket") {
			return "AFLTUROCKET"
		}
		return "AFLTU"
	}

	if strings.Contains(model, "5ac") || strings.Contains(model, "ac ") ||
		strings.Contains(model, "prism") || strings.Contains(model, "iso") ||
		strings.Contains(model, "liteap") {
		return "XC" // AC series
	}
	if strings.Contains(model, "m5") || strings.Contains(model, "m2") ||
		strings.Contains(model, "m3") || strings.Contains(model, "m900") {
		return "XM" // M series
	}
	// AirFiber detection from model
	if strings.Contains(model, "af-11") || strings.Contains(model, "af11") {
		return "AF11"
	}
	if strings.Contains(model, "af-5x") || strings.Contains(model, "af5x") {
		return "AF5X"
	}
	if strings.Contains(model, "af-5") || strings.Contains(model, "af5") {
		return "AF5"
	}
	if strings.Contains(model, "af-3") || strings.Contains(model, "af3") {
		return "AF3X"
	}
	if strings.Contains(model, "af-2") || strings.Contains(model, "af2") {
		return "AF2X"
	}

	return "" // Unknown
}

// IsAirOS8 returns true if the device runs AirOS 8 (AC series)
func (s *Status) IsAirOS8() bool {
	platform := s.DetectFirmwarePlatform()
	return platform == "XC" || platform == "WA"
}

// IsAirOS5 returns true if the device runs AirOS 5 (M series)
func (s *Status) IsAirOS5() bool {
	platform := s.DetectFirmwarePlatform()
	return platform == "XM" || platform == "XW" || platform == "TI"
}

// IsAirFiber returns true if the device is an AirFiber (AF2/AF3/AF5/AF11)
func (s *Status) IsAirFiber() bool {
	platform := s.DetectFirmwarePlatform()
	switch platform {
	case "AF2X", "AF3X", "AF5", "AF5U", "AF5X", "AF11":
		return true
	}
	// Also check if airfiber block is present in status
	if s.Airfiber != nil {
		return true
	}
	// Check model name
	model := strings.ToLower(s.GetModel())
	return strings.Contains(model, "airfiber") || strings.HasPrefix(model, "af")
}

// GetAirFiberBand returns the frequency band for AirFiber devices
func (s *Status) GetAirFiberBand() string {
	platform := s.DetectFirmwarePlatform()
	switch platform {
	case "AF2X":
		return "2GHz"
	case "AF3X":
		return "3GHz"
	case "AF5", "AF5U", "AF5X":
		return "5GHz"
	case "AF11":
		return "11GHz"
	}
	// Try to detect from model
	model := strings.ToLower(s.GetModel())
	if strings.Contains(model, "af-11") || strings.Contains(model, "af11") {
		return "11GHz"
	}
	if strings.Contains(model, "af-5") || strings.Contains(model, "af5") {
		return "5GHz"
	}
	if strings.Contains(model, "af-3") || strings.Contains(model, "af3") {
		return "3GHz"
	}
	if strings.Contains(model, "af-2") || strings.Contains(model, "af2") {
		return "2GHz"
	}
	return ""
}

// ExtractVersion extracts the version from firmware string, preserving suffixes
// "XC.qca955x.v8.7.0-devel-foo.hash" -> "v8.7.0-devel-foo"
// "v8.7.0-beta" -> "v8.7.0-beta"
func (s *Status) ExtractVersion() string {
	fw := s.Host.FWVersion

	// Remove platform prefix if present
	// "XC.qca955x.v8.7.0-devel..." -> "v8.7.0-devel..."
	idx := strings.Index(fw, ".v")
	if idx > 0 {
		fw = fw[idx+1:] // Skip the dot, keep the v
	}

	// Find where version ends (at hash or date portion)
	// v8.7.0-beta.abc123.251220 -> v8.7.0-beta
	// v8.7.0.abc123 -> v8.7.0
	if strings.HasPrefix(fw, "v") {
		parts := strings.Split(fw, ".")
		var result []string
		for i, p := range parts {
			if i == 0 {
				// First part is version start (v8 or v8-beta)
				result = append(result, p)
				continue
			}
			// Numeric parts are version numbers, mixed parts might be suffixes
			// Stop at hash-like or date-like parts
			if len(p) > 0 && ((p[0] >= '0' && p[0] <= '9') || p[0] == '-' || p[0] == '_') {
				// Check if it looks like a hash (6+ hex chars) or date (6 digits)
				if isHashOrDate(p) {
					break
				}
				result = append(result, p)
			} else {
				break
			}
		}
		return strings.Join(result, ".")
	}

	return fw
}

// isHashOrDate checks if a string looks like a git hash or date
func isHashOrDate(s string) bool {
	// Strip any prefix like version suffix
	s = strings.TrimLeft(s, "-_")

	// Date patterns: 6 digits (YYMMDD) or 8 digits (YYYYMMDD)
	if len(s) == 6 || len(s) == 8 {
		allDigit := true
		for _, c := range s {
			if c < '0' || c > '9' {
				allDigit = false
				break
			}
		}
		if allDigit {
			return true
		}
	}

	// Hash pattern: 6+ hex characters
	if len(s) >= 6 {
		allHex := true
		for _, c := range s {
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
				allHex = false
				break
			}
		}
		if allHex {
			return true
		}
	}

	return false
}

// UpgradeFirmware uploads and installs firmware on the device
// AirOS 8: POST /fwupl.cgi (multipart, X-CSRF-ID header) then POST /fwflash.cgi do_update=1
// AirOS 5/6: POST /system.cgi (multipart with token field) then POST /fwflash.cgi do_update=1
func (c *Client) UpgradeFirmware(firmwareData []byte, filename string) error {
	// Fetch CSRF token if we don't have one - needed for AirOS 8+
	if c.csrfToken == "" {
		c.fetchCSRFToken(c.scheme)
		// Ignore error - if it fails, we'll try AirOS 5 path
	}

	// AirOS 8+ uses X-CSRF-ID header, AirOS 5/6 uses token in form field
	if c.csrfToken != "" {
		return c.upgradeFirmwareAirOS8(firmwareData, filename)
	}
	return c.upgradeFirmwareAirOS5(firmwareData, filename)
}

// upgradeFirmwareAirOS8 handles AirOS 8+ firmware upgrade
func (c *Client) upgradeFirmwareAirOS8(firmwareData []byte, filename string) error {
	// Step 1: Upload firmware via /fwupl.cgi
	uploadURL := fmt.Sprintf("%s://%s/fwupl.cgi", c.scheme, c.host)

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	// Field name must be "fwfile" for AirOS 8
	part, err := writer.CreateFormFile("fwfile", filename)
	if err != nil {
		return fmt.Errorf("create form file: %w", err)
	}
	if _, err := part.Write(firmwareData); err != nil {
		return fmt.Errorf("write firmware data: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("finalize multipart: %w", err)
	}

	req, err := http.NewRequest("POST", uploadURL, &buf)
	if err != nil {
		return fmt.Errorf("create upload request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("X-CSRF-ID", c.csrfToken)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")

	// Build request description for error messages
	reqDesc := fmt.Sprintf("POST %s [X-CSRF-ID=%v, file=%s, size=%d]", uploadURL, c.csrfToken != "", filename, len(firmwareData))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("firmware upload failed: %s -> %w", reqDesc, err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("firmware upload failed: %s -> status %d: %s", reqDesc, resp.StatusCode, string(body))
	}

	return c.TriggerFlash()
}

// upgradeFirmwareAirOS5 handles AirOS 5/6 firmware upgrade
func (c *Client) upgradeFirmwareAirOS5(firmwareData []byte, filename string) error {
	// Step 1: GET /system.cgi to scrape the token from HTML
	systemURL := fmt.Sprintf("%s://%s/system.cgi", c.scheme, c.host)

	resp, err := c.httpClient.Get(systemURL)
	if err != nil {
		return fmt.Errorf("GET %s -> %w", systemURL, err)
	}
	pageBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// Extract token from hidden input: <input type="hidden" name="token" value="...">
	token := extractToken(string(pageBody))

	// Step 2: POST firmware to /system.cgi with token and fwfile
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	if token != "" {
		if err := writer.WriteField("token", token); err != nil {
			return fmt.Errorf("write token field: %w", err)
		}
	}

	part, err := writer.CreateFormFile("fwfile", filename)
	if err != nil {
		return fmt.Errorf("create form file: %w", err)
	}
	if _, err := part.Write(firmwareData); err != nil {
		return fmt.Errorf("write firmware data: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("finalize multipart: %w", err)
	}

	req, err := http.NewRequest("POST", systemURL, &buf)
	if err != nil {
		return fmt.Errorf("create upload request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("X-Requested-With", "XMLHttpRequest")

	// Build request description for error messages
	reqDesc := fmt.Sprintf("POST %s [token=%v, file=%s, size=%d]", systemURL, token != "", filename, len(firmwareData))

	uploadResp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("firmware upload failed: %s -> %w", reqDesc, err)
	}
	body, _ := io.ReadAll(uploadResp.Body)
	uploadResp.Body.Close()

	// system.cgi may return 200 or redirect - check for errors in body
	bodyLower := strings.ToLower(string(body))
	if strings.Contains(bodyLower, "error") && !strings.Contains(bodyLower, "success") {
		return fmt.Errorf("firmware upload failed: %s -> %s", reqDesc, string(body))
	}

	return c.TriggerFlash()
}

// TriggerFlash sends POST /fwflash.cgi with do_update=1
func (c *Client) TriggerFlash() error {
	// Fetch CSRF token if we don't have one - needed for AirOS 8+
	if c.csrfToken == "" {
		c.fetchCSRFToken(c.scheme)
		// Ignore error - we'll try without it
	}

	flashURL := fmt.Sprintf("%s://%s/fwflash.cgi", c.scheme, c.host)

	flashReq, err := http.NewRequest("POST", flashURL, strings.NewReader("do_update=1"))
	if err != nil {
		return fmt.Errorf("create flash request: %w", err)
	}
	flashReq.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	flashReq.Header.Set("X-Requested-With", "XMLHttpRequest")
	if c.csrfToken != "" {
		flashReq.Header.Set("X-CSRF-ID", c.csrfToken)
	}

	// Build request description for error messages
	reqDesc := fmt.Sprintf("POST %s [X-CSRF-ID=%v, X-Requested-With=XMLHttpRequest]", flashURL, c.csrfToken != "")

	// Use short timeout - device may reboot immediately after acknowledging
	// Must include cookie jar to maintain session
	flashClient := &http.Client{
		Timeout:   10 * time.Second,
		Transport: c.httpClient.Transport,
		Jar:       c.httpClient.Jar,
	}
	flashResp, err := flashClient.Do(flashReq)
	if err != nil {
		// Connection errors may be expected if device reboots immediately
		return nil
	}
	flashBody, _ := io.ReadAll(flashResp.Body)
	flashResp.Body.Close()

	if flashResp.StatusCode != http.StatusOK {
		// If CSRF is missing and we got 403, include the reason CSRF fetch failed
		if c.csrfToken == "" && c.csrfError != nil && flashResp.StatusCode == 403 {
			return fmt.Errorf("flash trigger failed: %s -> status %d (no CSRF token: %v)", reqDesc, flashResp.StatusCode, c.csrfError)
		}
		return fmt.Errorf("flash trigger failed: %s -> status %d: %s", reqDesc, flashResp.StatusCode, string(flashBody))
	}

	return nil
}

// extractToken extracts token value from HTML hidden input
func extractToken(html string) string {
	// Look for: <input type="hidden" name="token" value="...">
	// or: name="token" value="..."
	patterns := []string{
		`name="token"\s+value="([^"]+)"`,
		`name='token'\s+value='([^']+)'`,
		`value="([^"]+)"\s+name="token"`,
	}
	for _, pattern := range patterns {
		re := regexp.MustCompile(pattern)
		if matches := re.FindStringSubmatch(html); len(matches) > 1 {
			return matches[1]
		}
	}
	return ""
}

// Reboot reboots the device
func (c *Client) Reboot() error {
	url := fmt.Sprintf("%s://%s/reboot.cgi", c.scheme, c.host)

	data := strings.NewReader("reboot=yes")
	req, err := http.NewRequest("POST", url, data)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	// Include CSRF token if we have one
	if c.csrfToken != "" {
		req.Header.Set("X-CSRF-ID", c.csrfToken)
	}

	// Use short timeout - device may reboot immediately and not respond
	// Must include cookie jar to maintain session
	shortClient := &http.Client{
		Timeout:   5 * time.Second,
		Transport: c.httpClient.Transport,
		Jar:       c.httpClient.Jar,
	}
	resp, err := shortClient.Do(req)
	if err != nil {
		// Connection reset/EOF/timeout immediately after POST is expected when the
		// device accepts the reboot and drops management services. Do not hide
		// obvious pre-submit failures such as DNS/no-route/login mistakes.
		if strings.Contains(err.Error(), "connection reset") ||
			strings.Contains(err.Error(), "EOF") ||
			strings.Contains(err.Error(), "timeout") ||
			strings.Contains(err.Error(), "Client.Timeout") {
			return nil
		}
		return fmt.Errorf("reboot request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("reboot.cgi returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	return nil
}

// FetchConfig gets the device configuration backup
func (c *Client) FetchConfig() ([]byte, error) {
	// Note: Don't try to fetch CSRF token here - if we logged in via login.cgi,
	// calling /api/auth would invalidate our session. If we logged in via /api/auth,
	// we already have CSRF token. Just try with what we have.

	// AirOS 8 uses /cfg.cgi?timestamp=... for backup download
	timestamp := time.Now().UnixMilli()
	url := fmt.Sprintf("%s://%s/cfg.cgi?timestamp=%d", c.scheme, c.host, timestamp)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch config failed: %w", err)
	}

	// Add CSRF token header if we have one (from /api/auth login)
	if c.csrfToken != "" {
		req.Header.Set("X-CSRF-ID", c.csrfToken)
	}
	req.Header.Set("X-Requested-With", "XMLHttpRequest")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch config failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("fetch config returned %d: %s", resp.StatusCode, string(body))
	}

	return io.ReadAll(resp.Body)
}

// PushConfig uploads a configuration to the device
func (c *Client) PushConfig(config []byte) error {
	url := fmt.Sprintf("%s://%s/writecfg.cgi", c.scheme, c.host)

	data := "cfgData=" + string(config)
	req, err := http.NewRequest("POST", url, strings.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("push config failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("push config returned %d", resp.StatusCode)
	}

	return nil
}

// ApplyConfig applies pending configuration changes
func (c *Client) ApplyConfig() error {
	url := fmt.Sprintf("%s://%s/apply.cgi", c.scheme, c.host)

	data := strings.NewReader("apply=yes")
	req, err := http.NewRequest("POST", url, data)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("apply config failed: %w", err)
	}
	defer resp.Body.Close()

	return nil
}
