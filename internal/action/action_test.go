package action

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dortanes/prox/internal/config"
	"github.com/dortanes/prox/internal/resource"
)

func TestStatic_BasicResponse(t *testing.T) {
	resolver := resource.NewResolver(map[string]*config.Resource{
		"greeting": {Text: "Hello, World!"},
	})

	act := &config.Action{
		Type:    config.ActionTypeStatic,
		Status:  200,
		Headers: map[string]string{"Content-Type": "text/plain"},
		BodyRef: config.ResourceRef{Name: "greeting"},
	}

	handler, err := NewStatic(act, resolver)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain" {
		t.Errorf("expected Content-Type text/plain, got %q", ct)
	}
	if body := rec.Body.String(); body != "Hello, World!" {
		t.Errorf("expected body 'Hello, World!', got %q", body)
	}
}

func TestStatic_NoBody(t *testing.T) {
	resolver := resource.NewResolver(nil)

	act := &config.Action{
		Type:   config.ActionTypeStatic,
		Status: 204,
	}

	handler, err := NewStatic(act, resolver)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != 204 {
		t.Errorf("expected status 204, got %d", rec.Code)
	}
}

func TestProxy_ForwardsToUpstream(t *testing.T) {
	// Create a test upstream server.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream", "reached")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("upstream response"))
	}))
	defer upstream.Close()

	act := &config.Action{
		Type:     config.ActionTypeProxy,
		Upstream: upstream.URL,
	}

	handler, err := NewProxy(act, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/test", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
	if rec.Header().Get("X-Upstream") != "reached" {
		t.Error("expected X-Upstream header from upstream")
	}

	body, _ := io.ReadAll(rec.Body)
	if string(body) != "upstream response" {
		t.Errorf("expected 'upstream response', got %q", string(body))
	}
}

func TestProxy_CustomHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// In Go, Host header is stored in r.Host, not r.Header.
		if r.Host != "custom.example.com" {
			t.Errorf("expected Host 'custom.example.com', got %q", r.Host)
		}
		if r.Header.Get("X-Custom") != "value" {
			t.Errorf("expected X-Custom header 'value', got %q", r.Header.Get("X-Custom"))
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	act := &config.Action{
		Type:     config.ActionTypeProxy,
		Upstream: upstream.URL,
		Headers: map[string]string{
			"Host":     "custom.example.com",
			"X-Custom": "value",
		},
	}

	handler, err := NewProxy(act, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
}

func TestProxy_UpstreamDown(t *testing.T) {
	act := &config.Action{
		Type:     config.ActionTypeProxy,
		Upstream: "localhost:1", // Nothing listening on port 1.
		Timeout:  config.Duration{},
	}

	handler, err := NewProxy(act, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("expected 502 Bad Gateway, got %d", rec.Code)
	}
}

func TestBuild_Registry(t *testing.T) {
	resolver := resource.NewResolver(map[string]*config.Resource{
		"body": {Text: "test"},
	})

	actions := map[string]*config.Action{
		"proxy_action": {
			Type:     config.ActionTypeProxy,
			Upstream: "localhost:3000",
		},
		"static_action": {
			Type:    config.ActionTypeStatic,
			Status:  200,
			BodyRef: config.ResourceRef{Name: "body"},
		},
	}

	reg, err := Build(actions, resolver, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if reg.Get("proxy_action") == nil {
		t.Error("expected proxy_action handler")
	}
	if reg.Get("static_action") == nil {
		t.Error("expected static_action handler")
	}
	if reg.Get("nonexistent") != nil {
		t.Error("expected nil for nonexistent action")
	}
}
