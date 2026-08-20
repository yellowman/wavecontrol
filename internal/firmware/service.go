package firmware

import (
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yellowman/wavecontrol/internal/airmax"
	"github.com/yellowman/wavecontrol/internal/secrets"
	"github.com/yellowman/wavecontrol/internal/udebug"
	"github.com/yellowman/wavecontrol/internal/wave"
)

// Service handles firmware upgrades
type Service struct {
	db         *sql.DB
	httpClient *http.Client
	ultraDebug *udebug.Manager

	tlsManager TLSManager

	// Default credential pairs (cached, reload with ReloadConfig).
	configMu    sync.RWMutex
	apCreds     []Credential
	staCreds    []Credential
	secretStore *secrets.Manager
}

// Credential is a username/password pair. Pairs are kept intact so accounts
// with different usernames are never recombined with the wrong password.
type Credential struct {
	Username string
	Password string
}

// TLSManager interface for certificate verification
type TLSManager interface {
	GetTransport(deviceID int64) *http.Transport
	GetInsecureTransport() *http.Transport
}

// UpgradeJob represents a firmware upgrade job
type UpgradeJob struct {
	ID           int64
	DeviceID     int64
	DeviceIP     string
	DeviceMAC    string
	Hostname     string
	FirmwareFile string
	TargetVer    string
	Status       string // pending, uploading, rebooting, verifying, success, failed, skipped
	Message      string
	StartedAt    time.Time
	CompletedAt  time.Time
}

// UpgradeResult holds the result of an upgrade attempt
type UpgradeResult struct {
	DeviceID   int64  `json:"device_id"`
	DeviceIP   string `json:"device_ip"`
	DeviceMAC  string `json:"device_mac"`
	Hostname   string `json:"hostname"`
	Status     string `json:"status"` // success, failed, skipped
	Message    string `json:"message"`
	OldVersion string `json:"old_version"`
	NewVersion string `json:"new_version"`
}

// normalizeUpgradeResult guarantees bulk/fanout callers never publish a nil
// result when UpgradeDevice fails before it can construct device metadata.
// Individual upgrade failures remain represented in-band so the caller can
// report every requested device without panicking or emitting JSON nulls.
func normalizeUpgradeResult(result *UpgradeResult, deviceID int64, err error) *UpgradeResult {
	if result != nil {
		return result
	}
	message := "upgrade failed"
	if err != nil {
		message = err.Error()
	}
	return &UpgradeResult{DeviceID: deviceID, Status: "failed", Message: message}
}

// dbExecIgnore executes a query, logs errors but doesn't return them (fire-and-forget)
func dbExecIgnore(db *sql.DB, query string, args ...any) {
	if _, err := db.Exec(query, args...); err != nil {
		log.Printf("DB exec error: %v", err)
	}
}

// NewService creates a new firmware service
func NewService(db *sql.DB, tlsMgr TLSManager, udbg *udebug.Manager, secretStore *secrets.Manager) (*Service, error) {
	var transport http.RoundTripper
	if tlsMgr != nil {
		transport = tlsMgr.GetInsecureTransport() // Default, override per-device
	} else {
		transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}

	s := &Service{
		db:          db,
		ultraDebug:  udbg,
		tlsManager:  tlsMgr,
		secretStore: secretStore,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   10 * time.Minute,
		},
	}
	if err := s.loadConfig(); err != nil {
		return nil, err
	}
	return s, nil
}

// getDeviceClient returns an HTTP client for a specific device with TLS verification
func (s *Service) getDeviceClient(deviceID int64) *http.Client {
	// Base transport: insecure shared client when TLS manager is disabled,
	// otherwise a per-device transport from the TLS manager.
	var base http.RoundTripper
	if s.tlsManager == nil {
		base = s.httpClient.Transport
	} else {
		base = s.tlsManager.GetTransport(deviceID)
	}

	// When ultra debug is enabled for this device, return a per-device client
	// with a logging transport wrapper.
	if s.ultraDebug != nil && s.ultraDebug.IsEnabled(deviceID) {
		wrapped := udebug.WrapTransport(s.ultraDebug, deviceID, base, "firmware", udebug.DefaultCaptureLimit)
		return &http.Client{Transport: wrapped, Timeout: 10 * time.Minute}
	}

	// Fast path: reuse the shared pooled client when TLS manager isn't used.
	if s.tlsManager == nil {
		return s.httpClient
	}

	return &http.Client{Transport: base, Timeout: 10 * time.Minute}
}

// getInsecureClient returns the shared insecure HTTP client (skips TLS verification),
// wrapped with ultra-debug logging when enabled for the given device.
func (s *Service) getInsecureClient(deviceID int64) *http.Client {
	base := s.httpClient.Transport
	if s.ultraDebug != nil && s.ultraDebug.IsEnabled(deviceID) {
		wrapped := udebug.WrapTransport(s.ultraDebug, deviceID, base, "firmware_insecure", udebug.DefaultCaptureLimit)
		return &http.Client{Transport: wrapped, Timeout: 10 * time.Minute}
	}
	return s.httpClient
}

// inferDeviceFlavor attempts to determine a device firmware flavor (used for firmware selection)
// by directly querying the device when the database record is missing it.
//
// This is primarily needed for devices that are discovered indirectly (e.g. stations learned from an AP),
// where we may not have enough metadata saved yet to pick the correct firmware file.
func (s *Service) inferDeviceFlavor(ctx context.Context, deviceID int64, ipAddr, platformLower, username, password string) (string, error) {
	platformLower = strings.ToLower(strings.TrimSpace(platformLower))
	ipAddr = strings.TrimSpace(ipAddr)

	switch platformLower {
	case "wave", "ltu":
		hc := s.getDeviceClient(deviceID)
		wc := wave.NewClient(ipAddr, true)
		// Reuse our per-device HTTP client so ultra debug (if enabled) can capture
		// the full request/response flow.
		wc.HTTPClient = hc
		if err := wc.Login(username, password); err != nil {
			return "", fmt.Errorf("login failed: %w", err)
		}
		dev, err := wc.GetDevice()
		if err != nil {
			return "", fmt.Errorf("get device info failed: %w", err)
		}

		// Prefer the firmware prefix (e.g. "MGMP.*", "GMC.*", "AFLTUROCKET.*")
		if fw := strings.TrimSpace(dev.Identification.Firmware); fw != "" {
			parts := strings.Split(fw, ".")
			if len(parts) > 0 && strings.TrimSpace(parts[0]) != "" {
				return strings.ToUpper(strings.TrimSpace(parts[0])), nil
			}
		}

		// Fallback: supported firmwares list (when available)
		if len(dev.Capabilities.Device.SupportedFirmwares) > 0 {
			fl := strings.TrimSpace(dev.Capabilities.Device.SupportedFirmwares[0].Flavor)
			if fl != "" {
				return strings.ToUpper(fl), nil
			}
		}

		return "", fmt.Errorf("no flavor found in device info")
	case "airmax", "airfiber":
		hc := s.getDeviceClient(deviceID)
		transport := hc.Transport
		if transport == nil {
			transport = http.DefaultTransport
		}
		am := airmax.NewClientWithTransport(ipAddr, 30*time.Second, transport)
		if err := am.Login(username, password); err != nil {
			return "", fmt.Errorf("login failed: %w", err)
		}

		st, err := am.GetStatus()
		if err != nil {
			return "", fmt.Errorf("get status failed: %w", err)
		}

		fl := strings.TrimSpace(st.DetectFirmwarePlatform())
		if fl == "" {
			return "", fmt.Errorf("status did not include a firmware platform/flavor")
		}
		return strings.ToUpper(fl), nil
	default:
		return "", fmt.Errorf("platform %q not supported for flavor inference", platformLower)
	}
}

// getFirmwarePath returns the firmware path from settings (reads fresh from DB)
func (s *Service) getFirmwarePath() string {
	var path string
	s.db.QueryRow(`SELECT value FROM settings WHERE key = 'firmware_path'`).Scan(&path)
	if path == "" {
		return "firmware"
	}
	return path
}

// ReloadConfig reloads settings from the database. Existing credentials remain
// active if the replacement configuration cannot be read or decrypted.
func (s *Service) ReloadConfig() error {
	return s.loadConfig()
}

func (s *Service) decryptSecret(value string) (string, error) {
	if value == "" || s.secretStore == nil {
		return value, nil
	}
	return s.secretStore.Decrypt(value)
}

func (s *Service) readCredentialPairs(prefix string) ([]Credential, error) {
	pairs := make([]Credential, 0, 3)
	for i := 1; i <= 3; i++ {
		var user, stored string
		userKey := fmt.Sprintf("%s_cred%d_user", prefix, i)
		passKey := fmt.Sprintf("%s_cred%d_pass", prefix, i)
		if err := s.db.QueryRow(`SELECT value FROM settings WHERE key = $1`, userKey).Scan(&user); err != nil && err != sql.ErrNoRows {
			return nil, fmt.Errorf("read %s: %w", userKey, err)
		}
		if err := s.db.QueryRow(`SELECT value FROM settings WHERE key = $1`, passKey).Scan(&stored); err != nil && err != sql.ErrNoRows {
			return nil, fmt.Errorf("read %s: %w", passKey, err)
		}
		user = strings.TrimSpace(user)
		if user == "" || stored == "" {
			continue
		}
		pass, err := s.decryptSecret(stored)
		if err != nil {
			return nil, fmt.Errorf("decrypt %s: %w", passKey, err)
		}
		if pass != "" {
			pairs = append(pairs, Credential{Username: user, Password: pass})
		}
	}
	return dedupeCredentials(pairs), nil
}

func (s *Service) readLegacyCredentialPairs(prefix string) ([]Credential, error) {
	var username, stored string
	if err := s.db.QueryRow(`SELECT value FROM settings WHERE key = $1`, prefix+"_username").Scan(&username); err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if err := s.db.QueryRow(`SELECT value FROM settings WHERE key = $1`, prefix+"_passwords").Scan(&stored); err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	username = strings.TrimSpace(username)
	if username == "" || stored == "" {
		return nil, nil
	}
	plain, err := s.decryptSecret(stored)
	if err != nil {
		return nil, err
	}
	var passwords []string
	if err := json.Unmarshal([]byte(plain), &passwords); err != nil {
		return nil, fmt.Errorf("parse %s_passwords: %w", prefix, err)
	}
	pairs := make([]Credential, 0, len(passwords))
	for _, password := range passwords {
		if password != "" {
			pairs = append(pairs, Credential{Username: username, Password: password})
		}
	}
	return dedupeCredentials(pairs), nil
}

func dedupeCredentials(in []Credential) []Credential {
	out := make([]Credential, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, c := range in {
		c.Username = strings.TrimSpace(c.Username)
		if c.Username == "" || c.Password == "" {
			continue
		}
		key := c.Username + "\x00" + c.Password
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, c)
	}
	return out
}

func (s *Service) loadConfig() error {
	apCreds, err := s.readCredentialPairs("ap")
	if err != nil {
		return fmt.Errorf("load AP credentials: %w", err)
	}
	if len(apCreds) == 0 {
		apCreds, err = s.readLegacyCredentialPairs("ap")
		if err != nil {
			return fmt.Errorf("load legacy AP credentials: %w", err)
		}
	}

	staCreds, err := s.readCredentialPairs("sta")
	if err != nil {
		return fmt.Errorf("load STA credentials: %w", err)
	}
	if len(staCreds) == 0 {
		staCreds, err = s.readLegacyCredentialPairs("sta")
		if err != nil {
			return fmt.Errorf("load legacy STA credentials: %w", err)
		}
	}
	if len(staCreds) == 0 {
		staCreds = append([]Credential(nil), apCreds...)
	}

	s.configMu.Lock()
	s.apCreds = append([]Credential(nil), apCreds...)
	s.staCreds = append([]Credential(nil), staCreds...)
	s.configMu.Unlock()
	return nil
}

func (s *Service) credentialSnapshot(isAP bool) []Credential {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	if isAP {
		return append([]Credential(nil), s.apCreds...)
	}
	return append([]Credential(nil), s.staCreds...)
}

func (s *Service) firstCredential(isAP bool) (Credential, bool) {
	creds := s.credentialSnapshot(isAP)
	if len(creds) == 0 {
		return Credential{}, false
	}
	return creds[0], true
}

func (s *Service) resolveCredential(username, password string, isAP bool) (Credential, error) {
	username = strings.TrimSpace(username)
	if password != "" {
		plain, err := s.decryptSecret(password)
		if err != nil {
			return Credential{}, fmt.Errorf("decrypt device credential: %w", err)
		}
		password = plain
	}
	if username != "" && password != "" {
		return Credential{Username: username, Password: password}, nil
	}
	if username != "" || password != "" {
		return Credential{}, errors.New("username and password must be supplied together")
	}
	cred, ok := s.firstCredential(isAP)
	if !ok {
		return Credential{}, errors.New("no device credentials configured")
	}
	return cred, nil
}

const (
	firmwareIDPrefix = "fw_"
	maxFirmwareSize  = int64(1 << 30) // 1 GiB hard ceiling for all callers
)

func encodeFirmwareID(relativePath string) string {
	return firmwareIDPrefix + base64.RawURLEncoding.EncodeToString([]byte(filepath.ToSlash(relativePath)))
}

func decodeFirmwareID(reference string) (string, bool) {
	if !strings.HasPrefix(reference, firmwareIDPrefix) {
		return "", false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(reference, firmwareIDPrefix))
	if err != nil || len(decoded) == 0 {
		return "", false
	}
	return string(decoded), true
}

func (s *Service) canonicalFirmwareRoot(create bool) (string, error) {
	root := strings.TrimSpace(s.getFirmwarePath())
	if root == "" {
		return "", errors.New("firmware_path setting is empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve firmware directory: %w", err)
	}
	if create {
		if err := os.MkdirAll(abs, 0o750); err != nil {
			return "", fmt.Errorf("create firmware directory: %w", err)
		}
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("firmware_path is not a directory")
	}
	return real, nil
}

func (s *Service) resolveFirmwareReference(reference string) (FirmwareFile, error) {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return FirmwareFile{}, errors.New("firmware reference is required")
	}
	files, err := s.ListFirmware()
	if err != nil {
		return FirmwareFile{}, err
	}
	if rel, ok := decodeFirmwareID(reference); ok {
		rel = filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
		if rel == "." || strings.HasPrefix(rel, "../") || filepath.IsAbs(filepath.FromSlash(rel)) {
			return FirmwareFile{}, errors.New("invalid firmware reference")
		}
		for _, fw := range files {
			if fw.RelativePath == rel {
				return fw, nil
			}
		}
		return FirmwareFile{}, errors.New("firmware not found")
	}

	// Backwards compatibility for callers that still send a basename or exact
	// relative path. A duplicated basename is rejected rather than selecting an
	// arbitrary file from the recursive scan.
	var matches []FirmwareFile
	for _, fw := range files {
		if fw.RelativePath == filepath.ToSlash(reference) || fw.Name == reference {
			matches = append(matches, fw)
		}
	}
	if len(matches) == 0 {
		return FirmwareFile{}, errors.New("firmware not found")
	}
	if len(matches) > 1 {
		return FirmwareFile{}, fmt.Errorf("firmware name %q is ambiguous; use its id", reference)
	}
	return matches[0], nil
}

// ListFirmware returns available firmware files
// Path is internal. API clients use ID, which uniquely identifies the relative path.
func (s *Service) ListFirmware() ([]FirmwareFile, error) {
	firmwareRoot, err := s.canonicalFirmwareRoot(false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []FirmwareFile{}, nil
		}
		return nil, err
	}
	files := make([]FirmwareFile, 0)
	err = filepath.WalkDir(firmwareRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 || !strings.EqualFold(filepath.Ext(d.Name()), ".bin") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(firmwareRoot, path)
		if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return fmt.Errorf("firmware path outside configured root: %s", path)
		}
		rel = filepath.ToSlash(rel)
		fw := parseFirmwareFilename(filepath.Base(rel))
		fw.ID = encodeFirmwareID(rel)
		fw.RelativePath = rel
		fw.Path = path
		fw.Size = info.Size()
		files = append(files, fw)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].RelativePath < files[j].RelativePath })
	return files, nil
}

// ListFirmwarePublic returns firmware files with paths stripped for API responses
func (s *Service) ListFirmwarePublic() ([]FirmwareFile, error) {
	files, err := s.ListFirmware()
	if err != nil {
		return nil, err
	}
	// Strip absolute paths from responses; clients reference files by ID.
	for i := range files {
		files[i].Path = ""
	}
	return files, err
}

// DeleteFirmware removes a firmware file by stable ID or unambiguous legacy name.
func (s *Service) DeleteFirmware(reference string) error {
	fw, err := s.resolveFirmwareReference(reference)
	if err != nil {
		return err
	}
	if err := os.Remove(fw.Path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("firmware not found")
		}
		return fmt.Errorf("delete firmware: %w", err)
	}
	log.Printf("Deleted firmware: %s", fw.RelativePath)
	return nil
}

// GetFirmwareFilePath returns the validated filesystem path to a firmware file
func (s *Service) GetFirmwareFilePath(reference string) (string, error) {
	fw, err := s.resolveFirmwareReference(reference)
	if err != nil {
		return "", err
	}
	return fw.Path, nil
}

// SaveFirmware saves an uploaded firmware file
func (s *Service) SaveFirmware(name string, data io.Reader) (*FirmwareFile, error) {
	originalName := strings.TrimSpace(name)
	name = filepath.Base(originalName)
	if originalName == "" || name == "." || name == ".." || name != originalName || strings.ContainsAny(originalName, `/\`) {
		return nil, fmt.Errorf("invalid firmware name")
	}
	if !strings.EqualFold(filepath.Ext(name), ".bin") {
		return nil, fmt.Errorf("firmware must have .bin extension")
	}
	firmwareRoot, err := s.canonicalFirmwareRoot(true)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(firmwareRoot, name)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("firmware already exists")
	}
	if err != nil {
		return nil, fmt.Errorf("create firmware: %w", err)
	}
	keep := false
	defer func() {
		_ = f.Close()
		if !keep {
			_ = os.Remove(path)
		}
	}()

	limited := &io.LimitedReader{R: data, N: maxFirmwareSize + 1}
	size, err := io.Copy(f, limited)
	if err != nil {
		return nil, fmt.Errorf("write firmware: %w", err)
	}
	if size > maxFirmwareSize {
		return nil, fmt.Errorf("firmware exceeds %d-byte limit", maxFirmwareSize)
	}
	if err := f.Sync(); err != nil {
		return nil, fmt.Errorf("sync firmware: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("close firmware: %w", err)
	}
	keep = true
	fw := parseFirmwareFilename(name)
	fw.ID = encodeFirmwareID(name)
	fw.RelativePath = name
	fw.Path = path
	fw.Size = size
	return &fw, nil
}

// FirmwareFile represents an available firmware file
type FirmwareFile struct {
	ID           string `json:"id"`            // URL-safe stable reference derived from relative path
	Name         string `json:"name"`          // Basename for display
	RelativePath string `json:"relative_path"` // Unique path below the configured firmware root
	Path         string `json:"-"`             // Internal filesystem path; never serialized to API clients
	Size         int64  `json:"size"`
	Flavor       string `json:"flavor"`   // GMC, GMP, Rocket, LiteBeam, etc.
	Platform     string `json:"platform"` // wave, ltu, airmax
	AirMAX       string `json:"airmax"`   // XC, XM, XW (for AirMAX only)
	Version      string `json:"version"`  // Extracted version number
}

// FindFirmwareForFlavor finds the best firmware file for a flavor
func (s *Service) FindFirmwareForFlavor(flavor string) (string, error) {
	files, err := s.ListFirmware()
	if err != nil {
		return "", err
	}

	var candidates []string
	for _, f := range files {
		if strings.EqualFold(f.Flavor, flavor) {
			candidates = append(candidates, f.Path)
		}
	}

	if len(candidates) == 0 {
		return "", fmt.Errorf("no firmware for flavor %s", flavor)
	}

	// Pick newest by parsed version. A simple string sort breaks on v-prefixed
	// versions (e.g., v10 sorts before v9 lexicographically).
	bestPath := candidates[0]
	bestVer := parseFirmwareFilename(filepath.Base(bestPath)).Version
	for _, path := range candidates[1:] {
		v := parseFirmwareFilename(filepath.Base(path)).Version
		if v == "" {
			continue
		}
		if bestVer == "" || compareVersions(v, bestVer) > 0 {
			bestPath = path
			bestVer = v
		}
	}

	// If we couldn't parse versions, fall back to the previous string sort.
	if bestVer == "" {
		sort.Sort(sort.Reverse(sort.StringSlice(candidates)))
		return candidates[0], nil
	}

	return bestPath, nil
}

// SelectFirmwareForDevice finds and returns the best firmware file for a device flavor
func (s *Service) SelectFirmwareForDevice(flavor string) (*FirmwareFile, error) {
	files, err := s.ListFirmware()
	if err != nil {
		return nil, err
	}

	var best *FirmwareFile
	for i := range files {
		f := &files[i]
		if strings.EqualFold(f.Flavor, flavor) {
			if f.Version == "" {
				continue
			}
			if best == nil || compareVersions(f.Version, best.Version) > 0 {
				best = f
			}
		}
	}

	if best == nil {
		return nil, fmt.Errorf("no firmware for flavor %s", flavor)
	}

	return best, nil
}

// GetFirmwareInfo returns information about a specific firmware file
func (s *Service) GetFirmwareInfo(reference string) (*FirmwareFile, error) {
	fw, err := s.resolveFirmwareReference(reference)
	if err != nil {
		return nil, err
	}
	return &fw, nil
}

// FindFirmwareByVersion finds firmware matching version and flavor
func (s *Service) FindFirmwareByVersion(version, flavor string) (*FirmwareFile, error) {
	files, err := s.ListFirmware()
	if err != nil {
		return nil, err
	}

	for i := range files {
		f := &files[i]
		if !strings.EqualFold(f.Flavor, flavor) {
			continue
		}
		// Accept version strings with or without a leading "v" (the UI typically
		// displays versions without the prefix).
		if compareVersions(f.Version, version) == 0 {
			return f, nil
		}
	}

	return nil, fmt.Errorf("no firmware for version %s flavor %s", version, flavor)
}

// ListVersions returns unique versions with their available flavors
// Groups by version+platform to avoid collisions (e.g., LTU v2.4.1 vs Wave v2.4.1)
func (s *Service) ListVersions() ([]VersionInfo, error) {
	files, err := s.ListFirmware()
	if err != nil {
		return nil, err
	}

	// Group by version+platform (different platforms can have same version number)
	byVersionPlatform := make(map[string]*VersionInfo)
	for _, f := range files {
		if f.Version == "" {
			continue
		}
		key := f.Version + "|" + f.Platform
		v, ok := byVersionPlatform[key]
		if !ok {
			v = &VersionInfo{Version: f.Version, Platform: f.Platform}
			byVersionPlatform[key] = v
		}
		// Add flavor if not already present
		found := false
		for _, fl := range v.Flavors {
			if strings.EqualFold(fl, f.Flavor) {
				found = true
				break
			}
		}
		if !found && f.Flavor != "" {
			v.Flavors = append(v.Flavors, f.Flavor)
		}
	}

	// Convert to slice and sort by platform, then version descending
	versions := make([]VersionInfo, 0, len(byVersionPlatform))
	for _, v := range byVersionPlatform {
		versions = append(versions, *v)
	}
	sort.Slice(versions, func(i, j int) bool {
		// Primary sort by platform
		if versions[i].Platform != versions[j].Platform {
			return platformOrder(versions[i].Platform) < platformOrder(versions[j].Platform)
		}
		// Secondary sort by version descending
		return compareVersions(versions[i].Version, versions[j].Version) > 0
	})

	return versions, nil
}

// platformOrder returns sort order for platforms (Wave first, then MLO, LTU, airMAX)
func platformOrder(platform string) int {
	switch platform {
	case "Wave":
		return 1
	case "MLO":
		return 2
	case "LTU":
		return 3
	case "airMAX AC":
		return 4
	case "airMAX M":
		return 5
	case "AirFiber":
		return 6
	default:
		return 99
	}
}

// VersionInfo describes a firmware version and its available flavors
type VersionInfo struct {
	Version  string   `json:"version"`
	Platform string   `json:"platform"`
	Flavors  []string `json:"flavors"`
}

// compareVersions compares version strings (handles RC, beta, etc.)
// Returns >0 if a > b, <0 if a < b, 0 if equal
func compareVersions(a, b string) int {
	// Normalize common version prefixes for reliable comparison.
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	a = strings.TrimPrefix(a, "v")
	a = strings.TrimPrefix(a, "V")
	b = strings.TrimPrefix(b, "v")
	b = strings.TrimPrefix(b, "V")

	// Split on common separators
	partsA := splitVersion(a)
	partsB := splitVersion(b)

	for i := 0; i < len(partsA) && i < len(partsB); i++ {
		// Try numeric comparison first
		numA, errA := strconv.Atoi(partsA[i])
		numB, errB := strconv.Atoi(partsB[i])

		if errA == nil && errB == nil {
			if numA != numB {
				return numA - numB
			}
		} else {
			// String comparison - but RC/beta/alpha should sort lower
			cmp := strings.Compare(strings.ToLower(partsA[i]), strings.ToLower(partsB[i]))
			if cmp != 0 {
				// Special case: release candidate, beta, alpha sort lower than release
				aIsPrerelease := isPrerelease(partsA[i])
				bIsPrerelease := isPrerelease(partsB[i])
				if aIsPrerelease && !bIsPrerelease {
					return -1
				}
				if !aIsPrerelease && bIsPrerelease {
					return 1
				}
				return cmp
			}
		}
	}

	// Shorter version with same prefix is "release" (4.1.0 > 4.1.0-RC3)
	if len(partsA) < len(partsB) {
		// Check if extra parts in B are prereleases
		for i := len(partsA); i < len(partsB); i++ {
			if isPrerelease(partsB[i]) {
				return 1 // A is release, B is prerelease
			}
		}
	}
	if len(partsA) > len(partsB) {
		for i := len(partsB); i < len(partsA); i++ {
			if isPrerelease(partsA[i]) {
				return -1 // B is release, A is prerelease
			}
		}
	}

	return len(partsA) - len(partsB)
}

func splitVersion(v string) []string {
	// Split on ., -, _
	var parts []string
	var current strings.Builder
	for _, c := range v {
		if c == '.' || c == '-' || c == '_' {
			if current.Len() > 0 {
				parts = append(parts, current.String())
				current.Reset()
			}
		} else {
			current.WriteRune(c)
		}
	}
	if current.Len() > 0 {
		parts = append(parts, current.String())
	}
	return parts
}

func isPrerelease(s string) bool {
	lower := strings.ToLower(s)
	return strings.HasPrefix(lower, "rc") ||
		strings.HasPrefix(lower, "beta") ||
		strings.HasPrefix(lower, "alpha") ||
		strings.HasPrefix(lower, "dev") ||
		strings.HasPrefix(lower, "pre")
}

// ResolveFirmwarePath safely resolves a firmware name to a validated path
// This prevents directory traversal attacks by only accepting firmware names,
// not paths, and verifying the resolved path stays within firmwarePath
func (s *Service) ResolveFirmwarePath(reference string) (string, error) {
	fw, err := s.resolveFirmwareReference(reference)
	if err != nil {
		return "", err
	}
	return fw.Path, nil
}

// File returns the filename from a FirmwareFile
func (f *FirmwareFile) File() string {
	return f.Name
}

// UpgradeDevice upgrades a single device
func (s *Service) UpgradeDevice(ctx context.Context, deviceID int64, firmwareFile string, force bool) (*UpgradeResult, error) {
	log.Printf("UpgradeDevice: starting deviceID=%d firmwareFile=%q force=%v", deviceID, firmwareFile, force)

	// Get device info
	var ip, mac, hostname, username, password, flavor, platform, currentFW sql.NullString
	var parentID sql.NullInt64
	err := s.db.QueryRow(`
		SELECT ip_address, mac, hostname, username, password, flavor, platform, firmware, parent_id
		FROM devices WHERE id = $1
	`, deviceID).Scan(&ip, &mac, &hostname, &username, &password, &flavor, &platform, &currentFW, &parentID)

	if err != nil {
		log.Printf("UpgradeDevice: device %d not found: %v", deviceID, err)
		return nil, fmt.Errorf("device not found: %w", err)
	}

	log.Printf("UpgradeDevice: device %d ip=%s flavor=%s platform=%s currentFW=%s",
		deviceID, ip.String, flavor.String, platform.String, currentFW.String)

	result := &UpgradeResult{
		DeviceID:   deviceID,
		DeviceIP:   ip.String,
		DeviceMAC:  mac.String,
		Hostname:   hostname.String,
		OldVersion: currentFW.String,
	}

	// Use a complete per-device credential pair when present. Otherwise use the
	// first configured pair for the device role; never mix a username from one
	// slot with a password from another.
	isAP := !parentID.Valid
	storedPassword, err := s.decryptSecret(password.String)
	if err != nil {
		result.Status = "failed"
		result.Message = "stored device credential cannot be decrypted"
		return result, fmt.Errorf("decrypt device credential: %w", err)
	}
	credential, err := s.resolveCredential(username.String, storedPassword, isAP)
	if err != nil {
		result.Status = "failed"
		result.Message = err.Error()
		return result, err
	}
	user, pass := credential.Username, credential.Password

	// Normalize platform for lookups/dispatch
	platformLower := strings.ToLower(strings.TrimSpace(platform.String))

	// Some operations (auto-select latest, or version lookup) require a device flavor.
	// Stations learned indirectly may not have flavor populated in the DB yet, so infer it on-demand.
	isDirectReference := strings.HasPrefix(firmwareFile, firmwareIDPrefix) || strings.HasSuffix(strings.ToLower(firmwareFile), ".bin")
	needsFlavor := firmwareFile == "" || !isDirectReference
	if needsFlavor && strings.TrimSpace(flavor.String) == "" {
		inferred, infErr := s.inferDeviceFlavor(ctx, deviceID, ip.String, platformLower, user, pass)
		if infErr != nil {
			result.Status = "failed"
			result.Message = fmt.Sprintf("cannot determine device flavor: %v", infErr)
			return result, fmt.Errorf("cannot determine device flavor: %w", infErr)
		}
		fl := strings.TrimSpace(inferred)
		if fl == "" {
			result.Status = "failed"
			result.Message = "cannot determine device flavor"
			return result, fmt.Errorf("cannot determine device flavor")
		}
		flavor.String = fl
		flavor.Valid = true
		if _, err := s.db.ExecContext(ctx, `UPDATE devices SET flavor=$1 WHERE id=$2`, fl, deviceID); err != nil {
			log.Printf("UpgradeDevice: warning: failed to persist inferred flavor for device %d: %v", deviceID, err)
		}
	}
	// Determine firmware file - securely resolve the path
	var actualFirmwarePath string
	if firmwareFile == "" {
		// Auto-select based on flavor
		if flavor.String == "" {
			result.Status = "failed"
			result.Message = "cannot determine device flavor"
			return result, fmt.Errorf("cannot determine device flavor")
		}
		actualFirmwarePath, err = s.FindFirmwareForFlavor(flavor.String)
		if err != nil {
			result.Status = "failed"
			result.Message = err.Error()
			return result, err
		}
	} else if isDirectReference {
		// User-provided firmware name - securely resolve to path
		// This validates the input and prevents directory traversal
		actualFirmwarePath, err = s.ResolveFirmwarePath(firmwareFile)
		if err != nil {
			result.Status = "failed"
			result.Message = err.Error()
			return result, err
		}
	} else {
		// Treat as version string - resolve by version + device flavor
		if flavor.String == "" {
			result.Status = "failed"
			result.Message = "cannot determine device flavor for version lookup"
			return result, fmt.Errorf("cannot determine device flavor")
		}
		fw, err := s.FindFirmwareByVersion(firmwareFile, flavor.String)
		if err != nil {
			log.Printf("UpgradeDevice: FindFirmwareByVersion failed: %v", err)
			result.Status = "failed"
			result.Message = err.Error()
			return result, err
		}
		actualFirmwarePath = fw.Path
	}

	log.Printf("UpgradeDevice: resolved firmware path=%s", actualFirmwarePath)
	result.NewVersion = strings.TrimSuffix(filepath.Base(actualFirmwarePath), ".bin")

	// Check if already at target version
	if firmwareMatches(currentFW.String, filepath.Base(actualFirmwarePath)) && !force {
		log.Printf("UpgradeDevice: skipping - already at target version")
		result.Status = "skipped"
		result.Message = "already at target version"
		return result, nil
	}

	// Record job
	var jobID int64
	s.db.QueryRow(`
		INSERT INTO firmware_jobs (device_id, firmware_file, target_version, status, started_at)
		VALUES ($1, $2, $3, 'uploading', NOW())
		RETURNING id
	`, deviceID, filepath.Base(actualFirmwarePath), result.NewVersion).Scan(&jobID)

	// Perform upgrade based on platform
	log.Printf("UpgradeDevice: starting upload to %s platform=%s", ip.String, platform.String)
	if platformLower == "airmax" || platformLower == "airfiber" {
		// airMAX uses CGI API (fwupl.cgi + fwflash.cgi)
		err = s.doUpgradeAirMAX(ctx, deviceID, ip.String, user, pass, actualFirmwarePath)
	} else {
		// Wave and LTU use JSON API (/api/v1.0/system/upgrade/direct)
		err = s.doUpgrade(ctx, deviceID, ip.String, user, pass, actualFirmwarePath, platformLower)
	}

	if err != nil {
		log.Printf("UpgradeDevice: upgrade failed: %v", err)
		result.Status = "failed"
		result.Message = err.Error()
		// Detect auth failures specifically
		errLower := strings.ToLower(err.Error())
		if strings.Contains(errLower, "login failed") ||
			strings.Contains(errLower, "401") ||
			strings.Contains(errLower, "403") ||
			strings.Contains(errLower, "forbidden") ||
			strings.Contains(errLower, "unauthorized") {
			result.Status = "auth_failed"
		}
		dbExecIgnore(s.db, `UPDATE firmware_jobs SET status = 'failed', error_message = $1, completed_at = NOW() WHERE id = $2`,
			err.Error(), jobID)
		return result, err
	}

	log.Printf("UpgradeDevice: upgrade successful, device rebooting")
	result.Status = "success"
	result.Message = "upgrade initiated, device rebooting"
	dbExecIgnore(s.db, `UPDATE firmware_jobs SET status = 'success', completed_at = NOW() WHERE id = $1`, jobID)
	dbExecIgnore(s.db, `UPDATE devices SET status = 'upgrading' WHERE id = $1`, deviceID)

	return result, nil
}

func (s *Service) doUpgrade(ctx context.Context, deviceID int64, ip, username, password, firmwareFile, platform string) error {
	baseURL := fmt.Sprintf("https://%s", ip)
	client := s.getDeviceClient(deviceID)

	// Build list of credentials to try: device-specific first, then global pairs
	type cred struct{ user, pass string }
	var creds []cred
	if username != "" && password != "" {
		creds = append(creds, cred{username, password})
	}
	for _, c := range s.credentialSnapshot(true) {
		creds = append(creds, cred{c.Username, c.Password})
	}
	for _, c := range s.credentialSnapshot(false) {
		creds = append(creds, cred{c.Username, c.Password})
	}

	// Try all credential pairs
	var token string
	var err error
	seen := make(map[string]bool)
	for _, c := range creds {
		key := c.user + ":" + c.pass
		if seen[key] {
			continue
		}
		seen[key] = true
		token, err = s.loginWithClient(client, ip, c.user, c.pass)
		if err == nil {
			username = c.user // Remember which credential worked for LTU flash
			password = c.pass
			break
		}
		if isConnectionError(err) {
			break // No point trying more on connection errors
		}
	}

	if err != nil {
		return fmt.Errorf("login failed: %w", err)
	}

	// Upload firmware
	log.Printf("Uploading firmware to %s...", ip)
	if err := s.uploadFirmware(client, baseURL, token, firmwareFile); err != nil {
		return fmt.Errorf("upload failed: %w", err)
	}

	// Wait for verification - this polls status every 5 seconds which keeps token alive
	log.Printf("Waiting for firmware verification on %s...", ip)
	status, err := s.waitForUpgrade(client, ctx, baseURL, token, 5*time.Minute)
	if err != nil {
		return fmt.Errorf("verification failed: %w", err)
	}

	if hasSameVersionWarning(status) {
		return nil // Already at target
	}

	// LTU requires explicit flash trigger via airMAX CGI (doesn't auto-reboot like Wave)
	if platform == "ltu" {
		log.Printf("Triggering LTU flash on %s...", ip)
		return s.triggerLTUFlash(deviceID, ip, username, password)
	}

	// Wave auto-reboots, but call reboot API as backup
	log.Printf("Rebooting %s...", ip)
	return s.reboot(client, baseURL, token)
}

// triggerLTUFlash triggers firmware flash on LTU via airMAX CGI API
// LTU uses Wave API for upload but needs airMAX fwflash.cgi to actually flash
func (s *Service) triggerLTUFlash(deviceID int64, ip, username, password string) error {

	// Create AirMAX client
	var client *airmax.Client
	if s.tlsManager != nil {
		transport := s.tlsManager.GetInsecureTransport()
		var rt http.RoundTripper = transport
		if s.ultraDebug != nil && s.ultraDebug.IsEnabled(deviceID) {
			rt = udebug.WrapTransport(s.ultraDebug, deviceID, rt, "ltu_flash", udebug.DefaultCaptureLimit)
		}
		client = airmax.NewClientWithTransport(ip, 30*time.Second, rt)
	} else {
		if s.ultraDebug != nil && s.ultraDebug.IsEnabled(deviceID) {
			base := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
			rt := udebug.WrapTransport(s.ultraDebug, deviceID, base, "ltu_flash", udebug.DefaultCaptureLimit)
			client = airmax.NewClientWithTransport(ip, 30*time.Second, rt)
		} else {
			client = airmax.NewClient(ip, 30*time.Second)
		}
	}

	// Login via login.cgi + /api/auth to get CSRF token
	if err := client.Login(username, password); err != nil {
		return fmt.Errorf("airMAX login for flash trigger failed: %w", err)
	}

	// Trigger flash
	return client.TriggerFlash()
}

// doUpgradeAirMAX performs firmware upgrade on AirMAX/LTU devices via CGI API
// Uses login.cgi for auth, then fwupl.cgi/system.cgi for upload, then fwflash.cgi to flash
func (s *Service) doUpgradeAirMAX(ctx context.Context, deviceID int64, ip, username, password, firmwareFile string) error {
	// Read firmware file
	firmwareData, err := os.ReadFile(firmwareFile)
	if err != nil {
		return fmt.Errorf("read firmware: %w", err)
	}

	// Create AirMAX client with insecure TLS (self-signed certs common on these devices)
	var client *airmax.Client
	if s.tlsManager != nil {
		transport := s.tlsManager.GetInsecureTransport()
		var rt http.RoundTripper = transport
		if s.ultraDebug != nil && s.ultraDebug.IsEnabled(deviceID) {
			rt = udebug.WrapTransport(s.ultraDebug, deviceID, rt, "airmax_upgrade", udebug.DefaultCaptureLimit)
		}
		client = airmax.NewClientWithTransport(ip, 5*time.Minute, rt)
	} else {
		if s.ultraDebug != nil && s.ultraDebug.IsEnabled(deviceID) {
			base := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
			rt := udebug.WrapTransport(s.ultraDebug, deviceID, base, "airmax_upgrade", udebug.DefaultCaptureLimit)
			client = airmax.NewClientWithTransport(ip, 5*time.Minute, rt)
		} else {
			client = airmax.NewClient(ip, 5*time.Minute)
		}
	}

	// Build list of credentials to try: device-specific first, then global pairs
	var creds []airmax.Credential
	if username != "" && password != "" {
		creds = append(creds, airmax.Credential{Username: username, Password: password})
	}
	for _, c := range s.credentialSnapshot(true) {
		creds = append(creds, airmax.Credential{Username: c.Username, Password: c.Password})
	}
	for _, c := range s.credentialSnapshot(false) {
		creds = append(creds, airmax.Credential{Username: c.Username, Password: c.Password})
	}

	// Try all credentials
	loginErr := client.LoginWithCredentials(creds)
	if loginErr != nil {
		return fmt.Errorf("login failed: %w", loginErr)
	}

	// Upload and flash firmware
	log.Printf("Uploading firmware to AirMAX device %s...", ip)
	if err := client.UpgradeFirmware(firmwareData, filepath.Base(firmwareFile)); err != nil {
		return fmt.Errorf("upgrade failed: %w", err)
	}

	return nil
}

// login authenticates to a Wave device and returns the auth token
// host should be just the IP/hostname, not a full URL
func (s *Service) login(host, username, password string) (string, error) {
	baseURL := fmt.Sprintf("https://%s", host)
	loginURL := baseURL + "/api/v1.0/user/login"

	body, _ := json.Marshal(map[string]string{
		"username": username,
		"password": password,
	})

	req, err := http.NewRequest("POST", loginURL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// Build request description for error messages
	reqDesc := fmt.Sprintf("POST %s [user=%s]", loginURL, username)

	// Use a client that doesn't follow redirects for login
	client := &http.Client{
		Transport: s.httpClient.Transport,
		Timeout:   30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // Don't follow redirects
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("login failed: %s -> %w", reqDesc, err)
	}
	defer resp.Body.Close()

	// Drain body
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("login failed: %s -> status %d: %s", reqDesc, resp.StatusCode, string(respBody))
	}

	token := resp.Header.Get("x-auth-token")
	if token == "" {
		// Check if we got HTML (device probably returned login page)
		if bytes.Contains(respBody, []byte("<!doctype")) || bytes.Contains(respBody, []byte("<html")) {
			return "", fmt.Errorf("login failed: %s -> got HTML instead of JSON (wrong credentials or API issue)", reqDesc)
		}
		return "", fmt.Errorf("login failed: %s -> no token in response header", reqDesc)
	}

	return token, nil
}

// loginWithClient authenticates using a specific HTTP client (for TLS verification)
func (s *Service) loginWithClient(client *http.Client, host, username, password string) (string, error) {
	baseURL := fmt.Sprintf("https://%s", host)
	loginURL := baseURL + "/api/v1.0/user/login"

	body, _ := json.Marshal(map[string]string{
		"username": username,
		"password": password,
	})

	req, err := http.NewRequest("POST", loginURL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// Build request description for error messages
	reqDesc := fmt.Sprintf("POST %s [user=%s]", loginURL, username)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("login failed: %s -> %w", reqDesc, err)
	}
	defer resp.Body.Close()

	// Drain body
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("login failed: %s -> status %d: %s", reqDesc, resp.StatusCode, string(respBody))
	}

	token := resp.Header.Get("x-auth-token")
	if token == "" {
		return "", fmt.Errorf("login failed: %s -> no token in response header", reqDesc)
	}

	return token, nil
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
		strings.Contains(s, "connection reset")
}

type upgradeStatus struct {
	Status        string   `json:"status"`
	Warnings      []string `json:"warnings"`
	FailureReason string   `json:"failureReason"`
}

// getUpgradeStatusSnapshot fetches the Wave/LTU upgrade status endpoint and returns a compact
// human-readable snapshot for debugging and job logs.
//
// NOTE: This intentionally returns a string (instead of a struct) so callers can include
// the snapshot in error messages even when the payload shape varies across firmware versions.
func (s *Service) getUpgradeStatusSnapshot(client *http.Client, baseURL, token string) string {
	req, err := http.NewRequest("GET", baseURL+"/api/v1.0/system/upgrade", nil)
	if err != nil {
		return fmt.Sprintf("GET /api/v1.0/system/upgrade (build request): %v", err)
	}
	req.Header.Set("x-auth-token", token)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Sprintf("GET /api/v1.0/system/upgrade (request failed): %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	bodyStr := strings.TrimSpace(string(body))
	if bodyStr == "" {
		bodyStr = "<empty>"
	}
	// Keep this line reasonably short for the Jobs UI.
	if len(bodyStr) > 800 {
		bodyStr = bodyStr[:800] + "…"
	}

	return fmt.Sprintf("GET /api/v1.0/system/upgrade -> %s: %s", resp.Status, bodyStr)
}

func (s *Service) getUpgradeStatus(client *http.Client, baseURL, token string) (*upgradeStatus, error) {
	req, err := http.NewRequest("GET", baseURL+"/api/v1.0/system/upgrade", nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("x-auth-token", token)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var status upgradeStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &status, nil
}

func (s *Service) uploadFirmware(client *http.Client, baseURL, token, firmwarePath string) error {
	file, err := os.Open(firmwarePath)
	if err != nil {
		return err
	}
	defer file.Close()

	fi, _ := file.Stat()
	fileSize := fi.Size()

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

	uploadURL := baseURL + "/api/v1.0/system/upgrade/direct"
	req, err := http.NewRequest("POST", uploadURL, &buf)
	if err != nil {
		return fmt.Errorf("create upload request: %w", err)
	}
	req.Header.Set("x-auth-token", token)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	// Build request description for error messages
	reqDesc := fmt.Sprintf("POST %s [x-auth-token=set, file=%s, size=%d]", uploadURL, filepath.Base(firmwarePath), fileSize)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("upload failed: %s -> %w", reqDesc, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		bodyStr := strings.TrimSpace(string(body))
		if bodyStr == "" {
			bodyStr = "<empty>"
		}

		// If the device says an upgrade is already active, attach a snapshot of the current
		// upgrade state so we can debug why it's stuck/active on only certain radios.
		lower := strings.ToLower(bodyStr)
		if strings.Contains(lower, "upgrade") && strings.Contains(lower, "already active") {
			snapshot := s.getUpgradeStatusSnapshot(client, baseURL, token)
			return fmt.Errorf("upload failed: %s -> status %s: %s (%s)", reqDesc, resp.Status, bodyStr, snapshot)
		}

		return fmt.Errorf("upload failed: %s -> status %s: %s", reqDesc, resp.Status, bodyStr)
	}

	return nil
}

func (s *Service) waitForUpgrade(client *http.Client, ctx context.Context, baseURL, token string, timeout time.Duration) (*upgradeStatus, error) {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		status, err := s.getUpgradeStatus(client, baseURL, token)
		if err != nil {
			time.Sleep(5 * time.Second)
			continue
		}

		lower := strings.ToLower(status.Status)
		switch lower {
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

func (s *Service) reboot(client *http.Client, baseURL, token string) error {
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

	// Use the provided HTTP client when available, but ensure we always have a
	// transport and a reasonable timeout for reboot requests.
	hc := client
	if hc == nil {
		hc = &http.Client{
			Transport: s.httpClient.Transport,
			Timeout:   2 * time.Minute,
		}
	} else {
		// If the caller didn't set a timeout, use a conservative one.
		if hc.Timeout == 0 {
			clone := *hc
			clone.Timeout = 2 * time.Minute
			hc = &clone
		}
		// Ensure we have a transport. If not, borrow the service transport.
		if hc.Transport == nil {
			clone := *hc
			clone.Transport = s.httpClient.Transport
			hc = &clone
		}
	}

	resp, err := hc.Do(req)
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

func hasSameVersionWarning(status *upgradeStatus) bool {
	for _, w := range status.Warnings {
		if w == "SAME_VERSION" {
			return true
		}
	}
	return false
}

func firmwareMatches(current, filename string) bool {
	// First, keep legacy behavior: some platforms report a firmware string that
	// matches the uploaded firmware filename (without extension).
	target := strings.TrimSuffix(filename, ".bin")
	target = strings.TrimSuffix(target, ".BIN")
	if strings.EqualFold(current, target) {
		return true
	}

	// Otherwise, compare the extracted version components. Some devices report
	// versions without a leading "v" (e.g., "4.1.0"), while firmware metadata
	// commonly uses "v4.1.0".
	curVer := extractVersionFromAnyString(current)
	tgtVer := parseFirmwareFilename(filename).Version
	if tgtVer == "" {
		tgtVer = extractVersionFromAnyString(target)
	}
	if curVer == "" || tgtVer == "" {
		return false
	}
	return compareVersions(curVer, tgtVer) == 0
}

// extractVersionFromAnyString extracts a normalized "vX.Y.Z[.B]" style version
// from an arbitrary firmware/version string.
//
// Examples:
//
//	"v4.1.0" -> "v4.1.0"
//	"4.1.0" -> "v4.1.0"
//	"GMC.ipq5018.v4.1.0.deda4ab.251212" -> "v4.1.0"
func extractVersionFromAnyString(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	lower := strings.ToLower(s)
	if len(lower) > 0 && lower[0] >= '0' && lower[0] <= '9' {
		lower = "v" + lower
	}
	parts := strings.Split(lower, ".")
	return extractVersion(parts)
}

// parseFirmwareFilename extracts metadata from firmware filename
func parseFirmwareFilename(filename string) FirmwareFile {
	fw := FirmwareFile{Name: filename}

	// Remove .bin extension
	name := strings.TrimSuffix(strings.ToLower(filename), ".bin")
	parts := strings.Split(name, ".")

	if len(parts) == 0 {
		return fw
	}

	prefix := strings.ToUpper(parts[0])

	// Wave flavors: GMC.v1.2.3.bin or GMC.ipq5018.v4.1.0.hash.date.time.bin
	// Also GP (AirFiber 60) uses Wave API
	switch prefix {
	case "GMC", "GMP", "MGMP", "GP":
		fw.Platform = "Wave"
		fw.Flavor = prefix
		fw.Version = extractVersion(parts)
		return fw
	}

	// Wave MLO: MW.ipq53xx.v2.4.1.422de8ab.251220.0747.bin
	if prefix == "MW" {
		fw.Platform = "MLO"
		fw.Flavor = "MW"
		fw.Version = extractVersion(parts)
		return fw
	}

	// LTU: AFLTUROCKET (AP), AFLTU (AP/STA), AF5XHD
	lowerPrefix := strings.ToLower(prefix)
	if lowerPrefix == "aflturocket" || lowerPrefix == "afltu" || lowerPrefix == "af5xhd" {
		fw.Platform = "LTU"
		fw.Flavor = prefix
		fw.Version = extractVersion(parts)
		return fw
	}

	// AirOS 8 (AirMAX AC): XC.qca955x.v8.7.0.12345.bin, 2XC.v8.7.8.46705.bin
	// XC/2XC = AC series with QCA chipset (Rocket 5AC, PowerBeam 5AC, etc.)
	// Keep XC and 2XC as separate flavors - they are NOT interchangeable
	if prefix == "XC" || prefix == "2XC" {
		fw.Platform = "airMAX AC"
		fw.AirMAX = prefix
		fw.Flavor = prefix // XC or 2XC - must match for upgrades
		fw.Version = extractVersion(parts)
		return fw
	}

	// AirOS 8 (AirMAX AC): WA.v8.7.0.bin, 2WA.v8.7.8.46705.bin - WA/2WA variants
	// Keep WA and 2WA as separate flavors - they are NOT interchangeable
	if prefix == "WA" || prefix == "2WA" {
		fw.Platform = "airMAX AC"
		fw.AirMAX = prefix
		fw.Flavor = prefix // WA or 2WA - must match for upgrades
		fw.Version = extractVersion(parts)
		return fw
	}

	// AirOS 5 (AirMAX M): XM.ar7240.v6.3.0.bin, XM.v6.1.11.32949.bin
	// XM = M series with AR7240/AR9342 chipset (NanoStation M5, Rocket M5, etc.)
	if prefix == "XM" {
		fw.Platform = "airMAX M"
		fw.AirMAX = "XM"
		fw.Flavor = "XM" // Keep as XM - not interchangeable with XW/TI
		fw.Version = extractVersion(parts)
		return fw
	}

	// AirOS 5 (AirMAX M): XW.v6.3.0.bin - XW variant
	if prefix == "XW" {
		fw.Platform = "airMAX M"
		fw.AirMAX = "XW"
		fw.Flavor = "XW" // Keep as XW - not interchangeable with XM/TI
		fw.Version = extractVersion(parts)
		return fw
	}

	// AirOS 5 (AirMAX M): TI.v6.x.x.bin - TI variant (TI chipset devices)
	if prefix == "TI" {
		fw.Platform = "airMAX M"
		fw.AirMAX = "TI"
		fw.Flavor = "TI" // Keep as TI - not interchangeable with XM/XW
		fw.Version = extractVersion(parts)
		return fw
	}

	// AirFiber (future): AF11, AF24, AF2X, AF3X, AF5, AF5X
	// These use a different API and are not yet fully supported
	if strings.HasPrefix(prefix, "AF") && len(prefix) >= 3 {
		fw.Platform = "AirFiber"
		fw.Flavor = prefix
		fw.Version = extractVersion(parts)
		return fw
	}

	return fw
}

// isNumeric checks if a string contains only digits
func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// extractVersion finds and returns version string from dot-separated filename parts
// Handles both old format (v1.2.3 in single part) and new format (v1, 2, 3 split across parts)
// Preserves suffixes like -beta, -cust1, -dev-cust1, _beta, etc.
func extractVersion(parts []string) string {
	for i, p := range parts {
		if strings.HasPrefix(p, "v") && len(p) > 1 {
			rest := p[1:]
			if len(rest) > 0 && rest[0] >= '0' && rest[0] <= '9' {
				// Found version start, reconstruct from split parts
				version := p
				for j := i + 1; j < len(parts) && j < i+3; j++ {
					// Check if this part is numeric or numeric with suffix
					part := parts[j]
					// Handle parts like "0-beta" or "3_cust1"
					numEnd := 0
					for numEnd < len(part) && part[numEnd] >= '0' && part[numEnd] <= '9' {
						numEnd++
					}
					if numEnd > 0 {
						version += "." + part
						// If there's a suffix (non-numeric part), we're done with version parts
						if numEnd < len(part) {
							return version
						}
					} else {
						break
					}
				}
				return version
			}
		}
	}
	return ""
}

// extractFlavor is kept for backward compatibility
func extractFlavor(filename string) string {
	return parseFirmwareFilename(filename).Flavor
}

// UpgradeFanout upgrades an AP and all its connected STAs
func (s *Service) UpgradeFanout(ctx context.Context, apID int64, force bool) ([]*UpgradeResult, error) {
	var results []*UpgradeResult
	var mu sync.Mutex

	// Get AP info
	var apIP, apMAC, apFlavor, apFirmware sql.NullString
	err := s.db.QueryRow(`
		SELECT ip_address, mac, flavor, firmware FROM devices WHERE id = $1
	`, apID).Scan(&apIP, &apMAC, &apFlavor, &apFirmware)
	if err != nil {
		return nil, fmt.Errorf("AP not found: %w", err)
	}

	// Get STAs
	rows, err := s.db.Query(`
		SELECT id, ip_address, mac, hostname, flavor, firmware
		FROM devices WHERE parent_id = $1
	`, apID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type staInfo struct {
		ID       int64
		IP       string
		MAC      string
		Hostname string
		Flavor   string
		Firmware string
	}
	var stas []staInfo

	for rows.Next() {
		var sta staInfo
		var ip, mac, hostname, flavor, firmware sql.NullString
		rows.Scan(&sta.ID, &ip, &mac, &hostname, &flavor, &firmware)
		sta.IP = ip.String
		sta.MAC = mac.String
		sta.Hostname = hostname.String
		sta.Flavor = flavor.String
		sta.Firmware = firmware.String
		stas = append(stas, sta)
	}

	// Upgrade STAs first (in parallel)
	var wg sync.WaitGroup
	for _, sta := range stas {
		wg.Add(1)
		go func(sta staInfo) {
			defer wg.Done()

			// Find firmware for STA's flavor
			fwFile, err := s.FindFirmwareForFlavor(sta.Flavor)
			if err != nil {
				mu.Lock()
				results = append(results, &UpgradeResult{
					DeviceID:  sta.ID,
					DeviceIP:  sta.IP,
					DeviceMAC: sta.MAC,
					Hostname:  sta.Hostname,
					Status:    "skipped",
					Message:   fmt.Sprintf("no firmware for flavor %s", sta.Flavor),
				})
				mu.Unlock()
				return
			}

			// Check if already at target
			if firmwareMatches(sta.Firmware, filepath.Base(fwFile)) && !force {
				mu.Lock()
				results = append(results, &UpgradeResult{
					DeviceID:  sta.ID,
					DeviceIP:  sta.IP,
					DeviceMAC: sta.MAC,
					Hostname:  sta.Hostname,
					Status:    "skipped",
					Message:   "already at target version",
				})
				mu.Unlock()
				return
			}

			result, upgradeErr := s.UpgradeDevice(ctx, sta.ID, fwFile, force)
			result = normalizeUpgradeResult(result, sta.ID, upgradeErr)
			mu.Lock()
			results = append(results, result)
			mu.Unlock()
		}(sta)
	}
	wg.Wait()

	// Now upgrade AP
	apFWFile, err := s.FindFirmwareForFlavor(apFlavor.String)
	if err != nil {
		results = append(results, &UpgradeResult{
			DeviceID:  apID,
			DeviceIP:  apIP.String,
			DeviceMAC: apMAC.String,
			Status:    "skipped",
			Message:   fmt.Sprintf("no firmware for flavor %s", apFlavor.String),
		})
		return results, nil
	}

	if firmwareMatches(apFirmware.String, filepath.Base(apFWFile)) && !force {
		results = append(results, &UpgradeResult{
			DeviceID:  apID,
			DeviceIP:  apIP.String,
			DeviceMAC: apMAC.String,
			Status:    "skipped",
			Message:   "already at target version",
		})
		return results, nil
	}

	apResult, upgradeErr := s.UpgradeDevice(ctx, apID, apFWFile, force)
	apResult = normalizeUpgradeResult(apResult, apID, upgradeErr)
	results = append(results, apResult)

	return results, nil
}

// RetryUpgradeWithCredentials retries upgrade on specific devices with given credentials
func (s *Service) RetryUpgradeWithCredentials(ctx context.Context, deviceIDs []int64, username, password string, force bool) []*UpgradeResult {
	var results []*UpgradeResult
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, deviceID := range deviceIDs {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()

			// Get device info
			var ip, mac, hostname, flavor, platform, currentFW sql.NullString
			err := s.db.QueryRow(`
				SELECT ip_address, mac, hostname, flavor, platform, firmware
				FROM devices WHERE id = $1
			`, id).Scan(&ip, &mac, &hostname, &flavor, &platform, &currentFW)

			if err != nil {
				mu.Lock()
				results = append(results, &UpgradeResult{
					DeviceID: id,
					Status:   "failed",
					Message:  "device not found",
				})
				mu.Unlock()
				return
			}

			result := &UpgradeResult{
				DeviceID:   id,
				DeviceIP:   ip.String,
				DeviceMAC:  mac.String,
				Hostname:   hostname.String,
				OldVersion: currentFW.String,
			}

			// Find firmware for device
			fwFile, err := s.FindFirmwareForFlavor(flavor.String)
			if err != nil {
				result.Status = "skipped"
				result.Message = fmt.Sprintf("no firmware for flavor %s", flavor.String)
				mu.Lock()
				results = append(results, result)
				mu.Unlock()
				return
			}

			result.NewVersion = strings.TrimSuffix(filepath.Base(fwFile), ".bin")

			// Check if already at target
			if firmwareMatches(currentFW.String, filepath.Base(fwFile)) && !force {
				result.Status = "skipped"
				result.Message = "already at target version"
				mu.Lock()
				results = append(results, result)
				mu.Unlock()
				return
			}

			// Record job
			var jobID int64
			s.db.QueryRow(`
				INSERT INTO firmware_jobs (device_id, firmware_file, target_version, status, started_at)
				VALUES ($1, $2, $3, 'uploading', NOW())
				RETURNING id
			`, id, filepath.Base(fwFile), result.NewVersion).Scan(&jobID)

			// Perform upgrade with provided credentials (user gave explicit creds, but still retry on failure)
			var upgradeErr error
			platformLower := strings.ToLower(platform.String)
			if platformLower == "airmax" {
				// airMAX uses CGI API (fwupl.cgi + fwflash.cgi)
				upgradeErr = s.doUpgradeAirMAX(ctx, id, ip.String, username, password, fwFile)
			} else {
				// Wave and LTU use JSON API (/api/v1.0/system/upgrade/direct)
				upgradeErr = s.doUpgradeWithCreds(ctx, id, ip.String, username, password, fwFile, platformLower)
			}

			if upgradeErr != nil {
				result.Status = "failed"
				result.Message = upgradeErr.Error()
				errLower := strings.ToLower(upgradeErr.Error())
				if strings.Contains(errLower, "login failed") ||
					strings.Contains(errLower, "401") ||
					strings.Contains(errLower, "403") ||
					strings.Contains(errLower, "forbidden") ||
					strings.Contains(errLower, "unauthorized") {
					result.Status = "auth_failed"
				}
				dbExecIgnore(s.db, `UPDATE firmware_jobs SET status = 'failed', error_message = $1, completed_at = NOW() WHERE id = $2`,
					upgradeErr.Error(), jobID)
			} else {
				result.Status = "success"
				result.Message = "upgrade initiated, device rebooting"
				dbExecIgnore(s.db, `UPDATE firmware_jobs SET status = 'success', completed_at = NOW() WHERE id = $1`, jobID)
				dbExecIgnore(s.db, `UPDATE devices SET status = 'upgrading' WHERE id = $1`, id)
				// Persist the successful credential only in encrypted form.
				storedPassword := password
				if s.secretStore != nil {
					storedPassword, err = s.secretStore.Encrypt(password)
					if err != nil {
						log.Printf("firmware: cannot persist credential for device %d: %v", id, err)
						storedPassword = ""
					}
				}
				if storedPassword != "" {
					dbExecIgnore(s.db, `UPDATE devices SET username = $1, password = $2 WHERE id = $3`, username, storedPassword, id)
				}
			}

			mu.Lock()
			results = append(results, result)
			mu.Unlock()
		}(deviceID)
	}

	wg.Wait()
	return results
}

// doUpgradeWithCreds performs upgrade with explicit credentials (no fallback)
// Note: This uses the default HTTP client without TLS verification
func (s *Service) doUpgradeWithCreds(ctx context.Context, deviceID int64, ip, username, password, firmwareFile, platform string) error {
	baseURL := fmt.Sprintf("https://%s", ip)
	client := s.getInsecureClient(deviceID)

	// Login with provided credentials only
	token, err := s.loginWithClient(client, ip, username, password)
	if err != nil {
		return fmt.Errorf("login failed: %w", err)
	}

	// Upload firmware
	log.Printf("Uploading firmware to %s...", ip)
	if err := s.uploadFirmware(client, baseURL, token, firmwareFile); err != nil {
		return fmt.Errorf("upload failed: %w", err)
	}

	// Wait for verification
	log.Printf("Waiting for firmware verification on %s...", ip)
	status, err := s.waitForUpgrade(client, ctx, baseURL, token, 5*time.Minute)
	if err != nil {
		return fmt.Errorf("verification failed: %w", err)
	}

	if hasSameVersionWarning(status) {
		return nil
	}

	// LTU requires explicit flash trigger via airMAX CGI (doesn't auto-reboot like Wave)
	if platform == "ltu" {
		log.Printf("Triggering LTU flash on %s...", ip)
		return s.triggerLTUFlash(deviceID, ip, username, password)
	}

	// Wave auto-reboots, but call reboot API as backup
	log.Printf("Rebooting %s...", ip)
	return s.reboot(client, baseURL, token)
}

// UpgradeBulk upgrades multiple devices, for each AP: STAs first then AP
func (s *Service) UpgradeBulk(ctx context.Context, deviceIDs []int64, firmwareFile string, force bool) []*UpgradeResult {
	var results []*UpgradeResult
	var mu sync.Mutex

	// Group devices by AP
	// Map: AP ID -> list of STA IDs under that AP
	apSTAs := make(map[int64][]int64)
	standaloneAPs := []int64{}
	standaloneStas := []int64{} // STAs whose AP isn't in our list

	for _, id := range deviceIDs {
		var parentID sql.NullInt64
		s.db.QueryRow(`SELECT parent_id FROM devices WHERE id = $1`, id).Scan(&parentID)
		if parentID.Valid {
			// This is a STA
			apSTAs[parentID.Int64] = append(apSTAs[parentID.Int64], id)
		} else {
			// This is an AP
			standaloneAPs = append(standaloneAPs, id)
		}
	}

	// Check which APs are in our device list
	apInList := make(map[int64]bool)
	for _, id := range standaloneAPs {
		apInList[id] = true
	}

	// For each AP in our list: upgrade its STAs first, then the AP
	for _, apID := range standaloneAPs {
		stas := apSTAs[apID]

		// Upgrade STAs for this AP (in parallel)
		if len(stas) > 0 {
			log.Printf("UpgradeBulk: upgrading %d STAs for AP %d", len(stas), apID)
			var wg sync.WaitGroup
			for _, staID := range stas {
				wg.Add(1)
				go func(deviceID int64) {
					defer wg.Done()
					result, upgradeErr := s.UpgradeDevice(ctx, deviceID, firmwareFile, force)
					result = normalizeUpgradeResult(result, deviceID, upgradeErr)
					mu.Lock()
					results = append(results, result)
					mu.Unlock()
				}(staID)
			}
			wg.Wait()
		}

		// Now upgrade this AP
		log.Printf("UpgradeBulk: upgrading AP %d", apID)
		result, upgradeErr := s.UpgradeDevice(ctx, apID, firmwareFile, force)
		result = normalizeUpgradeResult(result, apID, upgradeErr)
		mu.Lock()
		results = append(results, result)
		mu.Unlock()

		// Remove these STAs from the map so we don't process them again
		delete(apSTAs, apID)
	}

	// Handle any remaining STAs whose AP wasn't in the list
	for apID, stas := range apSTAs {
		if !apInList[apID] {
			standaloneStas = append(standaloneStas, stas...)
		}
	}

	// Upgrade standalone STAs (AP not in list) in parallel
	if len(standaloneStas) > 0 {
		log.Printf("UpgradeBulk: upgrading %d standalone STAs", len(standaloneStas))
		var wg sync.WaitGroup
		for _, staID := range standaloneStas {
			wg.Add(1)
			go func(deviceID int64) {
				defer wg.Done()
				result, upgradeErr := s.UpgradeDevice(ctx, deviceID, firmwareFile, force)
				result = normalizeUpgradeResult(result, deviceID, upgradeErr)
				mu.Lock()
				results = append(results, result)
				mu.Unlock()
			}(staID)
		}
		wg.Wait()
	}

	return results
}

// RebootDevice reboots a device when only host/credentials are available.
// Prefer RebootDeviceByID for inventory-backed operations because it dispatches
// by stored platform/flavor instead of auto-detecting.
func (s *Service) RebootDevice(ip, username, password string) error {
	_, err := s.RebootDeviceTarget(context.Background(), 0, strings.TrimSpace(ip), "", "", username, password, "", "")
	return err
}

// FindByFlavor finds the newest firmware file for a flavor
func (s *Service) FindByFlavor(flavor string) *FirmwareFile {
	files, err := s.ListFirmware()
	if err != nil {
		return nil
	}

	var best *FirmwareFile
	for i := range files {
		if strings.EqualFold(files[i].Flavor, flavor) {
			if best == nil || files[i].Version > best.Version {
				best = &files[i]
			}
		}
	}
	return best
}

// FindByAirMAXPlatform finds firmware for AirMAX platform (XC, XM, XW)
func (s *Service) FindByAirMAXPlatform(platform string) *FirmwareFile {
	files, err := s.ListFirmware()
	if err != nil {
		return nil
	}

	var best *FirmwareFile
	for i := range files {
		if strings.EqualFold(files[i].AirMAX, platform) {
			if best == nil || files[i].Version > best.Version {
				best = &files[i]
			}
		}
	}
	return best
}

// FetchConfig retrieves the configuration backup from a device
func (s *Service) FetchConfig(ip, username, password string) ([]byte, error) {
	credential, err := s.resolveCredential(username, password, true)
	if err != nil {
		return nil, err
	}
	username, password = credential.Username, credential.Password

	// Login to Wave device
	token, err := s.login(ip, username, password)
	if err != nil {
		return nil, fmt.Errorf("login failed: %w", err)
	}

	// Fetch backup file via /api/v1.0/system/backup
	url := fmt.Sprintf("https://%s/api/v1.0/system/backup", ip)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("x-auth-token", token)
	req.Header.Set("Cache-Control", "no-store")

	client := &http.Client{
		Transport: s.httpClient.Transport,
		Timeout:   2 * time.Minute,
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch returned status %d", resp.StatusCode)
	}

	config, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read failed: %w", err)
	}

	return config, nil
}

// PushConfig uploads a configuration to a device
func (s *Service) PushConfig(ip, username, password string, config []byte) error {
	credential, err := s.resolveCredential(username, password, true)
	if err != nil {
		return err
	}
	username, password = credential.Username, credential.Password

	// Login to Wave device
	token, err := s.login(ip, username, password)
	if err != nil {
		return fmt.Errorf("login failed: %w", err)
	}

	// Upload config
	url := fmt.Sprintf("https://%s/api/v1.0/system/config", ip)
	req, err := http.NewRequest("PUT", url, bytes.NewReader(config))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("x-auth-token", token)
	req.Header.Set("Content-Type", "application/octet-stream")

	client := &http.Client{
		Transport: s.httpClient.Transport,
		Timeout:   2 * time.Minute,
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("push failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("push returned status %d", resp.StatusCode)
	}

	return nil
}

// ApplyConfig applies specific configuration changes to a device
func (s *Service) ApplyConfig(ip, username, password string, changes map[string]any) error {
	credential, err := s.resolveCredential(username, password, true)
	if err != nil {
		return err
	}
	username, password = credential.Username, credential.Password

	// Login to Wave device
	token, err := s.login(ip, username, password)
	if err != nil {
		return fmt.Errorf("login failed: %w", err)
	}

	// Build config update payload
	payload := make(map[string]any)

	if ssid, ok := changes["ssid"].(string); ok && ssid != "" {
		payload["wireless"] = map[string]any{"ssid": ssid}
	}
	if channel, ok := changes["channel"]; ok {
		if payload["wireless"] == nil {
			payload["wireless"] = make(map[string]any)
		}
		payload["wireless"].(map[string]any)["channel"] = channel
	}
	if power, ok := changes["tx_power"]; ok {
		if payload["wireless"] == nil {
			payload["wireless"] = make(map[string]any)
		}
		payload["wireless"].(map[string]any)["txPower"] = power
	}
	if pass, ok := changes["password"].(string); ok && pass != "" {
		payload["users"] = []map[string]any{
			{"name": username, "password": pass},
		}
	}

	if len(payload) == 0 {
		return nil // Nothing to apply
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

	url := fmt.Sprintf("https://%s/api/v1.0/system/config", ip)
	req, err := http.NewRequest("PATCH", url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("x-auth-token", token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{
		Transport: s.httpClient.Transport,
		Timeout:   2 * time.Minute,
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("apply failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("apply returned status %d", resp.StatusCode)
	}

	return nil
}
