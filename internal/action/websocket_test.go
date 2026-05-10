package action

import (
	"bufio"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dortanes/prox/internal/config"
)

func TestIsWebSocketUpgrade(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]string
		want    bool
	}{
		{
			name:    "standard websocket upgrade",
			headers: map[string]string{"Upgrade": "websocket", "Connection": "Upgrade"},
			want:    true,
		},
		{
			name:    "case-insensitive",
			headers: map[string]string{"Upgrade": "WebSocket", "Connection": "upgrade"},
			want:    true,
		},
		{
			name:    "connection with multiple values",
			headers: map[string]string{"Upgrade": "websocket", "Connection": "keep-alive, Upgrade"},
			want:    true,
		},
		{
			name:    "missing upgrade header",
			headers: map[string]string{"Connection": "Upgrade"},
			want:    false,
		},
		{
			name:    "missing connection header",
			headers: map[string]string{"Upgrade": "websocket"},
			want:    false,
		},
		{
			name:    "wrong upgrade protocol",
			headers: map[string]string{"Upgrade": "h2c", "Connection": "Upgrade"},
			want:    false,
		},
		{
			name:    "empty headers",
			headers: map[string]string{},
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &http.Request{Header: http.Header{}}
			for k, v := range tt.headers {
				r.Header.Set(k, v)
			}
			if got := isWebSocketUpgrade(r); got != tt.want {
				t.Errorf("isWebSocketUpgrade() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHeaderContains(t *testing.T) {
	tests := []struct {
		name   string
		header http.Header
		key    string
		value  string
		want   bool
	}{
		{
			name:   "single value match",
			header: http.Header{"Connection": {"Upgrade"}},
			key:    "Connection",
			value:  "upgrade",
			want:   true,
		},
		{
			name:   "comma-separated match",
			header: http.Header{"Connection": {"keep-alive, Upgrade"}},
			key:    "Connection",
			value:  "upgrade",
			want:   true,
		},
		{
			name:   "multiple header values",
			header: http.Header{"Connection": {"keep-alive", "Upgrade"}},
			key:    "Connection",
			value:  "upgrade",
			want:   true,
		},
		{
			name:   "no match",
			header: http.Header{"Connection": {"keep-alive"}},
			key:    "Connection",
			value:  "upgrade",
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := headerContains(tt.header, tt.key, tt.value); got != tt.want {
				t.Errorf("headerContains() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestWebSocketProxy verifies end-to-end WebSocket proxying via the Proxy handler.
// Uses a raw TCP echo server to simulate a WebSocket upstream.
func TestWebSocketProxy(t *testing.T) {
	// Start a minimal WebSocket echo server.
	// It accepts the upgrade, then echoes all received data back.
	echoServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isWebSocketUpgrade(r) {
			http.Error(w, "expected websocket upgrade", http.StatusBadRequest)
			return
		}

		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "hijack not supported", http.StatusInternalServerError)
			return
		}

		conn, buf, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()

		// Send 101 Switching Protocols.
		_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
		_, _ = buf.WriteString("Upgrade: websocket\r\n")
		_, _ = buf.WriteString("Connection: Upgrade\r\n")
		_, _ = buf.WriteString("\r\n")
		_ = buf.Flush()

		// Echo loop: read lines and send them back.
		scanner := bufio.NewScanner(conn)
		for scanner.Scan() {
			line := scanner.Text()
			_, _ = buf.WriteString(line + "\n")
			_ = buf.Flush()
		}
	}))
	defer echoServer.Close()

	// Build a Proxy action targeting the echo server.
	proxy, err := NewProxy(&config.Action{
		Type:     config.ActionTypeProxy,
		Upstream: echoServer.URL,
	}, nil)
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}

	// Create a test HTTP server wrapping the proxy handler.
	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	// Connect to the proxy and send a WebSocket upgrade.
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(proxyServer.URL, "http://"), 5*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()

	// Send upgrade request.
	req := "GET / HTTP/1.1\r\n" +
		"Host: localhost\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"\r\n"
	_, err = conn.Write([]byte(req))
	if err != nil {
		t.Fatalf("write upgrade request: %v", err)
	}

	// Read the 101 response.
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("expected 101, got %d", resp.StatusCode)
	}

	// Send a test message and verify echo.
	testMsg := "hello websocket\n"
	_, err = conn.Write([]byte(testMsg))
	if err != nil {
		t.Fatalf("write test message: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}

	if line != testMsg {
		t.Errorf("echo mismatch: got %q, want %q", line, testMsg)
	}
}

// TestWebSocketProxyWithHeaders verifies headers are forwarded during WebSocket upgrade.
func TestWebSocketProxyWithHeaders(t *testing.T) {
	var receivedHeaders http.Header

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaders = r.Header.Clone()
		// Reject to simplify — we only care about header forwarding.
		http.Error(w, "not a real websocket server", http.StatusBadRequest)
	}))
	defer upstream.Close()

	proxy, err := NewProxy(&config.Action{
		Type:     config.ActionTypeProxy,
		Upstream: upstream.URL,
		Headers: map[string]string{
			"X-Custom-Token": "secret-value",
		},
	}, nil)
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}

	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	// Connect raw to properly hijack.
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(proxyServer.URL, "http://"), 5*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()

	req := "GET /ws HTTP/1.1\r\n" +
		"Host: localhost\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"\r\n"
	_, _ = conn.Write([]byte(req))

	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()

	if receivedHeaders.Get("X-Custom-Token") != "secret-value" {
		t.Errorf("custom header not forwarded: got %q", receivedHeaders.Get("X-Custom-Token"))
	}
}

// TestWebSocketUpstreamReject verifies proper handling when upstream rejects the upgrade.
func TestWebSocketUpstreamReject(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Forbidden", http.StatusForbidden)
	}))
	defer upstream.Close()

	proxy, err := NewProxy(&config.Action{
		Type:     config.ActionTypeProxy,
		Upstream: upstream.URL,
	}, nil)
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}

	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(proxyServer.URL, "http://"), 5*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()

	req := "GET /ws HTTP/1.1\r\n" +
		"Host: localhost\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"\r\n"
	_, _ = conn.Write([]byte(req))

	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403, got %d", resp.StatusCode)
	}
}

// TestNonWebSocketPassthrough verifies regular HTTP requests still go through ReverseProxy.
func TestNonWebSocketPassthrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("regular response"))
	}))
	defer upstream.Close()

	proxy, err := NewProxy(&config.Action{
		Type:     config.ActionTypeProxy,
		Upstream: upstream.URL,
	}, nil)
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != "regular response" {
		t.Errorf("body mismatch: got %q", w.Body.String())
	}
}
