package logger

import (
	"testing"
)

func TestNewServerErrorLog(t *testing.T) {
	l := NewServerErrorLog()
	if l == nil {
		t.Fatal("expected non-nil logger")
	}

	// Verify the logger is usable — these should not panic and should
	// route through slog.Debug (which is a no-op at default INFO level).
	l.Printf("http: TLS handshake error from 1.2.3.4:443: test")
	l.Printf("http: some other net/http message")
}
