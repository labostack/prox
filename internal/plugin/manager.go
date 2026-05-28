package plugin

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dortanes/prox/internal/balancer"
	"github.com/dortanes/prox/internal/throttle"
)

const (
	restartBaseDelay = 1 * time.Second
	restartMaxDelay  = 30 * time.Second
)

// Binding associates a route with a plugin process and its balancer.
type Binding struct {
	Name     string // human-readable alias from config
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

// hookIndex is an immutable lookup structure for the request hot path.
// Rebuilt on configure/reconfigure/plugin-ready (rare), loaded atomically
// on every request (frequent). Eliminates mutex contention on the hot path.
type hookIndex struct {
	// callers maps routeID → hook → callers for that hook.
	callers map[string]map[string][]*Caller
	// timeouts maps routeID → plugin call timeout.
	timeouts map[string]time.Duration
}

// SpeedEntry holds per-connection bandwidth caps pushed by a plugin.
type SpeedEntry struct {
	DownloadBps int64 // bytes per second (0 = unlimited)
	UploadBps   int64 // bytes per second (0 = unlimited)
}

// speedIndex is an immutable map of route speed limits, swapped atomically.
type speedIndex struct {
	entries map[string]SpeedEntry // routeID → speed limit
}

// Manager supervises plugin processes and routes push messages to balancers.
// It also provides the hook call API (OnRequest, OnResponse, OnConnect,
// OnDisconnect) for the HTTP handler and L4 dispatcher.
type Manager struct {
	mu           sync.Mutex
	processes    map[string]*managed // keyed by absolute plugin path
	bindings     []*Binding
	routes       map[string]*RouteInfo // all routes with balancers (routeID → info)
	cancel       context.CancelFunc
	wg           sync.WaitGroup
	index        atomic.Pointer[hookIndex]  // lock-free hot-path lookup
	speeds       atomic.Pointer[speedIndex] // lock-free speed limit lookup
	groupBuckets *throttle.GroupRegistry    // shared group speed buckets
	disconnects  chan disconnectMsg          // buffered fire-and-forget channel
}

// disconnectMsg carries a pre-serialized frame to fire at plugin callers.
type disconnectMsg struct {
	frame   []byte
	callers []*Caller
}

// managed wraps a plugin process with restart state and hook caller.
type managed struct {
	name    string
	path    string
	proc    *Process
	restart int       // consecutive restart count for backoff
	caller  *Caller   // non-nil when plugin declared socket hooks
	hooks   []string  // declared hook capabilities
}

// NewManager creates a plugin manager. Call Start() to spawn processes.
func NewManager() *Manager {
	return &Manager{
		processes:    make(map[string]*managed),
		groupBuckets: throttle.NewGroupRegistry(),
		disconnects:  make(chan disconnectMsg, 256),
	}
}

// GroupBuckets returns the shared group speed bucket registry.
func (m *Manager) GroupBuckets() *throttle.GroupRegistry {
	return m.groupBuckets
}

// Configure sets the current route-to-plugin bindings and global route info.
// Call Start() after Configure() to spawn processes.
func (m *Manager) Configure(bindings []*Binding, routes map[string]*RouteInfo) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bindings = bindings
	m.routes = routes
	m.rebuildIndexLocked()
}

// Start spawns all plugin processes and begins processing pushes.
func (m *Manager) Start(ctx context.Context) error {
	ctx, m.cancel = context.WithCancel(ctx)

	m.mu.Lock()
	defer m.mu.Unlock()

	// Deduplicate plugin paths — one process per unique binary.
	needed := make(map[string]string) // path → name
	for _, b := range m.bindings {
		if _, ok := needed[b.Plugin]; !ok {
			needed[b.Plugin] = b.Name
		}
	}

	for pluginPath, pluginName := range needed {
		if _, ok := m.processes[pluginPath]; ok {
			continue // already running
		}

		proc, err := startProcess(pluginName, pluginPath)
		if err != nil {
			slog.Error("plugin start failed",
				"plugin", pluginName,
				"err", err,
			)
			continue
		}

		mg := &managed{name: pluginName, path: pluginPath, proc: proc}
		m.processes[pluginPath] = mg

		slog.Debug("plugin started",
			"plugin", pluginName,
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
				slog.Error("plugin configure failed",
					"plugin", pluginName,
					"route", b.RouteID,
					"err", err,
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

	// Start disconnect drain goroutine.
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.drainDisconnects(ctx)
	}()

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
	needed := make(map[string]string) // path → name
	for _, b := range bindings {
		if _, ok := needed[b.Plugin]; !ok {
			needed[b.Plugin] = b.Name
		}
	}

	// Stop plugins no longer referenced.
	for path, mg := range m.processes {
		if _, ok := needed[path]; !ok {
			slog.Debug("plugin stopped",
				"plugin", mg.name,
			)
			if mg.caller != nil {
				mg.caller.Close()
			}
			mg.proc.Stop()
			delete(m.processes, path)
		}
	}

	// Start new plugins and reconfigure existing ones.
	for pluginPath, pluginName := range needed {
		mg, exists := m.processes[pluginPath]

		if !exists {
			proc, err := startProcess(pluginName, pluginPath)
			if err != nil {
				slog.Error("plugin start failed",
					"plugin", pluginName,
					"err", err,
				)
				continue
			}

			mg = &managed{name: pluginName, path: pluginPath, proc: proc}
			m.processes[pluginPath] = mg

			slog.Debug("plugin started",
				"plugin", pluginName,
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
				slog.Error("plugin configure failed",
					"plugin", pluginName,
					"route", b.RouteID,
					"err", err,
				)
			}
		}
	}

	m.rebuildIndexLocked()
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
// Lock-free: reads from the atomic hook index.
func (m *Manager) HasHook(routeID string, hook string) bool {
	idx := m.index.Load()
	if idx == nil {
		return false
	}
	hooks := idx.callers[routeID]
	if hooks == nil {
		return false
	}
	return len(hooks[hook]) > 0
}

// OnRequest calls the on_request hook for all plugins bound to the route.
// Sequential execution, short-circuit on first deny.
func (m *Manager) OnRequest(ctx context.Context, routeID string, req *RequestInfo) (*AuthorizeResult, error) {
	callers, timeout := m.lookupHook(routeID, HookOnRequest)
	if len(callers) == 0 {
		return &AuthorizeResult{Allow: true}, nil
	}

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
		// Merge speed limits — use the lowest non-zero value per direction.
		if result.SpeedLimit != nil {
			if merged.SpeedLimit == nil {
				merged.SpeedLimit = &SpeedLimit{}
			}
			if result.SpeedLimit.DownloadMbps > 0 {
				if merged.SpeedLimit.DownloadMbps == 0 || result.SpeedLimit.DownloadMbps < merged.SpeedLimit.DownloadMbps {
					merged.SpeedLimit.DownloadMbps = result.SpeedLimit.DownloadMbps
				}
			}
			if result.SpeedLimit.UploadMbps > 0 {
				if merged.SpeedLimit.UploadMbps == 0 || result.SpeedLimit.UploadMbps < merged.SpeedLimit.UploadMbps {
					merged.SpeedLimit.UploadMbps = result.SpeedLimit.UploadMbps
				}
			}
			// First non-empty group key wins.
			if merged.SpeedLimit.GroupKey == "" && result.SpeedLimit.GroupKey != "" {
				merged.SpeedLimit.GroupKey = result.SpeedLimit.GroupKey
			}
		}
		if result.CleanQuery {
			merged.CleanQuery = true
		}
		if result.RewritePath != "" {
			merged.RewritePath = result.RewritePath
		}
	}

	return merged, nil
}

// OnResponse calls the on_response hook for all plugins bound to the route.
func (m *Manager) OnResponse(ctx context.Context, routeID string, req *RequestInfo, resp *UpstreamResponseInfo) (*ResponseModResult, error) {
	callers, timeout := m.lookupHook(routeID, HookOnResponse)
	if len(callers) == 0 {
		return &ResponseModResult{}, nil
	}

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
	callers, timeout := m.lookupHook(routeID, HookOnConnect)
	if len(callers) == 0 {
		return &ConnResult{Allow: true}, nil
	}

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

// OnDisconnect fires a disconnect notification to all plugins bound to the route.
// Non-blocking: marshals the frame and sends to a buffered channel.
// Dropped silently if the channel is full (fire-and-forget).
func (m *Manager) OnDisconnect(routeID string, info *DisconnectInfo) {
	callers, _ := m.lookupHook(routeID, HookOnDisconnect)
	if len(callers) == 0 {
		return
	}
	frame, err := MarshalEnvelope(HookTypeDisconnect, info)
	if err != nil {
		return
	}
	select {
	case m.disconnects <- disconnectMsg{frame: frame, callers: callers}:
	default:
	}
}

// drainDisconnects processes fire-and-forget disconnect notifications.
func (m *Manager) drainDisconnects(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-m.disconnects:
			for _, c := range msg.callers {
				if err := c.Fire(msg.frame); err != nil {
					slog.Debug("disconnect notify failed", "err", err)
				}
			}
		}
	}
}

// lookupHook returns callers and timeout for a route+hook combination.
// Lock-free: reads from the atomic hook index.
func (m *Manager) lookupHook(routeID, hook string) ([]*Caller, time.Duration) {
	idx := m.index.Load()
	if idx == nil {
		return nil, defaultCallTimeout
	}
	hooks := idx.callers[routeID]
	if hooks == nil {
		return nil, defaultCallTimeout
	}
	timeout := idx.timeouts[routeID]
	if timeout <= 0 {
		timeout = defaultCallTimeout
	}
	return hooks[hook], timeout
}

// rebuildIndexLocked builds a new hookIndex from current bindings and processes.
// Must be called with m.mu held. The resulting index is stored atomically
// and subsequent hot-path reads are lock-free.
func (m *Manager) rebuildIndexLocked() {
	idx := &hookIndex{
		callers:  make(map[string]map[string][]*Caller),
		timeouts: make(map[string]time.Duration),
	}

	for _, b := range m.bindings {
		// Record timeout.
		if b.Timeout > 0 {
			if _, ok := idx.timeouts[b.RouteID]; !ok {
				idx.timeouts[b.RouteID] = b.Timeout
			}
		}

		// Resolve managed process and its caller.
		mg, ok := m.processes[b.Plugin]
		if !ok || mg.caller == nil {
			continue
		}

		// Register caller for each hook it supports.
		for _, hook := range mg.hooks {
			hooks := idx.callers[b.RouteID]
			if hooks == nil {
				hooks = make(map[string][]*Caller)
				idx.callers[b.RouteID] = hooks
			}
			hooks[hook] = append(hooks[hook], mg.caller)
		}
	}

	m.index.Store(idx)
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

	case MethodSetSpeed:
		m.handleSetSpeed(mg, push)

	case MethodReady:
		m.handleReady(mg, push)

	case MethodLog:
		handleLog(mg, push)

	default:
		slog.Warn("plugin unknown push method",
			"plugin", mg.name,
			"method", push.Method,
		)
	}
}

// handleLog routes a plugin log message to prox's slog.
func handleLog(mg *managed, push Push) {
	var level slog.Level
	switch push.Params.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	attrs := make([]any, 0, 2+len(push.Params.Args))
	attrs = append(attrs, "plugin", mg.name)
	attrs = append(attrs, push.Params.Args...)
	slog.Log(context.Background(), level, push.Params.Message, attrs...)
}

// handleReady processes a plugin's capability declaration.
func (m *Manager) handleReady(mg *managed, push Push) {
	if push.Params.Socket == "" {
		slog.Warn("plugin ready without socket",
			"plugin", mg.name,
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

	// Rebuild the hot-path index now that this plugin has declared its hooks.
	m.mu.Lock()
	m.rebuildIndexLocked()
	m.mu.Unlock()

	slog.Debug("plugin ready",
		"plugin", mg.name,
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
		slog.Warn("plugin targets matched no routes",
			"plugin", mg.name,
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
			slog.Debug("plugin updated grouped targets",
				"plugin", mg.name,
				"route", routeID,
				"groups", len(push.Params.Groups),
				"targets", total,
			)
		} else {
			slog.Warn("plugin grouped targets ignored: balancer not keyed",
				"plugin", mg.name,
				"route", routeID,
			)
		}
	} else {
		bal.SwapTargets(push.Params.Targets)
		slog.Debug("plugin updated targets",
			"plugin", mg.name,
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

	slog.Warn("plugin exited, restarting",
		"plugin", mg.name,
		"attempt", mg.restart,
		"delay", delay,
	)

	select {
	case <-ctx.Done():
		mg.proc = nil
		return
	case <-time.After(delay):
	}

	proc, err := startProcess(mg.name, mg.path)
	if err != nil {
		slog.Error("plugin restart failed",
			"plugin", mg.name,
			"attempt", mg.restart,
			"err", err,
		)
		mg.proc = nil
		return
	}

	mg.proc = proc

	slog.Debug("plugin restarted",
		"plugin", mg.name,
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

// GetSpeedLimit returns the plugin-pushed speed limit for a route.
// Lock-free: reads from the atomic speed index.
func (m *Manager) GetSpeedLimit(routeID string) SpeedEntry {
	idx := m.speeds.Load()
	if idx == nil {
		return SpeedEntry{}
	}
	return idx.entries[routeID]
}

// handleSetSpeed routes speed limit pushes to the appropriate routes.
func (m *Manager) handleSetSpeed(mg *managed, push Push) {
	downloadBps := mbpsToBytes(push.Params.DownloadMbps)
	uploadBps := mbpsToBytes(push.Params.UploadMbps)

	entry := SpeedEntry{
		DownloadBps: downloadBps,
		UploadBps:   uploadBps,
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Load current index or create empty one.
	current := m.speeds.Load()
	entries := make(map[string]SpeedEntry)
	if current != nil {
		for k, v := range current.entries {
			entries[k] = v
		}
	}

	if push.Params.Action != "" || push.Params.RouteID == "*" {
		// Action-based or wildcard — apply to matching routes.
		count := 0
		for routeID, ri := range m.routes {
			if push.Params.Action != "" && ri.Action != push.Params.Action {
				continue
			}
			entries[routeID] = entry
			count++
		}
		if count == 0 {
			target := push.Params.RouteID
			if push.Params.Action != "" {
				target = push.Params.Action
			}
			slog.Warn("plugin speed limit matched no routes",
				"plugin", mg.name,
				"target", target,
			)
		} else {
			slog.Debug("plugin updated speed limits",
				"plugin", mg.name,
				"routes", count,
				"download_bps", downloadBps,
				"upload_bps", uploadBps,
			)
		}
	} else {
		// Route-bound targeting.
		entries[push.Params.RouteID] = entry
		slog.Debug("plugin updated speed limit",
			"plugin", mg.name,
			"route", push.Params.RouteID,
			"download_bps", downloadBps,
			"upload_bps", uploadBps,
		)
	}

	m.speeds.Store(&speedIndex{entries: entries})

	// Update rates for active group buckets if group key is specified.
	if push.Params.GroupKey != "" {
		m.groupBuckets.UpdateRate(push.Params.GroupKey, downloadBps, uploadBps)
	}
}

// mbpsToBytes converts megabits per second to bytes per second.
func mbpsToBytes(mbps float64) int64 {
	if mbps <= 0 {
		return 0
	}
	return int64(mbps * 1_000_000 / 8)
}

