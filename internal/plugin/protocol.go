// Package plugin implements external plugin process management.
//
// Plugins are external executables that communicate with prox over
// stdin/stdout using line-delimited JSON messages for lifecycle events,
// and over a Unix socket using length-prefixed msgpack frames for
// request-response hooks (on_request, on_response, on_connect).
package plugin

import (
	"bytes"
	"sync"

	"github.com/vmihailenco/msgpack/v5"
)

// Method constants for the stdin/stdout JSON protocol.
const (
	MethodConfigure  = "configure"
	MethodSetTargets = "set_targets"
	MethodReady      = "ready"
)

// Hook names advertised by plugins.
const (
	HookOnRequest  = "on_request"
	HookOnResponse = "on_response"
	HookOnConnect  = "on_connect"
)

// HookType identifies the hook being invoked over the socket.
type HookType byte

const (
	HookTypeRequest  HookType = 1
	HookTypeResponse HookType = 2
	HookTypeConnect  HookType = 3
)

// Request is a message sent from prox to a plugin via stdin.
type Request struct {
	Method string      `json:"method"`
	Params interface{} `json:"params,omitempty"`
}

// ConfigureParams is sent to the plugin on startup and after reload.
type ConfigureParams struct {
	RouteID string     `json:"route_id"`
	Match   *MatchInfo `json:"match,omitempty"`
}

// MatchInfo provides the route's match criteria to the plugin.
type MatchInfo struct {
	Domain string `json:"domain,omitempty"`
	Path   string `json:"path,omitempty"`
}

// Response is a simple acknowledgement from a plugin.
type Response struct {
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Push is a message sent from a plugin to prox via stdout.
type Push struct {
	Method string     `json:"method"`
	Params PushParams `json:"params"`
}

// PushParams carries the data for a push message.
type PushParams struct {
	RouteID string              `json:"route_id,omitempty"`
	Action  string              `json:"action,omitempty"` // target routes by action name
	Targets []string            `json:"targets,omitempty"`
	Groups  map[string][]string `json:"groups,omitempty"`

	// Ready-specific fields (only when Method == "ready").
	Socket string   `json:"socket,omitempty"`
	Hooks  []string `json:"hooks,omitempty"`
}

// --- Socket protocol types (msgpack) ---

// Envelope wraps all socket messages with a type discriminator.
type Envelope struct {
	Hook HookType `msgpack:"t"`
	Data []byte   `msgpack:"d"`
}

// RequestInfo carries the HTTP request context for on_request hooks.
type RequestInfo struct {
	RouteID       string            `msgpack:"r"`
	Method        string            `msgpack:"m"`
	Path          string            `msgpack:"p"`
	Query         string            `msgpack:"q,omitempty"`
	Domain        string            `msgpack:"d"`
	Host          string            `msgpack:"ho,omitempty"`
	Proto         string            `msgpack:"pr,omitempty"`
	RemoteAddr    string            `msgpack:"a"`
	ContentLength int64             `msgpack:"cl,omitempty"`
	Headers       map[string]string `msgpack:"h"`
	Body          []byte            `msgpack:"bd,omitempty"`
	MatchDomain   string            `msgpack:"md,omitempty"`
	MatchGlob     string            `msgpack:"mg,omitempty"`
	MatchPath     string            `msgpack:"mp,omitempty"`
	Vars          map[string]string `msgpack:"v,omitempty"`
}

// AuthorizeResult is the plugin's verdict for an on_request hook.
type AuthorizeResult struct {
	Allow   bool              `msgpack:"ok"`
	Drop    bool              `msgpack:"dr,omitempty"`
	Status  int               `msgpack:"s,omitempty"`
	Body    string            `msgpack:"b,omitempty"`
	Headers map[string]string `msgpack:"h,omitempty"`
}

// UpstreamResponseInfo carries upstream response context for on_response hooks.
type UpstreamResponseInfo struct {
	Status  int               `msgpack:"s"`
	Headers map[string]string `msgpack:"h"`
}

// ResponsePair bundles request + upstream response for the on_response hook.
type ResponsePair struct {
	Req  RequestInfo          `msgpack:"req"`
	Resp UpstreamResponseInfo `msgpack:"resp"`
}

// ResponseModResult describes modifications to apply to the upstream response.
type ResponseModResult struct {
	Status  int               `msgpack:"s,omitempty"`
	Headers map[string]string `msgpack:"h,omitempty"`
	Remove  []string          `msgpack:"rm,omitempty"`
}

// ConnInfo carries L4 connection context for on_connect hooks.
type ConnInfo struct {
	RouteID     string `msgpack:"r"`
	Domain      string `msgpack:"d"`
	RemoteAddr  string `msgpack:"a"`
	MatchDomain string `msgpack:"md,omitempty"`
	MatchGlob   string `msgpack:"mg,omitempty"`
}

// ConnResult is the plugin's verdict for an on_connect hook.
type ConnResult struct {
	Allow bool `msgpack:"ok"`
}

// envelopeBufPool reuses buffers for MarshalEnvelope to reduce allocations.
var envelopeBufPool = sync.Pool{
	New: func() any { return new(bytes.Buffer) },
}

// MarshalEnvelope creates a framed envelope for the given hook type and data.
// Uses a pooled buffer to avoid intermediate allocations.
func MarshalEnvelope(hook HookType, data interface{}) ([]byte, error) {
	buf := envelopeBufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer envelopeBufPool.Put(buf)

	// Encode the inner data into the pooled buffer.
	enc := msgpack.NewEncoder(buf)
	if err := enc.Encode(data); err != nil {
		return nil, err
	}

	// Encode the envelope with the data bytes.
	return msgpack.Marshal(&Envelope{Hook: hook, Data: buf.Bytes()})
}

