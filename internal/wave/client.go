// Package wave provides a client for the Ubiquiti Wave device API
package wave

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Client is a Wave device API client
type Client struct {
	BaseURL    string
	Token      string
	HTTPClient *http.Client
}

// DeviceInfo from /api/v1.0/device
type DeviceInfo struct {
	Identification struct {
		Firmware        string `json:"firmware"`
		FirmwareVersion string `json:"firmwareVersion"`
		Model           string `json:"model"`
		Product         string `json:"product"`
		MAC             string `json:"mac"`
	} `json:"identification"`
	Capabilities struct {
		Device struct {
			SupportedFirmwares []struct {
				Flavor string `json:"flavor"`
			} `json:"supportedFirmwares"`
		} `json:"device"`
	} `json:"capabilities"`
}

// Statistics from /api/v1.0/statistics
type Statistics struct {
	Timestamp int64 `json:"timestamp"`
	Device    struct {
		Uptime int `json:"uptime"`
	} `json:"device"`
	Wireless struct {
		Peers []Peer `json:"peers"`
	} `json:"wireless"`
}

// Peer represents a connected station
type Peer struct {
	Common struct {
		MgmtIP         string `json:"mgmtIp"`
		Hostname       string `json:"hostname"`
		Identification struct {
			Firmware        string `json:"firmware"`
			FirmwareVersion string `json:"firmwareVersion"`
			Model           string `json:"model"`
			Product         string `json:"product"`
			MAC             string `json:"mac"`
		} `json:"identification"`
		Uptime   int `json:"uptime"`
		Counters struct {
			TxRate  int64 `json:"txRate"`
			RxRate  int64 `json:"rxRate"`
			TxBytes int64 `json:"txBytes"`
			RxBytes int64 `json:"rxBytes"`
		} `json:"counters"`
	} `json:"common"`
	Local []struct {
		LinkQuality struct {
			Signal int `json:"signal"`
		} `json:"linkQuality"`
	} `json:"local"`
}

// UpgradeStatus from /api/v1.0/system/upgrade
type UpgradeStatus struct {
	Status   string   `json:"status"`
	Warnings []string `json:"warnings"`
	Metadata struct {
		Version string `json:"version"`
	} `json:"metadata"`
	FailureReason string `json:"failureReason"`
}

// NewClient creates a new Wave API client
func NewClient(host string, insecure bool) *Client {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure},
	}
	return &Client{
		BaseURL: fmt.Sprintf("https://%s", host),
		HTTPClient: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
		},
	}
}

// Login authenticates and stores the token
func (c *Client) Login(username, password string) error {
	body, _ := json.Marshal(map[string]string{
		"username": username,
		"password": password,
	})

	req, _ := http.NewRequest("POST", c.BaseURL+"/api/v1.0/user/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("login request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("login failed: %s", resp.Status)
	}

	c.Token = resp.Header.Get("x-auth-token")
	if c.Token == "" {
		return fmt.Errorf("no auth token in response")
	}

	return nil
}

// request makes an authenticated API request
func (c *Client) request(method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, c.BaseURL+path, body)
	if err != nil {
		return nil, err
	}

	req.Header.Set("x-auth-token", c.Token)
	req.Header.Set("Content-Type", "application/json")

	return c.HTTPClient.Do(req)
}

// GetDevice returns device info
func (c *Client) GetDevice() (*DeviceInfo, error) {
	resp, err := c.request("GET", "/api/v1.0/device", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("device info: %s", resp.Status)
	}

	var info DeviceInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, err
	}

	return &info, nil
}

// GetStatistics returns device statistics including connected peers
func (c *Client) GetStatistics() (*Statistics, error) {
	resp, err := c.request("GET", "/api/v1.0/statistics", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("statistics: %s", resp.Status)
	}

	var stats []Statistics
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		return nil, err
	}

	if len(stats) == 0 {
		return nil, fmt.Errorf("empty statistics response")
	}

	return &stats[0], nil
}

// GetUpgradeStatus returns current upgrade status
func (c *Client) GetUpgradeStatus() (*UpgradeStatus, error) {
	resp, err := c.request("GET", "/api/v1.0/system/upgrade", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("upgrade status: %s", resp.Status)
	}

	var status UpgradeStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, err
	}

	return &status, nil
}

// UploadFirmware uploads a firmware file
func (c *Client) UploadFirmware(firmwarePath string) error {
	file, err := os.Open(firmwarePath)
	if err != nil {
		return fmt.Errorf("open firmware: %w", err)
	}
	defer file.Close()

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	part, err := writer.CreateFormFile("file", filepath.Base(firmwarePath))
	if err != nil {
		return err
	}

	if _, err := io.Copy(part, file); err != nil {
		return err
	}

	if err := writer.Close(); err != nil {
		return fmt.Errorf("finalize multipart: %w", err)
	}

	req, err := http.NewRequest("POST", c.BaseURL+"/api/v1.0/system/upgrade/direct", &buf)
	if err != nil {
		return fmt.Errorf("create upload request: %w", err)
	}
	req.Header.Set("x-auth-token", c.Token)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	// Longer timeout for upload
	client := &http.Client{
		Transport: c.HTTPClient.Transport,
		Timeout:   5 * time.Minute,
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("upload failed: %s - %s", resp.Status, string(body))
	}

	return nil
}

// WaitForUpgrade polls until upgrade is ready or fails
func (c *Client) WaitForUpgrade(timeout time.Duration) (*UpgradeStatus, error) {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		status, err := c.GetUpgradeStatus()
		if err != nil {
			time.Sleep(5 * time.Second)
			continue
		}

		switch strings.ToLower(status.Status) {
		case "finished", "ready", "complete", "verified", "success", "done":
			return status, nil
		case "failed", "error", "failure", "invalid", "corrupt", "aborted":
			msg := status.FailureReason
			if msg == "" {
				msg = "upgrade failed"
			}
			return status, fmt.Errorf(msg)
		}

		time.Sleep(5 * time.Second)
	}

	return nil, fmt.Errorf("timeout waiting for upgrade")
}

// Reboot triggers a device reboot
func (c *Client) Reboot() error {
	body, _ := json.Marshal(map[string]int{"timeout": 0})

	resp, err := c.request("POST", "/api/v1.0/system/reboot", bytes.NewReader(body))
	if err != nil {
		// Connection reset during reboot is expected
		if strings.Contains(err.Error(), "connection") || strings.Contains(err.Error(), "EOF") {
			return nil
		}
		return err
	}
	defer resp.Body.Close()

	return nil
}

// Ping checks if device is responding (unauthenticated)
func (c *Client) Ping() bool {
	req, _ := http.NewRequest("GET", c.BaseURL+"/api/v1.0/public/ping", nil)

	client := &http.Client{
		Transport: c.HTTPClient.Transport,
		Timeout:   5 * time.Second,
	}

	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()

	return resp.StatusCode == 200
}

// GetFlavor extracts flavor from firmware string (e.g., GMC.ipq5018.v4.1.0... -> GMC)
func GetFlavor(firmware string) string {
	parts := strings.Split(firmware, ".")
	if len(parts) > 0 {
		upper := strings.ToUpper(parts[0])
		if upper == "GMC" || upper == "GMP" || upper == "MGMP" || upper == "MW" {
			return upper
		}
	}
	return ""
}

// FirmwareMatches checks if device firmware matches target filename
func FirmwareMatches(currentFirmware, firmwareFilename string) bool {
	// Remove .bin extension from filename
	target := strings.TrimSuffix(firmwareFilename, ".bin")
	target = strings.TrimSuffix(target, ".BIN")

	return strings.EqualFold(currentFirmware, target)
}

// HasSameVersionWarning checks if upgrade status has SAME_VERSION warning
func HasSameVersionWarning(status *UpgradeStatus) bool {
	for _, w := range status.Warnings {
		if w == "SAME_VERSION" {
			return true
		}
	}
	return false
}
