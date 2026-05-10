package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/titanous/json5"
)

// LoadResult holds a validated Config along with the list of all files
// that were read during loading (the root file + any nested refs).
// Callers can pass Paths to the file watcher for change detection.
type LoadResult struct {
	Config *Config
	Paths  []string
}

// LoadFile reads, parses, and validates a JSON5 configuration.
// path may be a single file or a directory of .json5 files.
func LoadFile(path string) (*LoadResult, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("reading config path: %w", err)
	}

	ctx := &loadContext{
		loaded: make(map[string]bool),
	}

	var cfg *Config

	if info.IsDir() {
		cfg, err = ctx.loadDirectory(path)
	} else {
		cfg, err = ctx.loadRootFile(path)
	}
	if err != nil {
		return nil, err
	}

	normalize(cfg)

	if err := Validate(cfg); err != nil {
		return nil, err
	}

	return &LoadResult{
		Config: cfg,
		Paths:  ctx.paths(),
	}, nil
}

// Load parses raw JSON5 bytes into a validated Config.
// Does not support nested file references (no base directory to resolve from).
func Load(data []byte) (*Config, error) {
	var cfg Config
	if err := json5.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	normalize(&cfg)

	if err := Validate(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// --- internal loader context ---

// loadContext tracks all loaded files to detect circular references
// and collect paths for the file watcher.
type loadContext struct {
	loaded map[string]bool // absolute paths already processed
}

// paths returns a sorted list of all files that were loaded.
func (lc *loadContext) paths() []string {
	out := make([]string, 0, len(lc.loaded))
	for p := range lc.loaded {
		out = append(out, p)
	}
	return out
}

// track marks a path as loaded and returns an error on circular reference.
func (lc *loadContext) track(absPath string) error {
	if lc.loaded[absPath] {
		return fmt.Errorf("circular config reference: %s", absPath)
	}
	lc.loaded[absPath] = true
	return nil
}

// --- root file loading ---

// rawConfig mirrors Config but allows services to be either inline objects
// or string file/directory references.
type rawConfig struct {
	Services  map[string]rawServiceEntry `json:"services"`
	Actions   map[string]*Action         `json:"actions"`
	Resources map[string]*Resource       `json:"resources"`
}

// rawServiceEntry holds either an inline Service or a string path.
type rawServiceEntry struct {
	Path   string      // file or directory path (when value is a JSON string)
	Inline *rawService // inline service definition (when value is a JSON object)
}

func (e *rawServiceEntry) UnmarshalJSON(data []byte) error {
	// String → file/directory path.
	if len(data) > 0 && data[0] == '"' {
		return json5.Unmarshal(data, &e.Path)
	}

	// Object → inline service.
	e.Inline = &rawService{}
	if err := json5.Unmarshal(data, e.Inline); err != nil {
		return fmt.Errorf("service must be a string path or an inline service object: %w", err)
	}
	return nil
}

// rawService mirrors Service but uses rawRouteEntry to support route includes.
type rawService struct {
	Listen  string          `json:"listen"`
	TLS     bool            `json:"tls"`
	TLSCert string          `json:"tls_cert,omitempty"`
	TLSKey  string          `json:"tls_key,omitempty"`
	Config  *ServerConfig   `json:"config,omitempty"`
	Routes  []rawRouteEntry `json:"routes"`
}

// rawRouteEntry holds either an inline Route object or a string path
// to a file containing an array of routes (route include).
type rawRouteEntry struct {
	Path   string // file path for route include
	Inline *Route // inline route definition
}

func (e *rawRouteEntry) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		return json5.Unmarshal(data, &e.Path)
	}

	e.Inline = &Route{}
	if err := json5.Unmarshal(data, e.Inline); err != nil {
		return fmt.Errorf("route must be a route object or a string path to include: %w", err)
	}
	return nil
}

// loadRootFile loads a single config file, resolving any nested service references.
func (lc *loadContext) loadRootFile(path string) (*Config, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolving config path: %w", err)
	}

	if err := lc.track(absPath); err != nil {
		return nil, err
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	var raw rawConfig
	if err := json5.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	baseDir := filepath.Dir(absPath)

	cfg := &Config{
		Actions:   raw.Actions,
		Resources: raw.Resources,
		Services:  make(map[string]*Service),
	}
	if cfg.Actions == nil {
		cfg.Actions = make(map[string]*Action)
	}
	if cfg.Resources == nil {
		cfg.Resources = make(map[string]*Resource)
	}

	// Resolve each service entry.
	for name, entry := range raw.Services {
		if entry.Inline != nil {
			routes, err := lc.resolveRoutes(entry.Inline.Routes, baseDir)
			if err != nil {
				return nil, fmt.Errorf("service %q: %w", name, err)
			}
			cfg.Services[name] = &Service{
				Listen:  entry.Inline.Listen,
				TLS:     entry.Inline.TLS,
				TLSCert: entry.Inline.TLSCert,
				TLSKey:  entry.Inline.TLSKey,
				Config:  entry.Inline.Config,
				Routes:  routes,
			}
			continue
		}

		// String reference — resolve path.
		refPath := entry.Path
		if !filepath.IsAbs(refPath) {
			refPath = filepath.Join(baseDir, refPath)
		}

		info, err := os.Stat(refPath)
		if err != nil {
			return nil, fmt.Errorf("service %q: cannot access %q: %w", name, entry.Path, err)
		}

		if info.IsDir() {
			// Directory reference — load all .json5 files as services.
			dirServices, err := lc.loadFragmentsFromDir(refPath, cfg)
			if err != nil {
				return nil, fmt.Errorf("service %q (dir %q): %w", name, entry.Path, err)
			}
			// Merge directory services — the key in the map is ignored (replaced by filenames).
			for svcName, svc := range dirServices {
				if _, exists := cfg.Services[svcName]; exists {
					return nil, fmt.Errorf("service %q: duplicate service name from directory", svcName)
				}
				cfg.Services[svcName] = svc
			}
		} else {
			// File reference — load as ServiceFragment.
			svc, err := lc.loadFragment(refPath, cfg)
			if err != nil {
				return nil, fmt.Errorf("service %q (%s): %w", name, entry.Path, err)
			}
			cfg.Services[name] = svc
		}
	}

	return cfg, nil
}

// --- directory loading ---

// loadDirectory loads all .json5 files from a directory as a merged config.
// Each file is a ServiceFragment; service name = filename without extension.
func (lc *loadContext) loadDirectory(dir string) (*Config, error) {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolving config directory: %w", err)
	}

	entries, err := os.ReadDir(absDir)
	if err != nil {
		return nil, fmt.Errorf("reading config directory: %w", err)
	}

	cfg := &Config{
		Services:  make(map[string]*Service),
		Actions:   make(map[string]*Action),
		Resources: make(map[string]*Resource),
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json5") {
			continue
		}

		filePath := filepath.Join(absDir, entry.Name())
		svcName := strings.TrimSuffix(entry.Name(), ".json5")

		svc, err := lc.loadFragment(filePath, cfg)
		if err != nil {
			return nil, fmt.Errorf("loading %s: %w", entry.Name(), err)
		}

		if _, exists := cfg.Services[svcName]; exists {
			return nil, fmt.Errorf("duplicate service %q in directory", svcName)
		}
		cfg.Services[svcName] = svc
	}

	return cfg, nil
}

// --- fragment loading ---

// rawFragment is the schema for nested config files.
// It combines a Service definition with optional local Actions and Resources.
// Uses rawRouteEntry to support route includes.
type rawFragment struct {
	Listen    string               `json:"listen"`
	TLS       bool                 `json:"tls"`
	TLSCert   string               `json:"tls_cert,omitempty"`
	TLSKey    string               `json:"tls_key,omitempty"`
	Config    *ServerConfig        `json:"config,omitempty"`
	Routes    []rawRouteEntry      `json:"routes"`
	Actions   map[string]*Action   `json:"actions"`
	Resources map[string]*Resource `json:"resources"`
}

// loadFragment loads a single .json5 file as a service fragment, merges its
// actions/resources into the parent config, and returns the Service.
func (lc *loadContext) loadFragment(path string, parent *Config) (*Service, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolving path: %w", err)
	}

	if err := lc.track(absPath); err != nil {
		return nil, err
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, fmt.Errorf("reading file: %w", err)
	}

	var frag rawFragment
	if err := json5.Unmarshal(data, &frag); err != nil {
		return nil, fmt.Errorf("parsing file: %w", err)
	}

	// Merge fragment actions into the parent config.
	for name, act := range frag.Actions {
		if _, exists := parent.Actions[name]; exists {
			return nil, fmt.Errorf("duplicate action %q (already defined in another config)", name)
		}
		parent.Actions[name] = act
	}

	// Merge fragment resources into the parent config.
	for name, res := range frag.Resources {
		if _, exists := parent.Resources[name]; exists {
			return nil, fmt.Errorf("duplicate resource %q (already defined in another config)", name)
		}
		parent.Resources[name] = res
	}

	fragDir := filepath.Dir(absPath)
	routes, err := lc.resolveRoutes(frag.Routes, fragDir)
	if err != nil {
		return nil, err
	}

	return &Service{
		Listen:  frag.Listen,
		TLS:     frag.TLS,
		TLSCert: frag.TLSCert,
		TLSKey:  frag.TLSKey,
		Config:  frag.Config,
		Routes:  routes,
	}, nil
}

// loadFragmentsFromDir loads all .json5 files from a directory as ServiceFragments.
func (lc *loadContext) loadFragmentsFromDir(dir string, parent *Config) (map[string]*Service, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading directory: %w", err)
	}

	services := make(map[string]*Service)

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json5") {
			continue
		}

		filePath := filepath.Join(dir, entry.Name())
		svcName := strings.TrimSuffix(entry.Name(), ".json5")

		svc, err := lc.loadFragment(filePath, parent)
		if err != nil {
			return nil, fmt.Errorf("loading %s: %w", entry.Name(), err)
		}

		services[svcName] = svc
	}

	return services, nil
}

// --- route include resolution ---

// resolveRoutes expands a list of raw route entries. String entries are treated
// as file paths to JSON5 files containing route arrays — they are loaded and
// spliced in place, preserving order. Object entries pass through as-is.
func (lc *loadContext) resolveRoutes(raw []rawRouteEntry, baseDir string) ([]*Route, error) {
	var routes []*Route

	for _, entry := range raw {
		if entry.Inline != nil {
			routes = append(routes, entry.Inline)
			continue
		}

		// String → route include file.
		included, err := lc.loadRouteInclude(entry.Path, baseDir)
		if err != nil {
			return nil, fmt.Errorf("route include %q: %w", entry.Path, err)
		}
		routes = append(routes, included...)
	}

	return routes, nil
}

// loadRouteInclude reads a JSON5 file containing an array of routes.
func (lc *loadContext) loadRouteInclude(path, baseDir string) ([]*Route, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(baseDir, path)
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolving path: %w", err)
	}

	if err := lc.track(absPath); err != nil {
		return nil, err
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, fmt.Errorf("reading file: %w", err)
	}

	// Route include files can be:
	// 1. A bare array: [{ match: ..., action: ... }, ...]
	// 2. An object with a "routes" key: { routes: [...] }
	// Try array first, fall back to object wrapper.
	var routes []*Route
	if len(data) > 0 {
		trimmed := strings.TrimSpace(string(data))
		if len(trimmed) > 0 && trimmed[0] == '[' {
			if err := json5.Unmarshal(data, &routes); err != nil {
				return nil, fmt.Errorf("parsing route array: %w", err)
			}
			return routes, nil
		}
	}

	// Object wrapper: { routes: [...] }
	var wrapper struct {
		Routes []*Route `json:"routes"`
	}
	if err := json5.Unmarshal(data, &wrapper); err != nil {
		return nil, fmt.Errorf("parsing route file (expected array or object with \"routes\" key): %w", err)
	}

	return wrapper.Routes, nil
}

// normalize hoists inline action/resource definitions into the top-level maps.
func normalize(cfg *Config) {
	if cfg.Actions == nil {
		cfg.Actions = make(map[string]*Action)
	}
	if cfg.Resources == nil {
		cfg.Resources = make(map[string]*Resource)
	}

	// Normalize inline actions in routes.
	for svcName, svc := range cfg.Services {
		for i, route := range svc.Routes {
			if route.Action.IsInline() {
				name := fmt.Sprintf("_inline_%s_%d", svcName, i)
				cfg.Actions[name] = route.Action.Inline
				svc.Routes[i].Action = ActionRef{Name: name}
			}
		}
	}

	// Normalize inline resources in actions.
	for actName, act := range cfg.Actions {
		if act.BodyRef.IsInline() {
			name := fmt.Sprintf("_inline_%s_body", actName)
			cfg.Resources[name] = act.BodyRef.Inline
			act.BodyRef = ResourceRef{Name: name}
		}
	}
}
