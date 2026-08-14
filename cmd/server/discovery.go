package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/yellowman/wavecontrol/internal/airmax"
	"github.com/yellowman/wavecontrol/internal/tlsutil"
	"github.com/yellowman/wavecontrol/internal/udebug"
)

// DeviceInfo holds discovered device information
type DeviceInfo struct {
	MAC             string
	IP              string
	Hostname        string
	Product         string
	Model           string
	Platform        string // "wave", "airmax"
	Flavor          string // GMC, GMP, MGMP, MW for wave; AFLTUROCKET, AFLTU, AF5XHD for LTU
	Firmware        string
	FirmwareVersion string
}

// STAInfo holds discovered STA information
type STAInfo struct {
	DeviceInfo
	SSID        string
	SignalLevel int
	Distance    float64
}

// Wave API response structures
type waveDeviceResponse struct {
	Identification struct {
		Firmware        string `json:"firmware"`
		FirmwareVersion string `json:"firmwareVersion"`
		Model           string `json:"model"`
		Product         string `json:"product"`
		MAC             string `json:"mac"`
		Hostname        string `json:"hostname"`
		Name            string `json:"name"`
	} `json:"identification"`
	Capabilities struct {
		Device struct {
			SupportedFirmwares []struct {
				Flavor string `json:"flavor"`
			} `json:"supportedFirmwares"`
		} `json:"device"`
	} `json:"capabilities"`
	// Hostname can be in multiple places
	Host struct {
		Hostname string `json:"hostname"`
		Name     string `json:"name"`
	} `json:"host"`
	Device struct {
		Name     string `json:"name"`
		Hostname string `json:"hostname"`
	} `json:"device"`
	General struct {
		Name       string `json:"name"`
		DeviceName string `json:"deviceName"`
	} `json:"general"`
}

type waveStatisticsResponse []struct {
	Device struct {
		Hostname string `json:"hostname"`
		Name     string `json:"name"`
	} `json:"device"`
	Wireless struct {
		Peers []struct {
			Common struct {
				MgmtIP         string  `json:"mgmtIp"`
				Hostname       string  `json:"hostname"`
				Distance       float64 `json:"distance"`
				Identification struct {
					Firmware        string `json:"firmware"`
					FirmwareVersion string `json:"firmwareVersion"`
					Model           string `json:"model"`
					Product         string `json:"product"`
					MAC             string `json:"mac"`
				} `json:"identification"`
			} `json:"common"`
			Local []struct {
				LinkQuality struct {
					Signal int `json:"signal"`
				} `json:"linkQuality"`
				SSID string `json:"ssid"`
			} `json:"local"`
		} `json:"peers"`
	} `json:"wireless"`
}

// Create HTTP client with TLS verification from manager
func newHTTPClientWithTLS(tlsMgr *tlsutil.Manager, ip string, timeout time.Duration, udbg *udebug.Manager, label string) *http.Client {
	var transport http.RoundTripper
	if tlsMgr != nil {
		transport = tlsMgr.GetTransportForIP(ip)
	} else {
		transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}
	if udbg != nil && udbg.IsHostEnabled(ip) {
		if strings.TrimSpace(label) == "" {
			label = "discover"
		}
		transport = udebug.WrapTransportHost(udbg, ip, transport, label, udebug.DefaultCaptureLimit)
	}
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}
}

// Create HTTP client without TLS verification (for internal AP->STA discovery)
func newHTTPClient(timeout time.Duration, host string, udbg *udebug.Manager, label string) *http.Client {
	var transport http.RoundTripper = &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	if udbg != nil && udbg.IsHostEnabled(host) {
		if strings.TrimSpace(label) == "" {
			label = "discover"
		}
		transport = udebug.WrapTransportHost(udbg, host, transport, label, udebug.DefaultCaptureLimit)
	}
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}
}

// discoverDevice connects to a device and retrieves its info
func discoverDevice(ip, username, password string, tlsMgr *tlsutil.Manager, udbg *udebug.Manager) (*DeviceInfo, error) {
	// Try Wave API first
	info, err := discoverWaveDevice(ip, username, password, tlsMgr, udbg)
	if err == nil {
		return info, nil
	}

	// Wave failed, try AirMAX
	logDebug("discoverDevice %s: Wave failed (%v), trying AirMAX", ip, err)
	info, err = discoverAirMAXDevice(ip, username, password, tlsMgr, udbg)
	if err == nil {
		return info, nil
	}

	logDebug("discoverDevice %s: AirMAX also failed: %v", ip, err)
	return nil, fmt.Errorf("all discovery methods failed")
}

// discoverWaveDevice tries Wave API
func discoverWaveDevice(ip, username, password string, tlsMgr *tlsutil.Manager, udbg *udebug.Manager) (*DeviceInfo, error) {
	client := newHTTPClientWithTLS(tlsMgr, ip, 30*time.Second, udbg, "discover:wave")
	baseURL := fmt.Sprintf("https://%s", ip)

	// Login
	loginBody, _ := json.Marshal(map[string]string{
		"username": username,
		"password": password,
	})

	loginReq, _ := http.NewRequest("POST", baseURL+"/api/v1.0/user/login", bytes.NewReader(loginBody))
	loginReq.Header.Set("Content-Type", "application/json")

	loginResp, err := client.Do(loginReq)
	if err != nil {
		return nil, fmt.Errorf("login request: %w", err)
	}
	defer loginResp.Body.Close()

	if loginResp.StatusCode != 200 {
		return nil, fmt.Errorf("login failed: %s", loginResp.Status)
	}

	token := loginResp.Header.Get("x-auth-token")
	if token == "" {
		return nil, fmt.Errorf("no auth token in response")
	}

	// Get device info
	deviceReq, _ := http.NewRequest("GET", baseURL+"/api/v1.0/device", nil)
	deviceReq.Header.Set("x-auth-token", token)

	deviceResp, err := client.Do(deviceReq)
	if err != nil {
		return nil, fmt.Errorf("device request: %w", err)
	}
	defer deviceResp.Body.Close()

	if deviceResp.StatusCode != 200 {
		return nil, fmt.Errorf("device info failed: %s", deviceResp.Status)
	}

	var deviceData waveDeviceResponse
	if err := json.NewDecoder(deviceResp.Body).Decode(&deviceData); err != nil {
		return nil, fmt.Errorf("parse device: %w", err)
	}

	// Get statistics for hostname
	var hostname string
	statsReq, _ := http.NewRequest("GET", baseURL+"/api/v1.0/statistics", nil)
	statsReq.Header.Set("x-auth-token", token)

	statsResp, err := client.Do(statsReq)
	if err == nil {
		defer statsResp.Body.Close()
		if statsResp.StatusCode == 200 {
			var statsData waveStatisticsResponse
			if json.NewDecoder(statsResp.Body).Decode(&statsData) == nil && len(statsData) > 0 {
				// Try multiple locations for hostname
				if statsData[0].Device.Hostname != "" {
					hostname = statsData[0].Device.Hostname
				} else if statsData[0].Device.Name != "" {
					hostname = statsData[0].Device.Name
				}
			}
		}
	}

	// Fallback to device endpoint for hostname
	if hostname == "" {
		// Try identification block
		if deviceData.Identification.Hostname != "" {
			hostname = deviceData.Identification.Hostname
		} else if deviceData.Identification.Name != "" {
			hostname = deviceData.Identification.Name
		}
	}
	if hostname == "" {
		// Try host block
		if deviceData.Host.Hostname != "" {
			hostname = deviceData.Host.Hostname
		} else if deviceData.Host.Name != "" {
			hostname = deviceData.Host.Name
		}
	}
	if hostname == "" {
		// Try device block
		if deviceData.Device.Name != "" {
			hostname = deviceData.Device.Name
		} else if deviceData.Device.Hostname != "" {
			hostname = deviceData.Device.Hostname
		}
	}
	if hostname == "" {
		// Try general block
		if deviceData.General.Name != "" {
			hostname = deviceData.General.Name
		} else if deviceData.General.DeviceName != "" {
			hostname = deviceData.General.DeviceName
		}
	}

	// Try /api/v1.0/system endpoint for device name (common on Wave/LTU)
	if hostname == "" {
		sysReq, _ := http.NewRequest("GET", baseURL+"/api/v1.0/system", nil)
		sysReq.Header.Set("x-auth-token", token)
		sysResp, err := client.Do(sysReq)
		if err == nil {
			defer sysResp.Body.Close()
			if sysResp.StatusCode == 200 {
				var system map[string]any
				if json.NewDecoder(sysResp.Body).Decode(&system) == nil {
					// Top-level fields
					if name, ok := system["name"].(string); ok && name != "" {
						hostname = name
					} else if name, ok := system["hostname"].(string); ok && name != "" {
						hostname = name
					} else if name, ok := system["deviceName"].(string); ok && name != "" {
						hostname = name
					}
					// Check general block (common on Wave/LTU)
					if hostname == "" {
						if general, ok := system["general"].(map[string]any); ok {
							if name, ok := general["name"].(string); ok && name != "" {
								hostname = name
							} else if name, ok := general["deviceName"].(string); ok && name != "" {
								hostname = name
							}
						}
					}
					// Check device block
					if hostname == "" {
						if device, ok := system["device"].(map[string]any); ok {
							if name, ok := device["name"].(string); ok && name != "" {
								hostname = name
							}
						}
					}
					// Debug log keys if not found
					if hostname == "" {
						keys := make([]string, 0, len(system))
						for k := range system {
							keys = append(keys, k)
						}
						logDebug("discoverWaveDevice %s: no hostname in /api/v1.0/system, keys: %v", ip, keys)
					}
				}
			}
		}
	}

	// Determine flavor
	flavor := ""
	if len(deviceData.Capabilities.Device.SupportedFirmwares) > 0 {
		flavor = deviceData.Capabilities.Device.SupportedFirmwares[0].Flavor
	}
	if flavor == "" {
		flavor = extractFlavor(deviceData.Identification.Firmware)
	}

	// Detect platform from firmware (wave vs ltu vs airmax)
	platform := detectPlatformWithModel(deviceData.Identification.Firmware, deviceData.Identification.Model)

	// Extract firmware version if not provided
	fwVersion := deviceData.Identification.FirmwareVersion
	if fwVersion == "" && deviceData.Identification.Firmware != "" {
		fwVersion = extractFirmwareVersion(deviceData.Identification.Firmware)
	}

	return &DeviceInfo{
		MAC:             strings.ToLower(deviceData.Identification.MAC), // Normalize MAC to lowercase
		IP:              ip,
		Hostname:        hostname,
		Product:         deviceData.Identification.Product,
		Model:           deviceData.Identification.Model,
		Platform:        platform,
		Flavor:          flavor,
		Firmware:        deviceData.Identification.Firmware,
		FirmwareVersion: fwVersion,
	}, nil
}

// discoverAirMAXDevice tries AirMAX API
func discoverAirMAXDevice(ip, username, password string, tlsMgr *tlsutil.Manager, udbg *udebug.Manager) (*DeviceInfo, error) {
	var transport http.RoundTripper
	if tlsMgr != nil {
		transport = tlsMgr.GetTransportForIP(ip)
	} else {
		transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}
	if udbg != nil && udbg.IsHostEnabled(ip) {
		transport = udebug.WrapTransportHost(udbg, ip, transport, "discover:airmax", udebug.DefaultCaptureLimit)
	}
	client := airmax.NewClientWithTransport(ip, 30*time.Second, transport)

	// Try login
	err := client.Login(username, password)
	if err != nil {
		return nil, fmt.Errorf("airmax login: %w", err)
	}

	// Get status
	status, err := client.GetStatus()
	if err != nil {
		return nil, fmt.Errorf("airmax status: %w", err)
	}

	// Get MAC using helper
	mac := status.GetMAC()
	if mac == "" {
		return nil, fmt.Errorf("airmax: no MAC address found")
	}

	// Get model and firmware using helpers
	model := status.GetModel()

	// Use Host.FWVersion for full firmware string with platform prefix (XC.qca955x.v8.7.0...)
	fullFirmware := status.Host.FWVersion

	// Extract flavor from firmware prefix (XC, WA, XM, XW, etc.)
	// Falls back to model-based detection if firmware prefix not recognized
	flavor := extractFlavor(fullFirmware)
	if flavor == "" {
		flavor = status.DetectFlavor()
	}

	logDebug("discoverAirMAXDevice %s: MAC=%s hostname=%s model=%s flavor=%s fw=%s", ip, mac, status.Host.Hostname, model, flavor, fullFirmware)

	return &DeviceInfo{
		MAC:             strings.ToLower(mac), // Normalize MAC to lowercase
		IP:              ip,
		Hostname:        status.Host.Hostname,
		Product:         model,
		Model:           model,
		Platform:        "airmax",
		Flavor:          flavor,
		Firmware:        fullFirmware,
		FirmwareVersion: status.ExtractVersion(),
	}, nil
}

// discoverSTAs gets connected stations from an AP
func discoverSTAs(apIP, username, password string, udbg *udebug.Manager) ([]STAInfo, error) {
	client := newHTTPClient(30*time.Second, apIP, udbg, "discover:stas")
	baseURL := fmt.Sprintf("https://%s", apIP)

	// Login
	loginBody, _ := json.Marshal(map[string]string{
		"username": username,
		"password": password,
	})

	loginReq, _ := http.NewRequest("POST", baseURL+"/api/v1.0/user/login", bytes.NewReader(loginBody))
	loginReq.Header.Set("Content-Type", "application/json")

	loginResp, err := client.Do(loginReq)
	if err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}
	defer loginResp.Body.Close()

	if loginResp.StatusCode != 200 {
		return nil, fmt.Errorf("login failed: %s", loginResp.Status)
	}

	token := loginResp.Header.Get("x-auth-token")
	if token == "" {
		return nil, fmt.Errorf("no auth token")
	}

	// Get statistics
	statsReq, _ := http.NewRequest("GET", baseURL+"/api/v1.0/statistics", nil)
	statsReq.Header.Set("x-auth-token", token)

	statsResp, err := client.Do(statsReq)
	if err != nil {
		return nil, fmt.Errorf("statistics: %w", err)
	}
	defer statsResp.Body.Close()

	body, _ := io.ReadAll(statsResp.Body)

	var statsData waveStatisticsResponse
	if err := json.Unmarshal(body, &statsData); err != nil {
		return nil, fmt.Errorf("parse statistics: %w", err)
	}

	if len(statsData) == 0 {
		return nil, nil
	}

	var stas []STAInfo
	for _, peer := range statsData[0].Wireless.Peers {
		if peer.Common.MgmtIP == "" {
			continue
		}

		signal := 0
		ssid := ""
		if len(peer.Local) > 0 {
			signal = peer.Local[0].LinkQuality.Signal
			ssid = peer.Local[0].SSID
		}

		ident := peer.Common.Identification
		flavor := extractFlavor(ident.Firmware)
		platform := detectPlatformWithModel(ident.Firmware, ident.Model)

		stas = append(stas, STAInfo{
			DeviceInfo: DeviceInfo{
				MAC:             ident.MAC,
				IP:              peer.Common.MgmtIP,
				Hostname:        peer.Common.Hostname,
				Product:         ident.Product,
				Model:           ident.Model,
				Platform:        platform,
				Flavor:          flavor,
				Firmware:        ident.Firmware,
				FirmwareVersion: ident.FirmwareVersion,
			},
			SSID:        ssid,
			SignalLevel: signal,
			Distance:    peer.Common.Distance,
		})
	}

	return stas, nil
}

// extractFlavor gets flavor from firmware string (e.g., GMC.ipq5018.v4.1.0... -> GMC)
func extractFlavor(firmware string) string {
	parts := strings.Split(firmware, ".")
	if len(parts) > 0 {
		upper := strings.ToUpper(parts[0])
		switch upper {
		// Wave flavors
		case "GMC", "GMP", "MGMP", "MW":
			return upper
		// LTU flavors
		case "AFLTUROCKET", "AFLTU", "AF5XHD":
			return upper
		// AirMAX AC prefixes - keep separate, NOT interchangeable
		case "XC", "2XC", "WA", "2WA":
			return upper
		// AirMAX M prefixes - keep separate, NOT interchangeable
		case "XM", "XW", "TI":
			return upper
		// AirFiber prefixes (AF2/AF3/AF5/AF11 use AirMAX-style API)
		case "AF11", "AF5X", "AF5U", "AF5", "AF3X", "AF2X":
			return upper
		}
	}
	return ""
}

// extractFirmwareVersion extracts a clean version from a firmware string
// Examples:
//
//	"AFLTU.ar934x.v2.4.0.hash123.date" -> "v2.4.0"
//	"GMC.ipq5018.v4.1.0.deda4ab.251212.0922" -> "v4.1.0"
//	"v4.1.0" -> "v4.1.0"
func extractFirmwareVersion(firmware string) string {
	// Find "v" followed by version number
	idx := strings.Index(firmware, ".v")
	if idx > 0 {
		firmware = firmware[idx+1:] // Skip the dot, keep the v
	} else if !strings.HasPrefix(firmware, "v") {
		// No version prefix found
		return ""
	}

	// Parse version parts
	if strings.HasPrefix(firmware, "v") {
		parts := strings.Split(firmware, ".")
		var result []string
		for i, p := range parts {
			if i == 0 {
				// First part starts with v
				result = append(result, p)
				continue
			}
			// Keep numeric version parts, stop at hash/date
			if len(p) > 0 {
				// Check if it's a numeric version part
				if p[0] >= '0' && p[0] <= '9' {
					// Check if it looks like a hash (6+ hex chars)
					if len(p) >= 6 && isHexString(p[:6]) {
						break
					}
					result = append(result, p)
				} else {
					break
				}
			}
		}
		if len(result) >= 2 {
			return strings.Join(result, ".")
		}
	}

	return ""
}

// isHexString checks if a string contains only hex characters
func isHexString(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// detectPlatform detects platform from firmware string
func detectPlatform(firmware string) string {
	return detectPlatformWithModel(firmware, "")
}

// detectPlatformWithModel detects platform from firmware string, using model as fallback
func detectPlatformWithModel(firmware, model string) string {
	upper := strings.ToUpper(firmware)
	// LTU (includes AF5XHD which uses LTU API)
	if strings.HasPrefix(upper, "AFLTUROCKET.") || strings.HasPrefix(upper, "AFLTU.") || strings.HasPrefix(upper, "AF5XHD.") {
		return "ltu"
	}
	// Wave (includes AirFiber 60 with GP prefix)
	if strings.HasPrefix(upper, "GMC.") || strings.HasPrefix(upper, "GMP.") || strings.HasPrefix(upper, "MGMP.") || strings.HasPrefix(upper, "MW.") || strings.HasPrefix(upper, "GP.") {
		return "wave"
	}
	// AirMAX (includes legacy AirFiber AF2/AF3/AF5/AF11 which use AirMAX-style API)
	// XC/2XC = 5GHz AC, WA/2WA = 2.4GHz AC, XM/XW = M series
	if strings.HasPrefix(upper, "XC.") || strings.HasPrefix(upper, "2XC.") ||
		strings.HasPrefix(upper, "WA.") || strings.HasPrefix(upper, "2WA.") ||
		strings.HasPrefix(upper, "XM.") || strings.HasPrefix(upper, "XW.") ||
		strings.HasPrefix(upper, "TI.") {
		return "airmax"
	}
	// AirFiber with AirMAX-style API (AF2X, AF3X, AF5, AF5U, AF5X, AF11) - prefixed firmware
	if strings.HasPrefix(upper, "AF11.") || strings.HasPrefix(upper, "AF5X.") || strings.HasPrefix(upper, "AF5U.") ||
		strings.HasPrefix(upper, "AF5.") || strings.HasPrefix(upper, "AF3X.") || strings.HasPrefix(upper, "AF2X.") {
		return "airmax" // These use AirMAX-style API (status.cgi, login.cgi)
	}

	// Fallback: check model name for AirFiber (firmware is plain "v4.x.x" with no prefix)
	modelUpper := strings.ToUpper(model)
	if strings.Contains(modelUpper, "AIRFIBER") || strings.HasPrefix(modelUpper, "AF") {
		return "airmax" // AirFiber 5X/11/24 use airmax-style API
	}

	// Default fallback
	return "wave"
}
