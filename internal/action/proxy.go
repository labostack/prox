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
)

// Proxy forwards requests to an upstream server.
type Proxy struct {
	proxy *httputil.ReverseProxy
}

// NewProxy creates a reverse proxy handler for the given action config.
func NewProxy(act *config.Action) (*Proxy, error) {
	target, err := parseUpstream(act.Upstream)
	if err != nil {
		return nil, err
	}

	headers := act.Headers

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

	timeout := 30 * time.Second
	if act.Timeout.Duration > 0 {
		timeout = act.Timeout.Duration
	}

	proxy.Transport = &http.Transport{
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

	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		slog.Error("upstream error",
			"upstream", act.Upstream,
			"path", r.URL.Path,
			"error", err,
		)
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
	}

	return &Proxy{proxy: proxy}, nil
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.proxy.ServeHTTP(w, r)
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

