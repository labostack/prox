# prox

Modular reverse proxy with config-driven routing, load balancing, L4/L7 dispatching, hot reload, and plugin middleware.

## Features

- **Config-driven** — JSON5 config with services, actions, and resources
- **L7 routing** — domain patterns, path matching, method filters
- **L4 dispatching** — SNI-based TCP pass-through alongside HTTP
- **Load balancing** — round-robin, random, least-connections
- **Speed limiting** — per-route bandwidth throttling with shared or per-connection budgets
- **Plugin system** — auth, response modification, target discovery via Go SDK
- **Hot reload** — zero-downtime config changes with file watcher
- **WebSocket** — transparent proxy with session pinning
- **TLS** — multi-cert SNI, directory-based cert loading

## Quick Start

```bash
go install github.com/dortanes/prox/cmd/prox@latest

prox serve -config config.json5
```

```json5
{
  services: {
    web: {
      listen: ":8080",
      routes: [
        { match: { domain: "api.example.com", path: "/v1/*" }, action: "api" },
        { match: { domain: "*.example.com" }, action: "site" },
        { action: { type: "drop" } },
      ],
    },
  },
  actions: {
    api: { type: "proxy", upstream: "localhost:3000", timeout: "5s" },
    site: { type: "serve", root: "./public" },
  },
}
```

## Action Types

| Type | Description |
|------|-------------|
| `proxy` | Reverse proxy with WebSocket, load balancing, custom headers |
| `static` | Fixed response with status, headers, template variables |
| `serve` | File server — directory or single file (SPA) |
| `pass` | L4 TCP pass-through — raw TLS relay without termination |
| `drop` | Silently close the connection |

## Plugin Middleware

Plugins extend prox with auth, response modification, and L4 gating via a [Go SDK](plugins/sdk.md):

```go
p := sdk.New()
p.OnRequest(func(req *sdk.Request) *sdk.Response {
    if !validateToken(req.Header("Authorization")) {
        return sdk.Deny(401, "Unauthorized")
    }
    return sdk.Allow(sdk.WithHeader("X-User-ID", "123"))
})
p.Run()
```

| Hook | Level | Description |
|------|-------|-------------|
| `OnRequest` | L7 | Authorize HTTP requests, inject headers |
| `OnResponse` | L7 | Modify upstream response headers/status |
| `OnConnect` | L4 | Gate raw TCP connections (pass routes) |
| `OnDisconnect` | L7 | Receive connection stats (bytes, duration) |
| `OnConfigure` | — | Lifecycle hook for target discovery |

## Docker

```yaml
services:
  prox:
    image: ghcr.io/dortanes/prox:latest
    ports:
      - "443:443"
      - "8080:8080"
    volumes:
      - ./config/:/etc/prox/config/
      - ./certs/:/etc/prox/certs/
    command: ["serve", "-config", "/etc/prox/config/"]
```
