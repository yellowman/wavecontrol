package poller

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"syscall"
)

// classifyPollError returns whether the device was unreachable (i.e., did not respond at all)
// and a stable, short reason string suitable for status_reason.
//
// IMPORTANT semantics:
//   - unreachable=true means "we could not talk to the device" (timeout / no route / DNS, etc.).
//     In most codepaths, this should *not* advance last_seen.
//   - unreachable=false means "the device responded in some way" (HTTP 401/403, TCP RST/REFUSED,
//     TLS/x509 errors, etc.). These should generally be treated as UNKNOWN, not OFFLINE.
func classifyPollError(err error) (unreachable bool, reason string) {
	if err == nil {
		return false, ""
	}

	// HTTP status errors mean we received a response.
	var hs *httpStatusError
	if errors.As(err, &hs) {
		switch hs.StatusCode {
		case 401:
			return false, "auth_401"
		case 403:
			return false, "auth_403"
		case 404:
			return false, "http_404"
		default:
			return false, fmt.Sprintf("http_%d", hs.StatusCode)
		}
	}

	// Context timeouts/cancellation.
	if errors.Is(err, context.DeadlineExceeded) {
		return true, "timeout"
	}
	if errors.Is(err, context.Canceled) {
		// Treat cancellations as "unreachable" (no response) so we don't incorrectly advance last_seen.
		return true, "canceled"
	}

	// DNS errors mean we never reached the device.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		if dnsErr.IsNotFound {
			return true, "dns_not_found"
		}
		if dnsErr.Timeout() {
			return true, "dns_timeout"
		}
		return true, "dns_error"
	}

	// x509 / TLS errors mean we reached something that spoke TLS (or at least responded), but
	// validation/handshake failed.
	var x509UA x509.UnknownAuthorityError
	if errors.As(err, &x509UA) {
		return false, "x509_unknown_authority"
	}
	var x509HN x509.HostnameError
	if errors.As(err, &x509HN) {
		return false, "x509_hostname"
	}
	var x509CI x509.CertificateInvalidError
	if errors.As(err, &x509CI) {
		return false, "x509_invalid"
	}
	var tlsRH tls.RecordHeaderError
	if errors.As(err, &tlsRH) {
		// Often indicates plain HTTP on an HTTPS port, or garbage on the socket.
		return false, "tls_record_header"
	}

	// url.Error frequently wraps net.OpError / syscall errno.
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		// url.Error implements net.Error in many cases.
		if urlErr.Timeout() {
			return true, "timeout"
		}
	}

	// net.Error timeout detection (covers many dial/read timeouts).
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return true, "timeout"
		}
	}

	// Syscall errno extraction (covers "no route", "host unreachable", "conn reset/refused", etc.).
	if errno, ok := extractSyscallErrno(err); ok {
		switch errno {
		case syscall.ETIMEDOUT:
			return true, "timeout"
		case syscall.EHOSTUNREACH, syscall.ENETUNREACH:
			return true, "unreachable"
		case syscall.EHOSTDOWN:
			return true, "host_down"
		case syscall.ENETDOWN:
			return true, "net_down"
		case syscall.ECONNREFUSED:
			return false, "conn_refused"
		case syscall.ECONNRESET:
			return false, "conn_reset"
		case syscall.ECONNABORTED:
			return false, "conn_aborted"
		case syscall.EPIPE:
			return false, "broken_pipe"
		case syscall.EADDRNOTAVAIL:
			// Local stack issue (rare) - treat as unreachable.
			return true, "addr_not_available"
		}
		// Unknown errno -> default to "error" but treat as reachable-ish to avoid false OFFLINE.
		return false, "syscall_error"
	}

	// EOF means a connection was made and then closed.
	if errors.Is(err, io.EOF) {
		return false, "eof"
	}

	// Final fallback: string heuristics for edge cases where types are lost.
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "no route to host") || strings.Contains(s, "network is unreachable") ||
		strings.Contains(s, "host unreachable") || strings.Contains(s, "host is down") {
		return true, "unreachable"
	}
	if strings.Contains(s, "i/o timeout") || strings.Contains(s, "timed out") ||
		strings.Contains(s, "timeout") || strings.Contains(s, "deadline exceeded") {
		return true, "timeout"
	}
	if strings.Contains(s, "connection refused") {
		return false, "conn_refused"
	}
	if strings.Contains(s, "connection reset") || strings.Contains(s, "reset by peer") {
		return false, "conn_reset"
	}
	if strings.Contains(s, "tls") && strings.Contains(s, "handshake") {
		return false, "tls_handshake"
	}
	if strings.Contains(s, "x509") {
		return false, "x509_error"
	}
	if strings.Contains(s, "unauthorized") || strings.Contains(s, "forbidden") {
		return false, "auth_failed"
	}
	if strings.Contains(s, "login") && strings.Contains(s, "fail") {
		return false, "auth_failed"
	}

	// Default: be conservative. Treat as reachable-ish (unknown), not unreachable.
	return false, "error"
}

func extractSyscallErrno(err error) (syscall.Errno, bool) {
	// Walk unwrap chain and look for syscall.Errno in common wrappers.
	for e := err; e != nil; e = errors.Unwrap(e) {
		switch v := e.(type) {
		case syscall.Errno:
			return v, true
		case *os.SyscallError:
			if errno, ok := v.Err.(syscall.Errno); ok {
				return errno, true
			}
			var errno syscall.Errno
			if errors.As(v.Err, &errno) {
				return errno, true
			}
		case *net.OpError:
			// net.OpError.Err frequently contains syscall.Errno or *os.SyscallError.
			if v.Err == nil {
				continue
			}
			if errno, ok := v.Err.(syscall.Errno); ok {
				return errno, true
			}
			if se, ok := v.Err.(*os.SyscallError); ok {
				if errno, ok := se.Err.(syscall.Errno); ok {
					return errno, true
				}
			}
			var errno syscall.Errno
			if errors.As(v.Err, &errno) {
				return errno, true
			}
		}
	}
	return 0, false
}

func isNetworkUnreachable(err error) bool {
	unreachable, _ := classifyPollError(err)
	return unreachable
}

func statusReasonFromError(err error) string {
	_, reason := classifyPollError(err)
	return reason
}
