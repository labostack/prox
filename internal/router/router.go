// Package router implements first-match HTTP request routing.
//
// Routes are evaluated in order. The first route whose path pattern,
// domain pattern, and method filter match the incoming request is selected.
// If no route matches, the router returns nil and the caller should respond with 404.
package router

import (
	"context"
	"net/http"
	"strings"

	"github.com/dortanes/prox/internal/balancer"
	"github.com/dortanes/prox/internal/config"
)

// ctxKey is an unexported type for context keys to avoid collisions.
type ctxKey struct{}

// matchResultKey is the context key for MatchResult.
var matchResultKey = ctxKey{}

// MatchResult holds data captured during route matching, available to handlers.
type MatchResult struct {
	RouteIndex    int    // index of the matched route in the service's route list
	Action        string // resolved action name
	DomainPattern string // the pattern from config, e.g. "*.myapp.dev"
	MatchDomain   string // captured "*" wildcard value(s), e.g. "sub" for *.myapp.dev
	MatchGlob     string // captured "**" glob suffix, e.g. "example.com" for *.storage.**
	MatchPath     string // the path pattern, e.g. "/api/*"
	Domain        string // actual request host (no port)
	Path          string // actual request path
	Target        string // selected target from balancer (empty if no balancer)
	Vars          map[string]string // route-level variables from "set"
	done          func() // release callback for connection-tracking balancers
}

// Done signals that the request/connection has completed.
// For connection-tracking balancers (e.g. leastconn), this decrements
// the active connection counter. Safe to call multiple times or on nil.
func (m *MatchResult) Done() {
	if m != nil && m.done != nil {
		m.done()
	}
}

// GetMatchResult retrieves the MatchResult from request context.
func GetMatchResult(r *http.Request) *MatchResult {
	v, _ := r.Context().Value(matchResultKey).(*MatchResult)
	return v
}

// Router holds compiled routes for a single service.
type Router struct {
	routes []*compiledRoute
}

// compiledRoute is an optimized, pre-processed representation of a config route.
type compiledRoute struct {
	action  string
	path    string
	isWild  bool            // true for wildcard paths like "/api/*"
	prefix  string          // for wildcards: the prefix before "*"
	methods map[string]bool // nil means "all methods"

	// Domain matching — segment-based glob.
	// Each "*" matches exactly one domain label.
	// "**" matches one or more labels (only valid as last segment).
	domain         string   // original pattern (for MatchResult)
	domainSegments []string // nil = match all hosts
	domainGlob     bool     // true when pattern ends with "**"

	// Load balancing.
	bal balancer.Balancer // nil = no balancing

	// Route-level variables from "set".
	vars map[string]string
}

// New compiles a list of config routes into a Router.
func New(routes []*config.Route) *Router {
	compiled := make([]*compiledRoute, 0, len(routes))

	for _, r := range routes {
		cr := &compiledRoute{
			action: r.Action.Name,
		}

		// Nil match = catch-all route (matches everything).
		if r.Match != nil {
			cr.path = r.Match.Path
			cr.domain = r.Match.Domain

			// Pre-compute wildcard prefix.
			if strings.HasSuffix(r.Match.Path, "/*") {
				cr.isWild = true
				cr.prefix = strings.TrimSuffix(r.Match.Path, "*")
			}

			// Pre-compute domain segments for glob matching.
			if r.Match.Domain != "" {
				cr.domainSegments = strings.Split(strings.ToLower(r.Match.Domain), ".")
				// Detect trailing "**" glob.
				if last := len(cr.domainSegments) - 1; last >= 0 && cr.domainSegments[last] == "**" {
					cr.domainGlob = true
					cr.domainSegments = cr.domainSegments[:last] // strip "**" from segments
				}
			}

			// Pre-compute method set for O(1) lookup.
			if len(r.Match.Methods) > 0 {
				cr.methods = make(map[string]bool, len(r.Match.Methods))
				for _, m := range r.Match.Methods {
					cr.methods[strings.ToUpper(m)] = true
				}
			}
		}

		cr.bal = buildBalancer(r.Balancer)
		cr.vars = r.Set

		compiled = append(compiled, cr)
	}

	return &Router{routes: compiled}
}

// RouteBalancer returns the balancer instance for the route at the given index.
// Returns nil if the index is out of range or the route has no balancer.
func (rt *Router) RouteBalancer(index int) balancer.Balancer {
	if index >= 0 && index < len(rt.routes) {
		return rt.routes[index].bal
	}
	return nil
}

// SetRouteBalancer replaces the balancer for the route at the given index.
// Used to wrap a flat balancer in a Grouped balancer for plugin-managed routes.
func (rt *Router) SetRouteBalancer(index int, bal balancer.Balancer) {
	if index >= 0 && index < len(rt.routes) {
		rt.routes[index].bal = bal
	}
}

// buildBalancer creates a balancer from the route's config.
// A balancer is always created when a config is present, even with an empty
// target list — plugins may populate targets dynamically via SwapTargets().
func buildBalancer(cfg *config.BalancerConfig) balancer.Balancer {
	if cfg == nil {
		return nil
	}
	switch cfg.Type {
	case config.BalancerRandom:
		return balancer.NewRandom(cfg.Targets)
	case config.BalancerLeastConn:
		return balancer.NewLeastConn(cfg.Targets)
	default:
		return balancer.NewRoundRobin(cfg.Targets)
	}
}

// Match finds the first route matching the given request.
// Returns the action name (empty if no match) and injects MatchResult into context.
func (rt *Router) Match(r *http.Request) (*http.Request, string) {
	host := stripPort(r.Host)

	for i, route := range rt.routes {
		ok, captures, globTail := route.matchDomain(host)
		if !ok {
			continue
		}
		if !route.matchPath(r.URL.Path) {
			continue
		}
		if !route.matchMethod(r.Method) {
			continue
		}

		result := &MatchResult{
			RouteIndex:    i,
			Action:        route.action,
			DomainPattern: route.domain,
			MatchDomain:   strings.Join(captures, "."),
			MatchGlob:     globTail,
			MatchPath:     route.path,
			Domain:        host,
			Path:          r.URL.Path,
			Vars:          route.vars,
		}

		// Select a target from the balancer if present.
		if route.bal != nil {
			if kb, ok := route.bal.(balancer.KeyedBalancer); ok && len(captures) > 0 {
				result.Target = kb.NextKeyed(captures[0])
			} else {
				result.Target = route.bal.Next()
			}
			bal := route.bal
			target := result.Target
			result.done = func() { bal.Done(target) }
		}

		ctx := context.WithValue(r.Context(), matchResultKey, result)

		return r.WithContext(ctx), route.action
	}
	return r, ""
}

// matchPath checks if the request path matches this route's pattern.
// An empty path matches all paths.
func (cr *compiledRoute) matchPath(reqPath string) bool {
	if cr.path == "" {
		return true
	}
	if cr.isWild {
		return strings.HasPrefix(reqPath, cr.prefix)
	}
	return reqPath == cr.path
}

// matchMethod checks if the request method is allowed.
// A nil method set means all methods are accepted.
func (cr *compiledRoute) matchMethod(method string) bool {
	if cr.methods == nil {
		return true
	}
	return cr.methods[method]
}

// matchDomain checks if the request host matches this route's domain pattern.
// Returns (matched, captures, globTail):
//   - captures: values matched by "*" segments (full or partial)
//   - globTail: the suffix matched by "**" (empty when no glob)
//
// nil domainSegments matches all hosts (returns true, nil, "").
func (cr *compiledRoute) matchDomain(host string) (bool, []string, string) {
	if cr.domainSegments == nil {
		return true, nil, ""
	}

	hostSegments := strings.Split(strings.ToLower(host), ".")

	if cr.domainGlob {
		// "**" was stripped — remaining segments are the prefix.
		// Host must have at least prefix + 1 label for the glob.
		if len(hostSegments) <= len(cr.domainSegments) {
			return false, nil, ""
		}
		var captures []string
		for i, pat := range cr.domainSegments {
			ok, cap := matchSegment(pat, hostSegments[i])
			if !ok {
				return false, nil, ""
			}
			if cap != "" {
				captures = append(captures, cap)
			}
		}
		// The trailing labels matched by "**" go into globTail.
		tail := strings.Join(hostSegments[len(cr.domainSegments):], ".")
		return true, captures, tail
	}

	// Exact segment count match.
	if len(hostSegments) != len(cr.domainSegments) {
		return false, nil, ""
	}

	var captures []string
	for i, pat := range cr.domainSegments {
		ok, cap := matchSegment(pat, hostSegments[i])
		if !ok {
			return false, nil, ""
		}
		if cap != "" {
			captures = append(captures, cap)
		}
	}

	return true, captures, ""
}

// matchSegment checks if a host segment matches a pattern segment.
// Returns (matched, capture). Full wildcard "*" captures the entire segment.
// Partial wildcards like "cdn-*" or "*-prod" capture the variable portion.
// Literals return an empty capture.
func matchSegment(pat, seg string) (bool, string) {
	if pat == "*" {
		return true, seg
	}
	starIdx := strings.Index(pat, "*")
	if starIdx == -1 {
		return pat == seg, ""
	}
	// Partial wildcard: split around "*" into prefix and suffix.
	prefix := pat[:starIdx]
	suffix := pat[starIdx+1:]
	if len(seg) < len(prefix)+len(suffix) {
		return false, ""
	}
	if !strings.HasPrefix(seg, prefix) || !strings.HasSuffix(seg, suffix) {
		return false, ""
	}
	return true, seg[len(prefix) : len(seg)-len(suffix)]
}

// stripPort removes the port from a host string.
func stripPort(host string) string {
	if idx := strings.LastIndex(host, ":"); idx != -1 {
		return host[:idx]
	}
	return host
}
