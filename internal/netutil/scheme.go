package netutil

import (
	"net"
	"strconv"
	"time"
)

// ProbeScheme returns "https" if the device appears to be listening on TCP/443,
// otherwise "http" if the device appears to be listening on TCP/80.
//
// If neither port appears reachable within the timeout, it falls back to "https".
//
// This is intentionally lightweight and does not attempt to validate TLS.
func ProbeScheme(ip string, timeout time.Duration) string {
	if ip == "" {
		return "https"
	}
	if dialTCP(ip, 443, timeout) {
		return "https"
	}
	if dialTCP(ip, 80, timeout) {
		return "http"
	}
	return "https"
}

func dialTCP(host string, port int, timeout time.Duration) bool {
	if timeout <= 0 {
		timeout = 500 * time.Millisecond
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
