// Package config defines the configuration model for prox.
//
// The config has three sections: services (listeners + routes),
// actions (handlers), and resources (reusable content).
// Cross-references use string keys, resolved at load time.
package config

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/titanous/json5"
)

// Config is the root configuration object.
type Config struct {
	Services  map[string]*Service  `json:"services"`
	Plugins   map[string]*Plugin   `json:"plugins"`
	Actions   map[string]*Action   `json:"actions"`
	Resources map[string]*Resource `json:"resources"`
	Logging   *LoggingConfig       `json:"logging,omitempty"`
	Admin     *AdminConfig         `json:"admin,omitempty"`
}

// AdminConfig controls the optional management API server.
// When configured, prox starts an HTTP API on the specified address
// for health checks, config reload, and runtime inspection.
type AdminConfig struct {
	Listen string `json:"listen"`          // TCP ("127.0.0.1:9090") or Unix socket ("unix:///var/run/prox.sock")
	Token  string `json:"token,omitempty"` // Bearer token for authentication (optional)
}

// LoggingConfig controls log output destinations and verbosity.
type LoggingConfig struct {
	Level     string `json:"level,omitempty"`      // debug, info, warn, error (overridden by LOG_LEVEL env)
	AccessLog string `json:"access_log,omitempty"` // global access log file path
	ErrorLog  string `json:"error_log,omitempty"`  // global error log file path
}

// Plugin defines global configuration for an external plugin.
type Plugin struct {
	Path      string `json:"path"`
	Autostart bool   `json:"autostart,omitempty"` // start at proxy startup without route bindings
}

// Service defines a single listener with its routing rules.
// Plugins declared at the service level apply to all routes in the service.
type Service struct {
	Listen  string        `json:"listen"`
	TLS     bool          `json:"tls"`
	TLSCert string        `json:"tls_cert,omitempty"`
	TLSKey  string        `json:"tls_key,omitempty"`
	ACME    *ACMEConfig   `json:"acme,omitempty"`
	H2      *bool         `json:"h2,omitempty"` // Enable HTTP/2 on TLS listener (default: true). Set to false for WebSocket support.
	Config  *ServerConfig `json:"config,omitempty"`
	Plugins []string      `json:"plugins,omitempty"`
	Routes  []*Route      `json:"routes"`
}

// ServerConfig tunes HTTP server and proxy transport behavior per service.
// Zero values fall back to built-in defaults.
type ServerConfig struct {
	// HTTP server timeouts.
	ReadTimeout  Duration `json:"read_timeout,omitempty"`
	WriteTimeout Duration `json:"write_timeout,omitempty"`
	IdleTimeout  Duration `json:"idle_timeout,omitempty"`

	// Proxy transport settings.
	ResponseHeaderTimeout Duration `json:"response_header_timeout,omitempty"`

	// FlushInterval controls how often buffered response data is flushed
	// to the client. Negative value (-1) flushes immediately (streaming).
	// Zero uses the default buffered behavior.
	FlushInterval Duration `json:"flush_interval,omitempty"`

	// Transport tuning — connection pool and keep-alive.
	DialTimeout         Duration `json:"dial_timeout,omitempty"`            // TCP dial timeout (default: action timeout)
	KeepAlive           Duration `json:"keep_alive,omitempty"`              // TCP keep-alive interval (default: 30s)
	MaxIdleConns        int      `json:"max_idle_conns,omitempty"`          // Max idle connections (default: 4096)
	MaxIdleConnsPerHost int      `json:"max_idle_conns_per_host,omitempty"` // Max idle per host (default: 4096)
	TLSHandshakeTimeout Duration `json:"tls_handshake_timeout,omitempty"`   // TLS handshake deadline (default: 10s)

	// Transport I/O buffer sizes in bytes (default: 32768).
	ReadBufferSize  int `json:"read_buffer_size,omitempty"`
	WriteBufferSize int `json:"write_buffer_size,omitempty"`

	// DisableCompression prevents the proxy from requesting gzip from upstreams.
	// When true (default), the client's Accept-Encoding is forwarded as-is and
	// compressed responses pass through without re-encoding — more efficient
	// for reverse proxy workloads. Set to false to let Go decompress upstream
	// responses transparently.
	DisableCompression *bool `json:"disable_compression,omitempty"`

	// HTTP/2 transport tuning.
	H2ReadIdleTimeout Duration `json:"h2_read_idle_timeout,omitempty"` // Ping after idle (default: 30s)
	H2PingTimeout     Duration `json:"h2_ping_timeout,omitempty"`     // Ping response deadline (default: 15s)

	// Connection limiting.
	MaxConnections int `json:"max_connections,omitempty"` // Maximum concurrent connections (0 = unlimited)
}

// SpeedConfig controls per-route bandwidth throttling.
// When Shared is true, all connections on the route share the bandwidth budget.
// When Shared is false (default), each connection gets its own independent limit.
type SpeedConfig struct {
	DownloadMbps float64 `json:"download_mbps,omitempty"` // upstream→client limit in Mbps (0 = unlimited)
	UploadMbps   float64 `json:"upload_mbps,omitempty"`   // client→upstream limit in Mbps (0 = unlimited)
	Shared       bool    `json:"shared,omitempty"`         // share bandwidth across all connections
}

// ACMEConfig controls automatic certificate issuance via ACME (e.g., Let's Encrypt).
type ACMEConfig struct {
	// Email for the ACME account (used for certificate expiration notices).
	Email string `json:"email"`

	// CA directory URL. Defaults to Let's Encrypt production.
	// Shorthand values: "staging" → Let's Encrypt staging,
	//                   "zerossl" → ZeroSSL production.
	CA string `json:"ca,omitempty"`

	// CAs defines fallback certificate authorities, tried in order.
	// When set, CA is ignored and CAs takes precedence.
	CAs []string `json:"cas,omitempty"`

	// Challenge type: "alpn" (TLS-ALPN-01, default), "http" (HTTP-01),
	// or "dns" (DNS-01, required for wildcards).
	Challenge string `json:"challenge,omitempty"`

	// DNS configures the DNS-01 challenge provider.
	// Required when challenge is "dns".
	DNS *ACMEDNSConfig `json:"dns,omitempty"`

	// StorageType selects the storage backend for certificates and ACME
	// account data: "file" (default) or "s3".
	StorageType string `json:"storage_type,omitempty"`

	// Storage path for certificates and ACME account data (file backend).
	// Default: "acme/" directory next to the config file.
	Storage string `json:"storage,omitempty"`

	// S3 configures S3-compatible object storage for ACME data.
	// Required when storage_type is "s3".
	S3 *ACMES3Config `json:"s3,omitempty"`

	// Domains to manage. If empty, domains are auto-discovered
	// from route match.domain patterns in this service.
	Domains []string `json:"domains,omitempty"`
}

// ACMEDNSConfig configures the DNS provider for DNS-01 challenges.
type ACMEDNSConfig struct {
	// Provider name: "cloudflare".
	Provider string `json:"provider"`

	// Token is the API token for the DNS provider.
	// If empty, read from the provider's environment variable:
	//   cloudflare → CF_DNS_API_TOKEN
	Token string `json:"token,omitempty"`

	// Discover fetches all domains from the provider account and manages
	// certificates for each zone (zone + *.zone). When enabled, acme.domains
	// and route auto-discovery are ignored.
	Discover bool `json:"discover,omitempty"`

	// Resolvers overrides the default DNS resolvers used for ACME zone
	// detection and challenge propagation checks. This is useful in
	// containerized environments (e.g., Docker) where the default
	// resolver cannot properly return SOA records for certain TLDs.
	// Format: ["1.1.1.1:53", "8.8.8.8:53"]
	Resolvers []string `json:"resolvers,omitempty"`
}

// ACMES3Config configures S3-compatible object storage for ACME certificate data.
type ACMES3Config struct {
	// Bucket is the S3 bucket name (required).
	Bucket string `json:"bucket"`

	// Region is the AWS region. Default: "us-east-1".
	Region string `json:"region,omitempty"`

	// Endpoint is a custom S3-compatible endpoint URL (e.g., MinIO, Cloudflare R2).
	// When empty, the default AWS S3 endpoint is used.
	Endpoint string `json:"endpoint,omitempty"`

	// AccessKey and SecretKey are static credentials.
	// When empty, the default AWS credential chain is used
	// (environment variables, shared config, IAM role, IMDS).
	AccessKey string `json:"access_key,omitempty"`
	SecretKey string `json:"secret_key,omitempty"`

	// Prefix is the key prefix within the bucket. Default: "" (bucket root).
	// Set to e.g. "acme/" to store under a subdirectory.
	Prefix string `json:"prefix"`

	// UsePathStyle forces path-style addressing (required for MinIO and
	// some S3-compatible providers that don't support virtual-hosted buckets).
	UsePathStyle bool `json:"use_path_style,omitempty"`
}

// Route binds a request matcher to an action — either by name or inline.
// An optional Balancer distributes requests across multiple targets.
// Plugins can dynamically manage balancer targets at runtime.
// Set defines route-level variables available as {key} in upstream templates.
type Route struct {
	Match         *Match            `json:"match"`
	Plugins       []string          `json:"plugins,omitempty"`
	PluginTimeout Duration          `json:"plugin_timeout,omitempty"` // per-request timeout for plugin hooks (default: 5s)
	Balancer      *BalancerConfig   `json:"balancer,omitempty"`
	Speed         *SpeedConfig      `json:"speed,omitempty"`
	Set           map[string]string `json:"set,omitempty"`
	Action        ActionRef         `json:"action"`
	AccessLog     string            `json:"access_log,omitempty"` // per-route access log file path
}

// Match defines the criteria for a route to activate.
// Path supports exact matches ("/styles.css") and wildcard prefixes ("/api/*").
// Domain supports exact matches ("example.com") and wildcard prefixes ("*.example.com").
// Methods is optional — an empty list matches all HTTP methods.
// ForwardProxy matches only forward proxy requests (absolute URL in request line).
// At least one of Path, Domain, or ForwardProxy must be specified.
// A nil Match acts as a catch-all route that matches everything.
type Match struct {
	Path         string   `json:"path,omitempty"`
	Domain       string   `json:"domain,omitempty"`
	Methods      []string `json:"methods,omitempty"`
	ForwardProxy bool     `json:"forward_proxy,omitempty"`
}

// BalancerType represents the load balancing strategy.
type BalancerType string

const (
	BalancerRoundRobin BalancerType = "roundrobin"
	BalancerRandom     BalancerType = "random"
	BalancerLeastConn  BalancerType = "leastconn"
)

// BalancerConfig defines a load balancer for distributing requests across targets.
// The selected target is available as {target} in the action's upstream field.
type BalancerConfig struct {
	Type    BalancerType `json:"type"`
	Targets []string     `json:"targets"`
}

// ActionType represents the kind of action to execute.
type ActionType string

const (
	ActionTypeProxy  ActionType = "proxy"
	ActionTypeStatic ActionType = "static"
	ActionTypeServe  ActionType = "serve"
	ActionTypePass   ActionType = "pass" // L4 TCP pass-through (no TLS termination)
	ActionTypeDrop   ActionType = "drop" // Silently close the connection
)

// Action defines what happens when a route matches.
// Plugins declared at the action level apply to all routes using this action.
type Action struct {
	Type      ActionType `json:"type"`
	Plugins   []string   `json:"plugins,omitempty"`
	AccessLog string     `json:"access_log,omitempty"` // "off" to disable, or file path for per-action log

	// Proxy-specific fields.
	Upstream string   `json:"upstream,omitempty"`
	Timeout  Duration `json:"timeout,omitempty"`
	Stream   bool     `json:"stream,omitempty"` // Use raw HTTP tunnel for bidirectional streaming.
	Proto    string   `json:"proto,omitempty"`  // Upstream protocol: "" (auto), "h2" (HTTP/2 cleartext).

	// Fallback action name — invoked when the primary action fails
	// (e.g. no target selected, upstream unreachable).
	Fallback string `json:"fallback,omitempty"`

	// Shared fields (proxy, static).
	Headers map[string]string `json:"headers,omitempty"`

	// Static-specific fields.
	Status  int         `json:"status,omitempty"`
	BodyRef ResourceRef `json:"body_ref,omitempty"`

	// Serve-specific fields.
	Root string `json:"root,omitempty"` // Directory to serve (e.g. "./public").
	File string `json:"file,omitempty"` // Single file to serve (e.g. "./index.html").
}

// Resource holds reusable content referenced by actions.
type Resource struct {
	Text string `json:"text,omitempty"`
	JSON any    `json:"json,omitempty"`
	File string `json:"file,omitempty"`
}

// ActionRef holds either a string reference to a named action or an inline action object.
type ActionRef struct {
	Name   string
	Inline *Action
}

// IsInline returns true if this is an inline action definition.
func (r ActionRef) IsInline() bool { return r.Inline != nil }

// IsEmpty returns true if no action is specified.
func (r ActionRef) IsEmpty() bool { return r.Name == "" && r.Inline == nil }

func (r ActionRef) MarshalJSON() ([]byte, error) {
	if r.Inline != nil {
		return json.Marshal(r.Inline)
	}
	return json.Marshal(r.Name)
}

func (r *ActionRef) UnmarshalJSON(data []byte) error {
	// Quick check: strings start with '"'.
	if len(data) > 0 && data[0] == '"' {
		return json.Unmarshal(data, &r.Name)
	}

	// Otherwise, parse as an inline action object.
	r.Inline = &Action{}
	if err := json5.Unmarshal(data, r.Inline); err != nil {
		return fmt.Errorf("action must be a string reference or an inline action object: %w", err)
	}
	return nil
}

// ResourceRef holds either a string reference to a named resource or an inline resource object.
type ResourceRef struct {
	Name   string
	Inline *Resource
}

// IsInline returns true if this is an inline resource definition.
func (r ResourceRef) IsInline() bool { return r.Inline != nil }

// IsEmpty returns true if no resource is referenced.
func (r ResourceRef) IsEmpty() bool { return r.Name == "" && r.Inline == nil }

func (r ResourceRef) MarshalJSON() ([]byte, error) {
	if r.Inline != nil {
		return json.Marshal(r.Inline)
	}
	if r.Name == "" {
		return []byte("null"), nil
	}
	return json.Marshal(r.Name)
}

func (r *ResourceRef) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		return json.Unmarshal(data, &r.Name)
	}

	r.Inline = &Resource{}
	if err := json5.Unmarshal(data, r.Inline); err != nil {
		return fmt.Errorf("body_ref must be a string reference or an inline resource object: %w", err)
	}
	return nil
}

// Duration wraps time.Duration to support JSON string parsing (e.g. "5s").
type Duration struct {
	time.Duration
}

// MarshalJSON encodes Duration as a human-readable string.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.Duration.String())
}

// UnmarshalJSON decodes a duration from either a string ("5s", "100ms", "1m30s")
// or a number (interpreted as seconds). Negative numbers are allowed (e.g. -1 for
// immediate flush).
func (d *Duration) UnmarshalJSON(b []byte) error {
	// Try numeric first (integer or float → seconds).
	var num float64
	if err := json.Unmarshal(b, &num); err == nil {
		d.Duration = time.Duration(num * float64(time.Second))
		return nil
	}

	// Try string format.
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string (e.g. \"5s\") or a number (seconds): %w", err)
	}

	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}

	d.Duration = parsed
	return nil
}
