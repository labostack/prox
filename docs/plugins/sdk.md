# Go SDK

The Go SDK (`github.com/dortanes/prox/sdk`) provides a callback-based API for building plugins. It handles all transport details — stdin/stdout JSON messaging, Unix socket msgpack framing, and the `ready` handshake.

## Installation

```bash
go get github.com/dortanes/prox/sdk
```

## Quick Start

### Auth Plugin

```go
package main

import "github.com/dortanes/prox/sdk"

func main() {
    p := sdk.New()

    p.OnRequest(func(req *sdk.Request) *sdk.Response {
        token := req.Header("Authorization")
        if token == "" {
            return sdk.Deny(401, "Unauthorized")
        }
        return sdk.Allow(sdk.WithHeader("X-User-ID", "123"))
    })

    p.Run()
}
```

### Target Discovery Plugin

```go
package main

import "github.com/dortanes/prox/sdk"

func main() {
    p := sdk.New()

    p.OnConfigure(func(route sdk.Route) {
        go func() {
            targets := discoverTargets(route.Domain)
            p.SetTargets(route.ID, targets)
        }()
    })

    p.Run()
}
```

## Hooks

### `OnConfigure(func(route Route))`

Called when the plugin receives route configuration (on startup and reload).

**Route fields:**

| Field | Type | Description |
|-------|------|-------------|
| `ID` | `string` | Stable identifier (`service:routeIndex`) |
| `Domain` | `string` | Domain pattern from config |
| `Path` | `string` | Path pattern from config |

### `OnRequest(func(req *Request) *Response)` — L7

Called for every HTTP request on the route. Returns a verdict:

- `sdk.Allow(opts...)` — approve the request, optionally inject headers.
- `sdk.Deny(status, body, opts...)` — reject with an HTTP error response.
- `sdk.Drop()` — silently close the connection (no HTTP response).

```go
p.OnRequest(func(req *sdk.Request) *sdk.Response {
    // req.Method, req.Path, req.Domain, req.RemoteAddr
    // req.Query, req.Host, req.Proto, req.ContentLength
    // req.Header("Authorization"), req.Headers, req.Body
    // req.QueryParam("token")
    // req.MatchDomain, req.MatchGlob, req.MatchPath, req.Vars, req.Target
    return sdk.Allow(sdk.WithHeader("X-Verified", "true"))
})
```

### `OnResponse(func(req *Request, resp *UpstreamResponse) *ResponseMod)` — L7

Called after the upstream responds, before headers are sent to the client. Modify or remove headers, or override the status code.

```go
p.OnResponse(func(req *sdk.Request, resp *sdk.UpstreamResponse) *sdk.ResponseMod {
    return sdk.ModifyResponse(
        sdk.WithResponseHeader("X-Frame-Options", "DENY"),
        sdk.RemoveResponseHeader("Server"),
        sdk.WithResponseStatus(200),
    )
})
```

### `OnConnect(func(conn *ConnRequest) *ConnResponse)` — L4

Called for raw TCP connections on `pass` routes (before TLS relay). Only SNI domain and remote address are available.

```go
p.OnConnect(func(conn *sdk.ConnRequest) *sdk.ConnResponse {
    if isBlacklisted(conn.RemoteAddr) {
        return sdk.RejectConn()
    }
    return sdk.AcceptConn()
})
```

### `OnDisconnect(func(event *DisconnectEvent))` — Fire-and-Forget

Called after a connection ends (handler returns). Receives connection statistics. Unlike other hooks, this is fire-and-forget — no response is sent back to prox.

```go
p.OnDisconnect(func(event *sdk.DisconnectEvent) {
    log.Printf("route=%s target=%s bytes_rx=%d bytes_tx=%d duration=%dms",
        event.RouteID, event.Target, event.BytesRx, event.BytesTx, event.DurationMs)
})
```

**DisconnectEvent fields:**

| Field | Type | Description |
|-------|------|-------------|
| `RouteID` | `string` | Route identifier (`service:routeIndex`) |
| `Target` | `string` | Backend target host (SNI/domain) |
| `RemoteAddr` | `string` | Client IP:port |
| `BytesRx` | `int64` | Bytes received from client (upload) |
| `BytesTx` | `int64` | Bytes transmitted to client (download) |
| `DurationMs` | `int64` | Connection duration in milliseconds |

## Request Fields

| Field | Type | Description |
|-------|------|-------------|
| `RouteID` | `string` | Route identifier (`service:routeIndex`) |
| `Method` | `string` | HTTP method (`GET`, `POST`, etc.) |
| `Path` | `string` | Request path (`/api/users`) |
| `Query` | `string` | Raw query string (`foo=bar&baz=1`) |
| `Domain` | `string` | Request host without port |
| `Host` | `string` | Full Host header including port |
| `Proto` | `string` | HTTP protocol (`HTTP/1.1`, `HTTP/2.0`) |
| `RemoteAddr` | `string` | Client IP:port |
| `ContentLength` | `int64` | Request body size (`-1` if unknown) |
| `Headers` | `map[string]string` | All request headers (first value only) |
| `Body` | `[]byte` | Request body (capped at 64KB) |
| `MatchDomain` | `string` | Captured `*` wildcard value(s) |
| `MatchGlob` | `string` | Captured `**` glob suffix |
| `MatchPath` | `string` | Path pattern from config |
| `Vars` | `map[string]string` | Route-level `set` variables |
| `Target` | `string` | Backend target selected by balancer |

### Helper Methods

| Method | Description |
|--------|-------------|
| `req.Header(key)` | Returns the value of a request header |
| `req.QueryParam(key)` | Returns the value of a query parameter |

## Response Helpers

### Request Verdicts

```go
sdk.Allow(opts...)                    // Approve request
sdk.Deny(status, body, opts...)       // Reject with HTTP response
sdk.Fallback(opts...)                 // Route to the fallback action
sdk.Drop()                            // Silently close connection

sdk.WithHeader(key, value)            // Inject header (allow: into request, deny: into response)
sdk.WithSpeedLimit(down, up)          // Per-connection bandwidth cap (Mbps)
sdk.WithSpeedLimit(down, up, groupKey)// Grouped bandwidth cap (shared by connections with same key)
sdk.WithCleanQuery()                  // Remove query string from upstream request
sdk.WithRewritePath(path)             // Override upstream request path
```

### Response Modifications

```go
sdk.ModifyResponse(opts...)           // Modify upstream response
sdk.NoResponseMod()                   // Pass through unchanged

sdk.WithResponseHeader(key, value)    // Add or override response header
sdk.RemoveResponseHeader(key)         // Remove response header
sdk.WithResponseStatus(status)        // Override status code
```

### L4 Connection Verdicts

```go
sdk.AcceptConn()                      // Allow TCP connection
sdk.RejectConn()                      // Close TCP connection
```

## Push API

Target discovery and speed limiting use push methods over stdin/stdout (no socket required).

### Target Updates

```go
// By route ID:
p.SetTargets(routeID, []string{"10.0.1.1:8080", "10.0.1.2:8080"})

// By action name — updates all routes using the given action:
p.SetActionTargets("dynamic_proxy", []string{"10.0.1.1:8080", "10.0.1.2:8080"})

// Wildcard — updates all routes with balancers:
p.SetTargets("*", []string{"10.0.1.1:8080"})
```

### Grouped Targets

```go
p.SetGroupedTargets(routeID, map[string][]string{
    "de": {"de-1:8080", "de-2:8080"},
    "us": {"us-1:8080"},
})
p.SetActionGroupedTargets("dynamic_proxy", map[string][]string{
    "de": {"de-1:8080", "de-2:8080"},
    "us": {"us-1:8080"},
})
```

With domain pattern `*.**`, a request to `de.example.com` captures `de` — the balancer picks from the `"de"` group only. Each group gets its own sub-balancer using the route's strategy.

### Speed Limiting

```go
// By route ID:
p.SetSpeedLimit(routeID, sdk.SpeedLimit{DownloadMbps: 50, UploadMbps: 10})

// By action name:
p.SetActionSpeedLimit("proxy", sdk.SpeedLimit{DownloadMbps: 100})

// Wildcard — all routes:
p.SetSpeedLimit("*", sdk.SpeedLimit{DownloadMbps: 25})
```

When `GroupKey` is set, the push updates the rate for active group buckets with that key:

```go
p.SetSpeedLimit(routeID, sdk.SpeedLimit{DownloadMbps: 50, GroupKey: userID})
```

When multiple limits apply (config, push, response), the most restrictive value wins.

### Method Reference

| Method | Description |
|--------|-------------|
| `SetTargets(routeID, targets)` | Push flat targets for a specific route (or `"*"` for all) |
| `SetGroupedTargets(routeID, groups)` | Push grouped targets for a specific route (or `"*"` for all) |
| `SetActionTargets(action, targets)` | Push flat targets for all routes using the given action |
| `SetActionGroupedTargets(action, groups)` | Push grouped targets for all routes using the given action |
| `SetSpeedLimit(routeID, limit)` | Push speed limit for a specific route (or `"*"` for all) |
| `SetActionSpeedLimit(action, limit)` | Push speed limit for all routes using the given action |
