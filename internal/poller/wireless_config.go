package poller

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/yellowman/wavecontrol/internal/airmax"
	"github.com/yellowman/wavecontrol/internal/stats"
)

// fetchWaveConfig fetches and parses configuration from Wave API
func (p *Poller) fetchWaveConfig(client *http.Client, baseURL, token string) (*stats.WirelessConfig, *stats.NetworkConfig) {
	req, err := http.NewRequest("GET", baseURL+"/api/v1.0/system/airos/configuration", nil)
	if err != nil {
		return nil, nil
	}
	req.Header.Set("x-auth-token", token)

	resp, err := client.Do(req)
	if err != nil {
		return nil, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		io.Copy(io.Discard, resp.Body) // Drain body for connection reuse
		return nil, nil
	}

	var data map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, nil
	}

	cfg := p.parseWaveConfig(data)
	netCfg := p.parseWaveNetwork(data)
	return cfg, netCfg
}

// parseWaveConfig parses Wave API config response into WirelessConfig
func (p *Poller) parseWaveConfig(data map[string]any) *stats.WirelessConfig {
	cfg := &stats.WirelessConfig{}

	wireless, ok := data["wireless"].(map[string]any)
	if !ok {
		return nil
	}

	interfacesRaw, ok := wireless["interfaces"].([]any)
	if !ok || len(interfacesRaw) == 0 {
		return nil
	}

	// Normalize interface list
	interfaces := make([]map[string]any, 0, len(interfacesRaw))
	for _, v := range interfacesRaw {
		if m, ok := v.(map[string]any); ok {
			interfaces = append(interfaces, m)
		}
	}
	if len(interfaces) == 0 {
		return nil
	}

	// SSID: prefer the first non-empty interface SSID
	for _, iface := range interfaces {
		if ssid := getString(iface, "ssid"); ssid != "" {
			cfg.SSID = ssid
			break
		}
	}

	// Mode: some Wave firmware does not populate wireless.mode; derive from per-interface apMode.
	if mode := strings.ToLower(getString(wireless, "mode")); mode != "" {
		cfg.Mode = mode
	} else {
		mode := "sta"
		for _, iface := range interfaces {
			if getBool(iface, "apMode") {
				mode = "ap"
				break
			}
		}
		cfg.Mode = mode
	}

	// Net mode: some firmware does not populate wireless.netMode; derive from per-interface ptpMode.
	if netMode := strings.ToLower(getString(wireless, "netMode")); netMode != "" {
		cfg.NetMode = netMode
	} else {
		for _, iface := range interfaces {
			if getBool(iface, "ptpMode") {
				cfg.NetMode = "ptp"
				break
			}
		}
	}

	// WaveAI (only for APs): if any interface has waveAi enabled, treat WaveAI as enabled.
	if cfg.Mode == "ap" {
		for _, iface := range interfaces {
			if getBool(iface, "waveAi") {
				cfg.WaveAI = true
				break
			}
		}
	}

	// Auto frequency per band.
	//
	// IMPORTANT: Wave MLO devices may present as 5+6 GHz, and older Wave LR/Pro as 60+5 GHz.
	// Do not assume interface ordering (main/backup) implies band.
	for _, iface := range interfaces {
		freq, ok := iface["frequency"].(map[string]any)
		if !ok {
			continue
		}

		auto := getBool(freq, "auto")
		band := inferWaveBandFromConfigInterface(iface)
		switch band {
		case waveBand60GHz:
			cfg.AutoFreq60 = cfg.AutoFreq60 || auto
		case waveBand6GHz:
			cfg.AutoFreq6 = cfg.AutoFreq6 || auto
		case waveBand5GHz:
			cfg.AutoFreq5 = cfg.AutoFreq5 || auto
		}
	}

	return cfg
}

// parseWaveNetwork parses Wave API config response into NetworkConfig.
// This pulls the most relevant network settings for display in the Host panel.
func (p *Poller) parseWaveNetwork(data map[string]any) *stats.NetworkConfig {
	networkRaw, ok := data["network"].(map[string]any)
	if !ok {
		return nil
	}

	nc := &stats.NetworkConfig{}

	// Basic network-wide settings
	nc.Mode = getString(networkRaw, "mode")
	nc.MTU = int(getFloat(networkRaw, "mtu"))

	// DNS servers
	if dnsRaw, ok := networkRaw["dnsServers"].([]any); ok {
		for _, v := range dnsRaw {
			if m, ok := v.(map[string]any); ok {
				addr := getString(m, "address")
				if addr != "" {
					nc.DNSServers = append(nc.DNSServers, addr)
				}
			}
		}
	}

	// Interface-specific data settings (mgmt VLAN + IPv4 config)
	if ifaces, ok := networkRaw["interfaces"].(map[string]any); ok {
		if dataIface, ok := ifaces["data"].(map[string]any); ok {
			nc.MgmtVLAN = int(getFloat(dataIface, "mgmtVLAN"))
			if ipv4, ok := dataIface["ipv4"].(map[string]any); ok {
				nc.DataIPv4CIDR = getString(ipv4, "cidr")
				nc.DataIPv4Mode = getString(ipv4, "mode")
				nc.DefaultGateway = getString(ipv4, "defaultGateway")
			}
		}
	}

	// If the device doesn't provide any of the expected fields, drop the section.
	if nc.Mode == "" && nc.MTU == 0 && nc.MgmtVLAN == 0 && nc.DataIPv4CIDR == "" && nc.DataIPv4Mode == "" && nc.DefaultGateway == "" && len(nc.DNSServers) == 0 {
		return nil
	}

	return nc
}

// inferWaveBandFromConfigInterface determines the effective band of a Wave wireless interface
// from the configuration payload.
func inferWaveBandFromConfigInterface(iface map[string]any) waveBand {
	// First: reuse the same inference used for statistics radios (tx freq / AFC / channel width).
	if band := inferWaveBand(iface, nil); band != waveBandUnknown {
		return band
	}

	freq, ok := iface["frequency"].(map[string]any)
	if !ok {
		return waveBandUnknown
	}

	// Wave config provides a stable "band" (e.g., 5000/6000/60000) even when tx is null (auto).
	for _, v := range []int{
		int(getFloat(freq, "band")),
		int(getFloat(freq, "control")),
		int(getFloat(freq, "tx")),
	} {
		if v == 0 {
			continue
		}
		if v >= 57000 {
			return waveBand60GHz
		}
		if v >= 5925 {
			return waveBand6GHz
		}
		if v >= 4900 {
			return waveBand5GHz
		}
		if v >= 2300 {
			return waveBandUnknown
		}
	}

	// AFC presence is a strong 6 GHz indicator even if band/control/tx are absent.
	if _, ok := iface["afc"].(map[string]any); ok {
		return waveBand6GHz
	}

	return waveBandUnknown
}

// parseAirMAXConfig builds WirelessConfig from AirMAX status.cgi data
func (p *Poller) parseAirMAXConfig(status *airmax.Status) *stats.WirelessConfig {
	if status == nil {
		return nil
	}

	cfg := &stats.WirelessConfig{}

	// SSID
	cfg.SSID = status.Wireless.ESSID

	// Parse mode (ap-ptmp, sta-ptp, etc.)
	mode := strings.ToLower(status.Wireless.Mode)
	if strings.HasPrefix(mode, "ap") {
		cfg.Mode = "ap"
	} else if strings.HasPrefix(mode, "sta") {
		cfg.Mode = "sta"
	}

	if strings.Contains(mode, "ptp") {
		cfg.NetMode = "ptp"
	} else if strings.Contains(mode, "ptmp") {
		cfg.NetMode = "ptmp"
	}

	// Polling features - polling can be object with gps_sync field
	polling := status.Wireless.GetPollingInfo()
	if polling != nil {
		if polling.FixedFrame {
			cfg.FrameMode = "fixed"
			cfg.FrameLen = polling.FFFrameDur
			cfg.DLRatio = polling.FFDLRatio
		} else if polling.FlexMode {
			cfg.FrameMode = "flex"
		}
		cfg.GPSSync = polling.GPSSync
	}

	// AirMAX AC GPS sync - gps_state field (1 = synced, 0 = not synced)
	// This is separate from polling object - used when polling is just "enabled" string
	if status.Wireless.GPSState > 0 {
		cfg.GPSSync = true
	}

	// LTU sync mode
	if status.Wireless.SyncMode > 0 {
		cfg.GPSSync = true
	}

	// LTU fixed frame from wireless fields
	if status.Wireless.FrameLength > 0 && status.Wireless.DutyCycle > 0 {
		cfg.FrameMode = "fixed"
		cfg.FrameLen = status.Wireless.FrameLength
		cfg.DLRatio = status.Wireless.DutyCycle
	}

	// 802.11n compatibility
	cfg.Compat11N = status.Wireless.Compat11N != 0

	return cfg
}
