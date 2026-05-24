package action

import (
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Hop-by-hop headers that must not be forwarded.
// https://www.rfc-editor.org/rfc/rfc9110#section-7.6.1
var hopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailers",
	"Transfer-Encoding",
	"Upgrade",
}

// fastStaticProxy handles the common case: static upstream, no custom headers,
// no streaming. Bypasses httputil.ReverseProxy to avoid the per-request
// req.Clone() header deep-copy, reducing allocations and improving throughput.
type fastStaticProxy struct {
	target    *url.URL
	transport http.RoundTripper
	fallback  http.Handler
}

func (fp *fastStaticProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	outreq := &http.Request{
		Method:        r.Method,
		URL:           fp.rewriteURL(r.URL),
		Header:        cloneHeaderShallow(r.Header),
		Body:          r.Body,
		ContentLength: r.ContentLength,
		Host:          fp.target.Host,
	}

	// Remove hop-by-hop headers from the outgoing request.
	removeConnectionHeaders(outreq.Header)
	removeHopHeaders(outreq.Header)

	// Set X-Forwarded-For.
	clientIP := r.RemoteAddr
	if i := strings.LastIndexByte(clientIP, ':'); i != -1 {
		clientIP = clientIP[:i]
	}
	if prior := outreq.Header.Get("X-Forwarded-For"); prior != "" {
		outreq.Header.Set("X-Forwarded-For", prior+", "+clientIP)
	} else {
		outreq.Header.Set("X-Forwarded-For", clientIP)
	}

	resp, err := fp.transport.RoundTrip(outreq)
	if err != nil {
		if fp.fallback != nil {
			fp.fallback.ServeHTTP(w, r)
			return
		}
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Remove hop-by-hop headers from the response.
	removeConnectionHeaders(resp.Header)
	removeHopHeaders(resp.Header)

	// Copy response headers.
	dst := w.Header()
	for k, vs := range resp.Header {
		dst[k] = vs
	}

	w.WriteHeader(resp.StatusCode)

	// Copy the response body using a pooled buffer.
	buf := proxyBufPool{}.Get()
	io.CopyBuffer(w, resp.Body, buf)
	proxyBufPool{}.Put(buf)
}

// rewriteURL constructs the upstream URL preserving the client's path and query.
func (fp *fastStaticProxy) rewriteURL(orig *url.URL) *url.URL {
	u := *fp.target
	u.Path = singleJoiningSlash(fp.target.Path, orig.Path)
	u.RawPath = ""
	u.RawQuery = orig.RawQuery
	return &u
}

// cloneHeaderShallow creates a new Header map sharing the underlying []string
// slices. Much cheaper than http.Header.Clone() which deep-copies all values.
func cloneHeaderShallow(src http.Header) http.Header {
	dst := make(http.Header, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// removeHopHeaders deletes standard hop-by-hop headers.
func removeHopHeaders(h http.Header) {
	for _, k := range hopHeaders {
		h.Del(k)
	}
}

// removeConnectionHeaders deletes headers listed in the Connection header.
func removeConnectionHeaders(h http.Header) {
	for _, f := range h["Connection"] {
		for _, sf := range strings.Split(f, ",") {
			if sf = strings.TrimSpace(sf); sf != "" {
				h.Del(sf)
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
