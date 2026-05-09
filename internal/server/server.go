// Package server manages HTTP/HTTPS listener lifecycle and hot reload.
package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dortanes/prox/internal/action"
	bal "github.com/dortanes/prox/internal/balancer"
	"github.com/dortanes/prox/internal/config"
	"github.com/dortanes/prox/internal/dispatcher"
	"github.com/dortanes/prox/internal/resource"
	"github.com/dortanes/prox/internal/router"
)

const (
	shutdownTimeout = 15 * time.Second
	readTimeout     = 10 * time.Second
	writeTimeout    = 30 * time.Second
	idleTimeout     = 120 * time.Second
)

// Group manages multiple HTTP servers, one per configured service.
type Group struct {
	servers  []*managedServer
	handlers map[string]*swappableHandler // keyed by service name
}

type managedServer struct {
	name     string
	server   *http.Server
	dispatch *dispatcher.Dispatcher // non-nil when service has "pass" routes
	rawLn    net.Listener           // raw TCP listener (when dispatcher is used)
}

// Build creates a server group from the loaded configuration.
func Build(cfg *config.Config) (*Group, error) {
	resolver := resource.NewResolver(cfg.Resources)

	hints := buildRouteHints(cfg)

	registry, err := action.Build(cfg.Actions, resolver, hints)
	if err != nil {
		return nil, fmt.Errorf("building actions: %w", err)
	}

	g := &Group{
		handlers: make(map[string]*swappableHandler),
	}

	for name, svc := range cfg.Services {
		srv, handler, err := buildServer(name, svc, cfg, registry)
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

	registry, err := action.Build(cfg.Actions, resolver, hints)
	if err != nil {
		return fmt.Errorf("building actions: %w", err)
	}

	swapped := 0

	for name, svc := range cfg.Services {
		handler, ok := g.handlers[name]
		if !ok {
			slog.Warn("new service in config requires restart to take effect",
				"service", name,
			)
			continue
		}

		rt := router.New(svc.Routes)
		handler.Swap(rt, registry)

		// Atomically swap dispatcher routes if this server has one.
		for _, ms := range g.servers {
			if ms.name == name && ms.dispatch != nil {
				routes := buildDispatcherRoutes(svc, cfg)
				ms.dispatch.SwapRoutes(routes)
				slog.Info("dispatcher routes reloaded", "service", name, "routes", len(routes))
			}
		}

		swapped++

		slog.Info("service reloaded", "service", name)
	}

	// Warn about removed services.
	for name := range g.handlers {
		if _, ok := cfg.Services[name]; !ok {
			slog.Warn("removed service in config requires restart to take effect",
				"service", name,
			)
		}
	}

	slog.Info("reload complete", "services_swapped", swapped)
	return nil
}

func buildServer(name string, svc *config.Service, cfg *config.Config, registry *action.Registry) (*managedServer, *swappableHandler, error) {
	rt := router.New(svc.Routes)
	handler := newSwappableHandler(name, rt, registry)

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

		slog.Info("loaded TLS certificates",
			"service", name,
			"count", len(certs),
		)
	}

	// Check if this service has any "pass" routes — if so, build a dispatcher.
	dispatchRoutes := buildDispatcherRoutes(svc, cfg)
	if len(dispatchRoutes) > 0 {
		ms.dispatch = dispatcher.New(dispatchRoutes)

		passCount := 0
		for _, r := range dispatchRoutes {
			if r.IsPass {
				passCount++
			}
		}
		slog.Info("l4 dispatcher enabled",
			"service", name,
			"total_routes", len(dispatchRoutes),
			"pass_routes", passCount,
		)
	}

	return ms, handler, nil
}

// buildDispatcherRoutes compiles L4 routes for the dispatcher.
// Returns nil if the service has no "pass" routes (no dispatcher needed).
// When the dispatcher is active, "drop" routes with domain patterns also
// participate in L4 matching as a bonus.
func buildDispatcherRoutes(svc *config.Service, cfg *config.Config) []*dispatcher.Route {
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
	for _, route := range svc.Routes {
		if route.Match == nil || route.Match.Domain == "" {
			continue // L4 dispatcher can only match on domain (SNI)
		}

		dr := &dispatcher.Route{
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
		slog.Info("shutdown signal received, draining connections...")
		g.shutdown()
		return nil
	}
}

// serveDirect starts an HTTP/HTTPS server without L4 dispatching (original path).
func (ms *managedServer) serveDirect() error {
	slog.Info("starting server",
		"service", ms.name,
		"addr", ms.server.Addr,
		"tls", ms.server.TLSConfig != nil,
	)

	if ms.server.TLSConfig != nil {
		// Certs are pre-loaded in TLSConfig.Certificates.
		return ms.server.ListenAndServeTLS("", "")
	}
	return ms.server.ListenAndServe()
}

// serveWithDispatcher starts a raw TCP listener, runs the L4 dispatcher,
// and feeds non-pass connections to the HTTP server via a synthetic listener.
func (ms *managedServer) serveWithDispatcher() error {
	ln, err := net.Listen("tcp", ms.server.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", ms.server.Addr, err)
	}
	ms.rawLn = ln // store for shutdown

	slog.Info("starting server with l4 dispatcher",
		"service", ms.name,
		"addr", ms.server.Addr,
		"tls", ms.server.TLSConfig != nil,
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

			if err := ms.server.Shutdown(ctx); err != nil {
				slog.Error("shutdown error",
					"service", ms.name,
					"error", err,
				)
			} else {
				slog.Info("server stopped", "service", ms.name)
			}

			// Wait for active pass relays to drain.
			if ms.dispatch != nil {
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

// swappableHandler wraps an atomic pointer to a routingSnapshot.
type swappableHandler struct {
	name    string
	current atomic.Pointer[routingSnapshot]
}

func newSwappableHandler(name string, rt *router.Router, registry *action.Registry) *swappableHandler {
	h := &swappableHandler{name: name}
	h.current.Store(&routingSnapshot{router: rt, registry: registry})
	return h
}

// Swap atomically replaces the routing logic.
func (h *swappableHandler) Swap(rt *router.Router, registry *action.Registry) {
	h.current.Store(&routingSnapshot{router: rt, registry: registry})
}

func (h *swappableHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if v := recover(); v != nil {
			// http.ErrAbortHandler is a Go internal signal, not a real panic.
			// Re-panic so the HTTP server handles it silently.
			if v == http.ErrAbortHandler {
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

	r, actionName := snap.router.Match(r)
	if actionName == "" {
		http.NotFound(w, r)
		return
	}

	handler := snap.registry.Get(actionName)
	if handler == nil {
		slog.Error("action handler not found",
			"service", h.name,
			"action", actionName,
		)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	slog.Debug("handling request",
		"service", h.name,
		"method", r.Method,
		"path", r.URL.Path,
		"action", actionName,
	)

	handler.ServeHTTP(w, r)
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
