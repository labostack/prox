package action

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/dortanes/prox/internal/config"
	"github.com/dortanes/prox/internal/router"
	"golang.org/x/net/http2"
)

// Proxy forwards requests to an upstream server.
// WebSocket upgrades are detected and handled via bidirectional TCP relay.
//
// When the upstream contains template placeholders (e.g. {target}),
// they are resolved per-request from the route's balancer and set variables.
type Proxy struct {
	proxy  *httputil.ReverseProxy // shared for both static and dynamic modes
	fast   *fastStaticProxy       // fast path for static upstream without custom headers
	target *url.URL               // static mode: fixed upstream URL

	upstreamTpl   string           // dynamic mode template (empty for static)
	stream        bool             // use raw HTTP tunnel for streaming

	headers  map[string]string
	timeout  time.Duration
	fallback http.Handler // invoked when the primary action fails
}

// dynTargetKey is the context key for passing the resolved target URL
// to the shared ReverseProxy's Rewrite func in dynamic mode.
type dynTargetKey struct{}

// NewProxy creates a reverse proxy handler for the given action config.
// svcCfg provides optional service-level transport tuning.
func NewProxy(act *config.Action, svcCfg *config.ServerConfig) (*Proxy, error) {
	headers := act.Headers

	timeout := 30 * time.Second
	if act.Timeout.Duration > 0 {
		timeout = act.Timeout.Duration
	}

	var responseHeaderTimeout time.Duration
	if svcCfg != nil && svcCfg.ResponseHeaderTimeout.Duration != 0 {
		responseHeaderTimeout = svcCfg.ResponseHeaderTimeout.Duration
	}

	var flushInterval time.Duration
	if svcCfg != nil && svcCfg.FlushInterval.Duration != 0 {
		flushInterval = svcCfg.FlushInterval.Duration
	}

	transport := buildTransport(act.Proto, svcCfg, timeout, responseHeaderTimeout)

	p := &Proxy{
		headers: headers,
		timeout: timeout,
		stream:  act.Stream,
	}

	// Dynamic mode: upstream contains template placeholders.
	if strings.Contains(act.Upstream, "{") {
		p.upstreamTpl = act.Upstream

		// Build a shared ReverseProxy that reads the target from request context.
		headersRef := headers
		proxy := &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				target, _ := pr.In.Context().Value(dynTargetKey{}).(*url.URL)
				if target != nil {
					pr.SetURL(target)
				}
				pr.SetXForwarded()
				for k, v := range headersRef {
					if http.CanonicalHeaderKey(k) == "Host" {
						pr.Out.Host = v
					} else {
						pr.Out.Header.Set(k, v)
					}
				}
			},
			FlushInterval: flushInterval,
			BufferPool:    proxyBufPool{},
		}
		proxy.Transport = transport
		proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			slog.Warn("upstream error",
				"path", r.URL.Path,
				"err", err,
			)
			if p.fallback != nil {
				p.fallback.ServeHTTP(w, r)
				return
			}
			http.Error(w, "Bad Gateway", http.StatusBadGateway)
		}
		p.proxy = proxy
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
		BufferPool:    proxyBufPool{},
	}
	proxy.Transport = transport
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		slog.Warn("upstream error",
			"upstream", act.Upstream,
			"path", r.URL.Path,
			"err", err,
		)
		if p.fallback != nil {
			p.fallback.ServeHTTP(w, r)
			return
		}
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
	}

	p.proxy = proxy
	p.target = target

	// Enable fast path when no custom headers and no streaming.
	if len(headers) == 0 && !act.Stream {
		p.fast = &fastStaticProxy{
			target:    target,
			transport: transport,
		}
	}

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
	if r.Method == http.MethodConnect {
		serveConnect(w, r, p.target, p.timeout)
		return
	}
	if p.stream {
		serveTunnel(w, r, p.target, p.headers, p.timeout)
		return
	}
	if p.fast != nil {
		p.fast.ServeHTTP(w, r)
		return
	}
	p.proxy.ServeHTTP(w, r)
}

// SetFallback sets the handler invoked when the primary action fails.
func (p *Proxy) SetFallback(h http.Handler) {
	p.fallback = h
	if p.fast != nil {
		p.fast.fallback = h
	}
}

// serveDynamic resolves template placeholders and proxies the request.
func (p *Proxy) serveDynamic(w http.ResponseWriter, r *http.Request) {
	match := router.GetMatchResult(r)
	needsTarget := strings.Contains(p.upstreamTpl, "{target}")
	if needsTarget && (match == nil || match.Target == "") {
		if p.fallback != nil {
			slog.Debug("no target, falling back",
				"host", r.Host,
				"path", r.URL.Path,
			)
			p.fallback.ServeHTTP(w, r)
			return
		}
		slog.Warn("no target available",
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
		slog.Error("invalid upstream URL",
			"upstream", resolved,
			"err", err,
		)
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}

	slog.Debug("proxy",
		"host", r.Host,
		"path", r.URL.Path,
		"upstream", resolved,
	)

	if isWebSocketUpgrade(r) {
		serveWebSocket(w, r, target, p.headers, p.timeout)
		return
	}
	if r.Method == http.MethodConnect {
		serveConnect(w, r, target, p.timeout)
		return
	}
	if p.stream {
		serveTunnel(w, r, target, p.headers, p.timeout)
		return
	}

	// Pass resolved target via context to the shared ReverseProxy.
	ctx := context.WithValue(r.Context(), dynTargetKey{}, target)
	p.proxy.ServeHTTP(w, r.WithContext(ctx))
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

// buildTransport creates the appropriate HTTP transport for the given protocol.
// All tuning values are read from svcCfg with sensible defaults.
//
// Supported values for proto:
//   - "" (empty): HTTP/1.1 transport (default)
//   - "h2": HTTP/2 cleartext (h2c) — required for upstreams expecting HTTP/2
//     over plain TCP (no TLS). Uses full-duplex streaming.
func buildTransport(proto string, svcCfg *config.ServerConfig, dialTimeout, responseHeaderTimeout time.Duration) http.RoundTripper {
	keepAlive := durationOr(svcCfg, func(c *config.ServerConfig) time.Duration { return c.KeepAlive.Duration }, 30*time.Second)
	if svcCfg != nil && svcCfg.DialTimeout.Duration > 0 {
		dialTimeout = svcCfg.DialTimeout.Duration
	}

	dialer := &net.Dialer{
		Timeout:   dialTimeout,
		KeepAlive: keepAlive,
	}

	if proto == "h2" {
		readIdle := durationOr(svcCfg, func(c *config.ServerConfig) time.Duration { return c.H2ReadIdleTimeout.Duration }, 30*time.Second)
		pingTimeout := durationOr(svcCfg, func(c *config.ServerConfig) time.Duration { return c.H2PingTimeout.Duration }, 15*time.Second)

		return &http2.Transport{
			AllowHTTP: true,
			// DialTLSContext is used even for non-TLS (AllowHTTP) connections.
			// We return a plain TCP connection — the transport handles h2c framing.
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				return dialer.DialContext(ctx, network, addr)
			},
			ReadIdleTimeout: readIdle,
			PingTimeout:     pingTimeout,
		}
	}

	maxIdle := 4096
	maxIdlePerHost := 4096
	tlsHandshake := 10 * time.Second
	readBuf := 4096
	writeBuf := 4096
	disableCompression := true // better default for reverse proxy
	if svcCfg != nil {
		if svcCfg.MaxIdleConns > 0 {
			maxIdle = svcCfg.MaxIdleConns
		}
		if svcCfg.MaxIdleConnsPerHost > 0 {
			maxIdlePerHost = svcCfg.MaxIdleConnsPerHost
		}
		if svcCfg.TLSHandshakeTimeout.Duration > 0 {
			tlsHandshake = svcCfg.TLSHandshakeTimeout.Duration
		}
		if svcCfg.ReadBufferSize > 0 {
			readBuf = svcCfg.ReadBufferSize
		}
		if svcCfg.WriteBufferSize > 0 {
			writeBuf = svcCfg.WriteBufferSize
		}
		if svcCfg.DisableCompression != nil {
			disableCompression = *svcCfg.DisableCompression
		}
	}

	return &http.Transport{
		DialContext:           dialer.DialContext,
		MaxIdleConns:          maxIdle,
		MaxIdleConnsPerHost:   maxIdlePerHost,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: responseHeaderTimeout,
		TLSHandshakeTimeout:   tlsHandshake,
		DisableCompression:    disableCompression,
		WriteBufferSize:       writeBuf,
		ReadBufferSize:        readBuf,
		ForceAttemptHTTP2:     false,
	}
}

// durationOr returns the duration from svcCfg if non-zero, otherwise the fallback.
func durationOr(svcCfg *config.ServerConfig, getter func(*config.ServerConfig) time.Duration, fallback time.Duration) time.Duration {
	if svcCfg != nil {
		if d := getter(svcCfg); d > 0 {
			return d
		}
	}
	return fallback
}

// dialUpstream connects to an upstream address, using TLS for https/wss schemes.
func dialUpstream(scheme, addr, serverName string, timeout time.Duration) (net.Conn, error) {
	switch scheme {
	case "https", "wss":
		dialer := &net.Dialer{Timeout: timeout}
		return tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{
			ServerName: serverName,
		})
	default:
		return net.DialTimeout("tcp", addr, timeout)
	}
}
