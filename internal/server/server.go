// Package server manages HTTP/HTTPS listener lifecycle and hot reload.
package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dortanes/prox/internal/action"
	bal "github.com/dortanes/prox/internal/balancer"
	"github.com/dortanes/prox/internal/config"
	"github.com/dortanes/prox/internal/dispatcher"
	"github.com/dortanes/prox/internal/logger"
	"github.com/dortanes/prox/internal/plugin"
	"github.com/dortanes/prox/internal/resource"
	"github.com/dortanes/prox/internal/router"
	"github.com/dortanes/prox/internal/throttle"
)

const (
	shutdownTimeout                 = 15 * time.Second
	defReadTimeout    time.Duration = 0
	defWriteTimeout   time.Duration = 0
	defIdleTimeout                  = 120 * time.Second
)

// Group manages multiple HTTP servers, one per configured service.
type Group struct {
	servers  []*managedServer
	handlers map[string]*swappableHandler // keyed by service name
	plugins  *plugin.Manager              // nil when no plugins configured
}

type managedServer struct {
	name     string
	server   *http.Server
	servers  []*http.Server         // parallel HTTP servers for SO_REUSEPORT
	dispatch *dispatcher.Dispatcher // non-nil when service has "pass" routes
	rawLn    net.Listener           // raw TCP listener
	rawLns   []net.Listener         // parallel raw TCP listeners for SO_REUSEPORT
	maxConns int                    // max concurrent connections (0 = unlimited)
}

// Build creates a server group from the loaded configuration.
func Build(cfg *config.Config) (*Group, error) {
	resolver := resource.NewResolver(cfg.Resources)

	hints := buildRouteHints(cfg)

	g := &Group{
		handlers: make(map[string]*swappableHandler),
	}

	// Collect route balancers for plugin binding (first pass).
	routeBalancers := make(map[string]bal.Balancer)
	routers := make(map[string]*router.Router)
	autostart := hasAutostartPlugins(cfg)

	for name, svc := range cfg.Services {
		rt := router.New(svc.Routes)
		routers[name] = rt

		for i, route := range svc.Routes {
			if route.Balancer != nil && (routeHasPlugins(svc, route, cfg) || autostart) {
				inner := rt.RouteBalancer(i)
				if inner != nil {
					grouped := bal.NewGrouped(string(route.Balancer.Type), inner)
					rt.SetRouteBalancer(i, grouped)
					routeID := fmt.Sprintf("%s:%d", name, i)
					routeBalancers[routeID] = grouped
				}
			} else if route.Balancer != nil {
				routeID := fmt.Sprintf("%s:%d", name, i)
				if b := rt.RouteBalancer(i); b != nil {
					routeBalancers[routeID] = b
				}
			}
		}
	}

	// Build plugin manager before servers so handlers can reference it.
	bindings := buildPluginBindings(cfg, routeBalancers)
	if len(bindings) > 0 {
		routeInfo := buildRouteInfo(cfg, routeBalancers)
		g.plugins = plugin.NewManager()
		g.plugins.Configure(bindings, routeInfo)
		slog.Debug("plugin bindings configured", "count", len(bindings))
	}

	// Build servers (second pass).
	for name, svc := range cfg.Services {
		rt := routers[name]

		svcRegistry, err := action.Build(cfg.Actions, resolver, hints, svc.Config)
		if err != nil {
			return nil, fmt.Errorf("building actions for service %q: %w", name, err)
		}

		srv, handler, err := buildServer(name, svc, cfg, svcRegistry, rt, g.plugins)
		if err != nil {
			return nil, fmt.Errorf("building service %q: %w", name, err)
		}
		g.servers = append(g.servers, srv)
		g.handlers[name] = handler
	}

	return g, nil
}

// Reload atomically swaps the routing logic for all services.
// Listeners keep running — zero downtime. If the new config changes listen
// addresses or adds/removes services, those changes require a full restart.
func (g *Group) Reload(cfg *config.Config) error {
	resolver := resource.NewResolver(cfg.Resources)

	hints := buildRouteHints(cfg)

	// Rebuild balancers for plugin binding.
	routeBalancers := make(map[string]bal.Balancer)

	swapped := 0

	for name, svc := range cfg.Services {
		handler, ok := g.handlers[name]
		if !ok {
			slog.Warn("service added, restart required",
				"service", name,
			)
			continue
		}

		rt := router.New(svc.Routes)

		svcRegistry, err := action.Build(cfg.Actions, resolver, hints, svc.Config)
		if err != nil {
			return fmt.Errorf("building actions for service %q: %w", name, err)
		}

		handler.Swap(rt, svcRegistry)

		// Atomically swap dispatcher routes if this server has one.
		for _, ms := range g.servers {
			if ms.name == name && ms.dispatch != nil {
				routes := buildDispatcherRoutes(name, svc, cfg)
				ms.dispatch.SwapRoutes(routes)
				slog.Debug("dispatcher routes reloaded", "service", name, "routes", len(routes))
			}
		}

		// Wrap balancers in Grouped for plugin-managed or autostart-targeted routes.
		for i, route := range svc.Routes {
			if route.Balancer != nil && (routeHasPlugins(svc, route, cfg) || hasAutostartPlugins(cfg)) {
				inner := rt.RouteBalancer(i)
				if inner != nil {
					grouped := bal.NewGrouped(string(route.Balancer.Type), inner)
					rt.SetRouteBalancer(i, grouped)
					routeID := fmt.Sprintf("%s:%d", name, i)
					routeBalancers[routeID] = grouped
				}
			} else if route.Balancer != nil {
				routeID := fmt.Sprintf("%s:%d", name, i)
				if b := rt.RouteBalancer(i); b != nil {
					routeBalancers[routeID] = b
				}
			}
		}

		swapped++

		slog.Debug("service reloaded", "service", name)
	}

	// Reconfigure plugins.
	bindings := buildPluginBindings(cfg, routeBalancers)
	routeInfo := buildRouteInfo(cfg, routeBalancers)
	if g.plugins != nil {
		if len(bindings) > 0 {
			g.plugins.Reconfigure(bindings, routeInfo)
		} else {
			g.plugins.Stop()
			g.plugins = nil
		}
	} else if len(bindings) > 0 {
		g.plugins = plugin.NewManager()
		g.plugins.Configure(bindings, routeInfo)
		_ = g.plugins.Start(context.Background())
	}

	// Warn about removed services.
	for name := range g.handlers {
		if _, ok := cfg.Services[name]; !ok {
			slog.Warn("service removed, restart required",
				"service", name,
			)
		}
	}

	slog.Info("reload complete", "services_swapped", swapped)
	return nil
}

func buildServer(name string, svc *config.Service, cfg *config.Config, registry *action.Registry, rt *router.Router, plugins *plugin.Manager) (*managedServer, *swappableHandler, error) {
	handler := newSwappableHandler(name, rt, registry, plugins)

	// Resolve per-service timeouts, falling back to defaults.
	readTimeout := defReadTimeout
	writeTimeout := defWriteTimeout
	idleTimeout := defIdleTimeout
	if svc.Config != nil {
		if svc.Config.ReadTimeout.Duration > 0 {
			readTimeout = svc.Config.ReadTimeout.Duration
		}
		if svc.Config.WriteTimeout.Duration > 0 {
			writeTimeout = svc.Config.WriteTimeout.Duration
		}
		if svc.Config.IdleTimeout.Duration > 0 {
			idleTimeout = svc.Config.IdleTimeout.Duration
		}
	}

	srv := &http.Server{
		Addr:         svc.Listen,
		Handler:      handler,
		ReadTimeout:  readTimeout,
		WriteTimeout: writeTimeout,
		IdleTimeout:  idleTimeout,
	}

	ms := &managedServer{
		name:   name,
		server: srv,
	}

	if svc.TLS {
		tlsCfg := &tls.Config{
			MinVersion: tls.VersionTLS12,
			CurvePreferences: []tls.CurveID{
				tls.X25519,
				tls.CurveP256,
			},
		}

		certs, err := loadCertificates(svc.TLSCert, svc.TLSKey)
		if err != nil {
			return nil, nil, fmt.Errorf("loading TLS certificates: %w", err)
		}

		tlsCfg.Certificates = certs
		srv.TLSConfig = tlsCfg

		// Disable HTTP/2 when explicitly configured (h2: false).
		// Go's HTTP/2 framing strips Connection and Upgrade hop-by-hop headers,
		// which prevents WebSocket upgrade detection. Services that need
		// WebSocket support must disable HTTP/2 on the listener.
		if svc.H2 != nil && !*svc.H2 {
			srv.TLSNextProto = make(map[string]func(*http.Server, *tls.Conn, http.Handler))
			slog.Debug("HTTP/2 disabled",
				"service", name,
			)
		}

		slog.Debug("loaded TLS certificates",
			"service", name,
			"count", len(certs),
		)
	}

	// Check if this service has any "pass" routes — if so, build a dispatcher.
	dispatchRoutes := buildDispatcherRoutes(name, svc, cfg)
	if len(dispatchRoutes) > 0 {
		ms.dispatch = dispatcher.New(dispatchRoutes, plugins)

		passCount := 0
		for _, r := range dispatchRoutes {
			if r.IsPass {
				passCount++
			}
		}
		slog.Debug("l4 dispatcher enabled",
			"service", name,
			"total_routes", len(dispatchRoutes),
			"pass_routes", passCount,
		)
	}

	if svc.Config != nil && svc.Config.MaxConnections > 0 {
		ms.maxConns = svc.Config.MaxConnections
	}

	return ms, handler, nil
}

// buildDispatcherRoutes compiles L4 routes for the dispatcher.
// Returns nil if the service has no "pass" routes (no dispatcher needed).
// When the dispatcher is active, "drop" routes with domain patterns also
// participate in L4 matching as a bonus.
func buildDispatcherRoutes(serviceName string, svc *config.Service, cfg *config.Config) []*dispatcher.Route {
	hasPass := false
	for _, route := range svc.Routes {
		if resolveActionType(route, cfg) == config.ActionTypePass {
			hasPass = true
			break
		}
	}
	if !hasPass {
		return nil
	}

	// Build all routes (not just pass/drop routes) — order matters for correct dispatching.
	routes := make([]*dispatcher.Route, 0, len(svc.Routes))
	for i, route := range svc.Routes {
		if route.Match == nil || route.Match.Domain == "" {
			continue // L4 dispatcher can only match on domain (SNI)
		}

		dr := &dispatcher.Route{
			RouteID:        fmt.Sprintf("%s:%d", serviceName, i),
			Domain:         route.Match.Domain,
			DomainSegments: strings.Split(strings.ToLower(route.Match.Domain), "."),
		}

		// Detect trailing "**" glob.
		if last := len(dr.DomainSegments) - 1; last >= 0 && dr.DomainSegments[last] == "**" {
			dr.DomainGlob = true
			dr.DomainSegments = dr.DomainSegments[:last]
		}

		if act := resolveAction(route, cfg); act != nil {
			switch act.Type {
			case config.ActionTypePass:
				dr.IsPass = true
				if route.Balancer != nil && strings.Contains(act.Upstream, "{target}") {
					dr.UpstreamTpl = act.Upstream
					dr.Bal = buildDispatcherBalancer(route.Balancer)
				} else {
					dr.Upstream = act.Upstream
				}
			case config.ActionTypeDrop:
				dr.IsDrop = true
			}
		}

		routes = append(routes, dr)
	}

	return routes
}

// resolveAction returns the Action for a route — either from the named
// reference in cfg.Actions or the inline definition.
func resolveAction(route *config.Route, cfg *config.Config) *config.Action {
	if route.Action.Inline != nil {
		return route.Action.Inline
	}
	if route.Action.Name != "" {
		return cfg.Actions[route.Action.Name]
	}
	return nil
}

// resolveActionType returns the ActionType for a route.
func resolveActionType(route *config.Route, cfg *config.Config) config.ActionType {
	if act := resolveAction(route, cfg); act != nil {
		return act.Type
	}
	return ""
}

// buildDispatcherBalancer creates a balancer for L4 dispatcher routes.
func buildDispatcherBalancer(cfg *config.BalancerConfig) bal.Balancer {
	if cfg == nil || len(cfg.Targets) == 0 {
		return nil
	}
	switch cfg.Type {
	case config.BalancerRandom:
		return bal.NewRandom(cfg.Targets)
	case config.BalancerLeastConn:
		return bal.NewLeastConn(cfg.Targets)
	default:
		return bal.NewRoundRobin(cfg.Targets)
	}
}

// loadCertificates loads TLS certificate+key pairs.
// If certPath is a directory, all .crt/.pem + matching .key pairs are loaded.
// If certPath is a file, a single pair is loaded using certPath and keyPath.
func loadCertificates(certPath, keyPath string) ([]tls.Certificate, error) {
	info, err := os.Stat(certPath)
	if err != nil {
		return nil, fmt.Errorf("stat %q: %w", certPath, err)
	}

	if !info.IsDir() {
		// Single file mode — classic tls_cert + tls_key.
		cert, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return nil, fmt.Errorf("loading keypair (%s, %s): %w", certPath, keyPath, err)
		}
		return []tls.Certificate{cert}, nil
	}

	// Directory mode — scan for all cert+key pairs.
	return loadCertificatesFromDir(certPath)
}

// loadCertificatesFromDir scans a directory for certificate+key pairs.
// Matches by basename: "example.com.crt" pairs with "example.com.key".
// Supported extensions: .crt, .pem (cert) and .key (key).
func loadCertificatesFromDir(dir string) ([]tls.Certificate, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading cert directory %q: %w", dir, err)
	}

	// Collect all cert files.
	certFiles := make(map[string]string) // basename (no ext) → full path
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if ext == ".crt" || ext == ".pem" {
			base := strings.TrimSuffix(e.Name(), ext)
			certFiles[base] = filepath.Join(dir, e.Name())
		}
	}

	if len(certFiles) == 0 {
		return nil, fmt.Errorf("no certificate files (.crt, .pem) found in %q", dir)
	}

	// Match each cert with its key.
	var certs []tls.Certificate
	for base, certFile := range certFiles {
		keyFile := filepath.Join(dir, base+".key")
		if _, err := os.Stat(keyFile); err != nil {
			return nil, fmt.Errorf("no matching key file for %q (expected %s)", certFile, keyFile)
		}

		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("loading keypair (%s, %s): %w", certFile, keyFile, err)
		}

		slog.Debug("loaded certificate pair",
			"cert", certFile,
			"key", keyFile,
		)
		certs = append(certs, cert)
	}

	return certs, nil
}

// ListenAndServe starts all servers and blocks until ctx is cancelled.
func (g *Group) ListenAndServe(ctx context.Context) error {
	// Start plugin processes.
	if g.plugins != nil {
		if err := g.plugins.Start(ctx); err != nil {
			slog.Error("plugin start failed", "err", err)
		}
	}

	errCh := make(chan error, len(g.servers))

	for _, ms := range g.servers {
		go func(ms *managedServer) {
			var err error
			if ms.dispatch != nil {
				err = ms.serveWithDispatcher()
			} else {
				err = ms.serveDirect()
			}
			if err != nil && err != http.ErrServerClosed {
				errCh <- fmt.Errorf("service %q: %w", ms.name, err)
			}
		}(ms)
	}

	select {
	case err := <-errCh:
		// A server failed — shut everything down.
		g.shutdown()
		return err
	case <-ctx.Done():
		slog.Info("shutting down")
		g.shutdown()
		return nil
	}
}


// serveDirect starts an HTTP/HTTPS server without L4 dispatching (original path).
// Spawns multiple parallel SO_REUSEPORT listeners and workers to bypass Go's
// single-threaded connection accept loop bottleneck.
func (ms *managedServer) serveDirect() error {
	numWorkers := runtime.GOMAXPROCS(0)
	if envWorkers := os.Getenv("PROX_WORKERS"); envWorkers != "" {
		if val, err := strconv.Atoi(envWorkers); err == nil && val > 0 {
			numWorkers = val
		}
	}
	if numWorkers < 1 {
		numWorkers = 1
	}

	// Check if running in a test suite, scale down workers to 1 to ensure
	// clean listener cycles and deterministic test execution.
	isTest := false
	if strings.HasSuffix(os.Args[0], ".test") {
		isTest = true
	} else {
		for _, arg := range os.Args {
			if strings.HasPrefix(arg, "-test.") {
				isTest = true
				break
			}
		}
	}
	if isTest {
		numWorkers = 1
	}

	// Determine network: force tcp4 for IPv4 addresses to bypass Go's dual-stack resolving overhead.
	network := "tcp"
	if !strings.Contains(ms.server.Addr, "[") && !strings.Contains(ms.server.Addr, "::") {
		network = "tcp4"
	}

	ms.rawLns = make([]net.Listener, numWorkers)
	for i := 0; i < numWorkers; i++ {
		ln, err := reusePortListen(network, ms.server.Addr)
		if err != nil {
			// Clean up any successfully opened listeners on error
			for j := 0; j < i; j++ {
				ms.rawLns[j].Close()
			}
			return fmt.Errorf("listen SO_REUSEPORT %s (worker %d): %w", ms.server.Addr, i, err)
		}
		if ms.maxConns > 0 {
			ln = newLimitListener(ln, ms.maxConns)
		}
		ms.rawLns[i] = ln
	}

	slog.Info("starting server with SO_REUSEPORT scaling",
		"service", ms.name,
		"addr", ms.server.Addr,
		"tls", ms.server.TLSConfig != nil,
		"max_conns", ms.maxConns,
		"workers", numWorkers,
	)

	ms.servers = make([]*http.Server, numWorkers)
	errCh := make(chan error, numWorkers)
	for i := 0; i < numWorkers; i++ {
		// Create a separate http.Server for each listener to avoid internal race/deadlock in net/http
		srv := &http.Server{
			Addr:         ms.server.Addr,
			Handler:      ms.server.Handler,
			ReadTimeout:  ms.server.ReadTimeout,
			WriteTimeout: ms.server.WriteTimeout,
			IdleTimeout:  ms.server.IdleTimeout,
			TLSConfig:    ms.server.TLSConfig,
		}
		ms.servers[i] = srv

		go func(s *http.Server, ln net.Listener) {
			var err error
			if s.TLSConfig != nil {
				err = s.ServeTLS(ln, "", "")
			} else {
				err = s.Serve(ln)
			}
			if err != nil && err != http.ErrServerClosed {
				errCh <- err
			} else {
				errCh <- nil
			}
		}(srv, ms.rawLns[i])
	}

	// Block until all workers finish, or return the first critical error.
	var firstErr error
	for i := 0; i < numWorkers; i++ {
		err := <-errCh
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// serveWithDispatcher starts a raw TCP listener, runs the L4 dispatcher,
// and feeds non-pass connections to the HTTP server via a synthetic listener.
func (ms *managedServer) serveWithDispatcher() error {
	ln, err := net.Listen("tcp", ms.server.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", ms.server.Addr, err)
	}
	if ms.maxConns > 0 {
		ln = newLimitListener(ln, ms.maxConns)
	}
	ms.rawLn = ln // store for shutdown

	slog.Info("starting server",
		"service", ms.name,
		"addr", ms.server.Addr,
		"tls", ms.server.TLSConfig != nil,
		"l4", true,
	)

	// The dispatcher accepts raw TCP, handles pass routes inline,
	// and returns a synthetic listener for non-pass connections.
	httpLn := ms.dispatch.Serve(ln)

	if ms.server.TLSConfig != nil {
		// ServeTLS wraps accepted connections with tls.Server —
		// the prefixConn replays the peeked ClientHello bytes.
		return ms.server.ServeTLS(httpLn, "", "")
	}
	return ms.server.Serve(httpLn)
}

// shutdown gracefully stops all servers with a timeout.
func (g *Group) shutdown() {
	// Stop plugin processes first.
	if g.plugins != nil {
		g.plugins.Stop()
	}

	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	var wg sync.WaitGroup

	for _, ms := range g.servers {
		wg.Add(1)
		go func(ms *managedServer) {
			defer wg.Done()

			// Close the raw TCP listener first — this unblocks the
			// dispatcher's acceptLoop so it can drain and exit.
			if ms.rawLn != nil {
				ms.rawLn.Close()
			}

			if len(ms.servers) > 0 {
				var swg sync.WaitGroup
				for _, srv := range ms.servers {
					swg.Add(1)
					go func(s *http.Server) {
						defer swg.Done()
						if err := s.Shutdown(ctx); err != nil {
							slog.Warn("shutdown error, forcing close",
								"service", ms.name,
								"err", err,
							)
							s.Close()
						}
					}(srv)
				}
				swg.Wait()
				slog.Info("server stopped", "service", ms.name)
			} else {
				if err := ms.server.Shutdown(ctx); err != nil {
					slog.Warn("shutdown timeout, forcing close",
						"service", ms.name,
						"err", err,
					)
					ms.server.Close()
				} else {
					slog.Info("server stopped", "service", ms.name)
				}
			}

			// Force-close active relay connections, then wait for goroutines.
			if ms.dispatch != nil {
				ms.dispatch.Close()
				ms.dispatch.Wait()
			}
		}(ms)
	}

	wg.Wait()
}

// routingSnapshot is an immutable pair of router + action registry, swapped atomically.
type routingSnapshot struct {
	router   *router.Router
	registry *action.Registry
}

// throttleBufPool reuses 32KB byte slices for throttled ReadFrom copies.
var throttleBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 32*1024)
		return &b
	},
}

// capturePool reuses ResponseCapture structs to reduce per-request allocations.
var capturePool = sync.Pool{
	New: func() any { return &logger.ResponseCapture{} },
}

func acquireCapture(w http.ResponseWriter) *logger.ResponseCapture {
	c := capturePool.Get().(*logger.ResponseCapture)
	c.Reset(w)
	return c
}

func releaseCapture(c *logger.ResponseCapture) {
	c.ResponseWriter = nil
	capturePool.Put(c)
}

// swappableHandler wraps an atomic pointer to a routingSnapshot.
type swappableHandler struct {
	name         string
	current      atomic.Pointer[routingSnapshot]
	plugins      *plugin.Manager          // nil when no plugins configured
	groupBuckets *throttle.GroupRegistry   // shared group speed buckets
}

func newSwappableHandler(name string, rt *router.Router, registry *action.Registry, plugins *plugin.Manager) *swappableHandler {
	h := &swappableHandler{name: name, plugins: plugins}
	if plugins != nil {
		h.groupBuckets = plugins.GroupBuckets()
	}
	h.current.Store(&routingSnapshot{router: rt, registry: registry})
	return h
}

// Swap atomically replaces the routing logic.
func (h *swappableHandler) Swap(rt *router.Router, registry *action.Registry) {
	h.current.Store(&routingSnapshot{router: rt, registry: registry})
}

func (h *swappableHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var start time.Time
	var capture *logger.ResponseCapture
	accessOn := logger.AccessEnabled()
	if accessOn {
		start = time.Now()
		capture = acquireCapture(w)
		w = capture
	}

	// Access log — runs after recovery. Skipped for dropped connections.
	dropped := false
	if accessOn {
		defer func() {
			if dropped {
				releaseCapture(capture)
				return
			}
			mr := router.GetMatchResult(r)
			routeID := ""
			if mr != nil {
				routeID = mr.RouteID(h.name)
			}
			logger.LogAccess(routeID, logger.AccessEntry{
				Timestamp: start,
				Service:   h.name,
				Method:    r.Method,
				Path:      r.URL.Path,
				Status:    capture.Status(),
				Duration:  float64(time.Since(start).Microseconds()) / 1000.0,
				BytesOut:  capture.BytesOut(),
				ClientIP:  logger.ClientIP(r),
				UserAgent: r.Header.Get("User-Agent"),
			})
			releaseCapture(capture)
		}()
	}

	defer func() {
		if v := recover(); v != nil {
			// http.ErrAbortHandler is a Go internal signal, not a real panic.
			// Re-panic so the HTTP server handles it silently.
			if v == http.ErrAbortHandler {
				dropped = true
				panic(v)
			}
			slog.Error("panic recovered",
				"service", h.name,
				"path", r.URL.Path,
				"panic", v,
			)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}()

	snap := h.current.Load()

	// Fast path: skip MatchResult allocation when nobody needs it.
	// Speed-limited routes need MatchResult for bucket access.
	var actionName string
	needMatch := h.plugins != nil || accessOn || snap.router.HasSpeed()
	if !needMatch {
		actionName = snap.router.MatchAction(r)
	} else {
		r, actionName = snap.router.Match(r)
	}
	if actionName == "" {
		http.NotFound(w, r)
		return
	}

	// Plugin on_request gate.
	var reqInfo *plugin.RequestInfo
	var pluginSpeedLimit *plugin.SpeedLimit
	useFallback := false
	if h.plugins != nil {
		mr := router.GetMatchResult(r)
		routeID := mr.RouteID(h.name)
		reqInfo = buildRequestInfo(r, mr, routeID)

		if h.plugins.HasHook(routeID, plugin.HookOnRequest) {
			res, err := h.plugins.OnRequest(r.Context(), routeID, reqInfo)
			if err != nil {
				slog.Error("plugin request hook failed",
					"service", h.name,
					"route", routeID,
					"err", err,
				)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}
			if res.Drop {
				panic(http.ErrAbortHandler)
			}
			if res.Fallback {
				// CONNECT tunnels cannot fall back — drop the connection.
				if r.Method == http.MethodConnect {
					panic(http.ErrAbortHandler)
				}
				useFallback = true
			} else if !res.Allow {
				status := res.Status
				if status == 0 {
					status = http.StatusForbidden
				}
				for k, v := range res.Headers {
					w.Header().Set(k, v)
				}
				http.Error(w, res.Body, status)
				return
			}
			// Inject allowed headers into the request.
			for k, v := range res.Headers {
				r.Header.Set(k, v)
			}
			pluginSpeedLimit = res.SpeedLimit
			if res.CleanQuery {
				r.URL.RawQuery = ""
			}
			if res.RewritePath != "" {
				r.URL.Path = res.RewritePath
				r.URL.RawPath = ""
			}
		}

		// Wrap response writer for on_response hook.
		if h.plugins.HasHook(routeID, plugin.HookOnResponse) {
			w = &hookResponseWriter{
				ResponseWriter: w,
				plugins:        h.plugins,
				routeID:        routeID,
				reqInfo:        reqInfo,
				ctx:            r.Context(),
			}
		}
	}

	// Apply speed limiting.
	if mr := router.GetMatchResult(r); mr != nil {
		var cleanup func()
		w, r, cleanup = h.applySpeedLimit(w, r, mr, pluginSpeedLimit)
		if cleanup != nil {
			defer cleanup()
		}
	}

	var handler http.Handler
	if useFallback {
		handler = snap.registry.GetFallback(actionName)
		if handler == nil {
			slog.Warn("plugin requested fallback, but no fallback defined",
				"service", h.name,
				"action", actionName,
			)
			http.Error(w, "Bad Gateway", http.StatusBadGateway)
			return
		}
	} else {
		handler = snap.registry.Get(actionName)
		if handler == nil {
			slog.Error("action not found",
				"service", h.name,
				"action", actionName,
			)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
	}

	handler.ServeHTTP(w, r)
}

// maxPluginBody is the maximum request body size forwarded to plugins.
// Larger bodies are truncated — the plugin receives only the first 64KB.
const maxPluginBody = 64 * 1024

// buildRequestInfo creates a RequestInfo from the matched request.
func buildRequestInfo(r *http.Request, mr *router.MatchResult, routeID string) *plugin.RequestInfo {
	headers := make(map[string]string, len(r.Header))
	for k, vals := range r.Header {
		if len(vals) > 0 {
			headers[k] = vals[0]
		}
	}

	info := &plugin.RequestInfo{
		RouteID:       routeID,
		Method:        r.Method,
		Path:          r.URL.Path,
		Query:         r.URL.RawQuery,
		Domain:        mr.Domain,
		Host:          r.Host,
		Proto:         r.Proto,
		RemoteAddr:    r.RemoteAddr,
		ContentLength: r.ContentLength,
		Headers:       headers,
		MatchDomain:   mr.MatchDomain,
		MatchGlob:     mr.MatchGlob,
		MatchPath:     mr.MatchPath,
		Vars:          mr.Vars,
	}

	// Read up to maxPluginBody bytes for the plugin, then restore the body.
	// Skip for methods that typically carry no meaningful body.
	if r.Body != nil && r.ContentLength > 0 && hasRequestBody(r.Method) {
		lr := io.LimitReader(r.Body, maxPluginBody+1)
		body, err := io.ReadAll(lr)
		if err == nil && len(body) > 0 {
			if len(body) > maxPluginBody {
				info.Body = body[:maxPluginBody]
			} else {
				info.Body = body
			}
			// Restore the body so the upstream handler can read it.
			r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), r.Body))
		}
	}

	return info
}

// hasRequestBody returns true for HTTP methods that typically carry a body.
func hasRequestBody(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodDelete:
		return false
	default:
		return true
	}
}

// hookResponseWriter intercepts WriteHeader to call the on_response plugin hook
// before headers are sent to the client.
type hookResponseWriter struct {
	http.ResponseWriter
	plugins     *plugin.Manager
	routeID     string
	reqInfo     *plugin.RequestInfo
	ctx         context.Context
	headersSent bool
}

func (hw *hookResponseWriter) WriteHeader(statusCode int) {
	if hw.headersSent {
		hw.ResponseWriter.WriteHeader(statusCode)
		return
	}
	hw.headersSent = true

	// Build upstream response info from the current headers.
	respHeaders := make(map[string]string, len(hw.ResponseWriter.Header()))
	for k, vals := range hw.ResponseWriter.Header() {
		if len(vals) > 0 {
			respHeaders[k] = vals[0]
		}
	}

	mod, err := hw.plugins.OnResponse(hw.ctx, hw.routeID, hw.reqInfo, &plugin.UpstreamResponseInfo{
		Status:  statusCode,
		Headers: respHeaders,
	})
	if err != nil {
		slog.Error("plugin response hook failed",
			"route", hw.routeID,
			"err", err,
		)
		// Continue with original response on error.
		hw.ResponseWriter.WriteHeader(statusCode)
		return
	}

	// Apply modifications.
	for _, key := range mod.Remove {
		hw.ResponseWriter.Header().Del(key)
	}
	for k, v := range mod.Headers {
		hw.ResponseWriter.Header().Set(k, v)
	}
	if mod.Status != 0 {
		statusCode = mod.Status
	}

	hw.ResponseWriter.WriteHeader(statusCode)
}

func (hw *hookResponseWriter) Write(b []byte) (int, error) {
	if !hw.headersSent {
		hw.WriteHeader(http.StatusOK)
	}
	return hw.ResponseWriter.Write(b)
}

// Unwrap returns the underlying ResponseWriter for middleware chaining.
func (hw *hookResponseWriter) Unwrap() http.ResponseWriter {
	return hw.ResponseWriter
}

// buildRouteHints maps action names to their route paths (for prefix stripping).
func buildRouteHints(cfg *config.Config) *action.RouteHints {
	hints := &action.RouteHints{
		PathByAction: make(map[string]string),
	}

	for _, svc := range cfg.Services {
		for _, route := range svc.Routes {
			if route.Action.Name != "" && route.Match != nil {
				hints.PathByAction[route.Action.Name] = route.Match.Path
			}
		}
	}

	return hints
}

// hasAutostartPlugins returns true if any plugin in the config has autostart enabled.
func hasAutostartPlugins(cfg *config.Config) bool {
	for _, p := range cfg.Plugins {
		if p.Autostart {
			return true
		}
	}
	return false
}

// buildPluginBindings creates plugin-to-route bindings from the config.
func buildPluginBindings(cfg *config.Config, balancers map[string]bal.Balancer) []*plugin.Binding {
	var bindings []*plugin.Binding

	// Track which plugin paths already have route bindings.
	bound := make(map[string]bool)

	for name, svc := range cfg.Services {
		for i, route := range svc.Routes {
			merged := mergePlugins(svc, route, cfg)
			if len(merged) == 0 {
				continue
			}

			routeID := fmt.Sprintf("%s:%d", name, i)
			b := balancers[routeID]

			var match *plugin.MatchInfo
			if route.Match != nil {
				match = &plugin.MatchInfo{
					Domain: route.Match.Domain,
					Path:   route.Match.Path,
				}
			}

			for _, pluginRef := range merged {
				pluginPath := pluginRef
				pluginName := pluginRef
				if p, ok := cfg.Plugins[pluginRef]; ok {
					pluginPath = p.Path
				} else {
					pluginName = strings.TrimSuffix(filepath.Base(pluginRef), ".go")
				}

				absPath, err := filepath.Abs(pluginPath)
				if err != nil {
					slog.Warn("invalid plugin path",
						"plugin", pluginRef,
						"path", pluginPath,
						"err", err,
					)
					continue
				}

				bound[absPath] = true

				bindings = append(bindings, &plugin.Binding{
					Name:     pluginName,
					RouteID:  routeID,
					Plugin:   absPath,
					Match:    match,
					Balancer: b,
					Timeout:  route.PluginTimeout.Duration,
				})
			}
		}
	}

	// Append autostart plugins that have no route bindings.
	for name, p := range cfg.Plugins {
		if !p.Autostart {
			continue
		}

		absPath, err := filepath.Abs(p.Path)
		if err != nil {
			slog.Warn("invalid plugin path",
				"plugin", name,
				"path", p.Path,
				"err", err,
			)
			continue
		}

		if bound[absPath] {
			continue // already started via route binding
		}

		bindings = append(bindings, &plugin.Binding{
			Name:   name,
			Plugin: absPath,
		})
	}

	return bindings
}

// mergePlugins combines plugins from service, action, and route levels.
// Order: service → action → route. Duplicates are removed (first occurrence wins).
func mergePlugins(svc *config.Service, route *config.Route, cfg *config.Config) []string {
	total := len(svc.Plugins) + len(route.Plugins)

	// Resolve action-level plugins.
	var actionPlugins []string
	if route.Action.Name != "" {
		if act, ok := cfg.Actions[route.Action.Name]; ok {
			actionPlugins = act.Plugins
			total += len(actionPlugins)
		}
	}

	if total == 0 {
		return nil
	}

	seen := make(map[string]bool, total)
	merged := make([]string, 0, total)

	for _, p := range svc.Plugins {
		if !seen[p] {
			seen[p] = true
			merged = append(merged, p)
		}
	}
	for _, p := range actionPlugins {
		if !seen[p] {
			seen[p] = true
			merged = append(merged, p)
		}
	}
	for _, p := range route.Plugins {
		if !seen[p] {
			seen[p] = true
			merged = append(merged, p)
		}
	}

	return merged
}

// routeHasPlugins checks if a route has any plugins from service, action, or route level.
func routeHasPlugins(svc *config.Service, route *config.Route, cfg *config.Config) bool {
	if len(svc.Plugins) > 0 || len(route.Plugins) > 0 {
		return true
	}
	if route.Action.Name != "" {
		if act, ok := cfg.Actions[route.Action.Name]; ok && len(act.Plugins) > 0 {
			return true
		}
	}
	return false
}

// buildRouteInfo creates a map of all routes with balancers and their action names.
// This gives the plugin manager the full picture for action-based and wildcard target pushes.
func buildRouteInfo(cfg *config.Config, balancers map[string]bal.Balancer) map[string]*plugin.RouteInfo {
	info := make(map[string]*plugin.RouteInfo)

	for name, svc := range cfg.Services {
		for i, route := range svc.Routes {
			routeID := fmt.Sprintf("%s:%d", name, i)
			b, ok := balancers[routeID]
			if !ok {
				continue
			}

			action := route.Action.Name
			if action == "" && route.Action.Inline != nil {
				action = routeID // use route ID as fallback for inline actions
			}

			info[routeID] = &plugin.RouteInfo{
				Action:   action,
				Balancer: b,
			}
		}
	}

	return info
}

// --- connection limiter ---

// limitListener wraps a net.Listener with a concurrency limit.
// When the limit is reached, Accept blocks until an existing connection closes.
type limitListener struct {
	net.Listener
	sem chan struct{}
}

func newLimitListener(ln net.Listener, maxConns int) net.Listener {
	return &limitListener{
		Listener: ln,
		sem:      make(chan struct{}, maxConns),
	}
}

func (l *limitListener) Accept() (net.Conn, error) {
	l.sem <- struct{}{} // acquire slot
	conn, err := l.Listener.Accept()
	if err != nil {
		<-l.sem // release on error
		return nil, err
	}
	return &limitConn{Conn: conn, sem: l.sem}, nil
}

// limitConn releases a semaphore slot when closed.
type limitConn struct {
	net.Conn
	sem  chan struct{}
	once sync.Once
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { <-c.sem })
	return err
}

// --- speed limiting ---

// applySpeedLimit resolves effective download/upload speed limits from three
// sources (config route, plugin push, plugin response) and wraps the
// ResponseWriter and/or request Body accordingly. Returns the (possibly wrapped)
// writer, request, and an optional cleanup function. The cleanup function must
// be called when the connection ends (releases shared group buckets).
// No-op when no speed limits apply.
func (h *swappableHandler) applySpeedLimit(
	w http.ResponseWriter,
	r *http.Request,
	mr *router.MatchResult,
	pluginSpeed *plugin.SpeedLimit,
) (http.ResponseWriter, *http.Request, func()) {

	var downBuckets, upBuckets []*throttle.Bucket
	var cleanup func()

	// 1. Config-level shared buckets.
	if mr.DownloadBucket != nil {
		downBuckets = append(downBuckets, mr.DownloadBucket)
	}
	if mr.UploadBucket != nil {
		upBuckets = append(upBuckets, mr.UploadBucket)
	}

	// 2. Per-connection rate from config (non-shared), plugin push, plugin response.
	// Use the minimum non-zero value across all sources.
	perConnDown := mr.DownloadBps
	perConnUp := mr.UploadBps

	if h.plugins != nil {
		routeID := mr.RouteID(h.name)
		sl := h.plugins.GetSpeedLimit(routeID)
		if sl.DownloadBps > 0 {
			perConnDown = minPositive(perConnDown, sl.DownloadBps)
		}
		if sl.UploadBps > 0 {
			perConnUp = minPositive(perConnUp, sl.UploadBps)
		}
	}

	if pluginSpeed != nil {
		if pluginSpeed.DownloadMbps > 0 {
			perConnDown = minPositive(perConnDown, throttle.MbpsToBytes(pluginSpeed.DownloadMbps))
		}
		if pluginSpeed.UploadMbps > 0 {
			perConnUp = minPositive(perConnUp, throttle.MbpsToBytes(pluginSpeed.UploadMbps))
		}
	}

	// 3. Create buckets from the resolved rates.
	groupKey := ""
	if pluginSpeed != nil {
		groupKey = pluginSpeed.GroupKey
	}

	if groupKey != "" && h.groupBuckets != nil {
		// Group mode: shared buckets across all connections with the same key.
		dl, ul := h.groupBuckets.Acquire(groupKey, perConnDown, perConnUp)
		if dl != nil {
			downBuckets = append(downBuckets, dl)
		}
		if ul != nil {
			upBuckets = append(upBuckets, ul)
		}
		cleanup = func() { h.groupBuckets.Release(groupKey) }
	} else {
		// Per-connection mode: independent bucket per connection.
		if perConnDown > 0 {
			downBuckets = append(downBuckets, throttle.NewBucket(perConnDown))
		}
		if perConnUp > 0 {
			upBuckets = append(upBuckets, throttle.NewBucket(perConnUp))
		}
	}

	// Nothing to throttle — return unchanged.
	if len(downBuckets) == 0 && len(upBuckets) == 0 {
		return w, r, cleanup
	}

	// Wrap response writer for download throttling.
	if tw := throttle.NewWriter(w, downBuckets...); tw != nil {
		w = &throttledResponseWriter{ResponseWriter: w, tw: tw}
	}

	// Wrap request body for upload throttling.
	if tr := throttle.NewReader(r.Body, upBuckets...); tr != nil {
		r.Body = io.NopCloser(tr)
	}

	// Store limits in context for handlers that hijack (WebSocket).
	r = r.WithContext(throttle.NewContext(r.Context(), &throttle.Limits{
		Download: downBuckets,
		Upload:   upBuckets,
	}))

	return w, r, cleanup
}

// throttledResponseWriter wraps http.ResponseWriter to enforce download speed limits.
//
// This wrapper intentionally does NOT implement Unwrap(). Exposing the inner
// ResponseWriter would let io.Copy discover the underlying http.response's
// ReadFrom method and bypass the throttled Write entirely.
type throttledResponseWriter struct {
	http.ResponseWriter
	tw *throttle.Writer
}

func (w *throttledResponseWriter) Write(b []byte) (int, error) {
	return w.tw.Write(b)
}

// ReadFrom implements io.ReaderFrom so that io.Copy routes through the
// throttled Write instead of bypassing it via the inner ResponseWriter.
func (w *throttledResponseWriter) ReadFrom(src io.Reader) (int64, error) {
	bufp := throttleBufPool.Get().(*[]byte)
	buf := *bufp
	defer throttleBufPool.Put(bufp)

	var total int64
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			nw, writeErr := w.tw.Write(buf[:n])
			total += int64(nw)
			if writeErr != nil {
				return total, writeErr
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return total, nil
			}
			return total, readErr
		}
	}
}

func (w *throttledResponseWriter) Flush() {
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *throttledResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

// minPositive returns the smaller of two positive values.
// Zero values are ignored (treated as unlimited).
func minPositive(a, b int64) int64 {
	if a <= 0 {
		return b
	}
	if b <= 0 {
		return a
	}
	if a < b {
		return a
	}
	return b
}

