package action

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/dortanes/prox/internal/config"
)

func TestServe_DirectoryMode(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>Home</h1>"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "css"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "css", "app.css"), []byte("body{}"), 0644); err != nil {
		t.Fatal(err)
	}

	act := &config.Action{
		Type: config.ActionTypeServe,
		Root: dir,
	}

	handler, err := NewServe(act, "/*")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tests := []struct {
		name       string
		path       string
		wantStatus int
		wantBody   string
	}{
		{"root serves index.html", "/", 200, "<h1>Home</h1>"},
		{"nested file", "/css/app.css", 200, "body{}"},
		{"missing file", "/nope.txt", 404, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("GET", tc.path, nil)
			handler.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("path %q: expected status %d, got %d", tc.path, tc.wantStatus, rec.Code)
			}
			if tc.wantBody != "" && rec.Body.String() != tc.wantBody {
				t.Errorf("path %q: expected body %q, got %q", tc.path, tc.wantBody, rec.Body.String())
			}
		})
	}
}

func TestServe_DirectoryNoListing(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "subdir"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "subdir", "file.txt"), []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}

	act := &config.Action{
		Type: config.ActionTypeServe,
		Root: dir,
	}

	handler, err := NewServe(act, "/*")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Root has no index.html → 404.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 for directory without index.html, got %d", rec.Code)
	}
}

func TestServe_PrefixStripping(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.js"), []byte("console.log('hi')"), 0644); err != nil {
		t.Fatal(err)
	}

	act := &config.Action{
		Type: config.ActionTypeServe,
		Root: dir,
	}

	// Route is /static/*, so /static/app.js → dir/app.js.
	handler, err := NewServe(act, "/static/*")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/static/app.js", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if body := rec.Body.String(); body != "console.log('hi')" {
		t.Errorf("expected js content, got %q", body)
	}
}

func TestServe_FileMode(t *testing.T) {
	// Single file mode: any path serves the same file (SPA fallback).
	dir := t.TempDir()
	htmlPath := filepath.Join(dir, "index.html")
	if err := os.WriteFile(htmlPath, []byte("<app/>"), 0644); err != nil {
		t.Fatal(err)
	}

	act := &config.Action{
		Type: config.ActionTypeServe,
		File: htmlPath,
	}

	handler, err := NewServe(act, "/app/*")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Any path should serve the same file.
	paths := []string{"/app/", "/app/dashboard", "/app/settings/profile"}
	for _, p := range paths {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", p, nil)
		handler.ServeHTTP(rec, req)

		if rec.Code != 200 {
			t.Errorf("path %q: expected 200, got %d", p, rec.Code)
		}
		if body := rec.Body.String(); body != "<app/>" {
			t.Errorf("path %q: expected '<app/>', got %q", p, body)
		}
	}
}
