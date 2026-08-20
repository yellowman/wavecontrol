package main

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

var tlsStateForTest = tls.ConnectionState{}

func TestTrustedProxyMiddlewareStripsUntrustedHeaders(t *testing.T) {
	h := trustedProxyMiddleware([]netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Forwarded-For"); got != "" {
			t.Errorf("X-Forwarded-For was not stripped: %q", got)
		}
		if got := r.Header.Get("X-Forwarded-Proto"); got != "" {
			t.Errorf("X-Forwarded-Proto was not stripped: %q", got)
		}
		if got := clientIPFromRequest(r); got != "203.0.113.10" {
			t.Errorf("client IP = %q, want direct peer", got)
		}
	}))

	req := httptest.NewRequest(http.MethodGet, "http://wavecontrol.test/", nil)
	req.RemoteAddr = "203.0.113.10:4321"
	req.Header.Set("X-Forwarded-For", "198.51.100.9")
	req.Header.Set("X-Forwarded-Proto", "https")
	h.ServeHTTP(httptest.NewRecorder(), req)
}

func TestTrustedProxyMiddlewareUsesTrustedEdgeValues(t *testing.T) {
	h := trustedProxyMiddleware([]netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := clientIPFromRequest(r); got != "198.51.100.9" {
			t.Errorf("client IP = %q, want 198.51.100.9", got)
		}
		if got := r.Header.Get("X-Forwarded-Proto"); got != "http" {
			t.Errorf("trusted proto = %q, want nearest proxy value http", got)
		}
	}))

	req := httptest.NewRequest(http.MethodGet, "http://wavecontrol.test/", nil)
	req.RemoteAddr = "10.0.0.2:4321"
	req.Header.Set("X-Forwarded-For", "198.51.100.9, 10.0.0.1")
	// A malicious client prefix must not override the value appended by the
	// nearest trusted proxy.
	req.Header.Set("X-Forwarded-Proto", "https, http")
	h.ServeHTTP(httptest.NewRecorder(), req)
}

func TestOriginGuardRequiresCSRFHeaderForCookieMutation(t *testing.T) {
	nextCalled := false
	h := originGuard(nil)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { nextCalled = true }))
	req := httptest.NewRequest(http.MethodPost, "https://wavecontrol.test/api", nil)
	req.Host = "wavecontrol.test"
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "token"})
	req.Header.Set("Origin", "https://wavecontrol.test")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden || nextCalled {
		t.Fatalf("status=%d nextCalled=%v, want 403/false", rr.Code, nextCalled)
	}
}

func TestOriginGuardRejectsUnlistedCrossOrigin(t *testing.T) {
	nextCalled := false
	h := originGuard(nil)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { nextCalled = true }))
	req := httptest.NewRequest(http.MethodPost, "https://wavecontrol.test/api", nil)
	req.Host = "wavecontrol.test"
	req.TLS = &tlsStateForTest
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "token"})
	req.Header.Set("X-WaveControl-CSRF", "1")
	req.Header.Set("Origin", "https://evil.example")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden || nextCalled {
		t.Fatalf("status=%d nextCalled=%v, want 403/false", rr.Code, nextCalled)
	}
}

func TestOriginGuardAllowsSameOriginMutation(t *testing.T) {
	nextCalled := false
	h := originGuard(nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodPost, "https://wavecontrol.test/api", nil)
	req.Host = "wavecontrol.test"
	req.TLS = &tlsStateForTest
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "token"})
	req.Header.Set("X-WaveControl-CSRF", "1")
	req.Header.Set("Origin", "https://wavecontrol.test")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent || !nextCalled {
		t.Fatalf("status=%d nextCalled=%v, want 204/true", rr.Code, nextCalled)
	}
}
