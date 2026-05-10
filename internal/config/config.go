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
	Actions   map[string]*Action   `json:"actions"`
	Resources map[string]*Resource `json:"resources"`
}

// Service defines a single listener with its routing rules.
type Service struct {
	Listen  string        `json:"listen"`
	TLS     bool          `json:"tls"`
	TLSCert string        `json:"tls_cert,omitempty"`
	TLSKey  string        `json:"tls_key,omitempty"`
	Config  *ServerConfig `json:"config,omitempty"`
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
}

// Route binds a request matcher to an action — either by name or inline.
// An optional Balancer distributes requests across multiple targets.
// Plugins can dynamically manage balancer targets at runtime.
// Set defines route-level variables available as {key} in upstream templates.
type Route struct {
	Match    *Match            `json:"match"`
	Plugins  []string          `json:"plugins,omitempty"`
	Balancer *BalancerConfig   `json:"balancer,omitempty"`
	Set      map[string]string `json:"set,omitempty"`
	Action   ActionRef         `json:"action"`
}

// Match defines the criteria for a route to activate.
// Path supports exact matches ("/styles.css") and wildcard prefixes ("/api/*").
// Domain supports exact matches ("example.com") and wildcard prefixes ("*.example.com").
// Methods is optional — an empty list matches all HTTP methods.
// At least one of Path or Domain must be specified.
// A nil Match acts as a catch-all route that matches everything.
type Match struct {
	Path    string   `json:"path,omitempty"`
	Domain  string   `json:"domain,omitempty"`
	Methods []string `json:"methods,omitempty"`
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
type Action struct {
	Type ActionType `json:"type"`

	// Proxy-specific fields.
	Upstream string   `json:"upstream,omitempty"`
	Timeout  Duration `json:"timeout,omitempty"`
	Stream   bool     `json:"stream,omitempty"` // Use raw HTTP tunnel for bidirectional streaming.

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
