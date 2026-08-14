package poller

import (
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/yellowman/wavecontrol/internal/airmax"
	"github.com/yellowman/wavecontrol/internal/udebug"
)

func (p *Poller) FetchDeviceBackup(deviceID int64, ip, platform, username, password string) ([]byte, error) {
	platformLower := strings.ToLower(platform)
	isWaveLTU := platformLower == "wave" || platformLower == "ltu"

	if isWaveLTU {
		// Try Wave API first
		config, err := p.fetchWaveBackup(deviceID, ip, username, password)
		if err == nil {
			return config, nil
		}
		// Fall back to airMAX (some LTU devices use airMAX API)
	}

	// Try airMAX
	return p.fetchAirMAXBackup(deviceID, ip, username, password)
}

func (p *Poller) fetchWaveBackup(deviceID int64, ip, username, password string) ([]byte, error) {
	baseURL := fmt.Sprintf("https://%s", ip)
	client := p.getDeviceClient(deviceID)

	// Build credential list - device-specific first, then global
	var creds []Credential
	if username != "" && password != "" {
		creds = append(creds, Credential{Username: username, Password: password})
	}
	creds = append(creds, p.cfgSnapshot().apCreds...)

	// Try all credential pairs
	var token string
	var lastErr error
	for _, cred := range creds {
		token, lastErr = p.login(client, baseURL, cred.Username, cred.Password)
		if lastErr == nil {
			break
		}
		// Stop on network errors
		if isNetworkUnreachable(lastErr) {
			break
		}
	}
	if token == "" {
		return nil, fmt.Errorf("login failed: %v", lastErr)
	}

	// Fetch backup file
	req, err := http.NewRequest("GET", baseURL+"/api/v1.0/system/backup", nil)
	if err != nil {
		return nil, fmt.Errorf("create backup request: %w", err)
	}
	req.Header.Set("x-auth-token", token)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("backup request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		preview := string(body)
		if len(preview) > 200 {
			preview = preview[:200]
		}
		return nil, fmt.Errorf("backup returned status %d: %s", resp.StatusCode, preview)
	}

	return io.ReadAll(resp.Body)
}

func (p *Poller) fetchAirMAXBackup(deviceID int64, ip, username, password string) ([]byte, error) {
	// Get TLS transport
	var transport http.RoundTripper
	if p.tlsManager != nil {
		transport = p.tlsManager.GetInsecureTransport()
	}

	// Ultra debug: when enabled for this device, wrap the transport to capture
	// request/response details into the per-device ring buffer.
	baseTransport := transport
	if baseTransport == nil && p.ultraDebug != nil && p.ultraDebug.IsEnabled(deviceID) {
		baseTransport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	}
	if p.ultraDebug != nil && p.ultraDebug.IsEnabled(deviceID) {
		baseTransport = udebug.WrapTransport(p.ultraDebug, deviceID, baseTransport, "backup:airmax", udebug.DefaultCaptureLimit)
	}

	client := airmax.NewClientWithTransport(ip, 30*time.Second, baseTransport)

	// Build credential list - device-specific first, then global AP creds, then STA creds
	var creds []airmax.Credential
	if username != "" && password != "" {
		creds = append(creds, airmax.Credential{Username: username, Password: password})
	}
	for _, c := range p.cfgSnapshot().apCreds {
		creds = append(creds, airmax.Credential{Username: c.Username, Password: c.Password})
	}
	for _, c := range p.cfgSnapshot().staCreds {
		creds = append(creds, airmax.Credential{Username: c.Username, Password: c.Password})
	}
	if len(creds) == 0 {
		creds = append(creds, airmax.Credential{Username: "ubnt", Password: "ubnt"})
	}

	if err := client.LoginWithCredentials(creds); err != nil {
		return nil, fmt.Errorf("login failed: %w", err)
	}

	return client.FetchConfig()
}

// isNetworkUnreachable checks if error indicates device is completely unreachable
// Returns true for timeout, no route, host unreachable - device didn't respond at all
// Returns false for connection reset, refused, HTTP errors - device responded in some way
