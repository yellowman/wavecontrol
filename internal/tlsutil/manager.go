package tlsutil

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/yellowman/wavecontrol/internal/netutil"
)

// VerifyMode determines how TLS certificates are verified
type VerifyMode string

const (
	ModeInsecure VerifyMode = "insecure" // Skip all verification (legacy default)
	ModeTOFU     VerifyMode = "tofu"     // Trust-on-first-use, auto-pin on connect
	ModeStrict   VerifyMode = "strict"   // Require pre-pinned and verified certs
)

// CertInfo holds certificate information
type CertInfo struct {
	DeviceID            int64      `json:"device_id,omitempty"`
	Fingerprint         string     `json:"fingerprint"`
	Subject             string     `json:"subject"`
	Issuer              string     `json:"issuer"`
	NotBefore           time.Time  `json:"not_before"`
	NotAfter            time.Time  `json:"not_after"`
	PinnedAt            time.Time  `json:"pinned_at,omitempty"`
	PinnedBy            int        `json:"pinned_by,omitempty"`
	Verified            bool       `json:"verified"`
	VerifiedAt          *time.Time `json:"verified_at,omitempty"`
	VerifiedBy          int        `json:"verified_by,omitempty"`
	PreviousFingerprint string     `json:"previous_fingerprint,omitempty"`
	ChangedAt           *time.Time `json:"changed_at,omitempty"`
	Hostname            string     `json:"hostname,omitempty"`
	IP                  string     `json:"ip,omitempty"`
	CertValid           bool       `json:"cert_valid"`
}

// Manager handles TLS certificate verification and pinning
type Manager struct {
	db   *sql.DB
	mode VerifyMode

	// In-memory cache
	mu           sync.RWMutex
	byDeviceID   map[int64]*cachedCert
	byIP         map[string]*cachedCert // IP -> cert (for discovery before device ID known)
	deviceIDByIP map[string]int64       // IP -> device ID mapping
}

type cachedCert struct {
	Fingerprint string
	Verified    bool
	DeviceID    int64
}

// NewManager creates a new TLS manager
func NewManager(db *sql.DB) *Manager {
	m := &Manager{
		db:           db,
		mode:         ModeInsecure,
		byDeviceID:   make(map[int64]*cachedCert),
		byIP:         make(map[string]*cachedCert),
		deviceIDByIP: make(map[string]int64),
	}
	m.loadMode()
	m.loadCerts()
	return m
}

func (m *Manager) loadMode() {
	var modeStr string
	if m.db.QueryRow(`SELECT value FROM settings WHERE key = 'tls_verify_mode'`).Scan(&modeStr) == nil {
		switch modeStr {
		case "tofu":
			m.mode = ModeTOFU
		case "strict":
			m.mode = ModeStrict
		default:
			m.mode = ModeInsecure
		}
	}
}

func (m *Manager) loadCerts() {
	rows, err := m.db.Query(`
		SELECT dc.device_id, dc.fingerprint, dc.verified, host(d.ip_address) AS ip_address
		FROM device_certs dc
		JOIN devices d ON d.id = dc.device_id
	`)
	if err != nil {
		log.Printf("Failed to load pinned certs: %v", err)
		return
	}
	defer rows.Close()

	m.mu.Lock()
	defer m.mu.Unlock()

	count := 0
	for rows.Next() {
		var deviceID int64
		var fp string
		var verified bool
		var ip sql.NullString
		if rows.Scan(&deviceID, &fp, &verified, &ip) == nil {
			cached := &cachedCert{
				Fingerprint: fp,
				Verified:    verified,
				DeviceID:    deviceID,
			}
			m.byDeviceID[deviceID] = cached
			if ip.Valid && ip.String != "" {
				m.byIP[ip.String] = cached
				m.deviceIDByIP[ip.String] = deviceID
			}
			count++
		}
	}
	log.Printf("Loaded %d pinned certificates", count)
}

// Mode returns the current verification mode
func (m *Manager) Mode() VerifyMode {
	return m.mode
}

// SetMode updates the verification mode
func (m *Manager) SetMode(mode VerifyMode) error {
	_, err := m.db.Exec(`INSERT INTO settings (key, value) VALUES ('tls_verify_mode', $1)
		ON CONFLICT (key) DO UPDATE SET value = $1`, string(mode))
	if err != nil {
		return err
	}
	m.mode = mode
	return nil
}

// GetTransport returns an http.Transport for a known device ID
func (m *Manager) GetTransport(deviceID int64) *http.Transport {
	return &http.Transport{
		TLSClientConfig: m.getTLSConfig(deviceID),
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		MaxIdleConns:          10,
		IdleConnTimeout:       30 * time.Second,
		DisableCompression:    true,
		MaxIdleConnsPerHost:   2,
		ResponseHeaderTimeout: 30 * time.Second,
	}
}

// GetTransportForIP returns a transport for an IP (when device ID not yet known)
func (m *Manager) GetTransportForIP(ip string) *http.Transport {
	return &http.Transport{
		TLSClientConfig: m.getTLSConfigByIP(ip),
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		MaxIdleConns:          10,
		IdleConnTimeout:       30 * time.Second,
		DisableCompression:    true,
		MaxIdleConnsPerHost:   2,
		ResponseHeaderTimeout: 30 * time.Second,
	}
}

// GetInsecureTransport returns a transport that skips verification
// ONLY for probing where NO credentials are sent
func (m *Manager) GetInsecureTransport() *http.Transport {
	return &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		MaxIdleConns:          10,
		IdleConnTimeout:       30 * time.Second,
		DisableCompression:    true,
		MaxIdleConnsPerHost:   2,
		ResponseHeaderTimeout: 30 * time.Second,
	}
}

func (m *Manager) getTLSConfig(deviceID int64) *tls.Config {
	switch m.mode {
	case ModeStrict:
		return &tls.Config{
			InsecureSkipVerify: true,
			VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
				return m.verifyStrict(deviceID, rawCerts)
			},
		}
	case ModeTOFU:
		return &tls.Config{
			InsecureSkipVerify: true,
			VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
				return m.verifyTOFU(deviceID, rawCerts)
			},
		}
	default:
		// Insecure mode - skip all verification, allow any TLS version
		return &tls.Config{
			InsecureSkipVerify: true,
		}
	}
}

func (m *Manager) getTLSConfigByIP(ip string) *tls.Config {
	switch m.mode {
	case ModeStrict:
		return &tls.Config{
			InsecureSkipVerify: true,
			VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
				return m.verifyStrictByIP(ip, rawCerts)
			},
		}
	case ModeTOFU:
		return &tls.Config{
			InsecureSkipVerify: true,
			VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
				return m.verifyTOFUByIP(ip, rawCerts)
			},
		}
	default:
		// Insecure mode - skip all verification, allow any TLS version
		return &tls.Config{
			InsecureSkipVerify: true,
		}
	}
}

// verifyStrict requires pinned AND verified certificate
func (m *Manager) verifyStrict(deviceID int64, rawCerts [][]byte) error {
	if len(rawCerts) == 0 {
		return fmt.Errorf("no certificate presented")
	}

	fp := sha256.Sum256(rawCerts[0])
	fingerprint := hex.EncodeToString(fp[:])

	m.mu.RLock()
	cached, ok := m.byDeviceID[deviceID]
	m.mu.RUnlock()

	if !ok {
		return fmt.Errorf("no pinned certificate for device %d", deviceID)
	}
	if !cached.Verified {
		return fmt.Errorf("certificate not verified for device %d", deviceID)
	}
	if fingerprint != cached.Fingerprint {
		go m.recordCertChange(deviceID, fingerprint, rawCerts[0])
		return fmt.Errorf("certificate changed for device %d", deviceID)
	}
	return nil
}

// verifyStrictByIP verifies by IP address
func (m *Manager) verifyStrictByIP(ip string, rawCerts [][]byte) error {
	if len(rawCerts) == 0 {
		return fmt.Errorf("no certificate presented")
	}

	fp := sha256.Sum256(rawCerts[0])
	fingerprint := hex.EncodeToString(fp[:])

	m.mu.RLock()
	cached, ok := m.byIP[ip]
	m.mu.RUnlock()

	if !ok {
		return fmt.Errorf("no pinned certificate for %s", ip)
	}
	if !cached.Verified {
		return fmt.Errorf("certificate not verified for %s", ip)
	}
	if fingerprint != cached.Fingerprint {
		go m.recordCertChange(cached.DeviceID, fingerprint, rawCerts[0])
		return fmt.Errorf("certificate changed for %s", ip)
	}
	return nil
}

// verifyTOFU allows connection but tracks changes
func (m *Manager) verifyTOFU(deviceID int64, rawCerts [][]byte) error {
	if len(rawCerts) == 0 {
		return fmt.Errorf("no certificate presented")
	}

	fp := sha256.Sum256(rawCerts[0])
	fingerprint := hex.EncodeToString(fp[:])

	m.mu.RLock()
	cached, ok := m.byDeviceID[deviceID]
	m.mu.RUnlock()

	if ok && fingerprint != cached.Fingerprint {
		go m.recordCertChange(deviceID, fingerprint, rawCerts[0])
	}
	return nil // TOFU allows connection
}

// verifyTOFUByIP verifies by IP, auto-pins on first use
func (m *Manager) verifyTOFUByIP(ip string, rawCerts [][]byte) error {
	if len(rawCerts) == 0 {
		return fmt.Errorf("no certificate presented")
	}

	fp := sha256.Sum256(rawCerts[0])
	fingerprint := hex.EncodeToString(fp[:])

	m.mu.RLock()
	cached, ok := m.byIP[ip]
	deviceID := m.deviceIDByIP[ip]
	m.mu.RUnlock()

	if ok {
		if fingerprint != cached.Fingerprint {
			go m.recordCertChange(cached.DeviceID, fingerprint, rawCerts[0])
		}
		return nil
	}

	// First use - try to auto-pin if we have a device ID
	if deviceID > 0 {
		go m.autoPinCert(deviceID, fingerprint, rawCerts[0])
	}
	return nil
}

// recordCertChange records when a certificate changes
func (m *Manager) recordCertChange(deviceID int64, newFP string, rawCert []byte) {
	if deviceID == 0 {
		return
	}

	cert, err := x509.ParseCertificate(rawCert)
	if err != nil {
		return
	}

	_, err = m.db.Exec(`
		UPDATE device_certs SET
			previous_fingerprint = fingerprint,
			fingerprint = $2,
			subject = $3,
			issuer = $4,
			not_before = $5,
			not_after = $6,
			changed_at = NOW(),
			verified = false,
			verified_at = NULL,
			verified_by = NULL
		WHERE device_id = $1
	`, deviceID, newFP, cert.Subject.String(), cert.Issuer.String(),
		cert.NotBefore, cert.NotAfter)

	if err != nil {
		log.Printf("Failed to record cert change for device %d: %v", deviceID, err)
		return
	}

	m.mu.Lock()
	if cached, ok := m.byDeviceID[deviceID]; ok {
		cached.Fingerprint = newFP
		cached.Verified = false
	}
	m.mu.Unlock()

	log.Printf("Device %d: certificate changed to %s...", deviceID, newFP[:16])
}

// autoPinCert pins a certificate automatically (TOFU mode)
func (m *Manager) autoPinCert(deviceID int64, fingerprint string, rawCert []byte) {
	cert, err := x509.ParseCertificate(rawCert)
	if err != nil {
		return
	}

	_, err = m.db.Exec(`
		INSERT INTO device_certs (device_id, fingerprint, subject, issuer, not_before, not_after, verified)
		VALUES ($1, $2, $3, $4, $5, $6, false)
		ON CONFLICT (device_id) DO NOTHING
	`, deviceID, fingerprint, cert.Subject.String(), cert.Issuer.String(),
		cert.NotBefore, cert.NotAfter)

	if err != nil {
		log.Printf("Failed to auto-pin cert for device %d: %v", deviceID, err)
		return
	}

	m.mu.Lock()
	m.byDeviceID[deviceID] = &cachedCert{
		Fingerprint: fingerprint,
		Verified:    false,
		DeviceID:    deviceID,
	}
	m.mu.Unlock()

	log.Printf("Device %d: auto-pinned certificate %s...", deviceID, fingerprint[:16])
}

// PinCertFromDevice connects to a device (TLS on :443) and stores its leaf certificate.
//
// If verify is false, the cert is pinned but left unverified so it can be reviewed later.
func (m *Manager) PinCertFromDevice(deviceID int64, ip string, userID int, verify bool) (*CertInfo, error) {
	conn, err := tls.DialWithDialer(
		&net.Dialer{Timeout: 10 * time.Second},
		"tcp", ip+":443",
		&tls.Config{InsecureSkipVerify: true},
	)
	if err != nil {
		return nil, fmt.Errorf("connect failed: %w", err)
	}
	defer conn.Close()

	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return nil, fmt.Errorf("no certificate presented")
	}

	cert := certs[0]
	fp := sha256.Sum256(cert.Raw)
	fingerprint := hex.EncodeToString(fp[:])
	now := time.Now()
	var verifiedAt interface{} = nil
	var verifiedBy interface{} = nil
	if verify {
		verifiedAt = now
		verifiedBy = userID
	}

	_, err = m.db.Exec(`
		INSERT INTO device_certs (
			device_id, fingerprint, subject, issuer, not_before, not_after,
			pinned_at, pinned_by,
			verified, verified_at, verified_by
		) VALUES (
			$1, $2, $3, $4, $5, $6,
			$7, $8,
			$9, $10, $11
		)
		ON CONFLICT (device_id) DO UPDATE SET
			previous_fingerprint = CASE
				WHEN $9 THEN NULL
				WHEN device_certs.fingerprint IS NOT NULL AND device_certs.fingerprint <> EXCLUDED.fingerprint THEN device_certs.fingerprint
				ELSE device_certs.previous_fingerprint
			END,
			changed_at = CASE
				WHEN $9 THEN NULL
				WHEN device_certs.fingerprint IS NOT NULL AND device_certs.fingerprint <> EXCLUDED.fingerprint THEN EXCLUDED.pinned_at
				ELSE device_certs.changed_at
			END,
			fingerprint = EXCLUDED.fingerprint,
			subject = EXCLUDED.subject,
			issuer = EXCLUDED.issuer,
			not_before = EXCLUDED.not_before,
			not_after = EXCLUDED.not_after,
			pinned_at = EXCLUDED.pinned_at,
			pinned_by = EXCLUDED.pinned_by,
			verified = EXCLUDED.verified,
			verified_at = EXCLUDED.verified_at,
			verified_by = EXCLUDED.verified_by
	`, deviceID, fingerprint, cert.Subject.String(), cert.Issuer.String(),
		cert.NotBefore, cert.NotAfter,
		now, userID,
		verify, verifiedAt, verifiedBy)

	if err != nil {
		return nil, fmt.Errorf("failed to pin: %w", err)
	}

	m.mu.Lock()
	m.byDeviceID[deviceID] = &cachedCert{
		Fingerprint: fingerprint,
		Verified:    verify,
		DeviceID:    deviceID,
	}
	m.byIP[ip] = m.byDeviceID[deviceID]
	m.deviceIDByIP[ip] = deviceID
	m.mu.Unlock()

	log.Printf("Device %d: pinned certificate %s... by user %d (verified=%t)", deviceID, fingerprint[:16], userID, verify)

	var vAt *time.Time
	if verify {
		vAt = &now
	}
	vBy := 0
	if verify {
		vBy = userID
	}

	return &CertInfo{
		DeviceID:    deviceID,
		Fingerprint: fingerprint,
		Subject:     cert.Subject.String(),
		Issuer:      cert.Issuer.String(),
		NotBefore:   cert.NotBefore,
		NotAfter:    cert.NotAfter,
		PinnedAt:    now,
		PinnedBy:    userID,
		Verified:    verify,
		VerifiedAt:  vAt,
		VerifiedBy:  vBy,
		CertValid:   time.Now().Before(cert.NotAfter) && time.Now().After(cert.NotBefore),
	}, nil
}

// VerifyCert marks a certificate as verified
func (m *Manager) VerifyCert(deviceID int64, userID int) error {
	now := time.Now()
	result, err := m.db.Exec(`
		UPDATE device_certs SET
			verified = true,
			verified_at = $2,
			verified_by = $3,
			previous_fingerprint = NULL,
			changed_at = NULL
		WHERE device_id = $1
	`, deviceID, now, userID)

	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return fmt.Errorf("no certificate for device %d", deviceID)
	}

	m.mu.Lock()
	if cached, ok := m.byDeviceID[deviceID]; ok {
		cached.Verified = true
	}
	m.mu.Unlock()

	log.Printf("Device %d: certificate verified by user %d", deviceID, userID)
	return nil
}

// UnpinCert removes a pinned certificate
func (m *Manager) UnpinCert(deviceID int64) error {
	_, err := m.db.Exec(`DELETE FROM device_certs WHERE device_id = $1`, deviceID)
	if err != nil {
		return err
	}

	m.mu.Lock()
	delete(m.byDeviceID, deviceID)
	// Also remove from IP cache
	for ip, cached := range m.byIP {
		if cached.DeviceID == deviceID {
			delete(m.byIP, ip)
			delete(m.deviceIDByIP, ip)
			break
		}
	}
	m.mu.Unlock()

	return nil
}

// UnpinGroup unpins all certs for devices in a site
func (m *Manager) UnpinGroup(siteID int) (int, error) {
	// Get device IDs in site
	rows, err := m.db.Query(`SELECT id FROM devices WHERE site_id = $1`, siteID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var deviceIDs []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			deviceIDs = append(deviceIDs, id)
		}
	}

	// Delete certs for all devices in site
	result, err := m.db.Exec(`DELETE FROM device_certs WHERE device_id IN (SELECT id FROM devices WHERE site_id = $1)`, siteID)
	if err != nil {
		return 0, err
	}

	count, _ := result.RowsAffected()

	// Clear from cache
	m.mu.Lock()
	for _, deviceID := range deviceIDs {
		delete(m.byDeviceID, deviceID)
		for ip, cached := range m.byIP {
			if cached.DeviceID == deviceID {
				delete(m.byIP, ip)
				delete(m.deviceIDByIP, ip)
			}
		}
	}
	m.mu.Unlock()

	return int(count), nil
}

// RegisterDeviceIP associates an IP with a device ID in the cache
func (m *Manager) RegisterDeviceIP(ip string, deviceID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.deviceIDByIP[ip] = deviceID
	if cached, ok := m.byDeviceID[deviceID]; ok {
		m.byIP[ip] = cached
	}
}

// IsPinned checks if device has a pinned certificate
func (m *Manager) IsPinned(deviceID int64) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.byDeviceID[deviceID]
	return ok
}

// IsPinnedByIP checks if IP has a pinned certificate
func (m *Manager) IsPinnedByIP(ip string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.byIP[ip]
	return ok
}

// IsVerified checks if device cert is verified
func (m *Manager) IsVerified(deviceID int64) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if cached, ok := m.byDeviceID[deviceID]; ok {
		return cached.Verified
	}
	return false
}

// BulkLearnCerts learns certificates from all devices
func (m *Manager) BulkLearnCerts(userID int, verifyImmediately bool) (learned, failed int, errors []string) {
	// Learn *missing* certificates from devices we contact directly.
	//
	// "Direct" means either:
	//   - the device is a root device (no parent_id), or
	//   - the device was explicitly added/managed (managed = true)
	//
	// "VerifyImmediately" controls whether newly learned certs are marked verified.
	rows, err := m.db.Query(`
		SELECT d.id, host(d.ip_address) AS ip_address, d.hostname
		FROM devices d
		LEFT JOIN device_certs dc ON d.id = dc.device_id
		WHERE d.ip_address IS NOT NULL
			AND (d.parent_id IS NULL OR d.managed = TRUE)
			AND dc.id IS NULL
	`)
	if err != nil {
		return 0, 0, []string{fmt.Sprintf("query failed: %v", err)}
	}
	defer rows.Close()

	type device struct {
		id       int64
		ip       string
		hostname string
	}
	var devices []device
	for rows.Next() {
		var d device
		if rows.Scan(&d.id, &d.ip, &d.hostname) == nil {
			devices = append(devices, d)
		}
	}

	sem := make(chan struct{}, 10)
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, d := range devices {
		wg.Add(1)
		go func(dev device) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			// If the device appears to be http-only (80 open, 443 closed), do not
			// spend time attempting a TLS handshake. This also avoids creating
			// misleading "missing cert" work for devices that simply don't serve https.
			if netutil.ResolveScheme(dev.ip, netutil.SchemeHint{Timeout: 500 * time.Millisecond}) == "http" {
				mu.Lock()
				failed++
				name := dev.hostname
				if name == "" {
					name = dev.ip
				}
				errors = append(errors, fmt.Sprintf("%s: http-only (port 443 closed) — skipping TLS pin", name))
				mu.Unlock()
				return
			}

			_, err := m.PinCertFromDevice(dev.id, dev.ip, userID, verifyImmediately)
			mu.Lock()
			if err != nil {
				failed++
				name := dev.hostname
				if name == "" {
					name = dev.ip
				}
				errors = append(errors, fmt.Sprintf("%s: %v", name, err))
			} else {
				learned++
			}
			mu.Unlock()
		}(d)
	}

	wg.Wait()
	log.Printf("Bulk cert learn: %d learned, %d failed", learned, failed)
	return
}

// BulkVerifyCerts verifies all pending certificates
func (m *Manager) BulkVerifyCerts(userID int) (int, error) {
	now := time.Now()
	result, err := m.db.Exec(`
		UPDATE device_certs SET
			verified = true,
			verified_at = $1,
			verified_by = $2,
			previous_fingerprint = NULL,
			changed_at = NULL
		WHERE verified = false OR changed_at IS NOT NULL
	`, now, userID)

	if err != nil {
		return 0, err
	}

	affected, _ := result.RowsAffected()
	m.loadCerts() // Reload cache

	log.Printf("Bulk verified %d certificates by user %d", affected, userID)
	return int(affected), nil
}

// GetPendingCerts returns certificates needing verification
func (m *Manager) GetPendingCerts() ([]CertInfo, error) {
	return m.queryCerts(`
		WHERE dc.verified = false OR dc.changed_at IS NOT NULL
		ORDER BY dc.changed_at DESC NULLS LAST, dc.pinned_at DESC
	`)
}

// GetAllCerts returns all certificates
func (m *Manager) GetAllCerts() ([]CertInfo, error) {
	return m.queryCerts(`ORDER BY d.hostname, host(d.ip_address)`)
}

func (m *Manager) queryCerts(whereOrder string) ([]CertInfo, error) {
	rows, err := m.db.Query(`
		SELECT dc.device_id, dc.fingerprint, dc.subject, dc.issuer, 
		       dc.not_before, dc.not_after, dc.pinned_at, dc.pinned_by,
		       dc.verified, dc.verified_at, dc.verified_by,
		       dc.previous_fingerprint, dc.changed_at,
		       d.hostname, host(d.ip_address) AS ip_address
		FROM device_certs dc
		JOIN devices d ON d.id = dc.device_id
	` + whereOrder)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var certs []CertInfo
	for rows.Next() {
		var c CertInfo
		var notBefore, notAfter, pinnedAt, verifiedAt, changedAt sql.NullTime
		var pinnedBy, verifiedBy sql.NullInt64
		var prevFP, hostname, ip sql.NullString

		err := rows.Scan(&c.DeviceID, &c.Fingerprint, &c.Subject, &c.Issuer,
			&notBefore, &notAfter, &pinnedAt, &pinnedBy,
			&c.Verified, &verifiedAt, &verifiedBy,
			&prevFP, &changedAt, &hostname, &ip)
		if err != nil {
			continue
		}

		if notBefore.Valid {
			c.NotBefore = notBefore.Time
		}
		if notAfter.Valid {
			c.NotAfter = notAfter.Time
			c.CertValid = time.Now().Before(notAfter.Time) && time.Now().After(notBefore.Time)
		}
		if pinnedAt.Valid {
			c.PinnedAt = pinnedAt.Time
		}
		if pinnedBy.Valid {
			c.PinnedBy = int(pinnedBy.Int64)
		}
		if verifiedAt.Valid {
			c.VerifiedAt = &verifiedAt.Time
		}
		if verifiedBy.Valid {
			c.VerifiedBy = int(verifiedBy.Int64)
		}
		if prevFP.Valid {
			c.PreviousFingerprint = prevFP.String
		}
		if changedAt.Valid {
			c.ChangedAt = &changedAt.Time
		}
		if hostname.Valid {
			c.Hostname = hostname.String
		}
		if ip.Valid {
			c.IP = ip.String
		}

		certs = append(certs, c)
	}
	return certs, nil
}

// GetCertInfo returns certificate info for a device
func (m *Manager) GetCertInfo(deviceID int64) (*CertInfo, error) {
	var c CertInfo
	var notBefore, notAfter, pinnedAt, verifiedAt, changedAt sql.NullTime
	var pinnedBy, verifiedBy sql.NullInt64
	var prevFP sql.NullString

	err := m.db.QueryRow(`
		SELECT fingerprint, subject, issuer, not_before, not_after, 
		       pinned_at, pinned_by, verified, verified_at, verified_by,
		       previous_fingerprint, changed_at
		FROM device_certs WHERE device_id = $1
	`, deviceID).Scan(&c.Fingerprint, &c.Subject, &c.Issuer,
		&notBefore, &notAfter, &pinnedAt, &pinnedBy,
		&c.Verified, &verifiedAt, &verifiedBy, &prevFP, &changedAt)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	c.DeviceID = deviceID
	if notBefore.Valid {
		c.NotBefore = notBefore.Time
	}
	if notAfter.Valid {
		c.NotAfter = notAfter.Time
		c.CertValid = time.Now().Before(notAfter.Time) && time.Now().After(notBefore.Time)
	}
	if pinnedAt.Valid {
		c.PinnedAt = pinnedAt.Time
	}
	if pinnedBy.Valid {
		c.PinnedBy = int(pinnedBy.Int64)
	}
	if verifiedAt.Valid {
		c.VerifiedAt = &verifiedAt.Time
	}
	if verifiedBy.Valid {
		c.VerifiedBy = int(verifiedBy.Int64)
	}
	if prevFP.Valid {
		c.PreviousFingerprint = prevFP.String
	}
	if changedAt.Valid {
		c.ChangedAt = &changedAt.Time
	}

	return &c, nil
}

// GetCurrentCert fetches current certificate from device (without pinning)
func (m *Manager) GetCurrentCert(ip string) (*CertInfo, error) {
	conn, err := tls.DialWithDialer(
		&net.Dialer{Timeout: 10 * time.Second},
		"tcp", ip+":443",
		&tls.Config{InsecureSkipVerify: true},
	)
	if err != nil {
		return nil, fmt.Errorf("connect failed: %w", err)
	}
	defer conn.Close()

	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return nil, fmt.Errorf("no certificate")
	}

	cert := certs[0]
	fp := sha256.Sum256(cert.Raw)

	return &CertInfo{
		Fingerprint: hex.EncodeToString(fp[:]),
		Subject:     cert.Subject.String(),
		Issuer:      cert.Issuer.String(),
		NotBefore:   cert.NotBefore,
		NotAfter:    cert.NotAfter,
		CertValid:   time.Now().Before(cert.NotAfter) && time.Now().After(cert.NotBefore),
	}, nil
}

// Stats returns certificate statistics
func (m *Manager) Stats() (total, verified, pending, changed, expired, noCert int) {
	// Only count devices that WaveControl contacts directly.
	//
	// "Direct" means either:
	//   - the device is a root device (AP), i.e. parent_id IS NULL, OR
	//   - the device was explicitly added for direct management (managed = TRUE).
	//
	// Note: columns are based on schema.sql / migrations (device_certs.not_after, device_certs.changed_at).
	if err := m.db.QueryRow(`
		SELECT
			COUNT(*) AS total,
			COUNT(*) FILTER (WHERE dc.verified = TRUE) AS verified,
			COUNT(*) FILTER (WHERE dc.verified = FALSE AND dc.changed_at IS NULL) AS pending,
			COUNT(*) FILTER (WHERE dc.changed_at IS NOT NULL) AS changed,
			COUNT(*) FILTER (WHERE dc.not_after IS NOT NULL AND dc.not_after < NOW()) AS expired
		FROM device_certs dc
		JOIN devices d ON d.id = dc.device_id
		WHERE d.ip_address IS NOT NULL
		  AND (d.parent_id IS NULL OR d.managed = TRUE)
	`).Scan(&total, &verified, &pending, &changed, &expired); err != nil {
		log.Printf("tlsutil.Manager.Stats: query device_certs counts failed: %v", err)
	}

	// Devices without cert count (directly contacted devices without a pinned cert)
	if err := m.db.QueryRow(`
		SELECT COUNT(*)
		FROM devices d
		LEFT JOIN device_certs dc ON dc.device_id = d.id
		WHERE d.ip_address IS NOT NULL
		  AND (d.parent_id IS NULL OR d.managed = TRUE)
		  AND dc.device_id IS NULL
	`).Scan(&noCert); err != nil {
		log.Printf("tlsutil.Manager.Stats: query devices without cert failed: %v", err)
	}

	return
}

// Reload refreshes cache from database
func (m *Manager) Reload() {
	m.loadMode()
	m.loadCerts()
}

// RequiresPinnedCert returns true if mode requires pinned cert before sending credentials
func (m *Manager) RequiresPinnedCert() bool {
	return m.mode == ModeStrict
}

// CanSendCredentials checks if it's safe to send credentials to this device
func (m *Manager) CanSendCredentials(deviceID int64) bool {
	if m.mode == ModeInsecure {
		return true
	}
	m.mu.RLock()
	cached, ok := m.byDeviceID[deviceID]
	m.mu.RUnlock()

	if m.mode == ModeStrict {
		return ok && cached.Verified
	}
	// TOFU - allow if pinned (even if not verified)
	return ok
}

// CanSendCredentialsByIP checks if safe to send credentials to IP
func (m *Manager) CanSendCredentialsByIP(ip string) bool {
	if m.mode == ModeInsecure {
		return true
	}
	m.mu.RLock()
	cached, ok := m.byIP[ip]
	m.mu.RUnlock()

	if m.mode == ModeStrict {
		return ok && cached.Verified
	}
	return ok
}
