package action

import (
	"io"
	"net/http"
	"net/url"
	"strings"
)

var (
	localXFF     = []string{"127.0.0.1"}
	localIPv6XFF = []string{"::1"}
)

// fastStaticProxy handles the common case: static upstream, no custom headers,
// no streaming. Bypasses httputil.ReverseProxy to avoid the per-request
// req.Clone() header deep-copy, reducing allocations and improving throughput.
type fastStaticProxy struct {
	target    *url.URL
	transport http.RoundTripper
	fallback  http.Handler
}

func (fp *fastStaticProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Rewrite request URL and Host directly in-place.
	r.URL.Scheme = fp.target.Scheme
	r.URL.Host = fp.target.Host
	
	targetPath := fp.target.Path
	if targetPath == "" || targetPath == "/" {
		// Path is already correct, no joining needed!
	} else {
		r.URL.Path = singleJoiningSlash(targetPath, r.URL.Path)
	}
	r.URL.RawPath = ""
	
	r.Host = fp.target.Host
	r.RequestURI = "" // Required by http.Transport.RoundTrip

	// Remove hop-by-hop headers from the incoming request's header.
	removeConnectionHeaders(r.Header)
	removeHopHeaders(r.Header)

	// Set X-Forwarded-For with zero allocations for localhost/IPv6-loopback.
	clientIP := r.RemoteAddr
	if i := strings.LastIndexByte(clientIP, ':'); i != -1 {
		clientIP = clientIP[:i]
	}
	if prior := r.Header["X-Forwarded-For"]; len(prior) > 0 {
		r.Header["X-Forwarded-For"] = []string{prior[0] + ", " + clientIP}
	} else {
		if clientIP == "127.0.0.1" {
			r.Header["X-Forwarded-For"] = localXFF
		} else if clientIP == "::1" {
			r.Header["X-Forwarded-For"] = localIPv6XFF
		} else {
			r.Header["X-Forwarded-For"] = []string{clientIP}
		}
	}

	resp, err := fp.transport.RoundTrip(r)
	if err != nil {
		if fp.fallback != nil {
			fp.fallback.ServeHTTP(w, r)
			return
		}
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}

	// Pre-process Connection header values to know which connection-specific headers to skip.
	var connSkip map[string]bool
	if connValues := resp.Header["Connection"]; len(connValues) > 0 {
		needsSkipMap := false
		for _, f := range connValues {
			if f != "keep-alive" && f != "Keep-Alive" && f != "close" && f != "Upgrade" && f != "upgrade" {
				needsSkipMap = true
				break
			}
		}
		if needsSkipMap {
			connSkip = make(map[string]bool, len(connValues))
			for _, f := range connValues {
				for _, sf := range strings.Split(f, ",") {
					if sf = strings.TrimSpace(sf); sf != "" {
						connSkip[http.CanonicalHeaderKey(sf)] = true
					}
				}
			}
		}
	}

	// Copy response headers in a single pass, skipping hop-by-hop headers
	// and Date header (which is automatically added by http.Server)
	// to avoid modifying resp.Header and bypass canonicalization.
	dst := w.Header()
	for k, vs := range resp.Header {
		if k == "Date" {
			continue
		}
		if isHopHeader(k) {
			continue
		}
		if connSkip != nil && connSkip[k] {
			continue
		}
		dst[k] = vs
	}

	w.WriteHeader(resp.StatusCode)

	// Copy the response body directly. Go's io.Copy automatically discovers
	// that w implements io.ReaderFrom and delegates to it, using net/http's
	// highly optimized internal buffer pool and completely bypassing sync.Pool.
	_, _ = io.Copy(w, resp.Body)
	resp.Body.Close() // Manual close to bypass defer registration overhead
}

// isHopHeader checks if a header is a hop-by-hop header using a compiler-optimized jump table.
func isHopHeader(k string) bool {
	switch k {
	case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "TE", "Trailer", "Transfer-Encoding", "Upgrade":
		return true
	default:
		return false
	}
}

// removeHopHeaders deletes standard hop-by-hop headers.
// Optimized using point-wise deletes to bypass slow runtime.mapiterinit overhead.
func removeHopHeaders(h http.Header) {
	delete(h, "Connection")
	delete(h, "Keep-Alive")
	delete(h, "Proxy-Authenticate")
	delete(h, "Proxy-Authorization")
	delete(h, "TE")
	delete(h, "Trailer")
	delete(h, "Transfer-Encoding")
	delete(h, "Upgrade")
}

// removeConnectionHeaders deletes headers listed in the Connection header.
// Includes a zero-allocation fast-path for standard "keep-alive" values.
func removeConnectionHeaders(h http.Header) {
	connHeaders := h["Connection"]
	if len(connHeaders) == 0 {
		return
	}
	if len(connHeaders) == 1 {
		val := connHeaders[0]
		if val == "keep-alive" || val == "Keep-Alive" || val == "close" {
			return
		}
	}
	for _, f := range connHeaders {
		for _, sf := range strings.Split(f, ",") {
			if sf = strings.TrimSpace(sf); sf != "" {
				delete(h, http.CanonicalHeaderKey(sf))
			}
		}
	}
}

// singleJoiningSlash joins base and suffix paths with exactly one slash.
func singleJoiningSlash(base, suffix string) string {
	bslash := strings.HasSuffix(base, "/")
	sslash := strings.HasPrefix(suffix, "/")
	switch {
	case bslash && sslash:
		return base + suffix[1:]
	case !bslash && !sslash:
		return base + "/" + suffix
	}
	return base + suffix
}
