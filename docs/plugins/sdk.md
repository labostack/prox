# Go SDK

The Go SDK (`github.com/dortanes/prox/sdk`) provides a clean callback API for writing plugins. It handles all transport details — stdin/stdout JSON, Unix socket msgpack framing, and the `ready` handshake.

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

The `Route` struct contains:

| Field    | Type   | Description                                |
|----------|--------|--------------------------------------------|
| `ID`     | string | Stable identifier (`service:routeIndex`)   |
| `Domain` | string | Domain pattern from config                 |
| `Path`   | string | Path pattern from config                   |

### `OnRequest(func(req *Request) *Response)` — L7

Called for every HTTP request on the route. Returns a verdict:

- `sdk.Allow(opts...)` — approve the request, optionally inject headers
- `sdk.Deny(status, body, opts...)` — reject with an HTTP error response
- `sdk.Drop()` — silently close the connection (no HTTP response)

```go
p.OnRequest(func(req *sdk.Request) *sdk.Response {
    // req.Method, req.Path, req.Domain, req.RemoteAddr
    // req.Query, req.Host, req.Proto, req.ContentLength
    // req.Header("Authorization"), req.Headers, req.Body
    // req.QueryParam("token")
    // req.MatchDomain, req.MatchGlob, req.MatchPath, req.Vars
    return sdk.Allow(sdk.WithHeader("X-Verified", "true"))
})
```

### `OnResponse(func(req *Request, resp *UpstreamResponse) *ResponseMod)` — L7

Called after the upstream responds, before headers are sent to the client. Modify or remove headers, override the status code.

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

Called for raw TCP connections on `pass` routes (before TLS relay). Only has access to SNI domain and remote address.

```go
p.OnConnect(func(conn *sdk.ConnRequest) *sdk.ConnResponse {
    if isBlacklisted(conn.RemoteAddr) {
        return sdk.RejectConn()
    }
    return sdk.AcceptConn()
})
```

## Request Fields

| Field           | Type              | Description                                  |
|-----------------|-------------------|----------------------------------------------|
| `RouteID`       | `string`          | Route identifier (`service:routeIndex`)      |
| `Method`        | `string`          | HTTP method (`GET`, `POST`, etc.)            |
| `Path`          | `string`          | Request path (`/api/users`)                  |
| `Query`         | `string`          | Raw query string (`foo=bar&baz=1`)           |
| `Domain`        | `string`          | Request host without port                    |
| `Host`          | `string`          | Full Host header including port              |
| `Proto`         | `string`          | HTTP protocol (`HTTP/1.1`, `HTTP/2.0`)       |
| `RemoteAddr`    | `string`          | Client IP:port                               |
| `ContentLength` | `int64`           | Request body size (`-1` if unknown)          |
| `Headers`       | `map[string]string` | All request headers (first value only)     |
| `Body`          | `[]byte`          | Request body (capped at 64KB)                |
| `MatchDomain`   | `string`          | Captured `*` wildcard value(s)               |
| `MatchGlob`     | `string`          | Captured `**` glob suffix                    |
| `MatchPath`     | `string`          | Path pattern from config                     |
| `Vars`          | `map[string]string` | Route-level `set` variables                |

### Helper Methods

- `req.Header(key)` — get a request header value
- `req.QueryParam(key)` — get a query parameter value

## Response Helpers

### Request Verdicts

```go
sdk.Allow(opts...)                    // approve request
sdk.Deny(status, body, opts...)       // reject with HTTP response
sdk.Drop()                            // silently close connection

sdk.WithHeader(key, value)            // inject header (on allow: into request, on deny: into response)
sdk.WithSpeedLimit(down, up)          // set per-connection bandwidth cap (Mbps)
```

### Response Modifications

```go
sdk.ModifyResponse(opts...)           // modify upstream response
sdk.NoResponseMod()                   // pass through unchanged

sdk.WithResponseHeader(key, value)    // add/override response header
sdk.RemoveResponseHeader(key)         // remove response header
sdk.WithResponseStatus(status)        // override status code
```

### L4 Connection Verdicts

```go
sdk.AcceptConn()                      // allow TCP connection
sdk.RejectConn()                      // close TCP connection
```

## Push API

For target discovery, use the push methods (these go over stdin/stdout, no socket needed):

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

With domain pattern `*.**`, a request to `de.example.com` captures `de` → the balancer picks from the `"de"` group only. Each group gets its own sub-balancer with the route's strategy.

### Speed Limiting

```go
// By route ID:
p.SetSpeedLimit(routeID, sdk.SpeedLimit{DownloadMbps: 50, UploadMbps: 10})

// By action name:
p.SetActionSpeedLimit("proxy", sdk.SpeedLimit{DownloadMbps: 100})

// Wildcard — all routes:
p.SetSpeedLimit("*", sdk.SpeedLimit{DownloadMbps: 25})
```

When multiple limits apply (config, push, response), the most restrictive value wins.

### Methods

| Method | Description |
|--------|-------------|
| `SetTargets(routeID, targets)` | Push flat targets for a specific route (or `"*"` for all) |
| `SetGroupedTargets(routeID, groups)` | Push grouped targets for a specific route (or `"*"` for all) |
| `SetActionTargets(action, targets)` | Push flat targets for all routes using the given action |
| `SetActionGroupedTargets(action, groups)` | Push grouped targets for all routes using the given action |
| `SetSpeedLimit(routeID, limit)` | Push per-connection speed limit for a specific route (or `"*"` for all) |
| `SetActionSpeedLimit(action, limit)` | Push per-connection speed limit for all routes using the given action |
