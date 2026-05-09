package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad_ValidConfig(t *testing.T) {
	raw := `{
		"services": {
			"web": {
				"listen": ":8080",
				"routes": [
					{
						"match": { "path": "/api/*" },
						"action": "backend"
					},
					{
						"match": { "path": "/health", "methods": ["GET"] },
						"action": "health_check"
					}
				]
			}
		},
		"actions": {
			"backend": {
				"type": "proxy",
				"upstream": "localhost:3000",
				"timeout": "5s"
			},
			"health_check": {
				"type": "static",
				"status": 200,
				"headers": { "Content-Type": "text/plain" },
				"body_ref": "health_body"
			}
		},
		"resources": {
			"health_body": {
				"text": "OK"
			}
		}
	}`

	cfg, err := Load([]byte(raw))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if len(cfg.Services) != 1 {
		t.Errorf("expected 1 service, got %d", len(cfg.Services))
	}

	web := cfg.Services["web"]
	if web.Listen != ":8080" {
		t.Errorf("expected listen :8080, got %s", web.Listen)
	}
	if len(web.Routes) != 2 {
		t.Errorf("expected 2 routes, got %d", len(web.Routes))
	}

	backend := cfg.Actions["backend"]
	if backend.Type != ActionTypeProxy {
		t.Errorf("expected proxy type, got %s", backend.Type)
	}
	if backend.Timeout.Seconds() != 5 {
		t.Errorf("expected 5s timeout, got %v", backend.Timeout)
	}
}

func TestLoad_BrokenActionRef(t *testing.T) {
	raw := `{
		"services": {
			"web": {
				"listen": ":80",
				"routes": [
					{
						"match": { "path": "/" },
						"action": "nonexistent"
					}
				]
			}
		},
		"actions": {
			"real": { "type": "proxy", "upstream": "localhost:3000" }
		}
	}`

	_, err := Load([]byte(raw))
	if err == nil {
		t.Fatal("expected validation error for broken action ref")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("error should mention the broken ref, got: %v", err)
	}
}

func TestLoad_BrokenBodyRef(t *testing.T) {
	raw := `{
		"services": {
			"web": {
				"listen": ":80",
				"routes": [
					{
						"match": { "path": "/" },
						"action": "page"
					}
				]
			}
		},
		"actions": {
			"page": {
				"type": "static",
				"status": 200,
				"body_ref": "missing_resource"
			}
		}
	}`

	_, err := Load([]byte(raw))
	if err == nil {
		t.Fatal("expected validation error for broken body_ref")
	}
	if !strings.Contains(err.Error(), "missing_resource") {
		t.Errorf("error should mention the broken ref, got: %v", err)
	}
}

func TestLoad_NoServices(t *testing.T) {
	raw := `{
		"services": {},
		"actions": {
			"a": { "type": "proxy", "upstream": "localhost:80" }
		}
	}`

	_, err := Load([]byte(raw))
	if err == nil {
		t.Fatal("expected error for empty services")
	}
	if !strings.Contains(err.Error(), "no services defined") {
		t.Errorf("expected 'no services defined', got: %v", err)
	}
}

func TestLoad_InvalidMethod(t *testing.T) {
	raw := `{
		"services": {
			"web": {
				"listen": ":80",
				"routes": [
					{
						"match": { "path": "/", "methods": ["FOOBAR"] },
						"action": "a"
					}
				]
			}
		},
		"actions": {
			"a": { "type": "proxy", "upstream": "localhost:80" }
		}
	}`

	_, err := Load([]byte(raw))
	if err == nil {
		t.Fatal("expected error for invalid HTTP method")
	}
	if !strings.Contains(err.Error(), "FOOBAR") {
		t.Errorf("error should mention the invalid method, got: %v", err)
	}
}

func TestLoad_UnknownActionType(t *testing.T) {
	raw := `{
		"services": {
			"web": {
				"listen": ":80",
				"routes": [
					{
						"match": { "path": "/" },
						"action": "a"
					}
				]
			}
		},
		"actions": {
			"a": { "type": "grpc", "upstream": "localhost:80" }
		}
	}`

	_, err := Load([]byte(raw))
	if err == nil {
		t.Fatal("expected error for unknown action type")
	}
	if !strings.Contains(err.Error(), "grpc") {
		t.Errorf("error should mention the unknown type, got: %v", err)
	}
}

func TestLoad_MultipleIssues(t *testing.T) {
	raw := `{
		"services": {
			"web": {
				"listen": "",
				"routes": [
					{
						"match": { "path": "" },
						"action": ""
					}
				]
			}
		},
		"actions": {}
	}`

	_, err := Load([]byte(raw))
	if err == nil {
		t.Fatal("expected validation errors")
	}

	if !IsValidationError(err) {
		t.Fatalf("expected ValidationError, got %T: %v", err, err)
	}

	ve := err.(*ValidationError)
	if len(ve.Issues) < 2 {
		t.Errorf("expected multiple issues, got %d: %v", len(ve.Issues), ve.Issues)
	}
}

func TestLoad_InvalidJSON(t *testing.T) {
	_, err := Load([]byte(`{not json`))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestLoad_WildcardInMiddle(t *testing.T) {
	raw := `{
		"services": {
			"web": {
				"listen": ":80",
				"routes": [
					{
						"match": { "path": "/api/*/users" },
						"action": "a"
					}
				]
			}
		},
		"actions": {
			"a": { "type": "proxy", "upstream": "localhost:80" }
		}
	}`

	_, err := Load([]byte(raw))
	if err == nil {
		t.Fatal("expected error for wildcard in middle of path")
	}
	if !strings.Contains(err.Error(), "wildcard") {
		t.Errorf("error should mention wildcard, got: %v", err)
	}
}

func TestLoad_ExampleConfig(t *testing.T) {
	raw := `{
		"services": {
			"main_site": {
				"listen": ":8443",
				"routes": [
					{
						"match": {
							"path": "/styles.css",
							"methods": ["GET"]
						},
						"action": "serve_static_css"
					},
					{
						"match": { "path": "/api/*" },
						"action": "proxy_to_backend"
					}
				]
			}
		},
		"actions": {
			"serve_static_css": {
				"type": "static",
				"status": 200,
				"headers": { "Content-Type": "text/css" },
				"body_ref": "css_content"
			},
			"proxy_to_backend": {
				"type": "proxy",
				"upstream": "localhost:8080",
				"timeout": "5s"
			}
		},
		"resources": {
			"css_content": {
				"text": "body { background: #000; }"
			}
		}
	}`

	cfg, err := Load([]byte(raw))
	if err != nil {
		t.Fatalf("example config should be valid, got: %v", err)
	}

	if _, ok := cfg.Services["main_site"]; !ok {
		t.Error("expected main_site service")
	}
}

func TestLoad_TLSRequiresCert(t *testing.T) {
	raw := `{
		"services": {
			"secure": {
				"listen": ":443",
				"tls": true,
				"routes": [
					{
						"match": { "path": "/" },
						"action": "a"
					}
				]
			}
		},
		"actions": {
			"a": { "type": "proxy", "upstream": "localhost:80" }
		}
	}`

	_, err := Load([]byte(raw))
	if err == nil {
		t.Fatal("expected error when tls is true but cert missing")
	}
	if !strings.Contains(err.Error(), "tls_cert") {
		t.Errorf("error should mention tls_cert, got: %v", err)
	}
}

func TestLoad_TLSWithCertAndKey(t *testing.T) {
	raw := `{
		"services": {
			"secure": {
				"listen": ":443",
				"tls": true,
				"tls_cert": "/etc/ssl/cert.pem",
				"tls_key": "/etc/ssl/key.pem",
				"routes": [
					{
						"match": { "path": "/" },
						"action": "a"
					}
				]
			}
		},
		"actions": {
			"a": { "type": "proxy", "upstream": "localhost:80" }
		}
	}`

	cfg, err := Load([]byte(raw))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	svc := cfg.Services["secure"]
	if !svc.TLS {
		t.Error("expected TLS to be true")
	}
	if svc.TLSCert != "/etc/ssl/cert.pem" {
		t.Errorf("expected cert path, got %q", svc.TLSCert)
	}
}

func TestLoad_InlineAction(t *testing.T) {
	raw := `{
		"services": {
			"web": {
				"listen": ":8080",
				"routes": [
					{
						"match": { "path": "/hello" },
						"action": {
							"type": "static",
							"status": 200,
							"headers": { "Content-Type": "text/plain" },
							"body_ref": "greeting"
						}
					}
				]
			}
		},
		"actions": {},
		"resources": {
			"greeting": { "text": "Hello!" }
		}
	}`

	cfg, err := Load([]byte(raw))
	if err != nil {
		t.Fatalf("inline action should be valid, got: %v", err)
	}

	// After normalization, the inline action should be in the actions map.
	route := cfg.Services["web"].Routes[0]
	if route.Action.Name == "" {
		t.Fatal("expected inline action to be normalized to a named ref")
	}

	act, ok := cfg.Actions[route.Action.Name]
	if !ok {
		t.Fatalf("normalized action %q not found in actions map", route.Action.Name)
	}
	if act.Type != ActionTypeStatic {
		t.Errorf("expected static type, got %s", act.Type)
	}
	if act.Status != 200 {
		t.Errorf("expected status 200, got %d", act.Status)
	}
}

func TestLoad_InlineActionWithInlineResource(t *testing.T) {
	raw := `{
		"services": {
			"web": {
				"listen": ":8080",
				"routes": [
					{
						"match": { "path": "/hello" },
						"action": {
							"type": "static",
							"status": 200,
							"headers": { "Content-Type": "text/html" },
							"body_ref": {
								"text": "Straight from route :3"
							}
						}
					}
				]
			}
		}
	}`

	cfg, err := Load([]byte(raw))
	if err != nil {
		t.Fatalf("fully inline config should be valid, got: %v", err)
	}

	// Both action and resource should be normalized.
	route := cfg.Services["web"].Routes[0]
	act := cfg.Actions[route.Action.Name]
	if act.BodyRef.Name == "" {
		t.Fatal("expected inline resource to be normalized to a named ref")
	}

	res, ok := cfg.Resources[act.BodyRef.Name]
	if !ok {
		t.Fatalf("normalized resource %q not found", act.BodyRef.Name)
	}
	if res.Text != "Straight from route :3" {
		t.Errorf("expected inline text, got %q", res.Text)
	}
}

func TestLoad_JSON5Features(t *testing.T) {
	raw := `{
		// This is a comment
		services: {
			web: {
				listen: ":8080",
				routes: [
					{
						match: { path: "/" },
						action: "a",  // trailing comma
					},
				],  // trailing comma in array
			},
		},
		actions: {
			a: { type: "proxy", upstream: "localhost:80" },
		},
	}`

	_, err := Load([]byte(raw))
	if err != nil {
		t.Fatalf("JSON5 features should be supported, got: %v", err)
	}
}

// --- Nested config file tests ---

// helper: create a temp dir with files, return dir path.
func writeTestFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLoadFile_NestedServiceRef(t *testing.T) {
	dir := writeTestFiles(t, map[string]string{
		"main.json5": `{
			services: {
				web: "./web.json5",
			},
			actions: {
				health: { type: "static", status: 200 },
			},
		}`,
		"web.json5": `{
			listen: ":8080",
			routes: [
				{ match: { path: "/health" }, action: "health" },
				{ match: { path: "/*" }, action: "frontend" },
			],
			actions: {
				frontend: { type: "serve", root: "./public" },
			},
		}`,
	})

	result, err := LoadFile(filepath.Join(dir, "main.json5"))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	cfg := result.Config

	// Service should be resolved.
	web, ok := cfg.Services["web"]
	if !ok {
		t.Fatal("expected 'web' service")
	}
	if web.Listen != ":8080" {
		t.Errorf("expected listen :8080, got %s", web.Listen)
	}
	if len(web.Routes) != 2 {
		t.Errorf("expected 2 routes, got %d", len(web.Routes))
	}

	// Fragment actions should be merged.
	if _, ok := cfg.Actions["frontend"]; !ok {
		t.Error("expected 'frontend' action from fragment")
	}
	if _, ok := cfg.Actions["health"]; !ok {
		t.Error("expected 'health' action from root")
	}

	// Paths should include both files.
	if len(result.Paths) != 2 {
		t.Errorf("expected 2 paths, got %d: %v", len(result.Paths), result.Paths)
	}
}

func TestLoadFile_NestedWithResources(t *testing.T) {
	dir := writeTestFiles(t, map[string]string{
		"main.json5": `{
			services: {
				web: "./web.json5",
			},
		}`,
		"web.json5": `{
			listen: ":8080",
			routes: [
				{ match: { path: "/" }, action: "home" },
			],
			actions: {
				home: { type: "static", status: 200, body_ref: "page" },
			},
			resources: {
				page: { text: "Hello from fragment!" },
			},
		}`,
	})

	result, err := LoadFile(filepath.Join(dir, "main.json5"))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	res, ok := result.Config.Resources["page"]
	if !ok {
		t.Fatal("expected 'page' resource from fragment")
	}
	if res.Text != "Hello from fragment!" {
		t.Errorf("expected fragment text, got %q", res.Text)
	}
}

func TestLoadFile_DuplicateAction(t *testing.T) {
	dir := writeTestFiles(t, map[string]string{
		"main.json5": `{
			services: {
				web: "./web.json5",
			},
			actions: {
				health: { type: "static", status: 200 },
			},
		}`,
		"web.json5": `{
			listen: ":8080",
			routes: [
				{ match: { path: "/" }, action: "health" },
			],
			actions: {
				health: { type: "static", status: 204 },
			},
		}`,
	})

	_, err := LoadFile(filepath.Join(dir, "main.json5"))
	if err == nil {
		t.Fatal("expected error for duplicate action")
	}
	if !strings.Contains(err.Error(), "duplicate action") {
		t.Errorf("error should mention duplicate, got: %v", err)
	}
}

func TestLoadFile_DirectoryMode(t *testing.T) {
	dir := writeTestFiles(t, map[string]string{
		"web.json5": `{
			listen: ":8080",
			routes: [
				{ match: { path: "/*" }, action: "frontend" },
			],
			actions: {
				frontend: { type: "serve", root: "./public" },
			},
		}`,
		"api.json5": `{
			listen: ":9090",
			routes: [
				{ match: { path: "/*" }, action: "backend" },
			],
			actions: {
				backend: { type: "proxy", upstream: "localhost:3000" },
			},
		}`,
	})

	result, err := LoadFile(dir)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	cfg := result.Config

	if len(cfg.Services) != 2 {
		t.Fatalf("expected 2 services, got %d", len(cfg.Services))
	}

	if _, ok := cfg.Services["web"]; !ok {
		t.Error("expected 'web' service (from web.json5)")
	}
	if _, ok := cfg.Services["api"]; !ok {
		t.Error("expected 'api' service (from api.json5)")
	}

	// Actions from both fragments should be merged.
	if _, ok := cfg.Actions["frontend"]; !ok {
		t.Error("expected 'frontend' action")
	}
	if _, ok := cfg.Actions["backend"]; !ok {
		t.Error("expected 'backend' action")
	}
}

func TestLoadFile_DirectorySkipsNonJSON5(t *testing.T) {
	dir := writeTestFiles(t, map[string]string{
		"web.json5": `{
			listen: ":8080",
			routes: [
				{ match: { path: "/*" }, action: "a" },
			],
			actions: {
				a: { type: "proxy", upstream: "localhost:80" },
			},
		}`,
		"readme.txt": "this should be ignored",
		".hidden":    "this too",
	})

	result, err := LoadFile(dir)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if len(result.Config.Services) != 1 {
		t.Errorf("expected 1 service, got %d", len(result.Config.Services))
	}
}

func TestLoadFile_NestedDirRef(t *testing.T) {
	dir := writeTestFiles(t, map[string]string{
		"main.json5": `{
			services: {
				_dir: "./services/",
			},
			actions: {
				shared_health: { type: "static", status: 200 },
			},
		}`,
		"services/web.json5": `{
			listen: ":8080",
			routes: [
				{ match: { path: "/*" }, action: "frontend" },
			],
			actions: {
				frontend: { type: "serve", root: "./public" },
			},
		}`,
		"services/api.json5": `{
			listen: ":9090",
			routes: [
				{ match: { path: "/*" }, action: "backend" },
			],
			actions: {
				backend: { type: "proxy", upstream: "localhost:3000" },
			},
		}`,
	})

	result, err := LoadFile(filepath.Join(dir, "main.json5"))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	cfg := result.Config

	// Directory services should be loaded with filename-based names.
	if _, ok := cfg.Services["web"]; !ok {
		t.Error("expected 'web' service from services/web.json5")
	}
	if _, ok := cfg.Services["api"]; !ok {
		t.Error("expected 'api' service from services/api.json5")
	}

	// Global action from root should be present.
	if _, ok := cfg.Actions["shared_health"]; !ok {
		t.Error("expected 'shared_health' action from root config")
	}
}

func TestLoadFile_MixedInlineAndRefs(t *testing.T) {
	dir := writeTestFiles(t, map[string]string{
		"main.json5": `{
			services: {
				web: "./web.json5",
				monitoring: {
					listen: ":9090",
					routes: [
						{ match: { path: "/*" }, action: "metrics" },
					],
				},
			},
			actions: {
				metrics: { type: "proxy", upstream: "localhost:9100" },
			},
		}`,
		"web.json5": `{
			listen: ":8080",
			routes: [
				{ match: { path: "/*" }, action: "frontend" },
			],
			actions: {
				frontend: { type: "serve", root: "./public" },
			},
		}`,
	})

	result, err := LoadFile(filepath.Join(dir, "main.json5"))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	cfg := result.Config

	if len(cfg.Services) != 2 {
		t.Fatalf("expected 2 services, got %d", len(cfg.Services))
	}
	if _, ok := cfg.Services["web"]; !ok {
		t.Error("expected 'web' service (from file ref)")
	}
	if _, ok := cfg.Services["monitoring"]; !ok {
		t.Error("expected 'monitoring' service (inline)")
	}
}

func TestLoadFile_InlineStillWorks(t *testing.T) {
	dir := writeTestFiles(t, map[string]string{
		"config.json5": `{
			services: {
				web: {
					listen: ":8080",
					routes: [
						{ match: { path: "/*" }, action: "a" },
					],
				},
			},
			actions: {
				a: { type: "proxy", upstream: "localhost:80" },
			},
		}`,
	})

	result, err := LoadFile(filepath.Join(dir, "config.json5"))
	if err != nil {
		t.Fatalf("existing inline config should still work, got: %v", err)
	}

	if len(result.Config.Services) != 1 {
		t.Errorf("expected 1 service, got %d", len(result.Config.Services))
	}
	if len(result.Paths) != 1 {
		t.Errorf("expected 1 path, got %d", len(result.Paths))
	}
}

// --- Route include tests ---

func TestLoadFile_RouteIncludeArray(t *testing.T) {
	dir := writeTestFiles(t, map[string]string{
		"main.json5": `{
			services: {
				web: {
					listen: ":8080",
					routes: [
						"./api_routes.json5",
						{ match: { path: "/*" }, action: "frontend" },
					],
				},
			},
			actions: {
				backend: { type: "proxy", upstream: "localhost:3000" },
				frontend: { type: "serve", root: "./public" },
			},
		}`,
		"api_routes.json5": `[
			{ match: { path: "/api/v1/*" }, action: "backend" },
			{ match: { path: "/api/v2/*" }, action: "backend" },
		]`,
	})

	result, err := LoadFile(filepath.Join(dir, "main.json5"))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	cfg := result.Config
	web := cfg.Services["web"]

	// 2 included + 1 inline = 3 routes.
	if len(web.Routes) != 3 {
		t.Fatalf("expected 3 routes, got %d", len(web.Routes))
	}

	// Order preserved: included routes first, then inline.
	if web.Routes[0].Match.Path != "/api/v1/*" {
		t.Errorf("route 0: expected /api/v1/*, got %s", web.Routes[0].Match.Path)
	}
	if web.Routes[1].Match.Path != "/api/v2/*" {
		t.Errorf("route 1: expected /api/v2/*, got %s", web.Routes[1].Match.Path)
	}
	if web.Routes[2].Match.Path != "/*" {
		t.Errorf("route 2: expected /*, got %s", web.Routes[2].Match.Path)
	}

	// Include file should be tracked for watcher.
	if len(result.Paths) != 2 {
		t.Errorf("expected 2 paths (main + include), got %d: %v", len(result.Paths), result.Paths)
	}
}

func TestLoadFile_RouteIncludeObjectWrapper(t *testing.T) {
	dir := writeTestFiles(t, map[string]string{
		"main.json5": `{
			services: {
				web: {
					listen: ":8080",
					routes: [
						"./realtime.json5",
						{ action: { type: "drop" } },
					],
				},
			},
		}`,
		"realtime.json5": `{
			routes: [
				{
					match: { domain: "*.**", path: "/ws" },
					action: { type: "proxy", upstream: "localhost:3505" },
				},
			],
		}`,
	})

	result, err := LoadFile(filepath.Join(dir, "main.json5"))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	web := result.Config.Services["web"]
	if len(web.Routes) != 2 {
		t.Fatalf("expected 2 routes, got %d", len(web.Routes))
	}
	if web.Routes[0].Match.Path != "/ws" {
		t.Errorf("route 0: expected /ws, got %s", web.Routes[0].Match.Path)
	}
}

func TestLoadFile_RouteIncludeInFragment(t *testing.T) {
	dir := writeTestFiles(t, map[string]string{
		"main.json5": `{
			services: {
				gateway: "./gateway.json5",
			},
		}`,
		"gateway.json5": `{
			listen: ":443",
			tls: true,
			tls_cert: "/etc/certs/",
			routes: [
				"./routes/realtime.json5",
				"./routes/fallback.json5",
			],
		}`,
		"routes/realtime.json5": `[
			{
				match: { domain: "*.**", path: "/ws" },
				action: { type: "proxy", upstream: "localhost:3505" },
			},
		]`,
		"routes/fallback.json5": `[
			{ action: { type: "drop" } },
		]`,
	})

	result, err := LoadFile(filepath.Join(dir, "main.json5"))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	gw := result.Config.Services["gateway"]
	if len(gw.Routes) != 2 {
		t.Fatalf("expected 2 routes, got %d", len(gw.Routes))
	}

	// All files should be tracked: main + gateway + 2 route includes.
	if len(result.Paths) != 4 {
		t.Errorf("expected 4 paths, got %d: %v", len(result.Paths), result.Paths)
	}
}

func TestLoadFile_RouteIncludeMultiple(t *testing.T) {
	dir := writeTestFiles(t, map[string]string{
		"main.json5": `{
			services: {
				web: {
					listen: ":8080",
					routes: [
						"./routes/a.json5",
						{ match: { path: "/middle" }, action: "mid" },
						"./routes/b.json5",
					],
				},
			},
			actions: {
				a1: { type: "proxy", upstream: "localhost:3001" },
				a2: { type: "proxy", upstream: "localhost:3002" },
				mid: { type: "proxy", upstream: "localhost:4000" },
				b1: { type: "proxy", upstream: "localhost:5001" },
			},
		}`,
		"routes/a.json5": `[
			{ match: { path: "/a1" }, action: "a1" },
			{ match: { path: "/a2" }, action: "a2" },
		]`,
		"routes/b.json5": `[
			{ match: { path: "/b1" }, action: "b1" },
		]`,
	})

	result, err := LoadFile(filepath.Join(dir, "main.json5"))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	web := result.Config.Services["web"]
	// 2 from a.json5 + 1 inline + 1 from b.json5 = 4.
	if len(web.Routes) != 4 {
		t.Fatalf("expected 4 routes, got %d", len(web.Routes))
	}

	// Verify order: a1, a2, middle, b1.
	paths := make([]string, len(web.Routes))
	for i, r := range web.Routes {
		if r.Match != nil {
			paths[i] = r.Match.Path
		}
	}
	expected := []string{"/a1", "/a2", "/middle", "/b1"}
	for i, e := range expected {
		if paths[i] != e {
			t.Errorf("route %d: expected %s, got %s", i, e, paths[i])
		}
	}
}

func TestLoadFile_RouteIncludeCircular(t *testing.T) {
	dir := writeTestFiles(t, map[string]string{
		"main.json5": `{
			services: {
				web: {
					listen: ":8080",
					routes: [
						"./routes.json5",
					],
				},
			},
			actions: {
				a: { type: "proxy", upstream: "localhost:80" },
			},
		}`,
		// This file is also loaded as the main config path tracker,
		// but we can create a self-referencing loop via includes.
		"routes.json5": `[
			{ match: { path: "/" }, action: "a" },
		]`,
	})

	// This should work — routes.json5 is only loaded once.
	result, err := LoadFile(filepath.Join(dir, "main.json5"))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if len(result.Config.Services["web"].Routes) != 1 {
		t.Errorf("expected 1 route, got %d", len(result.Config.Services["web"].Routes))
	}
}

func TestLoadFile_RouteIncludeMissing(t *testing.T) {
	dir := writeTestFiles(t, map[string]string{
		"main.json5": `{
			services: {
				web: {
					listen: ":8080",
					routes: [
						"./nonexistent.json5",
					],
				},
			},
		}`,
	})

	_, err := LoadFile(filepath.Join(dir, "main.json5"))
	if err == nil {
		t.Fatal("expected error for missing route include file")
	}
	if !strings.Contains(err.Error(), "nonexistent.json5") {
		t.Errorf("error should mention the missing file, got: %v", err)
	}
}

func TestLoadFile_RouteIncludeDirectoryMode(t *testing.T) {
	dir := writeTestFiles(t, map[string]string{
		"web.json5": `{
			listen: ":8080",
			routes: [
				"./routes/api.json5",
			],
			actions: {
				api: { type: "proxy", upstream: "localhost:3000" },
			},
		}`,
		"routes/api.json5": `[
			{ match: { path: "/api/*" }, action: "api" },
		]`,
	})

	result, err := LoadFile(dir) // directory mode
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	web := result.Config.Services["web"]
	if len(web.Routes) != 1 {
		t.Fatalf("expected 1 route, got %d", len(web.Routes))
	}
	if web.Routes[0].Match.Path != "/api/*" {
		t.Errorf("expected /api/*, got %s", web.Routes[0].Match.Path)
	}
}

// --- Balancer tests ---

func TestLoad_BalancerRoundRobin(t *testing.T) {
	raw := `{
		services: {
			web: {
				listen: ":8080",
				routes: [
					{
						match: { domain: "*.example.com", path: "/ws" },
						balancer: {
							type: "roundrobin",
							targets: ["10.0.0.1:3505", "10.0.0.2:3505"],
						},
						action: "balanced",
					},
				],
			},
		},
		actions: {
			balanced: { type: "proxy", upstream: "{target}" },
		},
	}`

	cfg, err := Load([]byte(raw))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	route := cfg.Services["web"].Routes[0]
	if route.Balancer == nil {
		t.Fatal("expected balancer to be set")
	}
	if route.Balancer.Type != BalancerRoundRobin {
		t.Errorf("expected roundrobin, got %s", route.Balancer.Type)
	}
	if len(route.Balancer.Targets) != 2 {
		t.Errorf("expected 2 targets, got %d", len(route.Balancer.Targets))
	}
}

func TestLoad_BalancerMissingType(t *testing.T) {
	raw := `{
		services: {
			web: {
				listen: ":8080",
				routes: [
					{
						match: { path: "/ws" },
						balancer: {
							targets: ["10.0.0.1:3505"],
						},
						action: "balanced",
					},
				],
			},
		},
		actions: {
			balanced: { type: "proxy", upstream: "{target}" },
		},
	}`

	_, err := Load([]byte(raw))
	if err == nil {
		t.Fatal("expected error for missing balancer type")
	}
	if !strings.Contains(err.Error(), "balancer.type is required") {
		t.Errorf("error should mention balancer type, got: %v", err)
	}
}

func TestLoad_BalancerEmptyTargets(t *testing.T) {
	raw := `{
		services: {
			web: {
				listen: ":8080",
				routes: [
					{
						match: { path: "/ws" },
						balancer: {
							type: "roundrobin",
							targets: [],
						},
						action: "balanced",
					},
				],
			},
		},
		actions: {
			balanced: { type: "proxy", upstream: "{target}" },
		},
	}`

	_, err := Load([]byte(raw))
	if err == nil {
		t.Fatal("expected error for empty targets")
	}
	if !strings.Contains(err.Error(), "at least one entry") {
		t.Errorf("error should mention empty targets, got: %v", err)
	}
}

func TestLoad_BalancerDuplicateTargets(t *testing.T) {
	raw := `{
		services: {
			web: {
				listen: ":8080",
				routes: [
					{
						match: { path: "/ws" },
						balancer: {
							type: "roundrobin",
							targets: ["10.0.0.1:3505", "10.0.0.1:3505"],
						},
						action: "balanced",
					},
				],
			},
		},
		actions: {
			balanced: { type: "proxy", upstream: "{target}" },
		},
	}`

	_, err := Load([]byte(raw))
	if err == nil {
		t.Fatal("expected error for duplicate targets")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error should mention duplicate, got: %v", err)
	}
}

func TestLoad_BalancerWrongActionType(t *testing.T) {
	raw := `{
		services: {
			web: {
				listen: ":8080",
				routes: [
					{
						match: { path: "/ws" },
						balancer: {
							type: "roundrobin",
							targets: ["10.0.0.1:3505"],
						},
						action: "page",
					},
				],
			},
		},
		actions: {
			page: { type: "static", status: 200 },
		},
	}`

	_, err := Load([]byte(raw))
	if err == nil {
		t.Fatal("expected error for balancer on static action")
	}
	if !strings.Contains(err.Error(), "only supported with proxy or pass") {
		t.Errorf("error should mention supported action types, got: %v", err)
	}
}

func TestLoad_BalancerMissingTargetPlaceholder(t *testing.T) {
	raw := `{
		services: {
			web: {
				listen: ":8080",
				routes: [
					{
						match: { path: "/ws" },
						balancer: {
							type: "roundrobin",
							targets: ["10.0.0.1:3505"],
						},
						action: "api",
					},
				],
			},
		},
		actions: {
			api: { type: "proxy", upstream: "localhost:3000" },
		},
	}`

	_, err := Load([]byte(raw))
	if err == nil {
		t.Fatal("expected error when upstream doesn't contain {target}")
	}
	if !strings.Contains(err.Error(), "{target}") {
		t.Errorf("error should mention {target} placeholder, got: %v", err)
	}
}

func TestLoad_BalancerInlineAction(t *testing.T) {
	raw := `{
		services: {
			web: {
				listen: ":8080",
				routes: [
					{
						match: { domain: "*.example.com", path: "/ws" },
						balancer: {
							type: "random",
							targets: ["10.0.0.1:3505", "10.0.0.2:3505", "10.0.0.3:3505"],
						},
						action: {
							type: "proxy",
							upstream: "{target}",
						},
					},
				],
			},
		},
	}`

	cfg, err := Load([]byte(raw))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	route := cfg.Services["web"].Routes[0]
	if route.Balancer == nil {
		t.Fatal("expected balancer to be set")
	}
	if route.Balancer.Type != BalancerRandom {
		t.Errorf("expected random, got %s", route.Balancer.Type)
	}
	if len(route.Balancer.Targets) != 3 {
		t.Errorf("expected 3 targets, got %d", len(route.Balancer.Targets))
	}
}

func TestLoad_BalancerLeastConn(t *testing.T) {
	raw := `{
		services: {
			web: {
				listen: ":8080",
				routes: [
					{
						match: { domain: "*.example.com", path: "/ws" },
						balancer: {
							type: "leastconn",
							targets: ["10.0.0.1:3505", "10.0.0.2:3505", "10.0.0.3:3505"],
						},
						action: "balanced",
					},
				],
			},
		},
		actions: {
			balanced: { type: "proxy", upstream: "{target}" },
		},
	}`

	cfg, err := Load([]byte(raw))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	route := cfg.Services["web"].Routes[0]
	if route.Balancer == nil {
		t.Fatal("expected balancer to be set")
	}
	if route.Balancer.Type != BalancerLeastConn {
		t.Errorf("expected leastconn, got %s", route.Balancer.Type)
	}
	if len(route.Balancer.Targets) != 3 {
		t.Errorf("expected 3 targets, got %d", len(route.Balancer.Targets))
	}
}
