package action

import (
	"bufio"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// serveTunnel forwards a request to upstream using a raw HTTP connection,
// bypassing httputil.ReverseProxy. This is required for streaming protocols
// (e.g. long-lived POST upload / GET download) where ReverseProxy's RoundTrip
// model cannot handle simultaneous body streaming in both directions.
//
// The approach:
//  1. Dial a raw TCP connection to upstream
//  2. Write the HTTP request (headers + body stream) to upstream
//  3. Read response headers from upstream
//  4. Write response headers to the client
//  5. Stream the response body to the client with immediate flushing
//
// The request body is streamed in a background goroutine so the response
// can begin flowing before the upload completes.
func serveTunnel(w http.ResponseWriter, r *http.Request, target *url.URL, headers map[string]string, timeout time.Duration) {
	host := target.Host
	if !strings.Contains(host, ":") {
		switch target.Scheme {
		case "https", "wss":
			host += ":443"
		default:
			host += ":80"
		}
	}

	upstream, err := net.DialTimeout("tcp", host, timeout)
	if err != nil {
		slog.Error("tunnel upstream dial failed",
			"upstream", host,
			"error", err,
		)
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}
	defer upstream.Close()

	// Build the outgoing request preserving the original path and body.
	outReq := r.Clone(r.Context())
	outReq.URL.Host = target.Host
	outReq.URL.Scheme = target.Scheme
	outReq.Host = target.Host
	outReq.RequestURI = outReq.URL.RequestURI()

	// Apply configured headers.
	for k, v := range headers {
		if http.CanonicalHeaderKey(k) == "Host" {
			outReq.Host = v
		} else {
			outReq.Header.Set(k, v)
		}
	}

	// X-Forwarded headers.
	if clientIP, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		outReq.Header.Set("X-Forwarded-For", clientIP)
	}
	outReq.Header.Set("X-Forwarded-Host", r.Host)
	outReq.Header.Set("X-Forwarded-Proto", schemeOf(r))

	// Remove hop-by-hop headers that should not be forwarded.
	outReq.Header.Del("Te")
	outReq.Header.Del("Trailers")

	// Write the request to upstream. For streaming bodies, req.Write sends
	// headers first then copies the body — this blocks until body EOF or error.
	// We run this in a goroutine so we can start reading the response before
	// the upload finishes (required for duplex streaming protocols).
	writeErr := make(chan error, 1)
	go func() {
		writeErr <- outReq.Write(upstream)
	}()

	// Read the upstream response.
	resp, err := http.ReadResponse(
		bufio.NewReader(upstream),
		outReq,
	)
	if err != nil {
		slog.Error("tunnel upstream response failed",
			"upstream", host,
			"path", r.URL.Path,
			"error", err,
		)
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Copy response headers.
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	// Stream the response body with immediate flushing.
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				break
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		if readErr != nil {
			break
		}
	}
}

// schemeOf returns "https" if the request used TLS, "http" otherwise.
func schemeOf(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}
