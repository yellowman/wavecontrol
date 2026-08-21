package sysmonalerter

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

type protocolServer struct {
	listener net.Listener
	caPEM    string

	mu       sync.Mutex
	commands []string
}

func newProtocolServer(t *testing.T) *protocolServer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "sysmon-web test"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	s := &protocolServer{listener: ln, caPEM: string(certPEM)}
	go s.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

func (s *protocolServer) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *protocolServer) handle(conn net.Conn) {
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		line := scanner.Text()
		s.mu.Lock()
		s.commands = append(s.commands, line)
		s.mu.Unlock()
		switch {
		case strings.HasPrefix(line, "ALERTER wavecontrol token-123 "):
			_, _ = conn.Write([]byte("333 welcome\n"))
		case line == "PING":
			_, _ = conn.Write([]byte("333 pong\n"))
		case strings.HasPrefix(line, "ALERT "):
			_, _ = conn.Write([]byte("333 ok\n"))
		case line == "QUIT":
			_, _ = conn.Write([]byte("333 bye\n"))
			return
		default:
			_, _ = conn.Write([]byte("444 malformed\n"))
		}
	}
}

func (s *protocolServer) config() Config {
	host, portRaw, _ := net.SplitHostPort(s.listener.Addr().String())
	port := 0
	_, _ = fmtSscanf(portRaw, &port)
	return Config{
		Enabled:     true,
		Host:        host,
		Port:        port,
		Name:        "wavecontrol",
		Token:       "token-123",
		Application: "WaveControl network alerts",
		CAPEM:       s.caPEM,
	}
}

// fmtSscanf is kept tiny so the test's intent stays on the protocol rather
// than importing strconv solely for one listener port.
func fmtSscanf(raw string, dst *int) (int, error) {
	var n int
	for _, r := range raw {
		if r < '0' || r > '9' {
			return 0, &net.AddrError{Err: "invalid port", Addr: raw}
		}
		n = n*10 + int(r-'0')
	}
	*dst = n
	return 1, nil
}

func TestClientSendAndTest(t *testing.T) {
	server := newProtocolServer(t)
	client, err := NewClient(server.config())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Send(ctx, "CRITICAL", "device-12-rule-7", "radio is offline\nsecond line"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := client.Send(ctx, "OK", "device-12-rule-7", "radio recovered"); err != nil {
		t.Fatalf("Send recovery: %v", err)
	}
	if err := client.Test(ctx); err != nil {
		t.Fatalf("Test: %v", err)
	}
	status := client.Status()
	// Test uses an isolated connection and deliberately leaves it closed; the
	// long-lived Run loop reconnects immediately in production when enabled.
	if !status.Configured || !status.LastTestOK {
		t.Fatalf("unexpected status: %+v", status)
	}

	server.mu.Lock()
	commands := append([]string(nil), server.commands...)
	server.mu.Unlock()
	joined := strings.Join(commands, "\n")
	for _, want := range []string{
		"ALERTER wavecontrol token-123 WaveControl network alerts",
		"ALERT CRITICAL device-12-rule-7 radio is offline second line",
		"ALERT OK device-12-rule-7 radio recovered",
		"PING",
		"QUIT",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("commands missing %q:\n%s", want, joined)
		}
	}
}

func TestClientRejectsUntrustedCertificate(t *testing.T) {
	server := newProtocolServer(t)
	other := newProtocolServer(t)
	cfg := server.config()
	cfg.CAPEM = other.caPEM
	client, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Send(ctx, "WARNING", "device-1-rule-1", "test"); err == nil || !strings.Contains(err.Error(), "verify pinned") {
		t.Fatalf("Send error = %v, want pinned-certificate failure", err)
	}
}

func TestValidationAndTextLimits(t *testing.T) {
	server := newProtocolServer(t)
	cfg := server.config()
	cfg.Name = "bad name"
	if err := ValidateConfig(cfg); err == nil {
		t.Fatal("ValidateConfig accepted invalid name")
	}
	cfg = server.config()
	cfg.Token = "token with spaces"
	if err := ValidateConfig(cfg); err == nil {
		t.Fatal("ValidateConfig accepted token whitespace")
	}
	if ValidProtocolName("bad:name") {
		t.Fatal("ValidProtocolName accepted punctuation")
	}
	long := strings.Repeat("x", 600)
	if got := []rune(SanitizeAlertText(long)); len(got) != maxAlertText {
		t.Fatalf("SanitizeAlertText length = %d, want %d", len(got), maxAlertText)
	}
}

func TestHostAndTrustValidation(t *testing.T) {
	for _, host := range []string{"sysmon-web.example.net", "127.0.0.1", "[2001:db8::1]"} {
		if err := ValidateHost(host); err != nil {
			t.Errorf("ValidateHost(%q): %v", host, err)
		}
	}
	for _, host := range []string{"", "example.net:1347", "http://example.net", "[2001:db8::1"} {
		if err := ValidateHost(host); err == nil {
			t.Errorf("ValidateHost(%q) succeeded, want error", host)
		}
	}
	if got := (Config{Port: DefaultPort}).Address(); got != "" {
		t.Fatalf("empty-host Address() = %q, want empty", got)
	}

	server := newProtocolServer(t)
	keyBlock := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("not a real private key")})
	if err := ValidateCAPEM(server.caPEM + string(keyBlock)); err == nil || !strings.Contains(err.Error(), "private key") {
		t.Fatalf("ValidateCAPEM(private key) = %v, want rejection", err)
	}
}

func TestRunConnectsAndMaintainsEnabledAlerter(t *testing.T) {
	server := newProtocolServer(t)
	client, err := NewClient(server.config())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		client.Run(ctx)
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for !client.Status().Connected && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if status := client.Status(); !status.Connected {
		cancel()
		<-done
		t.Fatalf("Run did not establish the enabled alerter session: %+v", status)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after context cancellation")
	}
	if client.Status().Connected {
		t.Fatal("client remained connected after Run stopped")
	}
}

func TestRunWakesWhenConfigurationIsEnabled(t *testing.T) {
	server := newProtocolServer(t)
	cfg := server.config()
	cfg.Enabled = false
	client, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		client.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	if client.Status().Connected {
		t.Fatal("disabled client connected")
	}
	cfg.Enabled = true
	if err := client.UpdateConfig(cfg); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !client.Status().Connected && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if status := client.Status(); !status.Connected {
		t.Fatalf("UpdateConfig did not wake the maintenance loop: %+v", status)
	}
}

func TestReadReplyRejectsOversizedLine(t *testing.T) {
	reader := bufio.NewReaderSize(strings.NewReader("333 "+strings.Repeat("x", maxReplyBytes)+"\n"), 64)
	if _, _, err := readReply(reader); err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("readReply oversized error = %v, want bounded-length rejection", err)
	}
}
