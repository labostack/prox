package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
)

// ValidationError collects all validation issues into a single error.
type ValidationError struct {
	Issues []string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("config validation failed with %d issue(s):\n  - %s",
		len(e.Issues), strings.Join(e.Issues, "\n  - "))
}

// Validate checks the configuration for correctness.
// Expects a normalized config (see loader.go).
func Validate(cfg *Config) error {
	v := &validator{cfg: cfg}

	v.validateServices()
	v.validatePluginsRegistry()
	v.validateActions()
	v.validateLogging()
	v.validateAdmin()

	if len(v.issues) > 0 {
		return &ValidationError{Issues: v.issues}
	}

	return nil
}

type validator struct {
	cfg    *Config
	issues []string
}

func (v *validator) addIssue(format string, args ...any) {
	v.issues = append(v.issues, fmt.Sprintf(format, args...))
}

// validateServices checks all service definitions and their routes.
func (v *validator) validateServices() {
	if len(v.cfg.Services) == 0 {
		v.addIssue("no services defined")
		return
	}

	for name, svc := range v.cfg.Services {
		v.validateService(name, svc)
	}
}

func (v *validator) validateService(name string, svc *Service) {
	if svc.Listen == "" {
		v.addIssue("service %q: listen address is required", name)
	}

	if svc.TLS {
		if svc.TLSCert == "" && svc.ACME == nil {
			v.addIssue("service %q: tls_cert is required when tls is enabled (file or directory path), unless acme is configured", name)
		}
		// tls_key is only required in file mode — in directory mode keys are
		// discovered automatically alongside their matching .crt/.pem files.
		// We can't stat the path at validation time (config may be validated
		// before deployment), so we accept missing tls_key and let
		// server.loadCertificates handle the error at runtime.
	}

	v.validateACME(name, svc)
	v.validatePluginNames(fmt.Sprintf("service %q", name), svc.Plugins)

	if len(svc.Routes) == 0 {
		v.addIssue("service %q: at least one route is required", name)
		return
	}

	for i, route := range svc.Routes {
		v.validateRoute(name, i, route)
	}
}

func (v *validator) validateRoute(svcName string, idx int, route *Route) {
	prefix := fmt.Sprintf("service %q route #%d", svcName, idx)

	// Nil match is allowed — it acts as a catch-all route.
	if route.Match != nil {
		if route.Match.Path == "" && route.Match.Domain == "" {
			v.addIssue("%s: match.path or match.domain is required (or omit match entirely for catch-all)", prefix)
		}

		v.validatePath(prefix, route.Match.Path)
		v.validateDomain(prefix, route.Match.Domain)
		v.validateMethods(prefix, route.Match.Methods)
	}

	if route.Action.IsEmpty() {
		v.addIssue("%s: action is required", prefix)
	} else if route.Action.Name != "" {
		// After normalization, all refs should be Name-based.
		act, ok := v.cfg.Actions[route.Action.Name]
		if !ok {
			v.addIssue("%s: action %q not found in actions", prefix, route.Action.Name)
		} else if act.Type == ActionTypePass {
			// Pass routes operate at L4 (pre-TLS) — only domain/SNI matching is available.
			if route.Match != nil {
				if route.Match.Path != "" {
					v.addIssue("%s: pass routes cannot use path matching (L4 operates before HTTP)", prefix)
				}
				if len(route.Match.Methods) > 0 {
					v.addIssue("%s: pass routes cannot use method matching (L4 operates before HTTP)", prefix)
				}
			}
			if route.Match == nil || route.Match.Domain == "" {
				v.addIssue("%s: pass routes require a domain pattern (SNI matching)", prefix)
			}
		}
	}

	if len(route.Plugins) > 0 {
		v.validatePluginNames(prefix, route.Plugins)
	}

	if route.Balancer != nil {
		v.validateBalancer(prefix, route)
	}

	if route.Speed != nil {
		v.validateSpeed(prefix, route.Speed)
	}
}

// validatePath ensures the path pattern is well-formed.
// Supports comma-separated patterns (e.g. "/api/*, /ws").
func (v *validator) validatePath(prefix, path string) {
	if path == "" {
		return // already reported above
	}

	for _, raw := range strings.Split(path, ",") {
		p := strings.TrimSpace(raw)
		if p == "" {
			continue
		}

		if !strings.HasPrefix(p, "/") {
			v.addIssue("%s: path %q must start with /", prefix, p)
			continue
		}

		// Wildcard is only valid at the end: "/api/*"
		starIdx := strings.Index(p, "*")
		if starIdx != -1 && starIdx != len(p)-1 {
			v.addIssue("%s: wildcard * is only allowed at the end of a path pattern (in %q)", prefix, p)
		}
	}
}

// validateDomain ensures the domain pattern is well-formed.
// Supports "*" as a single-label wildcard and "**" as a multi-label
// glob (only valid as the last segment).
func (v *validator) validateDomain(prefix, domain string) {
	if domain == "" {
		return
	}

	segments := strings.Split(domain, ".")
	if len(segments) < 2 {
		v.addIssue("%s: domain must have at least two segments (e.g. example.com)", prefix)
		return
	}

	for i, seg := range segments {
		if seg == "" {
			v.addIssue("%s: domain has an empty segment (double dot or leading/trailing dot)", prefix)
			return
		}
		// "**" is only valid as the very last segment.
		if seg == "**" {
			if i != len(segments)-1 {
				v.addIssue("%s: \"**\" glob is only allowed as the last domain segment", prefix)
			}
			continue
		}
		// A segment may contain at most one "*" for wildcard matching.
		// Full wildcard "*" matches the entire label.
		// Partial wildcards like "cdn-*" or "*-prod" match with prefix/suffix.
		if strings.Count(seg, "*") > 1 {
			v.addIssue("%s: segment %q has multiple wildcards — only one \"*\" per segment is allowed", prefix, seg)
		}
	}
}

func (v *validator) validateMethods(prefix string, methods []string) {
	validMethods := map[string]bool{
		http.MethodGet:     true,
		http.MethodHead:    true,
		http.MethodPost:    true,
		http.MethodPut:     true,
		http.MethodPatch:   true,
		http.MethodDelete:  true,
		http.MethodConnect: true,
		http.MethodOptions: true,
		http.MethodTrace:   true,
	}

	for _, m := range methods {
		if !validMethods[strings.ToUpper(m)] {
			v.addIssue("%s: unknown HTTP method %q", prefix, m)
		}
	}
}

func (v *validator) validateActions() {
	if len(v.cfg.Actions) == 0 {
		v.addIssue("no actions defined")
		return
	}

	for name, action := range v.cfg.Actions {
		v.validateAction(name, action)
	}
}

func (v *validator) validateAction(name string, action *Action) {
	v.validatePluginNames(fmt.Sprintf("action %q", name), action.Plugins)

	switch action.Type {
	case ActionTypeProxy:
		v.validateProxyAction(name, action)
	case ActionTypeStatic:
		v.validateStaticAction(name, action)
	case ActionTypeServe:
		v.validateServeAction(name, action)
	case ActionTypePass:
		v.validatePassAction(name, action)
	case ActionTypeDrop:
		// No fields to validate.
	case "":
		v.addIssue("action %q: type is required", name)
	default:
		v.addIssue("action %q: unknown type %q (expected %q, %q, %q, %q, or %q)",
			name, action.Type, ActionTypeProxy, ActionTypeStatic, ActionTypeServe, ActionTypePass, ActionTypeDrop)
	}
}

func (v *validator) validateProxyAction(name string, action *Action) {
	if action.Upstream == "" {
		v.addIssue("action %q (proxy): upstream is required", name)
	}
}

func (v *validator) validateStaticAction(name string, action *Action) {
	if action.Status == 0 {
		v.addIssue("action %q (static): status is required", name)
	} else if action.Status < 100 || action.Status > 599 {
		v.addIssue("action %q (static): status %d is not a valid HTTP status code", name, action.Status)
	}

	if action.BodyRef.Name != "" {
		if v.cfg.Resources == nil {
			v.addIssue("action %q (static): body_ref %q referenced but no resources defined",
				name, action.BodyRef.Name)
		} else if _, ok := v.cfg.Resources[action.BodyRef.Name]; !ok {
			v.addIssue("action %q (static): body_ref %q not found in resources",
				name, action.BodyRef.Name)
		}
	}
}

// IsValidationError checks if an error is a ValidationError.
func IsValidationError(err error) bool {
	var ve *ValidationError
	return errors.As(err, &ve)
}

func (v *validator) validateServeAction(name string, action *Action) {
	hasRoot := action.Root != ""
	hasFile := action.File != ""

	if !hasRoot && !hasFile {
		v.addIssue("action %q (serve): root or file is required", name)
	} else if hasRoot && hasFile {
		v.addIssue("action %q (serve): root and file are mutually exclusive", name)
	}
}

func (v *validator) validatePassAction(name string, action *Action) {
	if action.Upstream == "" {
		v.addIssue("action %q (pass): upstream is required", name)
	}
}

// validatePluginNames checks that plugin references are non-empty.
func (v *validator) validatePluginNames(prefix string, plugins []string) {
	for i, p := range plugins {
		if p == "" {
			v.addIssue("%s: plugins[%d] is empty", prefix, i)
		}
	}
}

func (v *validator) validatePluginsRegistry() {
	if len(v.cfg.Plugins) == 0 {
		return
	}

	for name, plugin := range v.cfg.Plugins {
		if plugin.Path == "" {
			v.addIssue("plugin %q: path is required", name)
		}
	}
}

func (v *validator) hasAutostartPlugins() bool {
	for _, p := range v.cfg.Plugins {
		if p.Autostart {
			return true
		}
	}
	return false
}

// routeHasPlugins checks whether the route has plugins from any level
// (route-level, service-level, or action-level).
func (v *validator) routeHasPlugins(route *Route) bool {
	if len(route.Plugins) > 0 {
		return true
	}
	// Check parent service and resolved action.
	for _, svc := range v.cfg.Services {
		if len(svc.Plugins) > 0 {
			for _, r := range svc.Routes {
				if r == route {
					return true
				}
			}
		}
	}
	if route.Action.Name != "" {
		if act, ok := v.cfg.Actions[route.Action.Name]; ok && len(act.Plugins) > 0 {
			return true
		}
	}
	return false
}

func (v *validator) validateBalancer(prefix string, route *Route) {
	bal := route.Balancer

	switch bal.Type {
	case BalancerRoundRobin, BalancerRandom, BalancerLeastConn:
		// Valid.
	case "":
		v.addIssue("%s: balancer.type is required", prefix)
	default:
		v.addIssue("%s: unknown balancer type %q (expected %q, %q, or %q)",
			prefix, bal.Type, BalancerRoundRobin, BalancerRandom, BalancerLeastConn)
	}

	// Empty targets are valid when plugins will populate them
	// (either route-bound, service-level, action-level, or global autostart plugins).
	if len(bal.Targets) == 0 && !v.routeHasPlugins(route) && !v.hasAutostartPlugins() {
		v.addIssue("%s: balancer.targets must have at least one entry (or use plugins)", prefix)
	}

	// Check for duplicates.
	seen := make(map[string]bool, len(bal.Targets))
	for _, t := range bal.Targets {
		if t == "" {
			v.addIssue("%s: balancer.targets contains an empty entry", prefix)
			continue
		}
		if seen[t] {
			v.addIssue("%s: balancer.targets contains duplicate %q", prefix, t)
		}
		seen[t] = true
	}

	// Validate that the action's upstream references {target}.
	if route.Action.Name != "" {
		if act, ok := v.cfg.Actions[route.Action.Name]; ok {
			if act.Type != ActionTypeProxy && act.Type != ActionTypePass {
				v.addIssue("%s: balancer is only supported with proxy or pass actions", prefix)
			}
			if !strings.Contains(act.Upstream, "{target}") {
				v.addIssue("%s: action upstream must contain {target} placeholder when using a balancer", prefix)
			}
		}
	}
}

func (v *validator) validateSpeed(prefix string, speed *SpeedConfig) {
	if speed.DownloadMbps < 0 {
		v.addIssue("%s: speed.download_mbps must be >= 0", prefix)
	}
	if speed.UploadMbps < 0 {
		v.addIssue("%s: speed.upload_mbps must be >= 0", prefix)
	}
	if speed.DownloadMbps == 0 && speed.UploadMbps == 0 {
		v.addIssue("%s: speed requires at least one of download_mbps or upload_mbps > 0", prefix)
	}
}

func (v *validator) validateACME(name string, svc *Service) {
	acme := svc.ACME
	if acme == nil {
		return
	}

	if acme.Email == "" {
		v.addIssue("service %q: acme.email is required", name)
	}

	switch acme.Challenge {
	case "", "alpn", "http", "dns":
		// Valid.
	default:
		v.addIssue("service %q: acme.challenge %q is invalid (expected \"alpn\", \"http\", or \"dns\")", name, acme.Challenge)
	}

	if acme.Challenge == "dns" {
		if acme.DNS == nil {
			v.addIssue("service %q: acme.dns is required when challenge is \"dns\"", name)
		} else {
			if acme.DNS.Provider == "" {
				v.addIssue("service %q: acme.dns.provider is required", name)
			} else {
				validProviders := map[string]bool{"cloudflare": true}
				if !validProviders[acme.DNS.Provider] {
					v.addIssue("service %q: acme.dns.provider %q is not supported (available: \"cloudflare\")", name, acme.DNS.Provider)
				}
			}
		}
	}

	if acme.CA != "" && len(acme.CAs) > 0 {
		v.addIssue("service %q: acme.ca and acme.cas are mutually exclusive", name)
	}

	// Wildcard domains require DNS challenge.
	for _, d := range acme.Domains {
		if strings.Contains(d, "*") && acme.Challenge != "dns" {
			v.addIssue("service %q: wildcard domain %q requires challenge \"dns\"", name, d)
		}
	}
}

var validLogLevels = map[string]bool{
	"debug": true,
	"info":  true,
	"warn":  true,
	"error": true,
}

func (v *validator) validateLogging() {
	if v.cfg.Logging == nil {
		return
	}

	if v.cfg.Logging.Level != "" && !validLogLevels[strings.ToLower(v.cfg.Logging.Level)] {
		v.addIssue("logging.level: unknown level %q (expected debug, info, warn, or error)", v.cfg.Logging.Level)
	}
}

func (v *validator) validateAdmin() {
	admin := v.cfg.Admin
	if admin == nil {
		return
	}

	if admin.Listen == "" {
		v.addIssue("admin: listen address is required")
		return
	}

	// Unix socket path validation.
	if strings.HasPrefix(admin.Listen, "unix://") {
		path := strings.TrimPrefix(admin.Listen, "unix://")
		if path == "" {
			v.addIssue("admin: unix socket path is empty")
		}
		return
	}

	// Warn if TCP listen address is not localhost.
	host := admin.Listen
	if h, _, err := net.SplitHostPort(admin.Listen); err == nil {
		host = h
	}
	if host != "127.0.0.1" && host != "localhost" && host != "::1" && host != "" {
		slog.Warn("admin API is listening on a non-localhost address",
			"listen", admin.Listen,
		)
	}
}
