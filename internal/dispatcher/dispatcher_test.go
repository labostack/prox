package dispatcher

import (
	"bytes"
	"crypto/tls"
	"net"
	"testing"
	"time"
)

func TestPeekSNI_RealClientHello(t *testing.T) {
	// Generate a real TLS ClientHello by starting a TLS handshake.
	serverDone := make(chan struct{})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	// Capture the ClientHello bytes on the server side.
	var capturedSNI string
	var capturedBuf []byte
	go func() {
		defer close(serverDone)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		capturedSNI, capturedBuf, _ = PeekSNI(conn)
	}()

	// Client sends a TLS ClientHello with SNI.
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	tlsConn := tls.Client(conn, &tls.Config{
		ServerName:         "app.example.com",
		InsecureSkipVerify: true,
	})
	// Start handshake (will fail since server isn't doing TLS, but that's fine —
	// we only need the ClientHello to be sent).
	go func() { _ = tlsConn.Handshake() }()

	// Wait for server to capture.
	select {
	case <-serverDone:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for ClientHello")
	}
	tlsConn.Close()

	if capturedSNI != "app.example.com" {
		t.Errorf("expected SNI %q, got %q", "app.example.com", capturedSNI)
	}
	if len(capturedBuf) == 0 {
		t.Error("expected non-empty buffer")
	}
}

func TestPeekSNI_NotTLS(t *testing.T) {
	data := []byte("GET / HTTP/1.1\r\nHost: example.com\r\n\r\n")
	sni, buf, err := PeekSNI(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sni != "" {
		t.Errorf("expected empty SNI for non-TLS, got %q", sni)
	}
	// Should still return the 5-byte TLS record header attempt.
	if len(buf) != 5 {
		t.Errorf("expected 5 buffered bytes, got %d", len(buf))
	}
}

func TestMatchDomain(t *testing.T) {
	tests := []struct {
		pattern  string
		host     string
		segments []string
		glob     bool
		want     bool
	}{
		{"*.cdn.example.com", "us.cdn.example.com", []string{"*", "cdn", "example", "com"}, false, true},
		{"*.cdn.example.com", "cdn.example.com", []string{"*", "cdn", "example", "com"}, false, false},
		{"*.cdn.example.com", "a.b.cdn.example.com", []string{"*", "cdn", "example", "com"}, false, false},
		{"api.*.example.com", "api.staging.example.com", []string{"api", "*", "example", "com"}, false, true},
		{"api.*.example.com", "api.example.com", []string{"api", "*", "example", "com"}, false, false},
		{"exact.example.com", "exact.example.com", []string{"exact", "example", "com"}, false, true},
		{"exact.example.com", "other.example.com", []string{"exact", "example", "com"}, false, false},
		// ** glob patterns (segments have ** stripped, glob=true)
		{"*.storage.**", "cdn.storage.example.com", []string{"*", "storage"}, true, true},
		{"*.storage.**", "cdn.storage.a.b.c", []string{"*", "storage"}, true, true},
		{"*.storage.**", "storage.example.com", []string{"*", "storage"}, true, false},  // missing prefix
		{"*.storage.**", "cdn.storage", []string{"*", "storage"}, true, false},           // no suffix
		{"*.storage.**", "cdn.other.example.com", []string{"*", "storage"}, true, false}, // "other" != "storage"
		// Partial wildcards
		{"cdn-*.example.com", "cdn-us.example.com", []string{"cdn-*", "example", "com"}, false, true},
		{"cdn-*.example.com", "cdn.example.com", []string{"cdn-*", "example", "com"}, false, false},   // no dash
		{"cdn-*.example.com", "web-us.example.com", []string{"cdn-*", "example", "com"}, false, false}, // wrong prefix
		{"*-prod.example.com", "api-prod.example.com", []string{"*-prod", "example", "com"}, false, true},
		{"*-prod.example.com", "api-staging.example.com", []string{"*-prod", "example", "com"}, false, false},
		// Partial wildcard + glob
		{"cdn-*.**", "cdn-us.example.com", []string{"cdn-*"}, true, true},
		{"cdn-*.**", "cdn-eu.a.b.c", []string{"cdn-*"}, true, true},
		{"cdn-*.**", "cdn.example.com", []string{"cdn-*"}, true, false},
	}

	for _, tt := range tests {
		t.Run(tt.pattern+"→"+tt.host, func(t *testing.T) {
			got := matchDomain(tt.segments, tt.glob, tt.host)
			if got != tt.want {
				t.Errorf("matchDomain(%v, %v, %q) = %v, want %v", tt.segments, tt.glob, tt.host, got, tt.want)
			}
		})
	}
}

func TestMatchDomain_NilPattern(t *testing.T) {
	if !matchDomain(nil, false, "anything.com") {
		t.Error("nil pattern should match everything")
	}
}

func TestPrefixConn(t *testing.T) {
	// Create a pipe to simulate a connection.
	server, client := net.Pipe()
	defer server.Close()

	prefix := []byte("HELLO")

	go func() {
		_, _ = client.Write([]byte(" WORLD"))
		client.Close()
	}()

	pc := newPrefixConn(server, prefix)

	buf := make([]byte, 64)
	var result []byte
	for {
		n, err := pc.Read(buf)
		if n > 0 {
			result = append(result, buf[:n]...)
		}
		if err != nil {
			break
		}
	}

	if string(result) != "HELLO WORLD" {
		t.Errorf("expected %q, got %q", "HELLO WORLD", string(result))
	}
}
