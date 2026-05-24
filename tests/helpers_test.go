// Package tests provides integration tests for prox.
//
// These tests spin up real HTTP servers (upstream + prox) and verify
// end-to-end behavior: proxying, routing, headers, access logging, etc.
package tests

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// freePort returns a random available TCP port.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

// upstream starts a test HTTP server that responds with the given label.
// Returns the port and a cleanup function.
func upstream(t *testing.T, label string) int {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream", label)
		fmt.Fprintf(w, "hello from %s", label)
	})
	mux.HandleFunc("/headers", func(w http.ResponseWriter, r *http.Request) {
		for k, v := range r.Header {
			fmt.Fprintf(w, "%s: %s\n", k, v[0])
		}
	})
	mux.HandleFunc("/echo-xff", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, r.Header.Get("X-Forwarded-For"))
	})

	port := freePort(t)
	srv := &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", port),
		Handler: mux,
	}
	go srv.ListenAndServe()
	t.Cleanup(func() { srv.Close() })

	// Wait for upstream to be ready.
	waitForPort(t, port, 3*time.Second)
	return port
}

// waitForPort polls until a TCP connection succeeds or the timeout is reached.
func waitForPort(t *testing.T, port int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("port %d not ready after %s", port, timeout)
}

// writeConfig writes a JSON5 config to a temp file and returns the path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json5")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("writeConfig: %v", err)
	}
	return path
}

// httpGet sends a GET request and returns status code and body.
func httpGet(t *testing.T, url string, headers ...string) (int, string) {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatalf("httpGet: %v", err)
	}
	for i := 0; i < len(headers)-1; i += 2 {
		if http.CanonicalHeaderKey(headers[i]) == "Host" {
			req.Host = headers[i+1]
		} else {
			req.Header.Set(headers[i], headers[i+1])
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("httpGet %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// httpMethod sends a request with given method and returns status code.
func httpMethod(t *testing.T, method, url string) int {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("httpMethod: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("httpMethod %s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	io.ReadAll(resp.Body)
	return resp.StatusCode
}

// mustContain asserts that body contains the expected substring.
func mustContain(t *testing.T, body, expected, context string) {
	t.Helper()
	if !strings.Contains(body, expected) {
		t.Errorf("%s: expected body to contain %q, got %q", context, expected, body)
	}
}
