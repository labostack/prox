// Package sdk provides the plugin authoring API for prox.
//
// Plugins are external executables that extend prox with custom logic.
// The SDK handles all transport details — plugin authors just register
// callbacks and call Run().
package sdk

// Route describes a configured route, sent during plugin initialization.
type Route struct {
	ID     string `msgpack:"id" json:"route_id"`
	Domain string `msgpack:"domain,omitempty" json:"domain,omitempty"`
	Path   string `msgpack:"path,omitempty" json:"path,omitempty"`
}

// Request carries the HTTP request context for on_request hooks.
type Request struct {
	RouteID       string            `msgpack:"r" json:"route_id"`
	Method        string            `msgpack:"m" json:"method"`
	Path          string            `msgpack:"p" json:"path"`
	Query         string            `msgpack:"q,omitempty" json:"query,omitempty"`
	Domain        string            `msgpack:"d" json:"domain"`
	Host          string            `msgpack:"ho,omitempty" json:"host,omitempty"`
	Proto         string            `msgpack:"pr,omitempty" json:"proto,omitempty"`
	RemoteAddr    string            `msgpack:"a" json:"remote_addr"`
	ContentLength int64             `msgpack:"cl,omitempty" json:"content_length,omitempty"`
	Headers       map[string]string `msgpack:"h" json:"headers"`
	Body          []byte            `msgpack:"bd,omitempty" json:"body,omitempty"`
	MatchDomain   string            `msgpack:"md,omitempty" json:"match_domain,omitempty"`
	MatchGlob     string            `msgpack:"mg,omitempty" json:"match_glob,omitempty"`
	MatchPath     string            `msgpack:"mp,omitempty" json:"match_path,omitempty"`
	Vars          map[string]string `msgpack:"v,omitempty" json:"vars,omitempty"`
	Target        string            `msgpack:"tg,omitempty" json:"target,omitempty"`
}

// Header returns the value of a request header (case-sensitive key).
func (r *Request) Header(key string) string {
	if r.Headers == nil {
		return ""
	}
	return r.Headers[key]
}

// QueryParam returns the first value of a query parameter.
// For raw access to the full query string, use r.Query.
func (r *Request) QueryParam(key string) string {
	for _, part := range splitQuery(r.Query) {
		k, v, _ := cutByte(part, '=')
		if k == key {
			return v
		}
	}
	return ""
}

func splitQuery(q string) []string {
	if q == "" {
		return nil
	}
	var parts []string
	for {
		i := indexByte(q, '&')
		if i < 0 {
			parts = append(parts, q)
			return parts
		}
		parts = append(parts, q[:i])
		q = q[i+1:]
	}
}

func cutByte(s string, sep byte) (string, string, bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// Response is the plugin's verdict for an on_request hook.
type Response struct {
	Allow      bool              `msgpack:"ok" json:"allow"`
	Drop       bool              `msgpack:"dr,omitempty" json:"drop,omitempty"`
	Fallback   bool              `msgpack:"fb,omitempty" json:"fallback,omitempty"`
	Status     int               `msgpack:"s,omitempty" json:"status,omitempty"`
	Body       string            `msgpack:"b,omitempty" json:"body,omitempty"`
	Headers    map[string]string `msgpack:"h,omitempty" json:"headers,omitempty"`
	SpeedLimit *SpeedLimit       `msgpack:"sp,omitempty" json:"speed_limit,omitempty"`
	CleanQuery  bool              `msgpack:"cq,omitempty" json:"clean_query,omitempty"`
	RewritePath string            `msgpack:"rp,omitempty" json:"rewrite_path,omitempty"`
}

// UpstreamResponse carries upstream response context for on_response hooks.
type UpstreamResponse struct {
	Status  int               `msgpack:"s" json:"status"`
	Headers map[string]string `msgpack:"h" json:"headers"`
}

// ResponseMod describes modifications to apply to the upstream response.
type ResponseMod struct {
	Status  int               `msgpack:"s,omitempty" json:"status,omitempty"`
	Headers map[string]string `msgpack:"h,omitempty" json:"headers,omitempty"`
	Remove  []string          `msgpack:"rm,omitempty" json:"remove,omitempty"`
}

// ConnRequest carries L4 connection context for on_connect hooks.
type ConnRequest struct {
	RouteID     string `msgpack:"r" json:"route_id"`
	Domain      string `msgpack:"d" json:"domain"`
	RemoteAddr  string `msgpack:"a" json:"remote_addr"`
	MatchDomain string `msgpack:"md,omitempty" json:"match_domain,omitempty"`
	MatchGlob   string `msgpack:"mg,omitempty" json:"match_glob,omitempty"`
}

// ConnResponse is the plugin's verdict for an on_connect hook.
type ConnResponse struct {
	Allow bool `msgpack:"ok" json:"allow"`
}

// DisconnectEvent carries connection statistics for on_disconnect hooks.
type DisconnectEvent struct {
	RouteID    string `msgpack:"r" json:"route_id"`
	Target     string `msgpack:"tg,omitempty" json:"target,omitempty"`
	RemoteAddr string `msgpack:"a" json:"remote_addr"`
	BytesRx    int64  `msgpack:"rx" json:"bytes_rx"`
	BytesTx    int64  `msgpack:"tx" json:"bytes_tx"`
	DurationMs int64  `msgpack:"ms" json:"duration_ms"`
}

// SpeedLimit defines bandwidth caps in Mbps.
// When GroupKey is set, all connections sharing the same key share a single
// bandwidth budget instead of each getting independent per-connection limits.
type SpeedLimit struct {
	DownloadMbps float64 `msgpack:"dl,omitempty" json:"download_mbps,omitempty"`
	UploadMbps   float64 `msgpack:"ul,omitempty" json:"upload_mbps,omitempty"`
	GroupKey     string  `msgpack:"gk,omitempty" json:"group_key,omitempty"`
}

// HookType identifies the hook being invoked over the socket.
type HookType byte

const (
	HookRequest    HookType = 1
	HookResponse   HookType = 2
	HookConnect    HookType = 3
	HookDisconnect HookType = 4
)

// Envelope wraps all socket messages with a type discriminator.
type Envelope struct {
	Hook HookType `msgpack:"t"`
	Data []byte   `msgpack:"d"`
}
