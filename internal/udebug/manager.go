package udebug

import (
	"container/list"
	"encoding/json"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultMaxBytes is the default per-buffer ring size.
//
// Ultra debug buffers are intentionally in-memory only.
const DefaultMaxBytes int64 = 32 << 20 // 32MB

// Manager holds per-target (device or host) ultra debug ring buffers.
//
// This is designed to be safe for concurrent use and to keep memory bounded:
// each enabled target gets a fixed-size ring buffer (maxBytes).
//
// Targets:
//   - device: keyed by device ID (int64)
//   - host: keyed by normalized host/ip string
//
// Non-deviceID flows (e.g. ad-hoc drilldown polls) can be captured using the
// host buffers.
type Manager struct {
	maxBytes int64

	mu         sync.RWMutex
	deviceBufs map[int64]*buffer
	hostBufs   map[string]*buffer // key: normalized host
}

// BufferInfo is returned by List() to describe enabled buffers.
//
// Key is stable and can be used by the UI to reference a buffer.
// Kind is either "device" or "host".
type BufferInfo struct {
	Key      string    `json:"key"`
	Kind     string    `json:"kind"`
	DeviceID int64     `json:"device_id,omitempty"`
	Host     string    `json:"host,omitempty"`
	Created  time.Time `json:"created"`
	Updated  time.Time `json:"updated"`
	Bytes    int64     `json:"bytes"`
	Entries  int       `json:"entries"`
}

// Snapshot returns a view of a buffer including its JSON log entries.
//
// Tail controls how many most-recent entries to return:
//   - tail <= 0 : return all
//   - tail > 0  : return last N
type Snapshot struct {
	Key      string            `json:"key"`
	Kind     string            `json:"kind"`
	DeviceID int64             `json:"device_id,omitempty"`
	Host     string            `json:"host,omitempty"`
	Created  time.Time         `json:"created"`
	Updated  time.Time         `json:"updated"`
	Bytes    int64             `json:"bytes"`
	Entries  int               `json:"entries"`
	Log      []json.RawMessage `json:"log"`
}

type buffer struct {
	kind     string
	deviceID int64
	host     string
	created  time.Time
	updated  time.Time
	maxBytes int64

	mu      sync.Mutex
	bytes   int64
	entries *list.List // of record
}

type record struct {
	raw  json.RawMessage
	size int64
}

// NewManager creates a new ultra-debug manager.
func NewManager(maxBytes int64) *Manager {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	return &Manager{
		maxBytes:   maxBytes,
		deviceBufs: make(map[int64]*buffer),
		hostBufs:   make(map[string]*buffer),
	}
}

func (m *Manager) MaxBytes() int64 {
	return m.maxBytes
}

// Enable enables ultra debug for a device ID, resetting any existing buffer.
func (m *Manager) Enable(deviceID int64) {
	if deviceID <= 0 {
		return
	}
	b := newBuffer("device", deviceID, "", m.maxBytes)
	m.mu.Lock()
	m.deviceBufs[deviceID] = b
	m.mu.Unlock()
}

// Disable disables ultra debug for a device ID.
func (m *Manager) Disable(deviceID int64) {
	if deviceID <= 0 {
		return
	}
	m.mu.Lock()
	delete(m.deviceBufs, deviceID)
	m.mu.Unlock()
}

// Clear resets (empties) an enabled device buffer without disabling it.
// Returns true if the buffer existed.
func (m *Manager) Clear(deviceID int64) bool {
	if deviceID <= 0 {
		return false
	}
	m.mu.RLock()
	b := m.deviceBufs[deviceID]
	m.mu.RUnlock()
	if b == nil {
		return false
	}
	b.clear()
	return true
}

// IsEnabled returns true if a device ultra debug buffer is enabled.
func (m *Manager) IsEnabled(deviceID int64) bool {
	if deviceID <= 0 {
		return false
	}
	m.mu.RLock()
	_, ok := m.deviceBufs[deviceID]
	m.mu.RUnlock()
	return ok
}

// Record appends a JSON-marshalled entry to a device buffer (if enabled).
func (m *Manager) Record(deviceID int64, entry any) {
	if deviceID <= 0 {
		return
	}
	m.mu.RLock()
	b := m.deviceBufs[deviceID]
	m.mu.RUnlock()
	if b == nil {
		return
	}
	b.append(entry)
}

// Snapshot returns the snapshot for a device buffer.
func (m *Manager) Snapshot(deviceID int64, tail int) (Snapshot, bool) {
	if deviceID <= 0 {
		return Snapshot{}, false
	}
	m.mu.RLock()
	b := m.deviceBufs[deviceID]
	m.mu.RUnlock()
	if b == nil {
		return Snapshot{}, false
	}
	snap := b.snapshot(tail)
	snap.Key = deviceKey(deviceID)
	return snap, true
}

// Download returns the device buffer as raw JSON (array of entries).
func (m *Manager) Download(deviceID int64) ([]byte, bool, error) {
	snap, ok := m.Snapshot(deviceID, 0)
	if !ok {
		return nil, false, nil
	}
	b, err := json.Marshal(snap.Log)
	if err != nil {
		return nil, false, err
	}
	return b, true, nil
}

// EnableHost enables ultra debug for a host/ip, resetting any existing buffer.
// Returns the normalized host string that was used.
func (m *Manager) EnableHost(host string) string {
	h := normalizeHost(host)
	if h == "" {
		return ""
	}
	b := newBuffer("host", 0, h, m.maxBytes)
	m.mu.Lock()
	m.hostBufs[h] = b
	m.mu.Unlock()
	return h
}

// DisableHost disables ultra debug for a host/ip.
func (m *Manager) DisableHost(host string) {
	h := normalizeHost(host)
	if h == "" {
		return
	}
	m.mu.Lock()
	delete(m.hostBufs, h)
	m.mu.Unlock()
}

// ClearHost resets (empties) an enabled host buffer without disabling it.
// Returns true if the buffer existed.
func (m *Manager) ClearHost(host string) bool {
	h := normalizeHost(host)
	if h == "" {
		return false
	}
	m.mu.RLock()
	b := m.hostBufs[h]
	m.mu.RUnlock()
	if b == nil {
		return false
	}
	b.clear()
	return true
}

// IsHostEnabled returns true if a host ultra debug buffer is enabled.
func (m *Manager) IsHostEnabled(host string) bool {
	h := normalizeHost(host)
	if h == "" {
		return false
	}
	m.mu.RLock()
	_, ok := m.hostBufs[h]
	m.mu.RUnlock()
	return ok
}

// RecordHost appends a JSON-marshalled entry to a host buffer (if enabled).
func (m *Manager) RecordHost(host string, entry any) {
	h := normalizeHost(host)
	if h == "" {
		return
	}
	m.mu.RLock()
	b := m.hostBufs[h]
	m.mu.RUnlock()
	if b == nil {
		return
	}
	b.append(entry)
}

// SnapshotHost returns the snapshot for a host buffer.
func (m *Manager) SnapshotHost(host string, tail int) (Snapshot, bool) {
	h := normalizeHost(host)
	if h == "" {
		return Snapshot{}, false
	}
	m.mu.RLock()
	b := m.hostBufs[h]
	m.mu.RUnlock()
	if b == nil {
		return Snapshot{}, false
	}
	snap := b.snapshot(tail)
	snap.Key = hostKey(h)
	return snap, true
}

// DownloadHost returns the host buffer as raw JSON (array of entries).
func (m *Manager) DownloadHost(host string) ([]byte, bool, error) {
	snap, ok := m.SnapshotHost(host, 0)
	if !ok {
		return nil, false, nil
	}
	b, err := json.Marshal(snap.Log)
	if err != nil {
		return nil, false, err
	}
	return b, true, nil
}

// List returns summaries for all enabled buffers (device + host).
func (m *Manager) List() []BufferInfo {
	// Collect pointers under a read lock.
	m.mu.RLock()
	devs := make(map[int64]*buffer, len(m.deviceBufs))
	for id, b := range m.deviceBufs {
		devs[id] = b
	}
	hosts := make(map[string]*buffer, len(m.hostBufs))
	for h, b := range m.hostBufs {
		hosts[h] = b
	}
	m.mu.RUnlock()

	out := make([]BufferInfo, 0, len(devs)+len(hosts))
	for id, b := range devs {
		info := b.info()
		info.Key = deviceKey(id)
		out = append(out, info)
	}
	for h, b := range hosts {
		info := b.info()
		info.Key = hostKey(h)
		out = append(out, info)
	}
	return out
}

// ListEnabled is kept for backwards compatibility with earlier in-repo usage.
func (m *Manager) ListEnabled() []BufferInfo {
	return m.List()
}

func newBuffer(kind string, deviceID int64, host string, maxBytes int64) *buffer {
	now := time.Now()
	return &buffer{
		kind:     kind,
		deviceID: deviceID,
		host:     host,
		created:  now,
		updated:  now,
		maxBytes: maxBytes,
		entries:  list.New(),
	}
}

func (b *buffer) clear() {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	b.created = now
	b.updated = now
	b.bytes = 0
	b.entries.Init()
}

func (b *buffer) info() BufferInfo {
	b.mu.Lock()
	defer b.mu.Unlock()

	return BufferInfo{
		Key:      "",
		Kind:     b.kind,
		DeviceID: b.deviceID,
		Host:     b.host,
		Created:  b.created,
		Updated:  b.updated,
		Bytes:    b.bytes,
		Entries:  b.entries.Len(),
	}
}

func (b *buffer) snapshot(tail int) Snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()

	n := b.entries.Len()
	start := 0
	if tail > 0 && tail < n {
		start = n - tail
	}

	log := make([]json.RawMessage, 0, n)
	i := 0
	for e := b.entries.Front(); e != nil; e = e.Next() {
		if i >= start {
			r := e.Value.(record)
			// Copy the RawMessage bytes so callers can't mutate internal state.
			cpy := append(json.RawMessage(nil), r.raw...)
			log = append(log, cpy)
		}
		i++
	}

	return Snapshot{
		Key:      "",
		Kind:     b.kind,
		DeviceID: b.deviceID,
		Host:     b.host,
		Created:  b.created,
		Updated:  b.updated,
		Bytes:    b.bytes,
		Entries:  n,
		Log:      log,
	}
}

func (b *buffer) append(entry any) {
	raw, err := json.Marshal(entry)
	if err != nil {
		// If we can't marshal, record a small synthetic entry.
		raw = []byte(`{"type":"marshal_error","error":"unable to marshal ultra debug entry"}`)
	}
	size := int64(len(raw))

	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	b.updated = now

	// Evict old entries until there is room.
	for b.bytes+size > b.maxBytes && b.entries.Len() > 0 {
		front := b.entries.Front()
		if front == nil {
			break
		}
		r := front.Value.(record)
		b.bytes -= r.size
		b.entries.Remove(front)
	}

	// If a single entry is larger than the entire buffer, keep only a truncated placeholder.
	if size > b.maxBytes {
		trunc := raw
		if int64(len(trunc)) > b.maxBytes {
			trunc = trunc[:b.maxBytes]
		}
		b.entries.Init()
		b.bytes = int64(len(trunc))
		b.entries.PushBack(record{raw: append(json.RawMessage(nil), trunc...), size: int64(len(trunc))})
		return
	}

	b.entries.PushBack(record{raw: append(json.RawMessage(nil), raw...), size: size})
	b.bytes += size
}

func deviceKey(deviceID int64) string {
	return "device:" + strconv.FormatInt(deviceID, 10)
}

func hostKey(host string) string {
	return "host:" + host
}

// normalizeHost turns a host/ip string into a stable key.
//
// It removes scheme, credentials, paths, query/fragment, and port (when
// possible). The result is lower-cased.
func normalizeHost(in string) string {
	h := strings.TrimSpace(in)
	if h == "" {
		return ""
	}

	// Strip common schemes.
	h = strings.TrimPrefix(h, "http://")
	h = strings.TrimPrefix(h, "https://")

	// Strip everything after /, ?, #
	if i := strings.IndexAny(h, "/?#"); i >= 0 {
		h = h[:i]
	}

	// Strip credentials if present.
	if at := strings.LastIndex(h, "@"); at >= 0 {
		h = h[at+1:]
	}

	// If host:port, split. net.SplitHostPort requires a port, so this is safe for plain hosts.
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}

	// Remove IPv6 brackets if present.
	h = strings.Trim(h, "[]")

	h = strings.TrimSpace(h)
	h = strings.ToLower(h)
	return h
}
