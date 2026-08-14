package udebug

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// DefaultCaptureLimit is the maximum number of bytes captured from request/response bodies per entry.
// (This is separate from the per-device ring buffer cap.)
const DefaultCaptureLimit int64 = 256 << 10 // 256KB

// Transport wraps an http.RoundTripper and records request/response details
// into the ultra debug buffer for a specific device ID.
//
// This is intended to be created per-device by poller/firmware code when
// ultra debug is enabled.
//
// It is safe for concurrent use as long as the underlying transport is.
//
// Notes on body capture:
//   - Firmware uploads and other binary/multipart payloads are NOT captured.
//   - Responses are captured up to captureLimit bytes (default 256KB). If the
//     response is larger, it is truncated in the log entry but the full body
//     is still returned to the caller.
//   - Request bodies are captured only when "safe" and small; otherwise a
//     placeholder is recorded.
//
// The resulting log entry is a JSON object with top-level fields:
//   - time
//   - type
//   - duration_ms
//   - query { method, url, headers, body, body_encoding, truncated, bytes }
//   - response { status, headers, body, body_encoding, truncated, bytes }
//   - error (optional)
//
// The UI can render these entries as JSON directly.
type Transport struct {
	base         http.RoundTripper
	mgr          *Manager
	deviceID     int64
	host         string
	label        string
	captureLimit int64
}

// WrapTransport wraps base with an ultra-debug logging transport.
// label is a short string describing the caller context (e.g. "poller", "firmware", "airmax").
// captureLimit controls per-entry body capture size.
func WrapTransport(mgr *Manager, deviceID int64, base http.RoundTripper, label string, captureLimit int64) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	if captureLimit <= 0 {
		captureLimit = DefaultCaptureLimit
	}
	return &Transport{base: base, mgr: mgr, deviceID: deviceID, label: label, captureLimit: captureLimit}
}

// WrapTransportHost wraps base with an ultra-debug logging transport that records
// entries into a host-scoped buffer (non-deviceID flows).
//
// The host string is normalized (scheme stripped, port stripped when possible, lowercased).
func WrapTransportHost(mgr *Manager, host string, base http.RoundTripper, label string, captureLimit int64) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	if captureLimit <= 0 {
		captureLimit = DefaultCaptureLimit
	}
	nh := normalizeHost(host)
	return &Transport{base: base, mgr: mgr, host: nh, label: label, captureLimit: captureLimit}
}

// WrapTransportTargets wraps base with an ultra-debug logging transport that can record
// to both a device-scoped buffer (deviceID) and a host-scoped buffer (host).
//
// This is useful for code paths where a device ID might be unknown/optional but an IP/host is.
func WrapTransportTargets(mgr *Manager, deviceID int64, host string, base http.RoundTripper, label string, captureLimit int64) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	if captureLimit <= 0 {
		captureLimit = DefaultCaptureLimit
	}
	nh := normalizeHost(host)
	return &Transport{base: base, mgr: mgr, deviceID: deviceID, host: nh, label: label, captureLimit: captureLimit}
}

type httpLogEntry struct {
	Time       time.Time    `json:"time"`
	Type       string       `json:"type"`
	DurationMs int64        `json:"duration_ms"`
	Query      httpLogQuery `json:"query"`
	Response   httpLogResp  `json:"response"`
	Error      string       `json:"error,omitempty"`
}

type httpLogQuery struct {
	Method  string              `json:"method"`
	URL     string              `json:"url"`
	Headers map[string][]string `json:"headers,omitempty"`
	// Body is either a string (for non-JSON payloads) or a decoded JSON value
	// (object/array/etc) when the content appears to be JSON.
	//
	// This avoids double-escaped "JSON-in-JSON" blobs in the UI.
	Body         any    `json:"body,omitempty"`
	BodyEncoding string `json:"body_encoding,omitempty"`
	Truncated    bool   `json:"truncated,omitempty"`
	Bytes        int64  `json:"bytes,omitempty"`
}

type httpLogResp struct {
	Status  int                 `json:"status"`
	Headers map[string][]string `json:"headers,omitempty"`
	// Body is either a string (for non-JSON payloads) or a decoded JSON value
	// (object/array/etc) when the content appears to be JSON.
	Body         any    `json:"body,omitempty"`
	BodyEncoding string `json:"body_encoding,omitempty"`
	Truncated    bool   `json:"truncated,omitempty"`
	Bytes        int64  `json:"bytes,omitempty"`
}

func looksLikeJSON(contentType string, body string) bool {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	if strings.Contains(ct, "application/json") || strings.Contains(ct, "+json") || strings.Contains(ct, "text/json") {
		return true
	}
	// Fallback heuristic for mislabelled endpoints.
	b := strings.TrimSpace(body)
	return strings.HasPrefix(b, "{") || strings.HasPrefix(b, "[")
}

func decodeJSONBody(body string, bodyEncoding string, contentType string, truncated bool) (any, bool) {
	if truncated {
		return nil, false
	}
	if body == "" {
		return nil, false
	}
	if bodyEncoding != "" && strings.ToLower(bodyEncoding) != "utf-8" {
		// We don't attempt to parse base64/binary bodies.
		return nil, false
	}
	if !looksLikeJSON(contentType, body) {
		return nil, false
	}
	var v any
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		return nil, false
	}
	return v, true
}

func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()

	entryType := inferType(req)
	if t.label != "" {
		entryType = t.label + ":" + entryType
	}

	logEntry := &httpLogEntry{
		Time: start,
		Type: entryType,
		Query: httpLogQuery{
			Method:  req.Method,
			URL:     req.URL.String(),
			Headers: cloneHeader(req.Header),
		},
		Response: httpLogResp{Status: 0},
	}

	// Capture request body (when safe). We must also preserve the body for the underlying transport.
	if req.Body != nil {
		captured, sendPrefix, truncated, totalBytes, enc, ok := captureRequestBody(req, t.captureLimit)
		if ok {
			if v, ok := decodeJSONBody(captured, enc, req.Header.Get("Content-Type"), truncated); ok {
				logEntry.Query.Body = v
			} else if captured != "" {
				logEntry.Query.Body = captured
			}
			logEntry.Query.BodyEncoding = enc
			logEntry.Query.Truncated = truncated
			logEntry.Query.Bytes = totalBytes
			// Re-wrap the request body so the transport sees the full stream.
			orig := req.Body
			req.Body = &multiReadCloser{r: io.MultiReader(bytes.NewReader(sendPrefix), orig), c: orig}
		} else {
			// Not captured (likely binary/multipart) - record a short placeholder.
			logEntry.Query.Body = "<body omitted>"
			logEntry.Query.BodyEncoding = "none"
			logEntry.Query.Bytes = req.ContentLength
		}
	}

	resp, err := t.base.RoundTrip(req)
	dur := time.Since(start)
	logEntry.DurationMs = dur.Milliseconds()

	if err != nil {
		logEntry.Error = err.Error()
		// Record and return.
		if t.mgr != nil {
			t.mgr.Record(t.deviceID, logEntry)
			if t.host != "" {
				t.mgr.RecordHost(t.host, logEntry)
			}
		}
		return nil, err
	}

	logEntry.Response.Status = resp.StatusCode
	logEntry.Response.Headers = cloneHeader(resp.Header)

	// Capture response body (textual, limited) while preserving full body for caller.
	if resp.Body != nil {
		captured, prefix, truncated, totalBytes, enc := captureResponseBody(resp, t.captureLimit)
		if v, ok := decodeJSONBody(captured, enc, resp.Header.Get("Content-Type"), truncated); ok {
			logEntry.Response.Body = v
		} else if captured != "" {
			logEntry.Response.Body = captured
		}
		logEntry.Response.BodyEncoding = enc
		logEntry.Response.Truncated = truncated
		logEntry.Response.Bytes = totalBytes

		orig := resp.Body
		resp.Body = &multiReadCloser{r: io.MultiReader(bytes.NewReader(prefix), orig), c: orig}
	}

	if t.mgr != nil {
		t.mgr.Record(t.deviceID, logEntry)

		hostKey := t.host
		if hostKey == "" {
			if req.URL != nil {
				hostKey = req.URL.Hostname()
			}
			if hostKey == "" && req.Host != "" {
				hostKey = req.Host
			}
		}
		if hostKey != "" {
			t.mgr.RecordHost(hostKey, logEntry)
		}
	}
	return resp, nil
}

type multiReadCloser struct {
	r io.Reader
	c io.Closer
}

func (m *multiReadCloser) Read(p []byte) (int, error) { return m.r.Read(p) }
func (m *multiReadCloser) Close() error               { return m.c.Close() }

func cloneHeader(h http.Header) map[string][]string {
	if h == nil {
		return nil
	}
	out := make(map[string][]string, len(h))
	for k, vv := range h {
		cpy := make([]string, len(vv))
		copy(cpy, vv)
		out[k] = cpy
	}
	return out
}

func inferType(req *http.Request) string {
	path := strings.ToLower(req.URL.Path)
	method := strings.ToUpper(req.Method)

	// Common auth endpoints.
	if strings.Contains(path, "login") || strings.Contains(path, "/api/auth") {
		return "auth"
	}
	// AirMAX legacy endpoints.
	if strings.Contains(path, "status.cgi") {
		return "status"
	}
	if strings.Contains(path, "sta.cgi") || strings.Contains(path, "stations") {
		return "stations"
	}
	// Firmware endpoints.
	if strings.Contains(path, "fwflash") || strings.Contains(path, "fwupl") || strings.Contains(path, "upgrade") {
		return "firmware"
	}
	if strings.Contains(path, "csrf") {
		return "csrf"
	}

	// Generic.
	if method == "GET" {
		return "get"
	}
	if method == "POST" {
		return "post"
	}
	return strings.ToLower(method)
}

var likelyBinaryContentType = regexp.MustCompile(`(?i)^(multipart/|application/octet-stream|application/zip|application/x-binary|application/pdf|image/|video/|audio/)`)

func captureRequestBody(req *http.Request, limit int64) (captured string, sendPrefix []byte, truncated bool, totalBytes int64, encoding string, ok bool) {
	// Skip for binary/multipart or huge payloads.
	ct := strings.TrimSpace(req.Header.Get("Content-Type"))
	if likelyBinaryContentType.MatchString(ct) {
		return "", nil, false, req.ContentLength, "none", false
	}
	// If the request is known to be huge, don't capture.
	if req.ContentLength > limit && req.ContentLength > 0 {
		return "", nil, true, req.ContentLength, "none", false
	}

	// Read up to limit+1 bytes.
	prefix, truncated, total := readPrefix(req.Body, limit)
	enc, bodyStr := encodeBody(prefix[:minInt(len(prefix), int(limit))])
	// We return sendPrefix as the full prefix we actually consumed from req.Body.
	// That includes the extra byte used to detect truncation.
	return bodyStr, prefix, truncated, total, enc, true
}

func captureResponseBody(resp *http.Response, limit int64) (captured string, prefix []byte, truncated bool, totalBytes int64, encoding string) {
	ct := strings.TrimSpace(resp.Header.Get("Content-Type"))
	// Skip binary responses.
	if likelyBinaryContentType.MatchString(ct) {
		return "<body omitted>", nil, false, resp.ContentLength, "none"
	}

	prefix, truncatedByRead, total := readPrefix(resp.Body, limit)
	truncated = truncatedByRead
	if resp.ContentLength > limit && resp.ContentLength > 0 {
		truncated = true
	}
	enc, bodyStr := encodeBody(prefix[:minInt(len(prefix), int(limit))])
	return bodyStr, prefix, truncated, total, enc
}

func readPrefix(r io.Reader, limit int64) (prefix []byte, truncated bool, totalBytes int64) {
	if limit <= 0 {
		limit = DefaultCaptureLimit
	}
	// Read limit+1 so we can tell if it was truncated without consuming the whole stream.
	lr := io.LimitReader(r, limit+1)
	b, _ := io.ReadAll(lr)
	totalBytes = int64(len(b))
	if int64(len(b)) > limit {
		truncated = true
	}
	return b, truncated, totalBytes
}

func encodeBody(b []byte) (encoding string, body string) {
	if len(b) == 0 {
		return "utf-8", ""
	}
	if utf8.Valid(b) {
		return "utf-8", string(b)
	}
	return "base64", base64.StdEncoding.EncodeToString(b)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
