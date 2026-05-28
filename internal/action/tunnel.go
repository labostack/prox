package action

import (
	"bufio"
	"context"
	"fmt"
	"io"
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

	upstream, err := dialUpstream(target.Scheme, host, target.Hostname(), timeout)
	if err != nil {
		slog.Warn("tunnel dial failed",
			"upstream", host,
			"err", err,
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
	br := bufioReaderPool.Get().(*bufio.Reader)
	br.Reset(upstream)
	defer bufioReaderPool.Put(br)

	resp, err := http.ReadResponse(br, outReq)
	if err != nil {
		slog.Warn("tunnel response failed",
			"upstream", host,
			"path", r.URL.Path,
			"err", err,
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

	bufp := copyBufPool.Get().(*[]byte)
	buf := *bufp
	defer copyBufPool.Put(bufp)
	var totalTx int64
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			totalTx += int64(n)
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
	if cs := ConnStatsFromContext(r.Context()); cs != nil {
		cs.AddTx(totalTx)
	}
}

// schemeOf returns "https" if the request used TLS, "http" otherwise.
func schemeOf(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// serveConnect establishes a bidirectional TCP tunnel for HTTP CONNECT requests.
// It chains the CONNECT to the upstream proxy: after dialing, it sends
// CONNECT with the original target host so the upstream knows where to dial.
//
// Two client-side modes are supported:
//   - HTTP/1.1: hijack the connection and relay raw TCP bytes.
//   - HTTP/2: use the ResponseWriter and request body as a framed
//     bidirectional stream (HTTP/2 does not support Hijack).
func serveConnect(w http.ResponseWriter, r *http.Request, target *url.URL, timeout time.Duration) {
	host := target.Host
	if !strings.Contains(host, ":") {
		switch target.Scheme {
		case "https", "wss":
			host += ":443"
		default:
			host += ":80"
		}
	}

	upstream, err := dialUpstream(target.Scheme, host, target.Hostname(), timeout)
	if err != nil {
		slog.Warn("connect dial failed",
			"upstream", host,
			"err", err,
		)
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}

	// Chain the CONNECT to the upstream proxy so it knows the actual target.
	connectTarget := r.Host
	if connectTarget == "" {
		connectTarget = r.URL.Host
	}
	if err := chainConnect(upstream, connectTarget, timeout); err != nil {
		upstream.Close()
		slog.Warn("upstream CONNECT failed",
			"upstream", host,
			"target", connectTarget,
			"err", err,
		)
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}

	if r.ProtoAtLeast(2, 0) {
		serveConnectH2(w, r, upstream)
		return
	}
	serveConnectH1(w, r.Context(), upstream)
}

// chainConnect sends a CONNECT request to the upstream proxy and waits for
// a successful (2xx) response. This establishes the second leg of the tunnel
// so the upstream proxy knows where to dial.
func chainConnect(upstream net.Conn, target string, timeout time.Duration) error {
	_ = upstream.SetDeadline(time.Now().Add(timeout))
	defer func() { _ = upstream.SetDeadline(time.Time{}) }()

	req := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n\r\n"
	if _, err := upstream.Write([]byte(req)); err != nil {
		return fmt.Errorf("write CONNECT: %w", err)
	}

	// Use a minimal reader — the response is a single status line + headers.
	// A large buffered reader could consume bytes beyond the response that
	// belong to the tunnel data stream.
	br := bufio.NewReaderSize(upstream, 256)

	// Pass CONNECT as the request method so http.ReadResponse knows
	// that 2xx responses carry no body (RFC 7231 §4.3.6). Without this,
	// resp.Body.Close() would try to drain the connection, consuming
	// tunnel data and causing the first proxied request to time out.
	connectReq := &http.Request{Method: http.MethodConnect}
	resp, err := http.ReadResponse(br, connectReq)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("upstream returned %d", resp.StatusCode)
	}
	return nil
}

// serveConnectH1 handles CONNECT over HTTP/1.1 by hijacking the raw connection.
func serveConnectH1(w http.ResponseWriter, ctx context.Context, upstream net.Conn) {
	clientConn, clientBuf, err := http.NewResponseController(w).Hijack()
	if err != nil {
		upstream.Close()
		slog.Error("connect hijack failed", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	_, err = clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	if err != nil {
		upstream.Close()
		clientConn.Close()
		return
	}

	go func() {
		defer upstream.Close()
		defer clientConn.Close()
		rx, _ := io.Copy(upstream, clientBuf)
		if cs := ConnStatsFromContext(ctx); cs != nil {
			cs.AddRx(rx)
		}
	}()

	defer upstream.Close()
	defer clientConn.Close()
	tx, _ := io.Copy(clientConn, upstream)
	if cs := ConnStatsFromContext(ctx); cs != nil {
		cs.AddTx(tx)
	}
}

// serveConnectH2 handles CONNECT over HTTP/2 using the framed stream.
// The request body carries client→upstream data and the ResponseWriter
// carries upstream→client data, with immediate flushing per chunk.
func serveConnectH2(w http.ResponseWriter, r *http.Request, upstream net.Conn) {
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	go func() {
		defer upstream.Close()
		bufp := copyBufPool.Get().(*[]byte)
		defer copyBufPool.Put(bufp)
		rx, _ := io.CopyBuffer(upstream, r.Body, *bufp)
		if cs := ConnStatsFromContext(r.Context()); cs != nil {
			cs.AddRx(rx)
		}
	}()

	defer upstream.Close()
	flusher, canFlush := w.(http.Flusher)
	bufp := copyBufPool.Get().(*[]byte)
	buf := *bufp
	defer copyBufPool.Put(bufp)
	var totalTx int64
	for {
		n, readErr := upstream.Read(buf)
		if n > 0 {
			totalTx += int64(n)
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				break
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if readErr != nil {
			break
		}
	}
	if cs := ConnStatsFromContext(r.Context()); cs != nil {
		cs.AddTx(totalTx)
	}
}
