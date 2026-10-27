package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	rand2 "math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// stateStore owns the plugin's private data directory. Everything in here is
// reconstructible except the credential copy, which exists so a restart does
// not depend on the desktop file still being mounted.
type stateStore struct {
	dir string
}

func newStateStore(dir string) (*stateStore, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		dir = defaultDataDir
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &stateStore{dir: dir}, nil
}

func (s *stateStore) path(name string) string {
	if s == nil {
		return ""
	}
	return filepath.Join(s.dir, name)
}

func (s *stateStore) deviceID() string {
	if s == nil {
		return ""
	}
	raw, err := os.ReadFile(s.path("device.json"))
	if err != nil {
		return ""
	}
	var file struct {
		ClientDeviceID string `json:"clientDeviceId"`
	}
	if json.Unmarshal(raw, &file) != nil {
		return ""
	}
	return strings.TrimSpace(file.ClientDeviceID)
}

func (s *stateStore) saveDeviceID(id string) error {
	if s == nil || strings.TrimSpace(id) == "" {
		return nil
	}
	raw, err := json.MarshalIndent(map[string]any{
		"clientDeviceId": id,
		"createdAt":      time.Now().UTC().Format(time.RFC3339Nano),
	}, "", "  ")
	if err != nil {
		return err
	}
	path := s.path("device.json")
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// randFraction returns a pseudo-random float in [0,1) for retry jitter.
func randFraction() float64 {
	return rand2.Float64()
}

func randomHex(n int) string {
	if n <= 0 {
		n = 8
	}
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(buf)
}

func newUUID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return uuidV7(make([]byte, 16))
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	hexed := hex.EncodeToString(buf)
	return hexed[0:8] + "-" + hexed[8:12] + "-" + hexed[12:16] + "-" + hexed[16:20] + "-" + hexed[20:32]
}

// logRing keeps the recent log tail for the management console.
type logRing struct {
	mu      sync.Mutex
	entries []logEntry
	limit   int
}

type logEntry struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level"`
	Scope   string    `json:"scope"`
	Message string    `json:"message"`
}

func newLogRing(limit int) *logRing {
	if limit <= 0 {
		limit = 500
	}
	return &logRing{limit: limit}
}

func (l *logRing) add(level, scope, message string) {
	entry := logEntry{Time: time.Now(), Level: level, Scope: scope, Message: message}
	l.mu.Lock()
	l.entries = append(l.entries, entry)
	if len(l.entries) > l.limit {
		l.entries = l.entries[len(l.entries)-l.limit:]
	}
	l.mu.Unlock()
	line := fmt.Sprintf("[%s] %-5s %s: %s", entry.Time.Format("15:04:05"), level, scope, message)
	fmt.Println(line)
}

func (l *logRing) info(scope, message string)  { l.add("info", scope, message) }
func (l *logRing) warn(scope, message string)  { l.add("warn", scope, message) }
func (l *logRing) error(scope, message string) { l.add("error", scope, message) }
func (l *logRing) debug(scope, message string) { l.add("debug", scope, message) }

func (l *logRing) tail(limit int) []logEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	if limit <= 0 || limit > len(l.entries) {
		limit = len(l.entries)
	}
	out := make([]logEntry, limit)
	copy(out, l.entries[len(l.entries)-limit:])
	return out
}

type metrics struct {
	mu          sync.Mutex
	requests    int64
	success     int64
	failures    int64
	streams     int64
	throttled   int64
	lastLatency int64
	totalMs     int64
	started     time.Time
}

func newMetrics() *metrics {
	return &metrics{started: time.Now()}
}

func (m *metrics) record(ok bool, latencyMs int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests++
	if ok {
		m.success++
	} else {
		m.failures++
	}
	m.lastLatency = latencyMs
	m.totalMs += latencyMs
}

func (m *metrics) markStream() {
	m.mu.Lock()
	m.streams++
	m.mu.Unlock()
}

// markThrottled counts the gateway's limiter refusals (200103), so the console
// shows how often the per-account admission gate was hit.
func (m *metrics) markThrottled() {
	m.mu.Lock()
	m.throttled++
	m.mu.Unlock()
}

func (m *metrics) snapshot() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	average := int64(0)
	if m.requests > 0 {
		average = m.totalMs / m.requests
	}
	return map[string]any{
		"requests":       m.requests,
		"success":        m.success,
		"failures":       m.failures,
		"streams":        m.streams,
		"throttled":      m.throttled,
		"last_latency":   m.lastLatency,
		"average_ms":     average,
		"uptime_seconds": int64(time.Since(m.started).Seconds()),
	}
}
