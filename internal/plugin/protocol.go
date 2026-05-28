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
	MethodSetSpeed   = "set_speed"
	MethodReady      = "ready"
	MethodLog        = "log"
)

// Hook names advertised by plugins.
const (
	HookOnRequest    = "on_request"
	HookOnResponse   = "on_response"
	HookOnConnect    = "on_connect"
	HookOnDisconnect = "on_disconnect"
)

// HookType identifies the hook being invoked over the socket.
type HookType byte

const (
	HookTypeRequest    HookType = 1
	HookTypeResponse   HookType = 2
	HookTypeConnect    HookType = 3
	HookTypeDisconnect HookType = 4
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

	DownloadMbps float64 `json:"download_mbps,omitempty"`
	UploadMbps   float64 `json:"upload_mbps,omitempty"`
	GroupKey     string  `json:"group_key,omitempty"`

	// Log-specific fields (only when Method == "log").
	Level   string `json:"level,omitempty"`
	Message string `json:"message,omitempty"`
	Args    []any  `json:"args,omitempty"`
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
	Target        string            `msgpack:"tg,omitempty"`
}

// AuthorizeResult is the plugin's verdict for an on_request hook.
type AuthorizeResult struct {
	Allow      bool              `msgpack:"ok"`
	Drop       bool              `msgpack:"dr,omitempty"`
	Fallback   bool              `msgpack:"fb,omitempty"`
	Status     int               `msgpack:"s,omitempty"`
	Body       string            `msgpack:"b,omitempty"`
	Headers    map[string]string `msgpack:"h,omitempty"`
	SpeedLimit *SpeedLimit       `msgpack:"sp,omitempty"`
	CleanQuery bool              `msgpack:"cq,omitempty"`
	RewritePath string           `msgpack:"rp,omitempty"`
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

// DisconnectInfo carries statistics for the on_disconnect hook.
type DisconnectInfo struct {
	RouteID    string `msgpack:"r"`
	Target     string `msgpack:"tg,omitempty"`
	RemoteAddr string `msgpack:"a"`
	BytesRx    int64  `msgpack:"rx"`
	BytesTx    int64  `msgpack:"tx"`
	DurationMs int64  `msgpack:"ms"`
}

// SpeedLimit holds bandwidth caps from plugin responses.
// When GroupKey is set, all connections with the same key share a single budget.
type SpeedLimit struct {
	DownloadMbps float64 `msgpack:"dl,omitempty"`
	UploadMbps   float64 `msgpack:"ul,omitempty"`
	GroupKey     string  `msgpack:"gk,omitempty"`
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

