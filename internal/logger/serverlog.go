package logger

import (
	"log"
	"log/slog"
	"strings"
)

// serverLogWriter is an io.Writer that redirects Go's net/http internal
// error messages to slog at DEBUG level. These messages (TLS handshake
// failures, connection resets, etc.) are operational noise from scanners
// and bots — useful for debugging but not worth polluting INFO logs.
type serverLogWriter struct{}

func (serverLogWriter) Write(p []byte) (int, error) {
	slog.Debug(strings.TrimRight(string(p), "\n"), "source", "net/http")
	return len(p), nil
}

// NewServerErrorLog returns a *log.Logger that routes all messages through
// slog at DEBUG level. Set this as the ErrorLog field on http.Server.
func NewServerErrorLog() *log.Logger {
	return log.New(serverLogWriter{}, "", 0)
}
