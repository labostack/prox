package action

import (
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/dortanes/prox/internal/config"
	"github.com/dortanes/prox/internal/router"
)

// Proxy forwards requests to an upstream server.
// WebSocket upgrade requests are detected automatically and handled
// via connection hijacking with bidirectional TCP relay.
//
// When the upstream contains {target}, it is resolved per-request
// using the target selected by the route's load balancer.
type Proxy struct {
	// Static mode (no {target} template).
	proxy  *httputil.ReverseProxy
	target *url.URL

	// Dynamic mode ({target} in upstream template).
	upstreamTpl string // raw template, e.g. "{target}" or "http://{target}/prefix"
	transport   *http.Transport

	headers map[string]string
	timeout time.Duration
}

// NewProxy creates a reverse proxy handler for the given action config.
func NewProxy(act *config.Action) (*Proxy, error) {
	headers := act.Headers

	timeout := 30 * time.Second
	if act.Timeout.Duration > 0 {
		timeout = act.Timeout.Duration
	}

	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   timeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: timeout,
		TLSHandshakeTimeout:   10 * time.Second,
	}

	p := &Proxy{
		headers: headers,
		timeout: timeout,
	}

	// Dynamic mode: upstream contains {target} placeholder.
	if strings.Contains(act.Upstream, "{target}") {
		p.upstreamTpl = act.Upstream
		p.transport = transport
		return p, nil
	}

	// Static mode: fixed upstream.
	target, err := parseUpstream(act.Upstream)
	if err != nil {
		return nil, err
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.SetXForwarded()
			for k, v := range headers {
				if http.CanonicalHeaderKey(k) == "Host" {
					pr.Out.Host = v
				} else {
					pr.Out.Header.Set(k, v)
				}
			}
		},
	}
	proxy.Transport = transport
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		slog.Error("upstream error",
			"upstream", act.Upstream,
			"path", r.URL.Path,
			"error", err,
		)
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
	}

	p.proxy = proxy
	p.target = target
	return p, nil
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if p.upstreamTpl != "" {
		p.serveDynamic(w, r)
		return
	}
	if isWebSocketUpgrade(r) {
		serveWebSocket(w, r, p.target, p.headers, p.timeout)
		return
	}
	p.proxy.ServeHTTP(w, r)
}

// serveDynamic resolves the {target} template and proxies the request.
func (p *Proxy) serveDynamic(w http.ResponseWriter, r *http.Request) {
	match := router.GetMatchResult(r)
	if match == nil || match.Target == "" {
		slog.Error("no target selected for balanced route",
			"upstream_tpl", p.upstreamTpl,
			"path", r.URL.Path,
		)
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}

	// Signal the balancer when this request completes.
	defer match.Done()

	resolved := strings.ReplaceAll(p.upstreamTpl, "{target}", match.Target)
	target, err := parseUpstream(resolved)
	if err != nil {
		slog.Error("failed to parse resolved upstream",
			"upstream", resolved,
			"error", err,
		)
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}

	if isWebSocketUpgrade(r) {
		serveWebSocket(w, r, target, p.headers, p.timeout)
		return
	}

	headers := p.headers
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.SetXForwarded()
			for k, v := range headers {
				if http.CanonicalHeaderKey(k) == "Host" {
					pr.Out.Host = v
				} else {
					pr.Out.Header.Set(k, v)
				}
			}
		},
	}
	proxy.Transport = p.transport
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		slog.Error("upstream error",
			"upstream", resolved,
			"path", r.URL.Path,
			"error", err,
		)
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
	}

	proxy.ServeHTTP(w, r)
}

// parseUpstream normalizes the upstream address into a URL.
// Accepts "host:port" or "http://host:port" formats.
func parseUpstream(raw string) (*url.URL, error) {
	// If a scheme is already present, parse as-is.
	if strings.Contains(raw, "://") {
		return url.Parse(raw)
	}

	// Otherwise, default to http.
	return url.Parse("http://" + raw)
}
