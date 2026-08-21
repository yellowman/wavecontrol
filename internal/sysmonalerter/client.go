package sysmonalerter

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	DefaultPort      = 1347
	maxNameRunes     = 64
	maxApplication   = 128
	maxAlertText     = 512
	maxReplyBytes    = 4096
	handshakeTimeout = 20 * time.Second
	commandTimeout   = 20 * time.Second
	keepaliveAfter   = 55 * time.Second
	reconnectInitial = 5 * time.Second
	reconnectMaximum = time.Minute
	maintenanceTick  = 5 * time.Second
)

var (
	protocolNamePattern     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	errConfigurationChanged = errors.New("sysmon-web alerter configuration changed")
)

// Config describes WaveControl's identity and pinned TLS connection to the
// sysmon-web agent listener. CAPEM is the public certificate (or issuing CA)
// exported as aggregator-ca.pem by sysmon-web.
type Config struct {
	Enabled     bool   `json:"enabled"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Name        string `json:"name"`
	Token       string `json:"-"`
	Application string `json:"application"`
	CAPEM       string `json:"-"`
}

// Status is safe to expose through the WaveControl API. It intentionally omits
// the bearer token and pinned certificate contents.
type Status struct {
	Enabled         bool       `json:"enabled"`
	Configured      bool       `json:"configured"`
	Connected       bool       `json:"connected"`
	Address         string     `json:"address,omitempty"`
	Name            string     `json:"name,omitempty"`
	Application     string     `json:"application,omitempty"`
	LastError       string     `json:"last_error,omitempty"`
	LastConnectedAt *time.Time `json:"last_connected_at,omitempty"`
	LastActivityAt  *time.Time `json:"last_activity_at,omitempty"`
	LastTestAt      *time.Time `json:"last_test_at,omitempty"`
	LastTestOK      bool       `json:"last_test_ok"`
}

type session struct {
	conn   net.Conn
	reader *bufio.Reader
}

// Client keeps one authenticated, long-lived alerter connection. opMu
// serializes the request/reply wire protocol, while mu protects only in-memory
// configuration and status. Network timeouts therefore never block Status or
// UpdateConfig.
type Client struct {
	mu   sync.RWMutex
	opMu sync.Mutex

	cfg        Config
	generation uint64
	session    *session

	lastError       string
	lastConnectedAt time.Time
	lastActivityAt  time.Time
	lastTestAt      time.Time
	lastTestOK      bool
	wake            chan struct{}
}

func NewClient(cfg Config) (*Client, error) {
	cfg = normalizeConfig(cfg)
	if cfg.Enabled {
		if err := ValidateConfig(cfg); err != nil {
			return nil, err
		}
	}
	return &Client{cfg: cfg, wake: make(chan struct{}, 1)}, nil
}

func normalizeConfig(cfg Config) Config {
	cfg.Host = strings.TrimSpace(cfg.Host)
	cfg.Name = strings.TrimSpace(cfg.Name)
	cfg.Token = strings.TrimSpace(cfg.Token)
	cfg.Application = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || unicode.IsControl(r) {
			return ' '
		}
		return r
	}, cfg.Application))
	cfg.Application = strings.Join(strings.Fields(cfg.Application), " ")
	cfg.CAPEM = strings.TrimSpace(cfg.CAPEM)
	if cfg.Port == 0 {
		cfg.Port = DefaultPort
	}
	return cfg
}

// ValidateConfig validates all fields required to authenticate and verify a
// sysmon-web alerter connection. It is intentionally strict: TLS verification
// may not be disabled and the bearer token may not contain protocol whitespace.
func ValidateConfig(cfg Config) error {
	cfg = normalizeConfig(cfg)
	if err := validateHost(cfg.Host); err != nil {
		return err
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		return errors.New("sysmon-web agent port must be between 1 and 65535")
	}
	if !protocolNamePattern.MatchString(cfg.Name) {
		return errors.New("alerter name must use only letters, digits, '-' or '_' and be 1-64 characters")
	}
	if cfg.Token == "" || len(cfg.Token) > 4096 || strings.ContainsAny(cfg.Token, " \t\r\n") {
		return errors.New("alerter token is required and may not contain whitespace")
	}
	if len([]rune(cfg.Application)) > maxApplication {
		return fmt.Errorf("application name must be at most %d characters", maxApplication)
	}
	return ValidateCAPEM(cfg.CAPEM)
}

// ValidateHost accepts a hostname or IP address without a port. IPv6 may be
// bracketed for operator convenience; Address normalizes it before dialing.
func ValidateHost(host string) error {
	return validateHost(host)
}

func validateHost(host string) error {
	host = strings.TrimSpace(host)
	if host == "" || len(host) > 253 || strings.ContainsAny(host, " /\\\t\r\n") {
		return errors.New("sysmon-web host is required and must be a hostname or IP address without a port")
	}
	unbracketed := host
	if strings.HasPrefix(host, "[") || strings.HasSuffix(host, "]") {
		if !strings.HasPrefix(host, "[") || !strings.HasSuffix(host, "]") {
			return errors.New("sysmon-web host has mismatched IPv6 brackets")
		}
		unbracketed = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	}
	if net.ParseIP(unbracketed) != nil {
		return nil
	}
	if strings.Contains(host, ":") {
		return errors.New("sysmon-web host must not include a port")
	}
	return nil
}

// ValidateCAPEM accepts one or more certificates and rejects private-key
// material. The alerter needs only sysmon-web's public certificate/CA.
func ValidateCAPEM(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return errors.New("a valid sysmon-web CA certificate PEM is required")
	}
	rest := []byte(raw)
	certificates := 0
	for len(strings.TrimSpace(string(rest))) > 0 {
		block, remaining := pem.Decode(rest)
		if block == nil {
			return errors.New("sysmon-web CA PEM contains malformed or non-PEM data")
		}
		rest = remaining
		blockType := strings.ToUpper(strings.TrimSpace(block.Type))
		if strings.Contains(blockType, "PRIVATE KEY") {
			return errors.New("sysmon-web CA PEM must not contain a private key")
		}
		if blockType != "CERTIFICATE" {
			continue
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return fmt.Errorf("parse sysmon-web certificate: %w", err)
		}
		certificates++
	}
	if certificates == 0 {
		return errors.New("a valid sysmon-web CA certificate PEM is required")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(raw)) {
		return errors.New("a valid sysmon-web CA certificate PEM is required")
	}
	return nil
}

func (cfg Config) Address() string {
	host := strings.TrimPrefix(strings.TrimSuffix(strings.TrimSpace(cfg.Host), "]"), "[")
	if host == "" {
		return ""
	}
	return net.JoinHostPort(host, strconv.Itoa(cfg.Port))
}

func sameConfig(a, b Config) bool {
	return a.Enabled == b.Enabled && a.Host == b.Host && a.Port == b.Port && a.Name == b.Name &&
		a.Token == b.Token && a.Application == b.Application && a.CAPEM == b.CAPEM
}

// UpdateConfig applies settings without restarting WaveControl. A changed
// identity, token, endpoint, or trust anchor immediately closes the old socket;
// an in-flight command is interrupted and the maintenance loop reconnects with
// the new generation.
func (c *Client) UpdateConfig(cfg Config) error {
	cfg = normalizeConfig(cfg)
	if cfg.Enabled {
		if err := ValidateConfig(cfg); err != nil {
			return err
		}
	}
	c.mu.Lock()
	if sameConfig(c.cfg, cfg) {
		c.mu.Unlock()
		return nil
	}
	old := c.session
	c.session = nil
	c.cfg = cfg
	c.generation++
	c.lastError = ""
	c.mu.Unlock()
	if old != nil {
		_ = old.conn.Close()
	}
	c.signalWake()
	return nil
}

func (c *Client) signalWake() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *Client) Close() error {
	c.mu.Lock()
	old := c.session
	c.session = nil
	c.mu.Unlock()
	if old == nil {
		return nil
	}
	return old.conn.Close()
}

func (c *Client) Status() Status {
	c.mu.RLock()
	defer c.mu.RUnlock()
	status := Status{
		Enabled:     c.cfg.Enabled,
		Configured:  ValidateConfig(c.cfg) == nil,
		Connected:   c.session != nil,
		Address:     c.cfg.Address(),
		Name:        c.cfg.Name,
		Application: c.cfg.Application,
		LastError:   c.lastError,
		LastTestOK:  c.lastTestOK,
	}
	if !c.lastConnectedAt.IsZero() {
		t := c.lastConnectedAt
		status.LastConnectedAt = &t
	}
	if !c.lastActivityAt.IsZero() {
		t := c.lastActivityAt
		status.LastActivityAt = &t
	}
	if !c.lastTestAt.IsZero() {
		t := c.lastTestAt
		status.LastTestAt = &t
	}
	return status
}

func (c *Client) configSnapshot() (Config, uint64, *session, time.Time) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cfg, c.generation, c.session, c.lastActivityAt
}

// Run maintains an authenticated long-lived alerter session while the channel
// is enabled. Connection attempts start immediately, back off from five seconds
// to one minute, and are interrupted when a configured session is replaced.
// A successful connection is kept alive with protocol PINGs.
func (c *Client) Run(ctx context.Context) {
	backoff := reconnectInitial
	for {
		enabled, connected, err := c.maintain(ctx)
		wait := maintenanceTick
		if err != nil {
			wait = backoff
			backoff *= 2
			if backoff > reconnectMaximum {
				backoff = reconnectMaximum
			}
		} else if !enabled || connected {
			backoff = reconnectInitial
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			stopAndDrainTimer(timer)
			_ = c.Close()
			return
		case <-c.wake:
			stopAndDrainTimer(timer)
			backoff = reconnectInitial
		case <-timer.C:
		}
	}
}

func stopAndDrainTimer(timer *time.Timer) {
	if timer == nil || timer.Stop() {
		return
	}
	select {
	case <-timer.C:
	default:
	}
}

func (c *Client) maintain(ctx context.Context) (enabled, connected bool, err error) {
	cfg, _, active, lastActivity := c.configSnapshot()
	if !cfg.Enabled {
		_ = c.Close()
		return false, false, nil
	}
	if err := ValidateConfig(cfg); err != nil {
		c.setLastError(cfg, err)
		_ = c.Close()
		return true, false, err
	}

	c.opMu.Lock()
	defer c.opMu.Unlock()
	if active == nil {
		if err := c.ensureConnected(ctx, cfg); err != nil {
			return true, false, err
		}
		return true, true, nil
	}
	if time.Since(lastActivity) >= keepaliveAfter {
		if err := c.commandCurrent(ctx, "PING", "pong"); err != nil {
			return true, false, err
		}
	}
	return true, true, nil
}

// Test performs an isolated handshake, PING, and clean QUIT. It may be used
// before Enabled is switched on, but all connection fields must be complete.
// The maintained session is closed first so servers that enforce one active
// connection per alerter identity do not reject the test as a duplicate. The
// maintenance loop is then woken to reconnect immediately when enabled.
func (c *Client) Test(ctx context.Context) error {
	cfg, generation, _, _ := c.configSnapshot()
	if err := ValidateConfig(cfg); err != nil {
		c.recordTest(cfg, generation, err)
		return err
	}

	c.opMu.Lock()
	defer c.opMu.Unlock()
	c.closeCurrentGracefully(ctx)

	s, err := dialAndHandshake(ctx, cfg)
	if err == nil {
		err = command(ctx, s, "PING", "pong")
	}
	if err == nil {
		_ = command(ctx, s, "QUIT", "bye")
	}
	if s != nil && s.conn != nil {
		_ = s.conn.Close()
	}
	if !c.configGenerationMatches(cfg, generation) {
		err = errConfigurationChanged
	}
	c.recordTest(cfg, generation, err)
	return err
}

func (c *Client) recordTest(cfg Config, generation uint64, err error) {
	c.mu.Lock()
	if sameConfig(c.cfg, cfg) && c.generation == generation {
		c.lastTestAt = time.Now()
		c.lastTestOK = err == nil
		if err != nil {
			c.lastError = err.Error()
		} else {
			c.lastError = ""
		}
	}
	c.mu.Unlock()
	c.signalWake()
}

// Send emits one protocol ALERT and waits for sysmon-web's 333/444 reply.
// CRITICAL, WARNING, and OK are the only accepted statuses.
func (c *Client) Send(ctx context.Context, status, object, text string) error {
	status = strings.ToUpper(strings.TrimSpace(status))
	if status != "CRITICAL" && status != "WARNING" && status != "OK" {
		return fmt.Errorf("unsupported sysmon alert status %q", status)
	}
	object = strings.TrimSpace(object)
	if !protocolNamePattern.MatchString(object) {
		return errors.New("sysmon alert object must use only letters, digits, '-' or '_' and be 1-64 characters")
	}
	text = sanitizeText(text, maxAlertText)

	cfg, _, _, lastActivity := c.configSnapshot()
	if !cfg.Enabled {
		return errors.New("sysmon-web alerter delivery is disabled")
	}
	if err := ValidateConfig(cfg); err != nil {
		return err
	}

	c.opMu.Lock()
	defer c.opMu.Unlock()
	if err := c.ensureConnected(ctx, cfg); err != nil {
		return err
	}
	if !lastActivity.IsZero() && time.Since(lastActivity) >= keepaliveAfter {
		if err := c.commandCurrent(ctx, "PING", "pong"); err != nil {
			if reconnectErr := c.ensureConnected(ctx, cfg); reconnectErr != nil {
				return reconnectErr
			}
		}
	}
	line := "ALERT " + status + " " + object
	if text != "" {
		line += " " + text
	}
	return c.commandCurrent(ctx, line, "ok")
}

// ensureConnected is called with opMu held. It performs network I/O without
// holding mu, then installs the socket only if the configuration generation is
// still current.
func (c *Client) ensureConnected(ctx context.Context, expected Config) error {
	cfg, generation, active, _ := c.configSnapshot()
	if active != nil {
		if !sameConfig(cfg, expected) {
			return errConfigurationChanged
		}
		return nil
	}
	if !sameConfig(cfg, expected) || !cfg.Enabled {
		return errConfigurationChanged
	}
	s, err := dialAndHandshake(ctx, cfg)
	if err != nil {
		c.setLastError(cfg, err)
		return err
	}

	c.mu.Lock()
	if c.generation != generation || !sameConfig(c.cfg, cfg) || !c.cfg.Enabled {
		c.mu.Unlock()
		_ = s.conn.Close()
		return errConfigurationChanged
	}
	if c.session != nil {
		c.mu.Unlock()
		_ = s.conn.Close()
		return nil
	}
	now := time.Now()
	c.session = s
	c.lastConnectedAt = now
	c.lastActivityAt = now
	c.lastError = ""
	c.mu.Unlock()
	return nil
}

func (c *Client) commandCurrent(ctx context.Context, line, expected string) error {
	c.mu.RLock()
	s := c.session
	c.mu.RUnlock()
	if s == nil {
		return errors.New("sysmon-web connection is not established")
	}
	err := command(ctx, s, line, expected)
	var closeConn net.Conn
	c.mu.Lock()
	if c.session == s {
		if err == nil {
			c.lastActivityAt = time.Now()
			c.lastError = ""
		} else {
			c.lastError = err.Error()
			var refusal *RefusalError
			if !errors.As(err, &refusal) {
				c.session = nil
				closeConn = s.conn
			}
		}
	}
	c.mu.Unlock()
	if closeConn != nil {
		_ = closeConn.Close()
	}
	return err
}

func (c *Client) closeCurrentGracefully(ctx context.Context) {
	c.mu.RLock()
	s := c.session
	c.mu.RUnlock()
	if s == nil {
		return
	}
	_ = c.commandCurrent(ctx, "QUIT", "bye")
	c.mu.Lock()
	if c.session == s {
		c.session = nil
	}
	c.mu.Unlock()
	_ = s.conn.Close()
}

func (c *Client) setLastError(cfg Config, err error) {
	if err == nil {
		return
	}
	c.mu.Lock()
	if sameConfig(c.cfg, cfg) {
		c.lastError = err.Error()
	}
	c.mu.Unlock()
}

func (c *Client) configGenerationMatches(cfg Config, generation uint64) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.generation == generation && sameConfig(c.cfg, cfg)
}

// RefusalError represents a syntactically valid 444 response. The protocol
// keeps the connection open after an ALERT refusal.
type RefusalError struct{ Reason string }

func (e *RefusalError) Error() string { return "sysmon-web refused command: " + e.Reason }

func dialAndHandshake(parent context.Context, cfg Config) (*session, error) {
	if err := ValidateConfig(cfg); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(parent, handshakeTimeout)
	defer cancel()
	tlsCfg, err := tlsConfig(cfg)
	if err != nil {
		return nil, err
	}
	dialer := tls.Dialer{NetDialer: &net.Dialer{Timeout: handshakeTimeout, KeepAlive: 30 * time.Second}, Config: tlsCfg}
	conn, err := dialer.DialContext(ctx, "tcp", cfg.Address())
	if err != nil {
		return nil, fmt.Errorf("connect to sysmon-web: %w", err)
	}
	s := &session{conn: conn, reader: bufio.NewReaderSize(conn, maxReplyBytes)}
	greeting := "ALERTER " + cfg.Name + " " + cfg.Token
	if cfg.Application != "" {
		greeting += " " + cfg.Application
	}
	if err := command(ctx, s, greeting, "welcome"); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("sysmon-web alerter handshake: %w", err)
	}
	return s, nil
}

func tlsConfig(cfg Config) (*tls.Config, error) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(cfg.CAPEM)) {
		return nil, errors.New("sysmon-web CA certificate PEM could not be parsed")
	}
	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, // Replaced below with chain verification that intentionally omits DNS-name matching.
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return errors.New("sysmon-web did not present a certificate")
			}
			intermediates := x509.NewCertPool()
			for _, cert := range state.PeerCertificates[1:] {
				intermediates.AddCert(cert)
			}
			_, err := state.PeerCertificates[0].Verify(x509.VerifyOptions{
				Roots:         roots,
				Intermediates: intermediates,
				KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			})
			if err != nil {
				return fmt.Errorf("verify pinned sysmon-web certificate: %w", err)
			}
			return nil
		},
	}, nil
}

func command(parent context.Context, s *session, line, expected string) error {
	ctx, cancel := context.WithTimeout(parent, commandTimeout)
	defer cancel()
	if deadline, ok := ctx.Deadline(); ok {
		_ = s.conn.SetDeadline(deadline)
		defer s.conn.SetDeadline(time.Time{})
	}
	if _, err := fmt.Fprintf(s.conn, "%s\n", line); err != nil {
		return fmt.Errorf("write command: %w", err)
	}
	code, message, err := readReply(s.reader)
	if err != nil {
		return err
	}
	if code == 444 {
		if message == "" {
			message = "rejected"
		}
		return &RefusalError{Reason: message}
	}
	if code != 333 {
		return fmt.Errorf("unexpected sysmon-web reply code %d", code)
	}
	if expected != "" && !strings.EqualFold(strings.TrimSpace(message), expected) {
		return fmt.Errorf("unexpected sysmon-web reply %q", message)
	}
	return nil
}

func readReply(reader *bufio.Reader) (int, string, error) {
	lineBytes := make([]byte, 0, min(maxReplyBytes, reader.Size()))
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(lineBytes)+len(fragment) > maxReplyBytes {
			return 0, "", errors.New("sysmon-web reply is too long")
		}
		lineBytes = append(lineBytes, fragment...)
		if err == nil {
			break
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return 0, "", fmt.Errorf("read reply: %w", err)
	}
	line := string(lineBytes)
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	if len(line) < 3 {
		return 0, "", fmt.Errorf("malformed sysmon-web reply %q", line)
	}
	code, err := strconv.Atoi(line[:3])
	if err != nil || (len(line) > 3 && line[3] != ' ') {
		return 0, "", fmt.Errorf("malformed sysmon-web reply %q", line)
	}
	message := ""
	if len(line) > 4 {
		message = strings.TrimSpace(line[4:])
	}
	return code, message, nil
}

func sanitizeText(value string, maxRunes int) string {
	value = strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) > maxRunes {
		runes = runes[:maxRunes]
	}
	return string(runes)
}

// ValidProtocolName is useful to callers constructing stable alert objects.
func ValidProtocolName(value string) bool { return protocolNamePattern.MatchString(value) }

// SanitizeAlertText applies the protocol's one-line, 512-character limit.
func SanitizeAlertText(value string) string { return sanitizeText(value, maxAlertText) }
