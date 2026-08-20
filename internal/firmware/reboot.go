package firmware

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/yellowman/wavecontrol/internal/airmax"
	"github.com/yellowman/wavecontrol/internal/udebug"
)

// RebootResult describes a device reboot request that has been accepted by the
// radio API. It intentionally omits usernames/passwords.
type RebootResult struct {
	DeviceID  int64  `json:"device_id,omitempty"`
	DeviceIP  string `json:"ip_address"`
	DeviceMAC string `json:"mac,omitempty"`
	Hostname  string `json:"hostname,omitempty"`
	Platform  string `json:"platform,omitempty"`
	Flavor    string `json:"flavor,omitempty"`
	API       string `json:"api"`
	Status    string `json:"status"`
	Message   string `json:"message"`
}

type rebootCredential struct {
	user string
	pass string
}

// RebootDeviceByID reboots a stored inventory device using its known platform
// instead of guessing. This keeps Wave/LTU on the REST reboot path and airMAX /
// legacy AirFiber on the CGI reboot path.
func (s *Service) RebootDeviceByID(ctx context.Context, deviceID int64) (*RebootResult, error) {
	var ip, mac, hostname, username, password, platform, flavor sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT host(ip_address), mac, COALESCE(hostname, ''), COALESCE(username, ''), COALESCE(password, ''),
		       COALESCE(platform, ''), COALESCE(flavor, '')
		FROM devices
		WHERE id = $1
	`, deviceID).Scan(&ip, &mac, &hostname, &username, &password, &platform, &flavor)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("device not found")
	}
	if err != nil {
		return nil, fmt.Errorf("load device: %w", err)
	}
	if strings.TrimSpace(ip.String) == "" {
		return nil, fmt.Errorf("device has no management IP")
	}

	return s.RebootDeviceTarget(ctx, deviceID, ip.String, mac.String, hostname.String, username.String, password.String, platform.String, flavor.String)
}

// RebootDeviceTarget reboots a device using the explicitly supplied platform / flavor.
// Platform dispatch:
//   - wave: Wave REST API POST /api/v1.0/system/reboot
//   - ltu:  LTU REST API POST /api/v1.0/system/reboot, with airMAX CGI fallback for old/odd builds
//   - airmax/airfiber/ac/m: airMAX CGI POST /reboot.cgi reboot=yes
//   - unknown: legacy auto-detect, Wave/LTU REST first, then airMAX CGI
func (s *Service) RebootDeviceTarget(ctx context.Context, deviceID int64, ip, mac, hostname, username, password, platform, flavor string) (*RebootResult, error) {
	_ = ctx // reserved for future request-context propagation; client timeouts protect current calls.
	ip = strings.TrimSpace(ip)
	if password != "" {
		plain, err := s.decryptSecret(password)
		if err != nil {
			return nil, fmt.Errorf("decrypt device credential: %w", err)
		}
		password = plain
	}
	platformLower := strings.ToLower(strings.TrimSpace(platform))
	flavorUpper := strings.ToUpper(strings.TrimSpace(flavor))
	if platformLower == "" {
		platformLower = inferRebootPlatformFromFlavor(flavorUpper)
	}

	base := &RebootResult{
		DeviceID:  deviceID,
		DeviceIP:  ip,
		DeviceMAC: strings.ToLower(strings.TrimSpace(mac)),
		Hostname:  hostname,
		Platform:  platformLower,
		Flavor:    flavorUpper,
		Status:    "rebooting",
		Message:   "reboot initiated",
	}

	switch platformLower {
	case "wave":
		api, err := s.rebootWaveLike(deviceID, ip, username, password, "wave_json")
		if err != nil {
			return nil, err
		}
		base.API = api
		return base, nil
	case "ltu":
		api, err := s.rebootWaveLike(deviceID, ip, username, password, "ltu_json")
		if err == nil {
			base.API = api
			return base, nil
		}
		// LTU normally has the REST reboot endpoint, but older/hybrid builds have
		// airMAX CGI auth around system actions. Fall back deliberately and expose
		// which API actually worked.
		if cgiErr := s.rebootAirMAXLike(deviceID, ip, username, password); cgiErr == nil {
			base.API = "ltu_airmax_cgi_fallback"
			return base, nil
		} else {
			return nil, fmt.Errorf("ltu REST reboot failed: %v; airMAX CGI fallback failed: %w", err, cgiErr)
		}
	case "airmax", "airfiber", "ac", "m":
		if err := s.rebootAirMAXLike(deviceID, ip, username, password); err != nil {
			return nil, err
		}
		base.API = "airmax_cgi"
		return base, nil
	default:
		api, err := s.rebootWaveLike(deviceID, ip, username, password, "auto_wave_json")
		if err == nil {
			base.API = api
			return base, nil
		}
		if isConnectionError(err) {
			return nil, err
		}
		if cgiErr := s.rebootAirMAXLike(deviceID, ip, username, password); cgiErr == nil {
			base.API = "auto_airmax_cgi"
			return base, nil
		} else {
			return nil, fmt.Errorf("auto Wave/LTU reboot failed: %v; airMAX CGI reboot failed: %w", err, cgiErr)
		}
	}
}

func inferRebootPlatformFromFlavor(flavor string) string {
	flavor = strings.ToUpper(strings.TrimSpace(flavor))
	switch {
	case strings.HasPrefix(flavor, "GMC"), strings.HasPrefix(flavor, "GMP"), strings.HasPrefix(flavor, "MGMP"), strings.HasPrefix(flavor, "MW"), strings.HasPrefix(flavor, "GP"):
		return "wave"
	case strings.HasPrefix(flavor, "AFLTU"), strings.HasPrefix(flavor, "AF5XHD"):
		return "ltu"
	case strings.HasPrefix(flavor, "XC"), strings.HasPrefix(flavor, "2XC"), strings.HasPrefix(flavor, "WA"), strings.HasPrefix(flavor, "2WA"), strings.HasPrefix(flavor, "XM"), strings.HasPrefix(flavor, "XW"):
		return "airmax"
	case strings.HasPrefix(flavor, "AF"):
		return "airfiber"
	default:
		return ""
	}
}

func (s *Service) rebootCredentialCandidates(username, password string) []rebootCredential {
	var out []rebootCredential
	add := func(user, pass string) {
		user = strings.TrimSpace(user)
		if user == "" || pass == "" {
			return
		}
		for _, existing := range out {
			if existing.user == user && existing.pass == pass {
				return
			}
		}
		out = append(out, rebootCredential{user: user, pass: pass})
	}

	add(username, password)
	configured := append(s.credentialSnapshot(true), s.credentialSnapshot(false)...)
	if username != "" && password == "" {
		for _, cred := range configured {
			if strings.EqualFold(strings.TrimSpace(cred.Username), strings.TrimSpace(username)) {
				add(cred.Username, cred.Password)
			}
		}
	}
	for _, cred := range configured {
		add(cred.Username, cred.Password)
	}

	return out
}

func (s *Service) rebootWaveLike(deviceID int64, ip, username, password, apiLabel string) (string, error) {
	creds := s.rebootCredentialCandidates(username, password)
	if len(creds) == 0 {
		return apiLabel, fmt.Errorf("no credentials available for REST reboot")
	}

	client := s.rebootHTTPClient(deviceID, 30*time.Second)
	baseURL := fmt.Sprintf("https://%s", ip)
	var lastErr error
	for _, cred := range creds {
		token, err := s.loginWithClient(client, ip, cred.user, cred.pass)
		if err != nil {
			lastErr = err
			if isConnectionError(err) {
				break
			}
			continue
		}
		if err := s.rebootREST(client, baseURL, token); err != nil {
			lastErr = err
			if isConnectionError(err) {
				break
			}
			continue
		}
		return apiLabel, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("all REST reboot credentials failed")
	}
	return apiLabel, lastErr
}

func (s *Service) rebootREST(client *http.Client, baseURL, token string) error {
	body, err := json.Marshal(map[string]int{"timeout": 0})
	if err != nil {
		return fmt.Errorf("marshal reboot body: %w", err)
	}

	req, err := http.NewRequest("POST", baseURL+"/api/v1.0/system/reboot", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create reboot request: %w", err)
	}
	req.Header.Set("x-auth-token", token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		// Connection reset/EOF/timeout after submitting reboot is common.
		if isConnectionError(err) || strings.Contains(err.Error(), "EOF") {
			return nil
		}
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusAccepted {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("REST reboot returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
}

func (s *Service) rebootHTTPClient(deviceID int64, timeout time.Duration) *http.Client {
	base := s.getDeviceClient(deviceID)
	clone := *base
	clone.Timeout = timeout
	return &clone
}

func (s *Service) rebootAirMAXLike(deviceID int64, ip, username, password string) error {
	creds := s.rebootCredentialCandidates(username, password)
	if len(creds) == 0 {
		return fmt.Errorf("no credentials available for airMAX CGI reboot")
	}
	amCreds := make([]airmax.Credential, 0, len(creds))
	for _, c := range creds {
		amCreds = append(amCreds, airmax.Credential{Username: c.user, Password: c.pass})
	}

	var rt http.RoundTripper
	if s.tlsManager != nil {
		rt = s.tlsManager.GetInsecureTransport()
	} else if s.httpClient != nil {
		rt = s.httpClient.Transport
	}
	if rt == nil {
		rt = http.DefaultTransport
	}
	if s.ultraDebug != nil && deviceID != 0 && s.ultraDebug.IsEnabled(deviceID) {
		rt = udebug.WrapTransport(s.ultraDebug, deviceID, rt, "airmax_reboot", udebug.DefaultCaptureLimit)
	}

	client := airmax.NewClientWithTransport(ip, 30*time.Second, rt)
	if err := client.LoginWithCredentials(amCreds); err != nil {
		return fmt.Errorf("airMAX login failed: %w", err)
	}
	return client.Reboot()
}
