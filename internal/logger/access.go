package logger

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// AccessEntry represents a single access log record.
type AccessEntry struct {
	Timestamp time.Time `json:"ts"`
	Service   string    `json:"service"`
	Method    string    `json:"method"`
	Path      string    `json:"path"`
	Status    int       `json:"status"`
	Duration  float64   `json:"duration_ms"`
	BytesOut  int64     `json:"bytes_out"`
	ClientIP  string    `json:"client_ip"`
	UserAgent string    `json:"user_agent,omitempty"`
}

var (
	accessMu          sync.RWMutex
	accessGlobal      *FileWriter
	accessRoutes      map[string]*FileWriter
	accessWriters     []*FileWriter // deduplicated for reopen/close
	accessEnabledFlag atomic.Bool
)

// SetupAccess configures access log file destinations.
// globalPath sets the default access log file. routePaths maps route IDs to
// per-route log files. Routes without an explicit path fall back to the global file.
func SetupAccess(globalPath string, routePaths map[string]string) error {
	accessMu.Lock()
	defer accessMu.Unlock()

	accessCloseLocked()

	var allWriters []*FileWriter
	writerByPath := make(map[string]*FileWriter)

	// Global access log.
	if globalPath != "" {
		w, err := NewFileWriter(globalPath)
		if err != nil {
			return fmt.Errorf("opening access log %q: %w", globalPath, err)
		}
		accessGlobal = w
		allWriters = append(allWriters, w)
		writerByPath[globalPath] = w
	}

	// Per-route access logs (deduplicate by path).
	accessRoutes = make(map[string]*FileWriter, len(routePaths))
	for routeID, path := range routePaths {
		if w, ok := writerByPath[path]; ok {
			accessRoutes[routeID] = w
			continue
		}
		w, err := NewFileWriter(path)
		if err != nil {
			return fmt.Errorf("opening access log %q for route %s: %w", path, routeID, err)
		}
		writerByPath[path] = w
		allWriters = append(allWriters, w)
		accessRoutes[routeID] = w
	}

	accessWriters = allWriters
	accessEnabledFlag.Store(accessGlobal != nil || len(accessRoutes) > 0)
	return nil
}

// AccessEnabled reports whether any access log output is active (file or console).
// Used by the handler to skip building AccessEntry when nothing would be emitted.
func AccessEnabled() bool {
	if accessEnabledFlag.Load() {
		return true
	}
	return slog.Default().Enabled(context.Background(), slog.LevelInfo)
}

// LogAccess writes an access log entry to the appropriate file and console.
//
// Console output uses slog with level based on status code:
//
//	2xx/3xx → info, 4xx → warn, 5xx → error
//
// File output is JSON lines (one object per line).
func LogAccess(routeID string, entry AccessEntry) {
	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now()
	}

	// Determine log level from status code.
	level := slog.LevelInfo
	if entry.Status >= 500 {
		level = slog.LevelError
	} else if entry.Status >= 400 {
		level = slog.LevelWarn
	}

	// Resolve file writer early.
	accessMu.RLock()
	w := accessRoutes[routeID]
	if w == nil {
		w = accessGlobal
	}
	accessMu.RUnlock()

	// Fast path: skip all formatting when nothing will be emitted.
	consoleEnabled := slog.Default().Enabled(context.Background(), level)
	if !consoleEnabled && w == nil {
		return
	}

	// Console output.
	if consoleEnabled {
		msg := fmt.Sprintf("%s %s %d %s",
			entry.Method, entry.Path,
			entry.Status, formatDuration(entry.Duration),
		)
		slog.Log(context.Background(), level, msg,
			"service", entry.Service,
			"client", entry.ClientIP,
			"bytes", entry.BytesOut,
		)
	}

	// File output — JSON lines.
	if w == nil {
		return
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	data = append(data, '\n')
	_, _ = w.Write(data)
}

// AccessReopenFiles reopens all access log files (called during SIGHUP rotation).
func AccessReopenFiles() error {
	accessMu.Lock()
	defer accessMu.Unlock()
	for _, w := range accessWriters {
		if err := w.Reopen(); err != nil {
			return err
		}
	}
	return nil
}

// AccessClose closes all access log files.
func AccessClose() {
	accessMu.Lock()
	defer accessMu.Unlock()
	accessCloseLocked()
}

func accessCloseLocked() {
	for _, w := range accessWriters {
		w.Close()
	}
	accessGlobal = nil
	accessRoutes = nil
	accessWriters = nil
	accessEnabledFlag.Store(false)
}

// formatDuration renders milliseconds in a human-friendly way.
func formatDuration(ms float64) string {
	if ms < 1 {
		return fmt.Sprintf("%.0fµs", ms*1000)
	}
	if ms < 1000 {
		return fmt.Sprintf("%.1fms", ms)
	}
	return fmt.Sprintf("%.2fs", ms/1000)
}

// --- ResponseCapture ---

// ResponseCapture wraps http.ResponseWriter to record the status code and bytes written.
// It is used by the access log middleware.
type ResponseCapture struct {
	http.ResponseWriter
	status   int
	bytesOut int64
	written  bool
}

// NewResponseCapture wraps a ResponseWriter for access log capturing.
func NewResponseCapture(w http.ResponseWriter) *ResponseCapture {
	return &ResponseCapture{ResponseWriter: w, status: http.StatusOK}
}

// Reset reinitializes a ResponseCapture for reuse from a pool.
func (rc *ResponseCapture) Reset(w http.ResponseWriter) {
	rc.ResponseWriter = w
	rc.status = http.StatusOK
	rc.bytesOut = 0
	rc.written = false
}

func (rc *ResponseCapture) WriteHeader(code int) {
	if rc.written {
		return
	}
	rc.written = true
	rc.status = code
	rc.ResponseWriter.WriteHeader(code)
}

func (rc *ResponseCapture) Write(b []byte) (int, error) {
	if !rc.written {
		rc.written = true
	}
	n, err := rc.ResponseWriter.Write(b)
	rc.bytesOut += int64(n)
	return n, err
}

// Unwrap returns the original ResponseWriter. This allows http.ResponseController
// to discover interfaces like http.Flusher and http.Hijacker on the inner writer.
func (rc *ResponseCapture) Unwrap() http.ResponseWriter {
	return rc.ResponseWriter
}

// Status returns the recorded HTTP status code.
func (rc *ResponseCapture) Status() int { return rc.status }

// BytesOut returns the total bytes written to the response body.
func (rc *ResponseCapture) BytesOut() int64 { return rc.bytesOut }

// ClientIP extracts the client IP address from a request, checking
// X-Forwarded-For, X-Real-Ip, and RemoteAddr in order.
func ClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i != -1 {
			return strings.TrimSpace(xff[:i])
		}
		return xff
	}
	if xri := r.Header.Get("X-Real-Ip"); xri != "" {
		return xri
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
