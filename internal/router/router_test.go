package router

import (
	"net/http"
	"testing"

	"github.com/dortanes/prox/internal/config"
)

func TestRouter_ExactMatch(t *testing.T) {
	rt := New([]*config.Route{
		{
			Match:  &config.Match{Path: "/styles.css"},
			Action: config.ActionRef{Name: "serve_css"},
		},
	})

	r, _ := http.NewRequest("GET", "/styles.css", nil)
	_, got := rt.Match(r)
	if got != "serve_css" {
		t.Errorf("expected serve_css, got %q", got)
	}
}

func TestRouter_WildcardMatch(t *testing.T) {
	rt := New([]*config.Route{
		{
			Match:  &config.Match{Path: "/api/*"},
			Action: config.ActionRef{Name: "proxy_backend"},
		},
	})

	tests := []struct {
		path string
		want string
	}{
		{"/api/users", "proxy_backend"},
		{"/api/users/123", "proxy_backend"},
		{"/api/", "proxy_backend"},
		{"/other", ""},
	}

	for _, tc := range tests {
		r, _ := http.NewRequest("GET", tc.path, nil)
		_, got := rt.Match(r)
		if got != tc.want {
			t.Errorf("path %q: expected %q, got %q", tc.path, tc.want, got)
		}
	}
}

func TestRouter_MethodFilter(t *testing.T) {
	rt := New([]*config.Route{
		{
			Match:  &config.Match{Path: "/data", Methods: []string{"GET", "HEAD"}},
			Action: config.ActionRef{Name: "get_data"},
		},
		{
			Match:  &config.Match{Path: "/data", Methods: []string{"POST"}},
			Action: config.ActionRef{Name: "post_data"},
		},
	})

	tests := []struct {
		method string
		want   string
	}{
		{"GET", "get_data"},
		{"HEAD", "get_data"},
		{"POST", "post_data"},
		{"DELETE", ""},
	}

	for _, tc := range tests {
		r, _ := http.NewRequest(tc.method, "/data", nil)
		_, got := rt.Match(r)
		if got != tc.want {
			t.Errorf("method %s: expected %q, got %q", tc.method, tc.want, got)
		}
	}
}

func TestRouter_FirstMatchWins(t *testing.T) {
	rt := New([]*config.Route{
		{
			Match:  &config.Match{Path: "/api/special"},
			Action: config.ActionRef{Name: "special"},
		},
		{
			Match:  &config.Match{Path: "/api/*"},
			Action: config.ActionRef{Name: "general"},
		},
	})

	r, _ := http.NewRequest("GET", "/api/special", nil)
	_, got := rt.Match(r)
	if got != "special" {
		t.Errorf("expected first-match 'special', got %q", got)
	}
}

func TestRouter_NoMatch(t *testing.T) {
	rt := New([]*config.Route{
		{
			Match:  &config.Match{Path: "/known"},
			Action: config.ActionRef{Name: "handler"},
		},
	})

	r, _ := http.NewRequest("GET", "/unknown", nil)
	_, got := rt.Match(r)
	if got != "" {
		t.Errorf("expected empty string for no match, got %q", got)
	}
}

func TestRouter_AllMethodsWhenEmpty(t *testing.T) {
	rt := New([]*config.Route{
		{
			Match:  &config.Match{Path: "/open"},
			Action: config.ActionRef{Name: "open"},
		},
	})

	methods := []string{"GET", "POST", "PUT", "DELETE", "PATCH", "OPTIONS"}
	for _, m := range methods {
		r, _ := http.NewRequest(m, "/open", nil)
		_, got := rt.Match(r)
		if got != "open" {
			t.Errorf("method %s: expected 'open', got %q", m, got)
		}
	}
}

// ── Domain matching tests ──────────────────────────────────────────────

func TestRouter_ExactDomain(t *testing.T) {
	rt := New([]*config.Route{
		{
			Match:  &config.Match{Domain: "example.com", Path: "/*"},
			Action: config.ActionRef{Name: "example"},
		},
	})

	tests := []struct {
		host string
		want string
	}{
		{"example.com", "example"},
		{"example.com:443", "example"},
		{"EXAMPLE.COM", "example"},
		{"other.com", ""},
		{"sub.example.com", ""},
	}

	for _, tc := range tests {
		r, _ := http.NewRequest("GET", "/", nil)
		r.Host = tc.host
		_, got := rt.Match(r)
		if got != tc.want {
			t.Errorf("host %q: expected %q, got %q", tc.host, tc.want, got)
		}
	}
}

func TestRouter_WildcardDomainPrefix(t *testing.T) {
	rt := New([]*config.Route{
		{
			Match:  &config.Match{Domain: "*.myapp.dev", Path: "/*"},
			Action: config.ActionRef{Name: "proxy"},
		},
	})

	tests := []struct {
		host string
		want string
	}{
		{"sub.myapp.dev", "proxy"},
		{"SUB.MYAPP.DEV", "proxy"},
		{"sub.myapp.dev:443", "proxy"},
		{"myapp.dev", ""},          // * matches exactly one segment
		{"deep.sub.myapp.dev", ""}, // too many segments
		{"other.click", ""},
	}

	for _, tc := range tests {
		r, _ := http.NewRequest("GET", "/", nil)
		r.Host = tc.host
		_, got := rt.Match(r)
		if got != tc.want {
			t.Errorf("host %q: expected %q, got %q", tc.host, tc.want, got)
		}
	}
}

func TestRouter_WildcardDomainMiddle(t *testing.T) {
	rt := New([]*config.Route{
		{
			Match:  &config.Match{Domain: "test.*.myapp.dev", Path: "/*"},
			Action: config.ActionRef{Name: "test_any"},
		},
	})

	tests := []struct {
		host string
		want string
	}{
		{"test.staging.myapp.dev", "test_any"},
		{"test.prod.myapp.dev", "test_any"},
		{"test.anything.myapp.dev", "test_any"},
		{"test.myapp.dev", ""},          // missing segment
		{"test.a.b.myapp.dev", ""},      // too many segments
		{"other.staging.myapp.dev", ""}, // first segment doesn't match
	}

	for _, tc := range tests {
		r, _ := http.NewRequest("GET", "/", nil)
		r.Host = tc.host
		_, got := rt.Match(r)
		if got != tc.want {
			t.Errorf("host %q: expected %q, got %q", tc.host, tc.want, got)
		}
	}
}

func TestRouter_WildcardDomainDeep(t *testing.T) {
	rt := New([]*config.Route{
		{
			Match:  &config.Match{Domain: "*.test.myapp.dev", Path: "/*"},
			Action: config.ActionRef{Name: "deep"},
		},
	})

	tests := []struct {
		host string
		want string
	}{
		{"api.test.myapp.dev", "deep"},
		{"web.test.myapp.dev", "deep"},
		{"test.myapp.dev", ""},     // no subdomain
		{"a.b.test.myapp.dev", ""}, // too many segments
	}

	for _, tc := range tests {
		r, _ := http.NewRequest("GET", "/", nil)
		r.Host = tc.host
		_, got := rt.Match(r)
		if got != tc.want {
			t.Errorf("host %q: expected %q, got %q", tc.host, tc.want, got)
		}
	}
}

func TestRouter_MultiWildcardDomain(t *testing.T) {
	rt := New([]*config.Route{
		{
			Match:  &config.Match{Domain: "*.*.myapp.dev", Path: "/*"},
			Action: config.ActionRef{Name: "double"},
		},
	})

	tests := []struct {
		host string
		want string
	}{
		{"a.b.myapp.dev", "double"},
		{"x.y.myapp.dev", "double"},
		{"a.myapp.dev", ""},     // only one level
		{"a.b.c.myapp.dev", ""}, // three levels
		{"myapp.dev", ""},
	}

	for _, tc := range tests {
		r, _ := http.NewRequest("GET", "/", nil)
		r.Host = tc.host
		_, got := rt.Match(r)
		if got != tc.want {
			t.Errorf("host %q: expected %q, got %q", tc.host, tc.want, got)
		}
	}
}

func TestRouter_DomainOnlyRoute(t *testing.T) {
	rt := New([]*config.Route{
		{
			Match:  &config.Match{Domain: "api.example.com"},
			Action: config.ActionRef{Name: "api"},
		},
	})

	r, _ := http.NewRequest("GET", "/any/path", nil)
	r.Host = "api.example.com"
	_, got := rt.Match(r)
	if got != "api" {
		t.Errorf("expected 'api', got %q", got)
	}

	r, _ = http.NewRequest("GET", "/any/path", nil)
	r.Host = "web.example.com"
	_, got = rt.Match(r)
	if got != "" {
		t.Errorf("expected empty for wrong domain, got %q", got)
	}
}

func TestRouter_DomainAndPath(t *testing.T) {
	rt := New([]*config.Route{
		{
			Match:  &config.Match{Domain: "api.example.com", Path: "/v1/*"},
			Action: config.ActionRef{Name: "api_v1"},
		},
		{
			Match:  &config.Match{Domain: "api.example.com", Path: "/*"},
			Action: config.ActionRef{Name: "api_fallback"},
		},
		{
			Match:  &config.Match{Path: "/*"},
			Action: config.ActionRef{Name: "default"},
		},
	})

	tests := []struct {
		host string
		path string
		want string
	}{
		{"api.example.com", "/v1/users", "api_v1"},
		{"api.example.com", "/v2/users", "api_fallback"},
		{"web.example.com", "/v1/users", "default"},
		{"web.example.com", "/anything", "default"},
	}

	for _, tc := range tests {
		r, _ := http.NewRequest("GET", tc.path, nil)
		r.Host = tc.host
		_, got := rt.Match(r)
		if got != tc.want {
			t.Errorf("host=%q path=%q: expected %q, got %q", tc.host, tc.path, tc.want, got)
		}
	}
}

func TestRouter_MultiDomainFirstMatchWins(t *testing.T) {
	rt := New([]*config.Route{
		{
			Match:  &config.Match{Domain: "*.api.myapp.dev", Path: "/*"},
			Action: config.ActionRef{Name: "api_wildcard"},
		},
		{
			Match:  &config.Match{Domain: "*.myapp.dev", Path: "/*"},
			Action: config.ActionRef{Name: "site_wildcard"},
		},
	})

	tests := []struct {
		host string
		want string
	}{
		{"v1.api.myapp.dev", "api_wildcard"},
		{"blog.myapp.dev", "site_wildcard"},
		{"shop.myapp.dev", "site_wildcard"},
	}

	for _, tc := range tests {
		r, _ := http.NewRequest("GET", "/", nil)
		r.Host = tc.host
		_, got := rt.Match(r)
		if got != tc.want {
			t.Errorf("host %q: expected %q, got %q", tc.host, tc.want, got)
		}
	}
}

// ── MatchResult context tests ──────────────────────────────────────────

func TestRouter_MatchResult(t *testing.T) {
	rt := New([]*config.Route{
		{
			Match:  &config.Match{Domain: "hi.*.myapp.dev", Path: "/*"},
			Action: config.ActionRef{Name: "greet"},
		},
	})

	r, _ := http.NewRequest("GET", "/hello", nil)
	r.Host = "hi.staging.myapp.dev"

	r, action := rt.Match(r)
	if action != "greet" {
		t.Fatalf("expected 'greet', got %q", action)
	}

	mr := GetMatchResult(r)
	if mr == nil {
		t.Fatal("expected MatchResult in context, got nil")
	}

	if mr.Domain != "hi.staging.myapp.dev" {
		t.Errorf("Domain: expected %q, got %q", "hi.staging.myapp.dev", mr.Domain)
	}
	if mr.DomainPattern != "hi.*.myapp.dev" {
		t.Errorf("DomainPattern: expected %q, got %q", "hi.*.myapp.dev", mr.DomainPattern)
	}
	if mr.MatchDomain != "staging" {
		t.Errorf("MatchDomain: expected %q, got %q", "staging", mr.MatchDomain)
	}
	if mr.Path != "/hello" {
		t.Errorf("Path: expected %q, got %q", "/hello", mr.Path)
	}
	if mr.MatchPath != "/*" {
		t.Errorf("MatchPath: expected %q, got %q", "/*", mr.MatchPath)
	}
}

func TestRouter_NoMatchResult(t *testing.T) {
	rt := New([]*config.Route{
		{
			Match:  &config.Match{Path: "/known"},
			Action: config.ActionRef{Name: "handler"},
		},
	})

	r, _ := http.NewRequest("GET", "/unknown", nil)
	r, action := rt.Match(r)
	if action != "" {
		t.Fatalf("expected no match, got %q", action)
	}

	mr := GetMatchResult(r)
	if mr != nil {
		t.Errorf("expected nil MatchResult for no match, got %+v", mr)
	}
}

// ── Double-star glob tests ─────────────────────────────────────────────

func TestRouter_GlobDomainSuffix(t *testing.T) {
	rt := New([]*config.Route{
		{
			Match:  &config.Match{Domain: "*.storage.**", Path: "/*"},
			Action: config.ActionRef{Name: "storage"},
		},
	})

	tests := []struct {
		host string
		want string
	}{
		{"files.storage.example.com", "storage"},
		{"cdn.storage.myapp.dev", "storage"},
		{"test.storage.a.b.c.dev", "storage"},
		{"FILES.STORAGE.EXAMPLE.COM", "storage"},
		{"files.storage.example.com:443", "storage"},
		{"storage.example.com", ""},   // missing prefix label
		{"files.cdn.example.com", ""}, // "cdn" != "storage"
		{"files.storage", ""},         // no suffix after **
	}

	for _, tc := range tests {
		r, _ := http.NewRequest("GET", "/", nil)
		r.Host = tc.host
		_, got := rt.Match(r)
		if got != tc.want {
			t.Errorf("host %q: expected %q, got %q", tc.host, tc.want, got)
		}
	}
}

func TestRouter_GlobDomainCaptures(t *testing.T) {
	rt := New([]*config.Route{
		{
			Match:  &config.Match{Domain: "*.storage.**", Path: "/*"},
			Action: config.ActionRef{Name: "storage"},
		},
	})

	r, _ := http.NewRequest("GET", "/file.txt", nil)
	r.Host = "cdn.storage.example.com"

	r, action := rt.Match(r)
	if action != "storage" {
		t.Fatalf("expected 'storage', got %q", action)
	}

	mr := GetMatchResult(r)
	if mr == nil {
		t.Fatal("expected MatchResult in context, got nil")
	}

	// "cdn" from *, "example.com" from **
	if mr.MatchDomain != "cdn" {
		t.Errorf("MatchDomain: expected %q, got %q", "cdn", mr.MatchDomain)
	}
	if mr.MatchGlob != "example.com" {
		t.Errorf("MatchGlob: expected %q, got %q", "example.com", mr.MatchGlob)
	}
	if mr.DomainPattern != "*.storage.**" {
		t.Errorf("DomainPattern: expected %q, got %q", "*.storage.**", mr.DomainPattern)
	}
	if mr.Domain != "cdn.storage.example.com" {
		t.Errorf("Domain: expected %q, got %q", "cdn.storage.example.com", mr.Domain)
	}
}

func TestRouter_GlobDomainOnly(t *testing.T) {
	// Pattern with only ** — matches any domain with 1+ labels.
	rt := New([]*config.Route{
		{
			Match:  &config.Match{Domain: "catchall.**", Path: "/*"},
			Action: config.ActionRef{Name: "catch"},
		},
	})

	tests := []struct {
		host string
		want string
	}{
		{"catchall.example.com", "catch"},
		{"catchall.a.b.c.d", "catch"},
		{"catchall", ""},          // no suffix
		{"other.example.com", ""}, // first label doesn't match
	}

	for _, tc := range tests {
		r, _ := http.NewRequest("GET", "/", nil)
		r.Host = tc.host
		_, got := rt.Match(r)
		if got != tc.want {
			t.Errorf("host %q: expected %q, got %q", tc.host, tc.want, got)
		}
	}
}

func TestRouter_GlobWithMultipleWildcards(t *testing.T) {
	rt := New([]*config.Route{
		{
			Match:  &config.Match{Domain: "*.*.storage.**", Path: "/*"},
			Action: config.ActionRef{Name: "deep"},
		},
	})

	tests := []struct {
		host string
		want string
	}{
		{"a.b.storage.example.com", "deep"},
		{"x.y.storage.a.b.c", "deep"},
		{"a.storage.example.com", ""}, // only one prefix label
		{"a.b.cdn.example.com", ""},   // "cdn" != "storage"
	}

	for _, tc := range tests {
		r, _ := http.NewRequest("GET", "/", nil)
		r.Host = tc.host
		_, got := rt.Match(r)
		if got != tc.want {
			t.Errorf("host %q: expected %q, got %q", tc.host, tc.want, got)
		}
	}
}

// ── Partial wildcard tests ─────────────────────────────────────────────

func TestRouter_PartialWildcardPrefix(t *testing.T) {
	rt := New([]*config.Route{
		{
			Match:  &config.Match{Domain: "cdn-*.example.com", Path: "/*"},
			Action: config.ActionRef{Name: "cdn"},
		},
	})

	tests := []struct {
		host string
		want string
	}{
		{"cdn-us.example.com", "cdn"},
		{"cdn-eu.example.com", "cdn"},
		{"cdn-asia-pacific.example.com", "cdn"},
		{"CDN-US.EXAMPLE.COM", "cdn"},
		{"cdn.example.com", ""},     // no dash suffix
		{"web-us.example.com", ""},  // wrong prefix
		{"cdn-us.other.com", ""},    // wrong domain
	}

	for _, tc := range tests {
		r, _ := http.NewRequest("GET", "/", nil)
		r.Host = tc.host
		_, got := rt.Match(r)
		if got != tc.want {
			t.Errorf("host %q: expected %q, got %q", tc.host, tc.want, got)
		}
	}
}

func TestRouter_PartialWildcardSuffix(t *testing.T) {
	rt := New([]*config.Route{
		{
			Match:  &config.Match{Domain: "*-prod.example.com", Path: "/*"},
			Action: config.ActionRef{Name: "prod"},
		},
	})

	tests := []struct {
		host string
		want string
	}{
		{"api-prod.example.com", "prod"},
		{"web-prod.example.com", "prod"},
		{"prod.example.com", ""},     // no prefix
		{"api-staging.example.com", ""}, // wrong suffix
	}

	for _, tc := range tests {
		r, _ := http.NewRequest("GET", "/", nil)
		r.Host = tc.host
		_, got := rt.Match(r)
		if got != tc.want {
			t.Errorf("host %q: expected %q, got %q", tc.host, tc.want, got)
		}
	}
}

func TestRouter_PartialWildcardCaptures(t *testing.T) {
	rt := New([]*config.Route{
		{
			Match:  &config.Match{Domain: "cdn-*.example.com", Path: "/*"},
			Action: config.ActionRef{Name: "cdn"},
		},
	})

	r, _ := http.NewRequest("GET", "/", nil)
	r.Host = "cdn-us.example.com"

	r, action := rt.Match(r)
	if action != "cdn" {
		t.Fatalf("expected 'cdn', got %q", action)
	}

	mr := GetMatchResult(r)
	if mr == nil {
		t.Fatal("expected MatchResult in context, got nil")
	}

	// Partial wildcard "cdn-*" matching "cdn-us" captures "us"
	if mr.MatchDomain != "us" {
		t.Errorf("MatchDomain: expected %q, got %q", "us", mr.MatchDomain)
	}
}

func TestRouter_PartialWildcardWithGlob(t *testing.T) {
	rt := New([]*config.Route{
		{
			Match:  &config.Match{Domain: "cdn-*.**", Path: "/*"},
			Action: config.ActionRef{Name: "cdn"},
		},
	})

	tests := []struct {
		host string
		want string
	}{
		{"cdn-us.example.com", "cdn"},
		{"cdn-eu.myapp.dev", "cdn"},
		{"cdn-us.a.b.c.d", "cdn"},
		{"cdn.example.com", ""},  // no dash
		{"web-us.example.com", ""}, // wrong prefix
		{"cdn-us", ""},           // no suffix after **
	}

	for _, tc := range tests {
		r, _ := http.NewRequest("GET", "/", nil)
		r.Host = tc.host
		_, got := rt.Match(r)
		if got != tc.want {
			t.Errorf("host %q: expected %q, got %q", tc.host, tc.want, got)
		}
	}
}

func TestRouter_PartialWildcardWithGlobCaptures(t *testing.T) {
	rt := New([]*config.Route{
		{
			Match:  &config.Match{Domain: "cdn-*.**", Path: "/*"},
			Action: config.ActionRef{Name: "cdn"},
		},
	})

	r, _ := http.NewRequest("GET", "/", nil)
	r.Host = "cdn-us.example.com"

	r, action := rt.Match(r)
	if action != "cdn" {
		t.Fatalf("expected 'cdn', got %q", action)
	}

	mr := GetMatchResult(r)
	if mr == nil {
		t.Fatal("expected MatchResult in context, got nil")
	}

	if mr.MatchDomain != "us" {
		t.Errorf("MatchDomain: expected %q, got %q", "us", mr.MatchDomain)
	}
	if mr.MatchGlob != "example.com" {
		t.Errorf("MatchGlob: expected %q, got %q", "example.com", mr.MatchGlob)
	}
}

func TestRouter_PartialWildcardInfix(t *testing.T) {
	rt := New([]*config.Route{
		{
			Match:  &config.Match{Domain: "app-*-v2.example.com", Path: "/*"},
			Action: config.ActionRef{Name: "v2"},
		},
	})

	tests := []struct {
		host string
		want string
	}{
		{"app-api-v2.example.com", "v2"},
		{"app-web-v2.example.com", "v2"},
		{"app-v2.example.com", ""},       // nothing between prefix and suffix
		{"app-api-v3.example.com", ""},    // wrong suffix
	}

	for _, tc := range tests {
		r, _ := http.NewRequest("GET", "/", nil)
		r.Host = tc.host
		_, got := rt.Match(r)
		if got != tc.want {
			t.Errorf("host %q: expected %q, got %q", tc.host, tc.want, got)
		}
	}
}
