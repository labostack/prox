package dispatcher

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/labostack/prox/internal/balancer"
	"github.com/labostack/prox/internal/plugin"
)

const (
	sniPeekTimeout      = 5 * time.Second
	upstreamDialTimeout = 10 * time.Second
)

// Route is a pre-compiled L4 route for domain-based dispatching.
type Route struct {
	RouteID        string            // e.g. "gateway:0" — matches plugin binding key
	Domain         string            // original pattern (e.g. "*.cdn.example.com")
	DomainSegments []string          // split + lowered segments for glob matching
	DomainGlob     bool              // true when pattern ends with "**"
	IsPass         bool              // true for action type "pass"
	IsDrop         bool              // true for action type "drop"
	Upstream       string            // dial address for pass routes (static)
	UpstreamTpl    string            // upstream template with {target} for balanced pass routes
	Bal            balancer.Balancer // nil = no balancing
}

// Dispatcher intercepts raw TCP connections on a listener, peeks the
// TLS ClientHello for SNI, and dispatches: "pass" routes get a raw
// TCP relay; everything else is fed to the HTTP server via a synthetic
// net.Listener.
type Dispatcher struct {
	routes  atomic.Pointer[[]*Route]
	wg      sync.WaitGroup  // tracks active pass relays
	plugins *plugin.Manager // optional, for on_connect hooks

	mu    sync.Mutex            // protects conns
	conns map[net.Conn]struct{} // active relay connections
}

// New creates a Dispatcher with the given pre-compiled routes.
// Routes are evaluated in order — first domain match wins.
func New(routes []*Route, plugins *plugin.Manager) *Dispatcher {
	d := &Dispatcher{
		conns:   make(map[net.Conn]struct{}),
		plugins: plugins,
	}
	d.routes.Store(&routes)
	return d
}

// SwapRoutes atomically replaces the dispatcher's route table.
// Active connections are not affected — new routes take effect
// on the next incoming connection.
func (d *Dispatcher) SwapRoutes(routes []*Route) {
	d.routes.Store(&routes)
}

// Serve starts the dispatcher in the background. It accepts connections
// from ln, peeks SNI, and dispatches. Connections that are not "pass"
// are sent to the returned net.Listener for consumption by
// http.Server.Serve / ServeTLS.
func (d *Dispatcher) Serve(ln net.Listener) net.Listener {
	httpLn := newChanListener(ln.Addr())

	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		d.acceptLoop(ln, httpLn)
	}()

	return httpLn
}

// Wait blocks until all active pass relays have finished.
func (d *Dispatcher) Wait() {
	d.wg.Wait()
}

// Close forcefully closes all active relay connections, causing their
// io.Copy goroutines to unblock and return. Called during shutdown
// to prevent the process from hanging on long-lived connections.
func (d *Dispatcher) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for c := range d.conns {
		c.Close()
	}
}

// trackConn registers a connection for shutdown tracking.
func (d *Dispatcher) trackConn(c net.Conn) {
	d.mu.Lock()
	d.conns[c] = struct{}{}
	d.mu.Unlock()
}

// untrackConn removes a connection from shutdown tracking.
func (d *Dispatcher) untrackConn(c net.Conn) {
	d.mu.Lock()
	delete(d.conns, c)
	d.mu.Unlock()
}

func (d *Dispatcher) acceptLoop(ln net.Listener, httpLn *chanListener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			// Listener closed — shut down.
			httpLn.Close()
			return
		}
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			d.handleConn(conn, httpLn)
		}()
	}
}

func (d *Dispatcher) handleConn(conn net.Conn, httpLn *chanListener) {
	// Deadline for the SNI peek — don't hang forever on slow/malicious clients.
	if err := conn.SetReadDeadline(time.Now().Add(sniPeekTimeout)); err != nil {
		conn.Close()
		return
	}

	sni, buf, err := PeekSNI(conn)

	// Clear the deadline for subsequent I/O.
	_ = conn.SetReadDeadline(time.Time{})

	if err != nil {
		slog.Debug("sni peek failed, closing connection",
			"err", err,
			"remote", conn.RemoteAddr(),
		)
		conn.Close()
		return
	}

	sniLower := strings.ToLower(sni)

	// Walk routes in config order — first domain match wins.
	routes := *d.routes.Load()
	for _, route := range routes {
		if !matchDomain(route.DomainSegments, route.DomainGlob, sniLower) {
			continue
		}

		if route.IsDrop {
			slog.Debug("l4 drop",
				"sni", sni,
				"pattern", route.Domain,
				"remote", conn.RemoteAddr(),
			)
			conn.Close()
			return
		}

		if route.IsPass {
			// Plugin on_connect gate.
			if d.plugins != nil && d.plugins.HasHook(route.RouteID, plugin.HookOnConnect) {
				matches, globTail := matchDomainCaptures(route.DomainSegments, route.DomainGlob, sniLower)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				res, err := d.plugins.OnConnect(ctx, route.RouteID, &plugin.ConnInfo{
					RouteID:     route.RouteID,
					Domain:      sni,
					RemoteAddr:  conn.RemoteAddr().String(),
					MatchDomain: strings.Join(matches, "."),
					MatchGlob:   globTail,
				})
				cancel()
				if err != nil || !res.Allow {
					slog.Debug("l4 plugin denied connection",
						"sni", sni,
						"pattern", route.Domain,
						"remote", conn.RemoteAddr(),
					)
					conn.Close()
					return
				}
			}

			// Resolve upstream — static or balanced.
			upstream := route.Upstream
			var target string
			if route.Bal != nil && route.UpstreamTpl != "" {
				// For keyed balancers (grouped targets), extract
				// the first wildcard capture from the SNI match
				if kb, ok := route.Bal.(balancer.KeyedBalancer); ok {
					captures, _ := matchDomainCaptures(route.DomainSegments, route.DomainGlob, sniLower)
					var key string
					if len(captures) > 0 {
						key = captures[0]
					}
					target = kb.NextKeyed(key)
				} else {
					target = route.Bal.Next()
				}
				upstream = strings.ReplaceAll(route.UpstreamTpl, "{target}", target)
			}

			slog.Debug("l4 pass",
				"sni", sni,
				"pattern", route.Domain,
				"upstream", upstream,
				"remote", conn.RemoteAddr(),
			)
			d.relayPass(conn, buf, upstream)

			// Signal the balancer after relay completes.
			if route.Bal != nil && target != "" {
				route.Bal.Done(target)
			}
			return
		}

		// L7 route — feed to HTTP server with buffered bytes replayed.
		slog.Debug("l4 → l7",
			"sni", sni,
			"pattern", route.Domain,
			"remote", conn.RemoteAddr(),
		)
		httpLn.Deliver(newPrefixConn(conn, buf))
		return
	}

	// No route matched — still feed to HTTP server (it will 404 / default-host).
	if sni != "" {
		slog.Debug("l4 no route match, forwarding to l7",
			"sni", sni,
			"remote", conn.RemoteAddr(),
		)
	}
	httpLn.Deliver(newPrefixConn(conn, buf))
}

func (d *Dispatcher) relayPass(client net.Conn, peekedBytes []byte, upstream string) {
	defer client.Close()

	up, err := net.DialTimeout("tcp", upstream, upstreamDialTimeout)
	if err != nil {
		slog.Warn("l4 upstream dial failed",
			"upstream", upstream,
			"err", err,
			"remote", client.RemoteAddr(),
		)
		return
	}
	defer up.Close()

	// Track both ends for graceful shutdown.
	d.trackConn(client)
	d.trackConn(up)
	defer d.untrackConn(client)
	defer d.untrackConn(up)

	// Replay the peeked ClientHello bytes to the upstream.
	if _, err := up.Write(peekedBytes); err != nil {
		slog.Warn("l4 upstream write failed",
			"upstream", upstream,
			"err", err,
		)
		return
	}

	_, _ = Relay(client, up)
}

// matchDomain checks if host matches the pattern segments.
// Each "*" matches exactly one domain label (full or partial like "cdn-*").
// When glob is true, the pattern had a trailing "**" (already stripped)
// and matches one or more remaining labels.
func matchDomain(patternSegments []string, glob bool, host string) bool {
	if len(patternSegments) == 0 && !glob {
		return true // nil pattern matches everything
	}

	hostSegments := strings.Split(host, ".")

	if glob {
		// Host must have more labels than the prefix.
		if len(hostSegments) <= len(patternSegments) {
			return false
		}
		for i, pat := range patternSegments {
			if !matchSegment(pat, hostSegments[i]) {
				return false
			}
		}
		return true
	}

	if len(hostSegments) != len(patternSegments) {
		return false
	}

	for i, pat := range patternSegments {
		if !matchSegment(pat, hostSegments[i]) {
			return false
		}
	}
	return true
}

// matchDomainCaptures extracts wildcard captures from a matched domain.
// Returns (captures, globTail). Only called after matchDomain returns true.
func matchDomainCaptures(patternSegments []string, glob bool, host string) ([]string, string) {
	hostSegments := strings.Split(host, ".")
	var captures []string

	for i, pat := range patternSegments {
		if i >= len(hostSegments) {
			break
		}
		if pat == "*" {
			captures = append(captures, hostSegments[i])
		}
	}

	var globTail string
	if glob && len(hostSegments) > len(patternSegments) {
		globTail = strings.Join(hostSegments[len(patternSegments):], ".")
	}

	return captures, globTail
}

// matchSegment checks if a host segment matches a pattern segment.
// Full wildcard "*" matches any segment. Partial wildcards like "cdn-*"
// or "*-prod" match segments with the given prefix/suffix.
func matchSegment(pat, seg string) bool {
	if pat == "*" {
		return true
	}
	starIdx := strings.Index(pat, "*")
	if starIdx == -1 {
		return pat == seg
	}
	// Partial wildcard: split around "*" into prefix and suffix.
	prefix := pat[:starIdx]
	suffix := pat[starIdx+1:]
	if len(seg) < len(prefix)+len(suffix) {
		return false
	}
	return strings.HasPrefix(seg, prefix) && strings.HasSuffix(seg, suffix)
}

// --- prefixConn: replays buffered bytes before reading from the real conn ---

type prefixConn struct {
	net.Conn
	reader io.Reader
}

func newPrefixConn(conn net.Conn, prefix []byte) net.Conn {
	if len(prefix) == 0 {
		return conn
	}
	return &prefixConn{
		Conn:   conn,
		reader: io.MultiReader(bytes.NewReader(prefix), conn),
	}
}

func (c *prefixConn) Read(b []byte) (int, error) {
	return c.reader.Read(b)
}

// --- chanListener: synthetic net.Listener fed by the dispatcher ---

type chanListener struct {
	ch   chan net.Conn
	addr net.Addr
	once sync.Once
	done chan struct{}
}

func newChanListener(addr net.Addr) *chanListener {
	return &chanListener{
		ch:   make(chan net.Conn, 64),
		addr: addr,
		done: make(chan struct{}),
	}
}

func (cl *chanListener) Accept() (net.Conn, error) {
	select {
	case conn, ok := <-cl.ch:
		if !ok {
			return nil, net.ErrClosed
		}
		return conn, nil
	case <-cl.done:
		return nil, net.ErrClosed
	}
}

func (cl *chanListener) Close() error {
	cl.once.Do(func() { close(cl.done) })
	return nil
}

func (cl *chanListener) Addr() net.Addr {
	return cl.addr
}

// Deliver sends a connection to the HTTP server.
// Returns false if the listener has been closed.
func (cl *chanListener) Deliver(conn net.Conn) bool {
	select {
	case cl.ch <- conn:
		return true
	case <-cl.done:
		conn.Close()
		return false
	}
}
