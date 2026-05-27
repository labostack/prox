package tests

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dortanes/prox/internal/config"
	"github.com/dortanes/prox/internal/logger"
	"github.com/dortanes/prox/internal/server"
)

// startProx loads a config, builds the server group, and starts it.
// Returns the cancel func and waits for the server to be ready.
func startProx(t *testing.T, configPath string, proxyPort int) func() {
	t.Helper()

	result, err := config.LoadFile(configPath)
	if err != nil {
		t.Fatalf("config load: %v", err)
	}

	// Configure access logging from config.
	globalPath := ""
	if result.Config.Logging != nil {
		globalPath = result.Config.Logging.AccessLog
	}
	routePaths := make(map[string]string)
	for name, svc := range result.Config.Services {
		for i, route := range svc.Routes {
			if route.AccessLog != "" {
				routePaths[fmt.Sprintf("%s:%d", name, i)] = route.AccessLog
			}
		}
	}
	if err := logger.SetupAccess(globalPath, routePaths); err != nil {
		t.Fatalf("access log setup: %v", err)
	}

	group, err := server.Build(result.Config)
	if err != nil {
		t.Fatalf("server build: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- group.ListenAndServe(ctx)
	}()

	waitForPort(t, proxyPort, 3*time.Second)

	return func() {
		cancel()
		<-errCh
	}
}

// --- Static Proxy (fast path) ---

func TestProxy_Static(t *testing.T) {
	upPort := upstream(t, "backend-A")
	proxyPort := freePort(t)

	cfg := writeConfig(t, fmt.Sprintf(`{
		services: {
			web: {
				listen: "127.0.0.1:%d",
				routes: [
					{ action: { type: "proxy", upstream: "127.0.0.1:%d" } }
				]
			}
		}
	}`, proxyPort, upPort))

	stop := startProx(t, cfg, proxyPort)
	defer stop()

	status, body := httpGet(t, fmt.Sprintf("http://127.0.0.1:%d/", proxyPort))
	if status != 200 {
		t.Errorf("expected 200, got %d", status)
	}
	mustContain(t, body, "hello from backend-A", "static proxy body")
}

// --- Custom Headers (httputil.ReverseProxy path) ---

func TestProxy_CustomHeaders(t *testing.T) {
	upPort := upstream(t, "backend-A")
	proxyPort := freePort(t)

	cfg := writeConfig(t, fmt.Sprintf(`{
		services: {
			web: {
				listen: "127.0.0.1:%d",
				routes: [
					{
						action: {
							type: "proxy",
							upstream: "127.0.0.1:%d",
							headers: { "X-Custom": "injected" }
						}
					}
				]
			}
		}
	}`, proxyPort, upPort))

	stop := startProx(t, cfg, proxyPort)
	defer stop()

	_, body := httpGet(t, fmt.Sprintf("http://127.0.0.1:%d/headers", proxyPort))
	mustContain(t, body, "X-Custom", "custom header key")
	mustContain(t, body, "injected", "custom header value")
}

// --- Domain-Based Routing ---

func TestRouting_Domain(t *testing.T) {
	upA := upstream(t, "backend-A")
	upB := upstream(t, "backend-B")
	proxyPort := freePort(t)

	cfg := writeConfig(t, fmt.Sprintf(`{
		services: {
			web: {
				listen: "127.0.0.1:%d",
				routes: [
					{
						match: { domain: "api.test.local" },
						action: { type: "proxy", upstream: "127.0.0.1:%d" }
					},
					{
						match: { domain: "web.test.local" },
						action: { type: "proxy", upstream: "127.0.0.1:%d" }
					},
					{
						action: { type: "static", status: 404 }
					}
				]
			}
		}
	}`, proxyPort, upA, upB))

	stop := startProx(t, cfg, proxyPort)
	defer stop()

	url := fmt.Sprintf("http://127.0.0.1:%d/", proxyPort)

	// Route to backend-A via domain.
	_, body := httpGet(t, url, "Host", "api.test.local")
	mustContain(t, body, "backend-A", "api domain route")

	// Route to backend-B via domain.
	_, body = httpGet(t, url, "Host", "web.test.local")
	mustContain(t, body, "backend-B", "web domain route")

	// Unknown domain → 404.
	status, _ := httpGet(t, url, "Host", "unknown.local")
	if status != 404 {
		t.Errorf("unknown domain: expected 404, got %d", status)
	}
}

// --- Path Routing + Method Filter ---

func TestRouting_PathAndMethod(t *testing.T) {
	upA := upstream(t, "backend-A")
	upB := upstream(t, "backend-B")
	proxyPort := freePort(t)

	cfg := writeConfig(t, fmt.Sprintf(`{
		services: {
			web: {
				listen: "127.0.0.1:%d",
				routes: [
					{
						match: { path: "/api/*", methods: ["GET"] },
						action: { type: "proxy", upstream: "127.0.0.1:%d" }
					},
					{
						match: { path: "/api/*", methods: ["POST"] },
						action: { type: "static", status: 405 }
					},
					{
						action: { type: "proxy", upstream: "127.0.0.1:%d" }
					}
				]
			}
		}
	}`, proxyPort, upA, upB))

	stop := startProx(t, cfg, proxyPort)
	defer stop()

	base := fmt.Sprintf("http://127.0.0.1:%d", proxyPort)

	// GET /api/* → backend-A.
	_, body := httpGet(t, base+"/api/test")
	mustContain(t, body, "backend-A", "GET /api/*")

	// POST /api/* → 405.
	status := httpMethod(t, "POST", base+"/api/test")
	if status != 405 {
		t.Errorf("POST /api/*: expected 405, got %d", status)
	}

	// Catch-all → backend-B.
	_, body = httpGet(t, base+"/other")
	mustContain(t, body, "backend-B", "catch-all")
}

// --- Static Action ---

func TestAction_Static(t *testing.T) {
	proxyPort := freePort(t)

	cfg := writeConfig(t, fmt.Sprintf(`{
		services: {
			web: {
				listen: "127.0.0.1:%d",
				routes: [
					{ action: { type: "static", status: 503 } }
				]
			}
		}
	}`, proxyPort))

	stop := startProx(t, cfg, proxyPort)
	defer stop()

	status, _ := httpGet(t, fmt.Sprintf("http://127.0.0.1:%d/", proxyPort))
	if status != 503 {
		t.Errorf("static action: expected 503, got %d", status)
	}
}

// --- X-Forwarded-For ---

func TestProxy_XForwardedFor(t *testing.T) {
	upPort := upstream(t, "backend-A")
	proxyPort := freePort(t)

	cfg := writeConfig(t, fmt.Sprintf(`{
		services: {
			web: {
				listen: "127.0.0.1:%d",
				routes: [
					{ action: { type: "proxy", upstream: "127.0.0.1:%d" } }
				]
			}
		}
	}`, proxyPort, upPort))

	stop := startProx(t, cfg, proxyPort)
	defer stop()

	url := fmt.Sprintf("http://127.0.0.1:%d/echo-xff", proxyPort)

	// New XFF should be set.
	_, body := httpGet(t, url)
	if body == "" {
		t.Error("X-Forwarded-For was not set")
	}

	// Existing XFF should be appended.
	_, body = httpGet(t, url, "X-Forwarded-For", "1.2.3.4")
	mustContain(t, body, "1.2.3.4", "XFF append")
}

// --- Access Logging ---

func TestLogging_AccessLogFile(t *testing.T) {
	upPort := upstream(t, "backend-A")
	proxyPort := freePort(t)
	logFile := filepath.Join(t.TempDir(), "access.log")

	cfg := writeConfig(t, fmt.Sprintf(`{
		"logging": {
			"access_log": %q
		},
		"services": {
			"web": {
				"listen": "127.0.0.1:%d",
				"routes": [
					{ "action": { "type": "proxy", "upstream": "127.0.0.1:%d" } }
				]
			}
		}
	}`, logFile, proxyPort, upPort))

	stop := startProx(t, cfg, proxyPort)
	defer stop()

	// Make a request.
	httpGet(t, fmt.Sprintf("http://127.0.0.1:%d/", proxyPort))
	time.Sleep(500 * time.Millisecond) // wait for async log write

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("reading access log: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("access log file is empty")
	}

	content := string(data)
	mustContain(t, content, `"status":200`, "access log status")
	mustContain(t, content, `"method":"GET"`, "access log method")
	mustContain(t, content, `"service":"web"`, "access log service")
}

// --- Config Validation ---

func TestConfig_ValidateValid(t *testing.T) {
	cfg := writeConfig(t, `{
		services: {
			web: {
				listen: ":8080",
				routes: [
					{ action: { type: "proxy", upstream: "localhost:3000" } }
				]
			}
		}
	}`)

	_, err := config.LoadFile(cfg)
	if err != nil {
		t.Errorf("expected valid config, got error: %v", err)
	}
}

func TestConfig_ValidateInvalid(t *testing.T) {
	cfg := writeConfig(t, `{ invalid }`)

	_, err := config.LoadFile(cfg)
	if err == nil {
		t.Error("expected error for invalid config, got nil")
	}
}

func TestConfig_UnknownActionType(t *testing.T) {
	cfg := writeConfig(t, `{
		services: {
			web: {
				listen: ":8080",
				routes: [
					{ action: { type: "nonexistent" } }
				]
			}
		}
	}`)

	_, err := config.LoadFile(cfg)
	if err == nil {
		t.Error("expected error for unknown action type, got nil")
	}
}

// --- Config Logging Section ---

func TestConfig_LoggingParsed(t *testing.T) {
	cfg := writeConfig(t, `{
		logging: {
			level: "debug",
			access_log: "/tmp/test.log",
			error_log: "/tmp/error.log"
		},
		services: {
			web: {
				listen: ":8080",
				routes: [
					{ action: { type: "proxy", upstream: "localhost:3000" } }
				]
			}
		}
	}`)

	result, err := config.LoadFile(cfg)
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	if result.Config.Logging == nil {
		t.Fatal("logging config is nil")
	}
	if result.Config.Logging.Level != "debug" {
		t.Errorf("expected level 'debug', got %q", result.Config.Logging.Level)
	}
	if result.Config.Logging.AccessLog != "/tmp/test.log" {
		t.Errorf("expected access_log '/tmp/test.log', got %q", result.Config.Logging.AccessLog)
	}
	if result.Config.Logging.ErrorLog != "/tmp/error.log" {
		t.Errorf("expected error_log '/tmp/error.log', got %q", result.Config.Logging.ErrorLog)
	}
}

// --- No Match → 404 ---

func TestRouting_NoMatch(t *testing.T) {
	proxyPort := freePort(t)

	cfg := writeConfig(t, fmt.Sprintf(`{
		services: {
			web: {
				listen: "127.0.0.1:%d",
				routes: [
					{
						match: { path: "/only-this" },
						action: { type: "static", status: 200 }
					}
				]
			}
		}
	}`, proxyPort))

	stop := startProx(t, cfg, proxyPort)
	defer stop()

	status, _ := httpGet(t, fmt.Sprintf("http://127.0.0.1:%d/anything-else", proxyPort))
	if status != 404 {
		t.Errorf("no match: expected 404, got %d", status)
	}
}

// --- Multiple Services ---

func TestMultipleServices(t *testing.T) {
	upA := upstream(t, "service-A")
	upB := upstream(t, "service-B")
	portA := freePort(t)
	portB := freePort(t)

	cfg := writeConfig(t, fmt.Sprintf(`{
		services: {
			svc_a: {
				listen: "127.0.0.1:%d",
				routes: [
					{ action: { type: "proxy", upstream: "127.0.0.1:%d" } }
				]
			},
			svc_b: {
				listen: "127.0.0.1:%d",
				routes: [
					{ action: { type: "proxy", upstream: "127.0.0.1:%d" } }
				]
			}
		}
	}`, portA, upA, portB, upB))

	stop := startProx(t, cfg, portA)
	defer stop()
	waitForPort(t, portB, 3*time.Second)

	_, bodyA := httpGet(t, fmt.Sprintf("http://127.0.0.1:%d/", portA))
	mustContain(t, bodyA, "service-A", "service A")

	_, bodyB := httpGet(t, fmt.Sprintf("http://127.0.0.1:%d/", portB))
	mustContain(t, bodyB, "service-B", "service B")
}

// --- Wildcard Domain ---

func TestRouting_WildcardDomain(t *testing.T) {
	upPort := upstream(t, "wildcard-hit")
	proxyPort := freePort(t)

	cfg := writeConfig(t, fmt.Sprintf(`{
		services: {
			web: {
				listen: "127.0.0.1:%d",
				routes: [
					{
						match: { domain: "*.example.com" },
						action: { type: "proxy", upstream: "127.0.0.1:%d" }
					},
					{
						action: { type: "static", status: 404 }
					}
				]
			}
		}
	}`, proxyPort, upPort))

	stop := startProx(t, cfg, proxyPort)
	defer stop()

	url := fmt.Sprintf("http://127.0.0.1:%d/", proxyPort)

	// Match wildcard.
	_, body := httpGet(t, url, "Host", "api.example.com")
	mustContain(t, body, "wildcard-hit", "wildcard domain match")

	// No match.
	status, _ := httpGet(t, url, "Host", "other.org")
	if status != 404 {
		t.Errorf("non-matching domain: expected 404, got %d", status)
	}
}

// --- Transport Config ---

func TestConfig_TransportSettings(t *testing.T) {
	cfg := writeConfig(t, `{
		services: {
			web: {
				listen: ":8080",
				config: {
					max_idle_conns: 512,
					max_idle_conns_per_host: 64,
					read_buffer_size: 32768,
					write_buffer_size: 32768,
					disable_compression: false
				},
				routes: [
					{ action: { type: "proxy", upstream: "localhost:3000" } }
				]
			}
		}
	}`)

	result, err := config.LoadFile(cfg)
	if err != nil {
		t.Fatalf("config load: %v", err)
	}

	svc := result.Config.Services["web"]
	if svc.Config == nil {
		t.Fatal("service config is nil")
	}
	if svc.Config.MaxIdleConns != 512 {
		t.Errorf("max_idle_conns: expected 512, got %d", svc.Config.MaxIdleConns)
	}
	if svc.Config.MaxIdleConnsPerHost != 64 {
		t.Errorf("max_idle_conns_per_host: expected 64, got %d", svc.Config.MaxIdleConnsPerHost)
	}
	if svc.Config.ReadBufferSize != 32768 {
		t.Errorf("read_buffer_size: expected 32768, got %d", svc.Config.ReadBufferSize)
	}
	if svc.Config.WriteBufferSize != 32768 {
		t.Errorf("write_buffer_size: expected 32768, got %d", svc.Config.WriteBufferSize)
	}
	if svc.Config.DisableCompression == nil || *svc.Config.DisableCompression != false {
		t.Error("disable_compression: expected false")
	}
}

// --- Speed Limiting Config ---

func TestConfig_SpeedParsed(t *testing.T) {
	cfg := writeConfig(t, `{
		services: {
			web: {
				listen: ":8080",
				routes: [
					{
						speed: { download_mbps: 50, upload_mbps: 10, shared: true },
						action: { type: "proxy", upstream: "localhost:3000" }
					}
				]
			}
		}
	}`)

	result, err := config.LoadFile(cfg)
	if err != nil {
		t.Fatalf("config load: %v", err)
	}

	route := result.Config.Services["web"].Routes[0]
	if route.Speed == nil {
		t.Fatal("speed config is nil")
	}
	if route.Speed.DownloadMbps != 50 {
		t.Errorf("download_mbps: expected 50, got %v", route.Speed.DownloadMbps)
	}
	if route.Speed.UploadMbps != 10 {
		t.Errorf("upload_mbps: expected 10, got %v", route.Speed.UploadMbps)
	}
	if !route.Speed.Shared {
		t.Error("shared: expected true")
	}
}

func TestConfig_SpeedFractional(t *testing.T) {
	cfg := writeConfig(t, `{
		services: {
			web: {
				listen: ":8080",
				routes: [
					{
						speed: { download_mbps: 0.5 },
						action: { type: "proxy", upstream: "localhost:3000" }
					}
				]
			}
		}
	}`)

	result, err := config.LoadFile(cfg)
	if err != nil {
		t.Fatalf("config load: %v", err)
	}

	route := result.Config.Services["web"].Routes[0]
	if route.Speed == nil {
		t.Fatal("speed config is nil")
	}
	if route.Speed.DownloadMbps != 0.5 {
		t.Errorf("download_mbps: expected 0.5, got %v", route.Speed.DownloadMbps)
	}
}

func TestConfig_SpeedValidationNegative(t *testing.T) {
	cfg := writeConfig(t, `{
		services: {
			web: {
				listen: ":8080",
				routes: [
					{
						speed: { download_mbps: -10 },
						action: { type: "proxy", upstream: "localhost:3000" }
					}
				]
			}
		}
	}`)

	_, err := config.LoadFile(cfg)
	if err == nil {
		t.Error("expected error for negative download_mbps, got nil")
	}
}

func TestConfig_SpeedValidationZeroBoth(t *testing.T) {
	cfg := writeConfig(t, `{
		services: {
			web: {
				listen: ":8080",
				routes: [
					{
						speed: { shared: true },
						action: { type: "proxy", upstream: "localhost:3000" }
					}
				]
			}
		}
	}`)

	_, err := config.LoadFile(cfg)
	if err == nil {
		t.Error("expected error when both mbps are 0, got nil")
	}
}

// --- Speed Limiting E2E ---

func TestProxy_SpeedLimit(t *testing.T) {
	// Upstream that responds with a known payload size.
	payloadSize := 256 * 1024 // 256 KB
	upPort := freePort(t)
	payload := make([]byte, payloadSize)
	for i := range payload {
		payload[i] = 'x'
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", payloadSize))
		_, _ = w.Write(payload)
	})
	srv := &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", upPort),
		Handler: mux,
	}
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { srv.Close() })
	waitForPort(t, upPort, 3*time.Second)

	proxyPort := freePort(t)

	// 0.5 Mbps = 62,500 bytes/sec.
	// 256 KB at 62,500 B/s takes ~4s, minus ~1s burst = ~3s effective.
	cfg := writeConfig(t, fmt.Sprintf(`{
		services: {
			web: {
				listen: "127.0.0.1:%d",
				routes: [
					{
						speed: { download_mbps: 0.5 },
						action: { type: "proxy", upstream: "127.0.0.1:%d" }
					}
				]
			}
		}
	}`, proxyPort, upPort))

	stop := startProx(t, cfg, proxyPort)
	defer stop()

	start := time.Now()
	status, body := httpGet(t, fmt.Sprintf("http://127.0.0.1:%d/", proxyPort))
	elapsed := time.Since(start)

	if status != 200 {
		t.Fatalf("expected 200, got %d", status)
	}
	if len(body) != payloadSize {
		t.Fatalf("expected %d bytes, got %d", payloadSize, len(body))
	}

	// Account for burst (first ~62KB is instant), so effective throttled
	// payload is ~194KB at 62,500 B/s = ~3.1s. Use 2s as safe lower bound.
	minExpected := 2 * time.Second
	if elapsed < minExpected {
		t.Errorf("speed limit not enforced: transferred %d bytes in %v (expected >= %v)", payloadSize, elapsed, minExpected)
	}
}

