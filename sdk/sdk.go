package sdk

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"
)

// RequestHandler processes an incoming HTTP request and returns a verdict.
type RequestHandler func(req *Request) *Response

// ResponseHandler processes an upstream response and returns modifications.
type ResponseHandler func(req *Request, resp *UpstreamResponse) *ResponseMod

// ConnectHandler processes an L4 connection and returns a verdict.
type ConnectHandler func(conn *ConnRequest) *ConnResponse

// ConfigureHandler is called when the plugin receives route configuration.
type ConfigureHandler func(route Route)

// Plugin is the main entry point for writing prox plugins.
type Plugin struct {
	mu        sync.Mutex
	stdout    *json.Encoder
	onCfg     ConfigureHandler
	onReq     RequestHandler
	onResp    ResponseHandler
	onConn    ConnectHandler
}

// New creates a new plugin instance.
func New() *Plugin {
	return &Plugin{
		stdout: json.NewEncoder(os.Stdout),
	}
}

// OnConfigure registers a handler for route configuration events.
func (p *Plugin) OnConfigure(h ConfigureHandler) {
	p.onCfg = h
}

// OnRequest registers a handler for L7 HTTP request authorization.
// When registered, the plugin advertises the "on_request" capability.
func (p *Plugin) OnRequest(h RequestHandler) {
	p.onReq = h
}

// OnResponse registers a handler for upstream response modification.
// When registered, the plugin advertises the "on_response" capability.
func (p *Plugin) OnResponse(h ResponseHandler) {
	p.onResp = h
}

// OnConnect registers a handler for L4 connection authorization.
// When registered, the plugin advertises the "on_connect" capability.
func (p *Plugin) OnConnect(h ConnectHandler) {
	p.onConn = h
}

// SetTargets pushes a flat target list for the given route.
// Use "*" as routeID to target all routes with balancers.
func (p *Plugin) SetTargets(routeID string, targets []string) {
	p.send(pushMsg{
		Method: "set_targets",
		Params: pushParams{
			RouteID: routeID,
			Targets: targets,
		},
	})
}

// SetGroupedTargets pushes grouped targets for the given route.
// Use "*" as routeID to target all routes with balancers.
func (p *Plugin) SetGroupedTargets(routeID string, groups map[string][]string) {
	p.send(pushMsg{
		Method: "set_targets",
		Params: pushParams{
			RouteID: routeID,
			Groups:  groups,
		},
	})
}

// SetActionTargets pushes a flat target list for all routes using the given action.
func (p *Plugin) SetActionTargets(action string, targets []string) {
	p.send(pushMsg{
		Method: "set_targets",
		Params: pushParams{
			Action:  action,
			Targets: targets,
		},
	})
}

// SetActionGroupedTargets pushes grouped targets for all routes using the given action.
func (p *Plugin) SetActionGroupedTargets(action string, groups map[string][]string) {
	p.send(pushMsg{
		Method: "set_targets",
		Params: pushParams{
			Action: action,
			Groups: groups,
		},
	})
}

// Run starts the plugin event loop. It blocks until stdin is closed.
// Call this after registering all handlers.
func (p *Plugin) Run() {
	log.SetOutput(os.Stderr)

	// Determine capabilities based on registered handlers.
	hooks := p.hooks()

	var sockPath string
	if len(hooks) > 0 {
		// Start the Unix socket listener for request-response hooks.
		path, err := startSocketListener(p)
		if err != nil {
			log.Fatalf("failed to start socket listener: %v", err)
		}
		sockPath = path
	}

	// Process stdin messages (configure, etc.).
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req stdinRequest
		if err := json.Unmarshal(line, &req); err != nil {
			log.Printf("invalid message: %v", err)
			continue
		}

		switch req.Method {
		case "configure":
			p.handleConfigure(req.Params, sockPath, hooks)
		default:
			log.Printf("unknown method: %s", req.Method)
		}
	}
}

func (p *Plugin) handleConfigure(raw json.RawMessage, sockPath string, hooks []string) {
	var params configureParams
	if err := json.Unmarshal(raw, &params); err != nil {
		log.Printf("bad configure params: %v", err)
		return
	}

	// Acknowledge.
	p.send(map[string]string{"result": "ok"})

	// Send ready with socket info if hooks are registered.
	if sockPath != "" {
		p.send(readyMsg{
			Method: "ready",
			Params: readyParams{
				Socket: sockPath,
				Hooks:  hooks,
			},
		})
	}

	if p.onCfg != nil {
		route := Route{
			ID:     params.RouteID,
			Domain: params.Match.Domain,
			Path:   params.Match.Path,
		}
		p.onCfg(route)
	}
}

func (p *Plugin) hooks() []string {
	var h []string
	if p.onReq != nil {
		h = append(h, "on_request")
	}
	if p.onResp != nil {
		h = append(h, "on_response")
	}
	if p.onConn != nil {
		h = append(h, "on_connect")
	}
	return h
}

func (p *Plugin) send(msg interface{}) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.stdout.Encode(msg); err != nil {
		log.Printf("failed to send message: %v", err)
	}
}

// --- stdin protocol types ---

type stdinRequest struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type configureParams struct {
	RouteID string `json:"route_id"`
	Match   struct {
		Domain string `json:"domain"`
		Path   string `json:"path"`
	} `json:"match"`
}

type readyMsg struct {
	Method string      `json:"method"`
	Params readyParams `json:"params"`
}

type readyParams struct {
	Socket string   `json:"socket"`
	Hooks  []string `json:"hooks"`
}

type pushMsg struct {
	Method string     `json:"method"`
	Params pushParams `json:"params"`
}

type pushParams struct {
	RouteID string              `json:"route_id,omitempty"`
	Action  string              `json:"action,omitempty"`
	Targets []string            `json:"targets,omitempty"`
	Groups  map[string][]string `json:"groups,omitempty"`
}

func init() {
	// Prefix plugin logs for easier identification.
	log.SetPrefix(fmt.Sprintf("[plugin:%d] ", os.Getpid()))
}
