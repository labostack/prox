package plugin

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/dortanes/prox/internal/balancer"
)

const (
	restartBaseDelay = 1 * time.Second
	restartMaxDelay  = 30 * time.Second
)

// Binding associates a route with a plugin process and its balancer.
type Binding struct {
	RouteID  string
	Plugin   string // absolute path to plugin binary
	Match    *MatchInfo
	Balancer balancer.Balancer
	Timeout  time.Duration // per-request plugin call timeout
}

// RouteInfo describes a route's balancer and action for global target pushes.
type RouteInfo struct {
	Action   string
	Balancer balancer.Balancer
}

// Manager supervises plugin processes and routes push messages to balancers.
// It also provides the hook call API (OnRequest, OnResponse, OnConnect)
// for the HTTP handler and L4 dispatcher.
type Manager struct {
	mu        sync.Mutex
	processes map[string]*managed // keyed by absolute plugin path
	bindings  []*Binding
	routes    map[string]*RouteInfo // all routes with balancers (routeID → info)
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

// managed wraps a plugin process with restart state and hook caller.
type managed struct {
	path    string
	proc    *Process
	restart int       // consecutive restart count for backoff
	caller  *Caller   // non-nil when plugin declared socket hooks
	hooks   []string  // declared hook capabilities
}

// hasHook returns true if this plugin declared the given hook.
func (mg *managed) hasHook(hook string) bool {
	for _, h := range mg.hooks {
		if h == hook {
			return true
		}
	}
	return false
}

// NewManager creates a plugin manager. Call Start() to spawn processes.
func NewManager() *Manager {
	return &Manager{
		processes: make(map[string]*managed),
	}
}

// Configure sets the current route-to-plugin bindings and global route info.
// Call Start() after Configure() to spawn processes.
func (m *Manager) Configure(bindings []*Binding, routes map[string]*RouteInfo) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bindings = bindings
	m.routes = routes
}

// Start spawns all plugin processes and begins processing pushes.
func (m *Manager) Start(ctx context.Context) error {
	ctx, m.cancel = context.WithCancel(ctx)

	m.mu.Lock()
	defer m.mu.Unlock()

	// Deduplicate plugin paths — one process per unique binary.
	needed := make(map[string]bool)
	for _, b := range m.bindings {
		needed[b.Plugin] = true
	}

	for pluginPath := range needed {
		if _, ok := m.processes[pluginPath]; ok {
			continue // already running
		}

		proc, err := startProcess(pluginPath)
		if err != nil {
			slog.Error("failed to start plugin",
				"plugin", pluginPath,
				"error", err,
			)
			continue
		}

		mg := &managed{path: pluginPath, proc: proc}
		m.processes[pluginPath] = mg

		slog.Info("plugin started",
			"plugin", filepath.Base(pluginPath),
			"pid", proc.cmd.Process.Pid,
		)

		// Send configure for all routes bound to this plugin.
		for _, b := range m.bindings {
			if b.Plugin != pluginPath {
				continue
			}
			if err := proc.Send(Request{
				Method: MethodConfigure,
				Params: ConfigureParams{
					RouteID: b.RouteID,
					Match:   b.Match,
				},
			}); err != nil {
				slog.Error("failed to configure plugin",
					"plugin", filepath.Base(pluginPath),
					"route", b.RouteID,
					"error", err,
				)
			}
		}

		// Process pushes in background.
		m.wg.Add(1)
		go func(mg *managed) {
			defer m.wg.Done()
			m.processPushes(ctx, mg)
		}(mg)
	}

	return nil
}

// Reconfigure updates bindings and reconfigures running plugins.
// New plugins are started, removed plugins are stopped.
func (m *Manager) Reconfigure(bindings []*Binding, routes map[string]*RouteInfo) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.bindings = bindings
	m.routes = routes

	// Determine which plugins are still needed.
	needed := make(map[string]bool)
	for _, b := range bindings {
		needed[b.Plugin] = true
	}

	// Stop plugins no longer referenced.
	for path, mg := range m.processes {
		if !needed[path] {
			slog.Info("stopping unreferenced plugin",
				"plugin", filepath.Base(path),
			)
			if mg.caller != nil {
				mg.caller.Close()
			}
			mg.proc.Stop()
			delete(m.processes, path)
		}
	}

	// Start new plugins and reconfigure existing ones.
	for pluginPath := range needed {
		mg, exists := m.processes[pluginPath]

		if !exists {
			proc, err := startProcess(pluginPath)
			if err != nil {
				slog.Error("failed to start plugin on reconfigure",
					"plugin", pluginPath,
					"error", err,
				)
				continue
			}

			mg = &managed{path: pluginPath, proc: proc}
			m.processes[pluginPath] = mg

			slog.Info("plugin started",
				"plugin", filepath.Base(pluginPath),
				"pid", proc.cmd.Process.Pid,
			)

			// Process pushes for the new process.
			m.wg.Add(1)
			go func(mg *managed) {
				defer m.wg.Done()
				m.processPushes(context.Background(), mg)
			}(mg)
		}

		// Send fresh configure for all routes bound to this plugin.
		for _, b := range bindings {
			if b.Plugin != pluginPath {
				continue
			}
			if err := mg.proc.Send(Request{
				Method: MethodConfigure,
				Params: ConfigureParams{
					RouteID: b.RouteID,
					Match:   b.Match,
				},
			}); err != nil {
				slog.Error("failed to reconfigure plugin",
					"plugin", filepath.Base(pluginPath),
					"route", b.RouteID,
					"error", err,
				)
			}
		}
	}
}

// Stop gracefully terminates all plugin processes.
func (m *Manager) Stop() {
	if m.cancel != nil {
		m.cancel()
	}

	m.mu.Lock()
	for _, mg := range m.processes {
		if mg.caller != nil {
			mg.caller.Close()
		}
		mg.proc.Stop()
	}
	m.mu.Unlock()

	m.wg.Wait()
}

// HasHook returns true if any plugin bound to the route supports the given hook.
func (m *Manager) HasHook(routeID string, hook string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, b := range m.bindings {
		if b.RouteID != routeID {
			continue
		}
		mg, ok := m.processes[b.Plugin]
		if ok && mg.hasHook(hook) {
			return true
		}
	}
	return false
}

// OnRequest calls the on_request hook for all plugins bound to the route.
// Sequential execution, short-circuit on first deny.
func (m *Manager) OnRequest(ctx context.Context, routeID string, req *RequestInfo) (*AuthorizeResult, error) {
	callers := m.callersForHook(routeID, HookOnRequest)
	if len(callers) == 0 {
		return &AuthorizeResult{Allow: true}, nil
	}

	timeout := m.timeoutForRoute(routeID)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	merged := &AuthorizeResult{Allow: true}

	for _, c := range callers {
		result, err := c.CallRequest(ctx, req)
		if err != nil {
			return nil, err
		}
		if !result.Allow {
			return result, nil
		}
		// Merge injected headers from all plugins.
		for k, v := range result.Headers {
			if merged.Headers == nil {
				merged.Headers = make(map[string]string)
			}
			merged.Headers[k] = v
		}
	}

	return merged, nil
}

// OnResponse calls the on_response hook for all plugins bound to the route.
func (m *Manager) OnResponse(ctx context.Context, routeID string, req *RequestInfo, resp *UpstreamResponseInfo) (*ResponseModResult, error) {
	callers := m.callersForHook(routeID, HookOnResponse)
	if len(callers) == 0 {
		return &ResponseModResult{}, nil
	}

	timeout := m.timeoutForRoute(routeID)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	merged := &ResponseModResult{}

	for _, c := range callers {
		result, err := c.CallResponse(ctx, req, resp)
		if err != nil {
			return nil, err
		}
		// Merge modifications.
		if result.Status != 0 {
			merged.Status = result.Status
		}
		for k, v := range result.Headers {
			if merged.Headers == nil {
				merged.Headers = make(map[string]string)
			}
			merged.Headers[k] = v
		}
		merged.Remove = append(merged.Remove, result.Remove...)
	}

	return merged, nil
}

// OnConnect calls the on_connect hook for all plugins bound to the route.
// Sequential execution, short-circuit on first deny.
func (m *Manager) OnConnect(ctx context.Context, routeID string, conn *ConnInfo) (*ConnResult, error) {
	callers := m.callersForHook(routeID, HookOnConnect)
	if len(callers) == 0 {
		return &ConnResult{Allow: true}, nil
	}

	timeout := m.timeoutForRoute(routeID)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for _, c := range callers {
		result, err := c.CallConnect(ctx, conn)
		if err != nil {
			return nil, err
		}
		if !result.Allow {
			return result, nil
		}
	}

	return &ConnResult{Allow: true}, nil
}

// callersForHook returns callers for all plugins bound to the route that support the hook.
func (m *Manager) callersForHook(routeID, hook string) []*Caller {
	m.mu.Lock()
	defer m.mu.Unlock()

	var callers []*Caller
	for _, b := range m.bindings {
		if b.RouteID != routeID {
			continue
		}
		mg, ok := m.processes[b.Plugin]
		if !ok || mg.caller == nil || !mg.hasHook(hook) {
			continue
		}
		callers = append(callers, mg.caller)
	}
	return callers
}

// timeoutForRoute returns the plugin timeout for the given route.
func (m *Manager) timeoutForRoute(routeID string) time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, b := range m.bindings {
		if b.RouteID == routeID && b.Timeout > 0 {
			return b.Timeout
		}
	}
	return defaultCallTimeout
}

// processPushes reads from the plugin's push channel and dispatches
// set_targets to the appropriate balancer. Handles process restart on crash.
func (m *Manager) processPushes(ctx context.Context, mg *managed) {
	for {
		select {
		case <-ctx.Done():
			return
		case push, ok := <-mg.proc.Pushes():
			if !ok {
				// Plugin process died — attempt restart.
				if ctx.Err() != nil {
					return // shutting down
				}
				m.restartPlugin(ctx, mg)
				if mg.proc == nil {
					return // restart failed permanently
				}
				continue
			}

			mg.restart = 0 // reset backoff on successful message

			m.handlePush(mg, push)
		}
	}
}

// handlePush dispatches a single push message to the right handler.
func (m *Manager) handlePush(mg *managed, push Push) {
	switch push.Method {
	case MethodSetTargets:
		m.handleSetTargets(mg, push)

	case MethodReady:
		m.handleReady(mg, push)

	default:
		slog.Debug("unknown plugin push method",
			"plugin", filepath.Base(mg.path),
			"method", push.Method,
		)
	}
}

// handleReady processes a plugin's capability declaration.
func (m *Manager) handleReady(mg *managed, push Push) {
	if push.Params.Socket == "" {
		slog.Warn("plugin sent ready without socket path",
			"plugin", filepath.Base(mg.path),
		)
		return
	}

	mg.hooks = push.Params.Hooks

	timeout := defaultCallTimeout
	// Use the first matching binding's timeout if set.
	m.mu.Lock()
	for _, b := range m.bindings {
		if b.Plugin == mg.path && b.Timeout > 0 {
			timeout = b.Timeout
			break
		}
	}
	m.mu.Unlock()

	// Close old caller if re-readying after restart.
	if mg.caller != nil {
		mg.caller.Close()
	}

	mg.caller = NewCaller(push.Params.Socket, defaultPoolSize, timeout)

	slog.Info("plugin ready with hooks",
		"plugin", filepath.Base(mg.path),
		"socket", push.Params.Socket,
		"hooks", push.Params.Hooks,
	)
}

// handleSetTargets routes target updates to the appropriate balancer.
func (m *Manager) handleSetTargets(mg *managed, push Push) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Action-based or wildcard targeting — resolve via routes map.
	if push.Params.Action != "" || push.Params.RouteID == "*" {
		m.applyGlobalTargets(mg, push)
		return
	}

	// Route-bound targeting — match via bindings.
	for _, b := range m.bindings {
		if b.Plugin == mg.path && b.RouteID == push.Params.RouteID {
			m.applyTargets(mg, push.Params.RouteID, b.Balancer, push)
			break
		}
	}
}

// applyGlobalTargets pushes targets to routes matched by action name or wildcard.
func (m *Manager) applyGlobalTargets(mg *managed, push Push) {
	count := 0
	for routeID, ri := range m.routes {
		if ri.Balancer == nil {
			continue
		}
		if push.Params.Action != "" && ri.Action != push.Params.Action {
			continue
		}
		m.applyTargets(mg, routeID, ri.Balancer, push)
		count++
	}

	if count == 0 {
		target := push.Params.RouteID
		if push.Params.Action != "" {
			target = push.Params.Action
		}
		slog.Warn("plugin set_targets matched no routes",
			"plugin", filepath.Base(mg.path),
			"target", target,
		)
	}
}

// applyTargets applies flat or grouped targets to a single balancer.
func (m *Manager) applyTargets(mg *managed, routeID string, bal balancer.Balancer, push Push) {
	if bal == nil {
		return
	}

	if push.Params.Groups != nil {
		if kb, ok := bal.(balancer.KeyedBalancer); ok {
			kb.SwapGroupedTargets(push.Params.Groups)
			total := 0
			for _, t := range push.Params.Groups {
				total += len(t)
			}
			slog.Info("plugin updated grouped targets",
				"plugin", filepath.Base(mg.path),
				"route", routeID,
				"groups", len(push.Params.Groups),
				"targets", total,
			)
		} else {
			slog.Warn("plugin sent grouped targets but balancer is not keyed",
				"plugin", filepath.Base(mg.path),
				"route", routeID,
			)
		}
	} else {
		bal.SwapTargets(push.Params.Targets)
		slog.Info("plugin updated targets",
			"plugin", filepath.Base(mg.path),
			"route", routeID,
			"targets", len(push.Params.Targets),
		)
	}
}

// restartPlugin attempts to restart a crashed plugin with exponential backoff.
func (m *Manager) restartPlugin(ctx context.Context, mg *managed) {
	mg.restart++
	delay := restartBaseDelay * time.Duration(1<<min(mg.restart-1, 4))
	if delay > restartMaxDelay {
		delay = restartMaxDelay
	}

	slog.Warn("plugin process exited, restarting",
		"plugin", filepath.Base(mg.path),
		"attempt", mg.restart,
		"delay", delay,
	)

	select {
	case <-ctx.Done():
		mg.proc = nil
		return
	case <-time.After(delay):
	}

	proc, err := startProcess(mg.path)
	if err != nil {
		slog.Error("failed to restart plugin",
			"plugin", mg.path,
			"attempt", mg.restart,
			"error", err,
		)
		mg.proc = nil
		return
	}

	mg.proc = proc

	slog.Info("plugin restarted",
		"plugin", filepath.Base(mg.path),
		"pid", proc.cmd.Process.Pid,
		"attempt", mg.restart,
	)

	// Re-send configure for all bound routes.
	m.mu.Lock()
	for _, b := range m.bindings {
		if b.Plugin != mg.path {
			continue
		}
		_ = proc.Send(Request{
			Method: MethodConfigure,
			Params: ConfigureParams{
				RouteID: b.RouteID,
				Match:   b.Match,
			},
		})
	}
	m.mu.Unlock()
}
