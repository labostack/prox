// Package router implements first-match HTTP request routing.
//
// Routes are evaluated in order. The first route whose path pattern,
// domain pattern, and method filter match the incoming request is selected.
// If no route matches, the router returns nil and the caller should respond with 404.
package router

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/labostack/prox/internal/balancer"
	"github.com/labostack/prox/internal/config"
	"github.com/labostack/prox/internal/throttle"
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
	ForwardProxy  bool   // true when matched via a forward_proxy route
	done          func() // release callback for connection-tracking balancers

	// Speed limiting — populated from route config.
	// Shared buckets are route-wide; BPS rates are for per-connection bucket creation.
	DownloadBucket *throttle.Bucket // shared download bucket (nil when per-conn or no limit)
	UploadBucket   *throttle.Bucket // shared upload bucket (nil when per-conn or no limit)
	DownloadBps    int64             // per-connection download bytes/sec (0 = no limit)
	UploadBps      int64             // per-connection upload bytes/sec (0 = no limit)
}

// Done signals that the request/connection has completed.
// For connection-tracking balancers (e.g. leastconn), this decrements
// the active connection counter. Safe to call multiple times or on nil.
func (m *MatchResult) Done() {
	if m != nil && m.done != nil {
		m.done()
	}
}

// RouteID returns a compact identifier for this route (e.g. "web:0").
func (m *MatchResult) RouteID(service string) string {
	return service + ":" + strconv.Itoa(m.RouteIndex)
}

// GetMatchResult retrieves the MatchResult from request context.
func GetMatchResult(r *http.Request) *MatchResult {
	v, _ := r.Context().Value(matchResultKey).(*MatchResult)
	return v
}

// Router holds compiled routes for a single service.
type Router struct {
	routes           []*compiledRoute
	needsDomainMatch bool // true if any route has domain-based matching
	isCatchAllOnly   bool // true if there is only one catch-all route
	catchAllAction   string
	hasSpeed         bool // true if any route has speed limiting
}

// HasSpeed returns true if any route in this router has speed limiting configured.
func (rt *Router) HasSpeed() bool { return rt.hasSpeed }

// compiledRoute is an optimized, pre-processed representation of a config route.
type compiledRoute struct {
	action  string
	paths   []pathEntry     // one or more path patterns (OR logic); empty = match all
	methods map[string]bool // nil means "all methods"

	// Domain matching — segment-based glob.
	// Each "*" matches exactly one domain label.
	// "**" matches one or more labels (only valid as last segment).
	domain         string   // original pattern (for MatchResult)
	domainSegments []string // nil = match all hosts
	domainGlob     bool     // true when pattern ends with "**"

	// Forward proxy matching — only matches requests with absolute URL.
	forwardProxy bool

	// Load balancing.
	bal balancer.Balancer // nil = no balancing

	// Route-level variables from "set".
	vars map[string]string

	// Speed limiting.
	downloadBucket *throttle.Bucket // shared download bucket (nil if per-conn or no limit)
	uploadBucket   *throttle.Bucket // shared upload bucket
	downloadBps    int64            // per-connection download rate in bytes/sec
	uploadBps      int64            // per-connection upload rate in bytes/sec
}

// pathEntry holds a single precomputed path pattern.
type pathEntry struct {
	raw    string // original pattern, e.g. "/api/*"
	isWild bool   // true for wildcard paths like "/api/*"
	prefix string // for wildcards: the prefix before "*"
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
			cr.forwardProxy = r.Match.ForwardProxy
			cr.domain = r.Match.Domain

			// Pre-compute path entries — split comma-separated patterns.
			if r.Match.Path != "" {
				for _, raw := range strings.Split(r.Match.Path, ",") {
					p := strings.TrimSpace(raw)
					if p == "" {
						continue
					}
					pe := pathEntry{raw: p}
					if strings.HasSuffix(p, "/*") {
						pe.isWild = true
						pe.prefix = strings.TrimSuffix(p, "*")
					}
					cr.paths = append(cr.paths, pe)
				}
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

		// Pre-compute speed limiting buckets/rates.
		if r.Speed != nil {
			downBps := throttle.MbpsToBytes(r.Speed.DownloadMbps)
			upBps := throttle.MbpsToBytes(r.Speed.UploadMbps)
			if r.Speed.Shared {
				cr.downloadBucket = throttle.NewBucket(downBps)
				cr.uploadBucket = throttle.NewBucket(upBps)
			} else {
				cr.downloadBps = downBps
				cr.uploadBps = upBps
			}
		}

		compiled = append(compiled, cr)
	}

	rt := &Router{routes: compiled}
	for _, cr := range compiled {
		if cr.domainSegments != nil {
			rt.needsDomainMatch = true
		}
		if cr.downloadBucket != nil || cr.uploadBucket != nil || cr.downloadBps > 0 || cr.uploadBps > 0 {
			rt.hasSpeed = true
		}
		if rt.needsDomainMatch && rt.hasSpeed {
			break
		}
	}

	if len(compiled) == 1 {
		cr := compiled[0]
		if len(cr.paths) == 0 && cr.domainSegments == nil && cr.methods == nil && !cr.forwardProxy {
			rt.isCatchAllOnly = true
			rt.catchAllAction = cr.action
		}
	}

	return rt
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

// MatchAction returns only the action name for the matching route, without
// allocating a MatchResult or modifying the request context. Used by the
// handler fast path when plugins and access logging are disabled.
func (rt *Router) MatchAction(r *http.Request) string {
	if rt.isCatchAllOnly {
		return rt.catchAllAction
	}

	var hostSegments []string
	if rt.needsDomainMatch {
		host := resolveHost(r)
		hostSegments = strings.Split(strings.ToLower(host), ".")
	}

	for _, route := range rt.routes {
		if !route.matchForwardProxy(r) {
			continue
		}
		ok, _, _ := route.matchDomain(hostSegments)
		if !ok {
			continue
		}
		if !route.matchPath(r.URL.Path) {
			continue
		}
		if !route.matchMethod(r.Method) {
			continue
		}
		return route.action
	}
	return ""
}

// Match finds the first matching route and returns the enriched request with
// MatchResult stored in context, plus the action name.
func (rt *Router) Match(r *http.Request) (*http.Request, string) {
	if rt.isCatchAllOnly {
		route := rt.routes[0]
		host := resolveHost(r)
		result := &MatchResult{
			RouteIndex:     0,
			Action:         route.action,
			DomainPattern:  route.domain,
			Domain:         host,
			Path:           r.URL.Path,
			Vars:           route.vars,
			DownloadBucket: route.downloadBucket,
			UploadBucket:   route.uploadBucket,
			DownloadBps:    route.downloadBps,
			UploadBps:      route.uploadBps,
		}

		if route.bal != nil {
			result.Target = route.bal.Next()
			bal := route.bal
			target := result.Target
			result.done = func() { bal.Done(target) }
		}

		ctx := context.WithValue(r.Context(), matchResultKey, result)
		return r.WithContext(ctx), route.action
	}

	var hostSegments []string
	host := resolveHost(r)
	if rt.needsDomainMatch {
		hostSegments = strings.Split(strings.ToLower(host), ".")
	}

	for i, route := range rt.routes {
		if !route.matchForwardProxy(r) {
			continue
		}
		ok, captures, globTail := route.matchDomain(hostSegments)
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
			RouteIndex:     i,
			Action:         route.action,
			DomainPattern:  route.domain,
			MatchDomain:    strings.Join(captures, "."),
			MatchGlob:      globTail,
			MatchPath:      route.matchedPathPattern(r.URL.Path),
			Domain:         host,
			Path:           r.URL.Path,
			Vars:           route.vars,
			ForwardProxy:   route.forwardProxy,
			DownloadBucket: route.downloadBucket,
			UploadBucket:   route.uploadBucket,
			DownloadBps:    route.downloadBps,
			UploadBps:      route.uploadBps,
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

// matchPath checks if the request path matches any of this route's path patterns.
// An empty paths slice matches all paths.
func (cr *compiledRoute) matchPath(reqPath string) bool {
	if len(cr.paths) == 0 {
		return true
	}
	for _, pe := range cr.paths {
		if pe.isWild {
			if strings.HasPrefix(reqPath, pe.prefix) {
				return true
			}
		} else if reqPath == pe.raw {
			return true
		}
	}
	return false
}

// matchedPathPattern returns the specific pattern that matched the request path.
func (cr *compiledRoute) matchedPathPattern(reqPath string) string {
	for _, pe := range cr.paths {
		if pe.isWild {
			if strings.HasPrefix(reqPath, pe.prefix) {
				return pe.raw
			}
		} else if reqPath == pe.raw {
			return pe.raw
		}
	}
	return ""
}

// matchMethod checks if the request method is allowed.
// A nil method set means all methods are accepted.
func (cr *compiledRoute) matchMethod(method string) bool {
	if cr.methods == nil {
		return true
	}
	return cr.methods[method]
}

// IsForwardProxyRequest reports whether the HTTP request is a forward proxy
// request. Two cases are detected:
//
//  1. HTTP/1.1: absolute URL in request line ("GET http://host/path HTTP/1.1").
//     Go's net/http parses this into r.URL with Scheme and Host populated.
//
//  2. HTTP/2 over TLS: the :authority pseudo-header (mapped to r.Host) differs
//     from the TLS SNI hostname (r.TLS.ServerName). This happens when a browser
//     connects to the proxy via HTTPS/H2 and sends requests for a different target.
//     Go's HTTP/2 server does not populate r.URL.Scheme from the :scheme
//     pseudo-header, so case 1 does not match.
func IsForwardProxyRequest(r *http.Request) bool {
	// HTTP/1.1: absolute URL in request line.
	if r.URL.Scheme != "" && r.URL.Host != "" {
		return true
	}
	// HTTP/2 over TLS: :authority differs from TLS SNI.
	if r.ProtoMajor >= 2 && r.TLS != nil && r.TLS.ServerName != "" {
		reqHost := r.Host
		if h, _, err := net.SplitHostPort(reqHost); err == nil {
			reqHost = h
		}
		if !strings.EqualFold(reqHost, r.TLS.ServerName) {
			return true
		}
	}
	return false
}

// matchForwardProxy checks if the request matches this route's forward proxy setting.
// When forwardProxy is true, only forward proxy requests (absolute URL or
// HTTP/2 SNI mismatch) match.
// When forwardProxy is false, forward proxy requests are rejected — they must
// be routed to a dedicated forward_proxy route or return 404.
func (cr *compiledRoute) matchForwardProxy(r *http.Request) bool {
	if cr.forwardProxy {
		return IsForwardProxyRequest(r)
	}
	return !IsForwardProxyRequest(r)
}

// matchDomain checks if the request host matches this route's domain pattern.
// Returns (matched, captures, globTail):
//   - captures: values matched by "*" segments (full or partial)
//   - globTail: the suffix matched by "**" (empty when no glob)
//
// nil domainSegments matches all hosts (returns true, nil, "").
func (cr *compiledRoute) matchDomain(hostSegments []string) (bool, []string, string) {
	if cr.domainSegments == nil {
		return true, nil, ""
	}

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

// resolveHost returns the effective hostname for routing.
// For CONNECT requests with TLS, it uses the SNI server name (which is
// the clean hostname), because r.Host typically contains host:port.
func resolveHost(r *http.Request) string {
	if r.Method == "CONNECT" && r.TLS != nil && r.TLS.ServerName != "" {
		return r.TLS.ServerName
	}
	return stripPort(r.Host)
}

// stripPort removes the port from a host string.
func stripPort(host string) string {
	if idx := strings.LastIndexByte(host, ':'); idx != -1 {
		return host[:idx]
	}
	return host
}
