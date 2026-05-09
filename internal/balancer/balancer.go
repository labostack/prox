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
	Next() string

	// Done signals that a request/connection to the given target has finished.
	// This is a no-op for strategies that don't track active connections.
	Done(target string)
}

// RoundRobin distributes requests evenly across targets in order.
type RoundRobin struct {
	targets []string
	counter atomic.Uint64
}

// NewRoundRobin creates a round-robin balancer.
func NewRoundRobin(targets []string) *RoundRobin {
	return &RoundRobin{targets: targets}
}

func (rr *RoundRobin) Next() string {
	n := rr.counter.Add(1)
	return rr.targets[(n-1)%uint64(len(rr.targets))]
}

func (rr *RoundRobin) Done(string) {}

// Random selects a target at random.
type Random struct {
	targets []string
}

// NewRandom creates a random balancer.
func NewRandom(targets []string) *Random {
	return &Random{targets: targets}
}

func (r *Random) Next() string {
	return r.targets[rand.Intn(len(r.targets))]
}

func (r *Random) Done(string) {}

// LeastConn routes to the target with the fewest active connections.
// When multiple targets share the minimum, the first one found is selected.
type LeastConn struct {
	targets []string
	conns   []atomic.Int64
	mu      sync.Mutex // protects the find-min-and-increment in Next()
}

// NewLeastConn creates a least-connections balancer.
func NewLeastConn(targets []string) *LeastConn {
	return &LeastConn{
		targets: targets,
		conns:   make([]atomic.Int64, len(targets)),
	}
}

// Next returns the target with the fewest active connections and
// atomically increments its counter.
func (lc *LeastConn) Next() string {
	lc.mu.Lock()
	defer lc.mu.Unlock()

	minIdx := 0
	minVal := lc.conns[0].Load()
	for i := 1; i < len(lc.targets); i++ {
		v := lc.conns[i].Load()
		if v < minVal {
			minVal = v
			minIdx = i
		}
	}

	lc.conns[minIdx].Add(1)
	return lc.targets[minIdx]
}

// Done decrements the active connection counter for the target.
func (lc *LeastConn) Done(target string) {
	for i, t := range lc.targets {
		if t == target {
			lc.conns[i].Add(-1)
			return
		}
	}
}

// Conns returns the current active connection count for a target.
// Intended for testing and diagnostics only.
func (lc *LeastConn) Conns(target string) int64 {
	for i, t := range lc.targets {
		if t == target {
			return lc.conns[i].Load()
		}
	}
	return 0
}
