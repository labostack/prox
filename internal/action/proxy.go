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
// WebSocket upgrades are detected and handled via bidirectional TCP relay.
//
// When the upstream contains template placeholders (e.g. {target}),
// they are resolved per-request from the route's balancer and set variables.
type Proxy struct {
	proxy  *httputil.ReverseProxy // static mode
	target *url.URL

	upstreamTpl   string         // dynamic mode template
	transport     *http.Transport
	flushInterval time.Duration
	stream        bool // use raw HTTP tunnel for streaming

	headers  map[string]string
	timeout  time.Duration
	fallback http.Handler // invoked when the primary action fails
}

// NewProxy creates a reverse proxy handler for the given action config.
// svcCfg provides optional service-level transport tuning.
func NewProxy(act *config.Action, svcCfg *config.ServerConfig) (*Proxy, error) {
	headers := act.Headers

	timeout := 30 * time.Second
	if act.Timeout.Duration > 0 {
		timeout = act.Timeout.Duration
	}

	responseHeaderTimeout := timeout
	if svcCfg != nil && svcCfg.ResponseHeaderTimeout.Duration != 0 {
		responseHeaderTimeout = svcCfg.ResponseHeaderTimeout.Duration
	}

	var flushInterval time.Duration
	if svcCfg != nil && svcCfg.FlushInterval.Duration != 0 {
		flushInterval = svcCfg.FlushInterval.Duration
	}

	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   timeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: responseHeaderTimeout,
		TLSHandshakeTimeout:   10 * time.Second,
	}

	p := &Proxy{
		headers: headers,
		timeout: timeout,
		stream:  act.Stream,
	}

	// Dynamic mode: upstream contains template placeholders.
	if strings.Contains(act.Upstream, "{") {
		p.upstreamTpl = act.Upstream
		p.transport = transport
		p.flushInterval = flushInterval
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
		FlushInterval: flushInterval,
	}
	proxy.Transport = transport
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		slog.Error("upstream error",
			"upstream", act.Upstream,
			"path", r.URL.Path,
			"error", err,
		)
		if p.fallback != nil {
			p.fallback.ServeHTTP(w, r)
			return
		}
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
	if p.stream {
		serveTunnel(w, r, p.target, p.headers, p.timeout)
		return
	}
	p.proxy.ServeHTTP(w, r)
}

// SetFallback sets the handler invoked when the primary action fails.
func (p *Proxy) SetFallback(h http.Handler) {
	p.fallback = h
}

// serveDynamic resolves template placeholders and proxies the request.
func (p *Proxy) serveDynamic(w http.ResponseWriter, r *http.Request) {
	match := router.GetMatchResult(r)
	needsTarget := strings.Contains(p.upstreamTpl, "{target}")
	if needsTarget && (match == nil || match.Target == "") {
		if p.fallback != nil {
			slog.Debug("no target, using fallback",
				"host", r.Host,
				"path", r.URL.Path,
			)
			p.fallback.ServeHTTP(w, r)
			return
		}
		slog.Error("no target selected",
			"upstream_tpl", p.upstreamTpl,
			"host", r.Host,
			"path", r.URL.Path,
		)
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}

	// Signal the balancer when this request completes.
	defer match.Done()

	resolved := resolveTemplate(p.upstreamTpl, match)
	target, err := parseUpstream(resolved)
	if err != nil {
		slog.Error("failed to parse resolved upstream",
			"upstream", resolved,
			"error", err,
		)
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}

	slog.Debug("proxying dynamic request",
		"host", r.Host,
		"path", r.URL.Path,
		"upstream", resolved,
	)

	if isWebSocketUpgrade(r) {
		serveWebSocket(w, r, target, p.headers, p.timeout)
		return
	}
	if p.stream {
		serveTunnel(w, r, target, p.headers, p.timeout)
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
		FlushInterval: p.flushInterval,
	}
	proxy.Transport = p.transport
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		slog.Error("upstream error",
			"upstream", resolved,
			"path", r.URL.Path,
			"error", err,
		)
		if p.fallback != nil {
			p.fallback.ServeHTTP(w, r)
			return
		}
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
	}

	proxy.ServeHTTP(w, r)
}

// parseUpstream normalizes the upstream address into a URL.
func parseUpstream(raw string) (*url.URL, error) {
	if strings.Contains(raw, "://") {
		return url.Parse(raw)
	}
	return url.Parse("http://" + raw)
}

// resolveTemplate replaces {target} and route-level {key} vars in the template.
func resolveTemplate(tpl string, match *router.MatchResult) string {
	s := strings.ReplaceAll(tpl, "{target}", match.Target)
	for k, v := range match.Vars {
		s = strings.ReplaceAll(s, "{"+k+"}", v)
	}
	return s
}
