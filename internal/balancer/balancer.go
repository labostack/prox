// Package balancer implements load balancing strategies for upstream selection.
package balancer

import (
	"math/rand"
	"sync"
	"sync/atomic"
)

// Balancer selects a target from a pool of upstreams.
type Balancer interface {
	// Next returns the next target address.
	// For connection-tracking strategies (e.g. LeastConn), this also
	// marks the target as having one more active connection.
	// Returns "" if the pool is empty.
	Next() string

	// Done signals that a request/connection to the given target has finished.
	// This is a no-op for strategies that don't track active connections.
	Done(target string)

	// SwapTargets atomically replaces the target pool.
	// Active connections tracked by Done() are reset.
	SwapTargets(targets []string)

	// Targets returns a copy of the current target pool.
	Targets() []string
}

// RoundRobin distributes requests evenly across targets in order.
type RoundRobin struct {
	pool    atomic.Pointer[rrPool]
	counter atomic.Uint64
}

type rrPool struct {
	targets []string
}

// NewRoundRobin creates a round-robin balancer.
func NewRoundRobin(targets []string) *RoundRobin {
	rr := &RoundRobin{}
	rr.pool.Store(&rrPool{targets: targets})
	return rr
}

func (rr *RoundRobin) Next() string {
	p := rr.pool.Load()
	if len(p.targets) == 0 {
		return ""
	}
	n := rr.counter.Add(1)
	return p.targets[(n-1)%uint64(len(p.targets))]
}

func (rr *RoundRobin) Done(string) {}

func (rr *RoundRobin) SwapTargets(targets []string) {
	rr.pool.Store(&rrPool{targets: targets})
}

func (rr *RoundRobin) Targets() []string {
	p := rr.pool.Load()
	return append([]string(nil), p.targets...)
}

// Random selects a target at random.
type Random struct {
	pool atomic.Pointer[randPool]
}

type randPool struct {
	targets []string
}

// NewRandom creates a random balancer.
func NewRandom(targets []string) *Random {
	r := &Random{}
	r.pool.Store(&randPool{targets: targets})
	return r
}

func (r *Random) Next() string {
	p := r.pool.Load()
	if len(p.targets) == 0 {
		return ""
	}
	return p.targets[rand.Intn(len(p.targets))]
}

func (r *Random) Done(string) {}

func (r *Random) SwapTargets(targets []string) {
	r.pool.Store(&randPool{targets: targets})
}

func (r *Random) Targets() []string {
	p := r.pool.Load()
	return append([]string(nil), p.targets...)
}

// LeastConn routes to the target with the fewest active connections.
// When multiple targets share the minimum, the first one found is selected.
type LeastConn struct {
	pool atomic.Pointer[lcPool]
	mu   sync.Mutex // protects the find-min-and-increment in Next()
}

type lcPool struct {
	targets []string
	conns   []atomic.Int64
	index   map[string]int // target → slice index for O(1) lookup
}

// NewLeastConn creates a least-connections balancer.
func NewLeastConn(targets []string) *LeastConn {
	lc := &LeastConn{}
	lc.pool.Store(newLCPool(targets))
	return lc
}

func newLCPool(targets []string) *lcPool {
	idx := make(map[string]int, len(targets))
	for i, t := range targets {
		idx[t] = i
	}
	return &lcPool{
		targets: targets,
		conns:   make([]atomic.Int64, len(targets)),
		index:   idx,
	}
}

// Next returns the target with the fewest active connections and
// atomically increments its counter.
func (lc *LeastConn) Next() string {
	lc.mu.Lock()
	defer lc.mu.Unlock()

	p := lc.pool.Load()
	if len(p.targets) == 0 {
		return ""
	}

	minIdx := 0
	minVal := p.conns[0].Load()
	for i := 1; i < len(p.targets); i++ {
		v := p.conns[i].Load()
		if v < minVal {
			minVal = v
			minIdx = i
		}
	}

	p.conns[minIdx].Add(1)
	return p.targets[minIdx]
}

// Done decrements the active connection counter for the target.
func (lc *LeastConn) Done(target string) {
	p := lc.pool.Load()
	if i, ok := p.index[target]; ok {
		p.conns[i].Add(-1)
	}
}

// SwapTargets atomically replaces the target pool.
// Active connection counts are reset to zero.
func (lc *LeastConn) SwapTargets(targets []string) {
	lc.mu.Lock()
	defer lc.mu.Unlock()

	lc.pool.Store(newLCPool(targets))
}

func (lc *LeastConn) Targets() []string {
	p := lc.pool.Load()
	return append([]string(nil), p.targets...)
}

// Conns returns the current active connection count for a target.
// Intended for testing and diagnostics only.
func (lc *LeastConn) Conns(target string) int64 {
	p := lc.pool.Load()
	if i, ok := p.index[target]; ok {
		return p.conns[i].Load()
	}
	return 0
}

// KeyedBalancer extends Balancer with key-based target selection.
// Used by grouped balancers where the key (e.g., a domain wildcard capture)
// determines which sub-pool of targets to pick from.
type KeyedBalancer interface {
	Balancer
	NextKeyed(key string) string
	SwapGroupedTargets(groups map[string][]string)
}

// Grouped wraps a balancing strategy and provides per-key target pools.
// In flat mode (via SwapTargets), all requests use the inner balancer.
// In grouped mode (via SwapGroupedTargets), each key has its own sub-balancer.
type Grouped struct {
	strategy string
	inner    Balancer // flat-mode fallback
	groups   atomic.Pointer[groupedMap]
	mu       sync.Mutex // protects SwapGroupedTargets
}

type groupedMap struct {
	m           map[string]Balancer
	targetGroup map[string]string // target → group key for O(1) Done() routing
}

// NewGrouped creates a grouped balancer wrapping an inner flat balancer.
// The strategy name is used to create per-key sub-balancers.
func NewGrouped(strategy string, inner Balancer) *Grouped {
	return &Grouped{
		strategy: strategy,
		inner:    inner,
	}
}

// Next delegates to the inner flat balancer (ignores grouping).
func (g *Grouped) Next() string {
	return g.inner.Next()
}

// NextKeyed selects a target from the sub-pool matching the key.
// Falls back to the inner balancer if no groups are configured or key is empty.
func (g *Grouped) NextKeyed(key string) string {
	if gm := g.groups.Load(); gm != nil && key != "" {
		if bal, ok := gm.m[key]; ok {
			return bal.Next()
		}
		return ""
	}
	return g.inner.Next()
}

// Done decrements the active connection counter for the target.
// Checks grouped sub-balancers first, then falls back to the inner balancer.
func (g *Grouped) Done(target string) {
	if gm := g.groups.Load(); gm != nil {
		if key, ok := gm.targetGroup[target]; ok {
			gm.m[key].Done(target)
		}
		return
	}
	g.inner.Done(target)
}

// SwapTargets replaces the flat target pool and clears any grouped state.
func (g *Grouped) SwapTargets(targets []string) {
	g.inner.SwapTargets(targets)
	g.groups.Store(nil)
}

func (g *Grouped) Targets() []string {
	return g.inner.Targets()
}

// SwapGroupedTargets atomically replaces the per-key target pools.
// Existing sub-balancers for unchanged keys are reused (preserving
// connection tracking state for leastconn).
func (g *Grouped) SwapGroupedTargets(groups map[string][]string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	old := g.groups.Load()
	newMap := make(map[string]Balancer, len(groups))

	for key, targets := range groups {
		// Reuse existing sub-balancer to preserve leastconn state.
		if old != nil {
			if existing, ok := old.m[key]; ok {
				existing.SwapTargets(targets)
				newMap[key] = existing
				continue
			}
		}
		newMap[key] = newByStrategy(g.strategy, targets)
	}

	tg := make(map[string]string, len(groups)*2)
	for key, targets := range groups {
		for _, t := range targets {
			tg[t] = key
		}
	}

	g.groups.Store(&groupedMap{m: newMap, targetGroup: tg})
}

// NewByType creates a flat balancer of the given type.
// Valid types: "random", "leastconn", "roundrobin" (default).
func NewByType(strategy string, targets []string) Balancer {
	return newByStrategy(strategy, targets)
}

// newByStrategy creates a flat balancer of the given type.
func newByStrategy(strategy string, targets []string) Balancer {
	switch strategy {
	case "random":
		return NewRandom(targets)
	case "leastconn":
		return NewLeastConn(targets)
	default:
		return NewRoundRobin(targets)
	}
}
