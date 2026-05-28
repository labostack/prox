package action

import (
	"bufio"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/dortanes/prox/internal/throttle"
)

// isWebSocketUpgrade returns true if the request is a WebSocket upgrade.
// Supports both HTTP/1.1 (Connection: Upgrade) and HTTP/2 Extended CONNECT (RFC 8441).
func isWebSocketUpgrade(r *http.Request) bool {
	// HTTP/1.1: standard upgrade mechanism.
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		headerContains(r.Header, "Connection", "upgrade") {
		return true
	}
	// HTTP/2: Extended CONNECT with :protocol pseudo-header (RFC 8441).
	// Currently disabled by default in Go, but future-proofed for when it is enabled.
	if r.Method == http.MethodConnect && r.ProtoMajor == 2 &&
		strings.EqualFold(r.Header.Get(":protocol"), "websocket") {
		return true
	}
	return false
}

// headerContains checks if a comma-separated header contains a value (case-insensitive).
func headerContains(h http.Header, key, value string) bool {
	for _, v := range h[http.CanonicalHeaderKey(key)] {
		for _, s := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(s), value) {
				return true
			}
		}
	}
	return false
}

// serveWebSocket hijacks the client connection and establishes a bidirectional
// tunnel to the upstream WebSocket server. The full HTTP upgrade handshake
// is forwarded transparently — no WebSocket framing is interpreted.
func serveWebSocket(w http.ResponseWriter, r *http.Request, target *url.URL, headers map[string]string, timeout time.Duration) {
	// Determine upstream dial address.
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
		slog.Warn("websocket dial failed",
			"upstream", host,
			"err", err,
		)
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}
	defer upstream.Close()

	// Rewrite the request URL path for upstream.
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

	// Preserve X-Forwarded headers.
	if clientIP, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		outReq.Header.Set("X-Forwarded-For", clientIP)
	}
	outReq.Header.Set("X-Forwarded-Host", r.Host)

	// Write the upgrade request directly to the upstream connection.
	if err := outReq.Write(upstream); err != nil {
		slog.Warn("websocket write failed",
			"err", err,
		)
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}

	// Hijack the client connection.
	rc := http.NewResponseController(w)
	client, clientBuf, err := rc.Hijack()
	if err != nil {
		slog.Error("websocket hijack failed",
			"err", err,
		)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	defer client.Close()

	// Read the upstream response and forward to client.
	upstreamBuf := bufioReaderPool.Get().(*bufio.Reader)
	upstreamBuf.Reset(upstream)
	defer bufioReaderPool.Put(upstreamBuf)
	resp, err := http.ReadResponse(upstreamBuf, outReq)
	if err != nil {
		slog.Warn("websocket response failed",
			"err", err,
		)
		return
	}
	defer resp.Body.Close()

	// Write the raw response to the client.
	if err := resp.Write(client); err != nil {
		slog.Warn("websocket client write failed",
			"err", err,
		)
		return
	}

	// If the upstream rejected the upgrade, we're done.
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return
	}

	// Bidirectional relay between client and upstream.
	// Apply speed limits from context if configured.
	var upReader io.Reader = clientBuf
	var downReader io.Reader = upstreamBuf

	if limits := throttle.FromContext(r.Context()); limits != nil {
		if tr := throttle.NewReader(clientBuf, limits.Upload...); tr != nil {
			upReader = tr
		}
		if tr := throttle.NewReader(upstreamBuf, limits.Download...); tr != nil {
			downReader = tr
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)

	var uploadBytes, downloadBytes int64

	go func() {
		defer wg.Done()
		uploadBytes, _ = io.Copy(upstream, upReader)
		if tc, ok := upstream.(interface{ CloseWrite() error }); ok {
			_ = tc.CloseWrite()
		}
	}()

	go func() {
		defer wg.Done()
		downloadBytes, _ = io.Copy(client, downReader)
		if tc, ok := client.(interface{ CloseWrite() error }); ok {
			_ = tc.CloseWrite()
		}
	}()

	wg.Wait()

	if cs := ConnStatsFromContext(r.Context()); cs != nil {
		cs.AddRx(uploadBytes)
		cs.AddTx(downloadBytes)
	}
}
