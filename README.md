# prox

Modular reverse proxy with config-driven routing, load balancing, L4/L7 dispatching, hot reload, and plugin middleware.

[![CI](https://github.com/dortanes/prox/actions/workflows/ci.yml/badge.svg)](https://github.com/dortanes/prox/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/go-%E2%89%A5%201.25-brightgreen.svg)](https://golang.org/)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

**[Documentation](https://dortanes.github.io/prox)** · [Getting Started](https://dortanes.github.io/prox/getting-started) · [Configuration](https://dortanes.github.io/prox/configuration/) · [Plugins](https://dortanes.github.io/prox/plugins/) · [Deployment](https://dortanes.github.io/prox/deployment)

> ⚠️ **Note:** This project is currently under active development. Core features, APIs, and configuration structures may undergo significant breaking changes.

## Quick Start

```bash
go install github.com/dortanes/prox/cmd/prox@latest

prox serve -config config.json5
```

Or build from source:

```bash
go build -o prox ./cmd/prox
```

## Features

- **Config-driven routing** — JSON5 config with services, actions, and resources
- **Domain matching** — segment wildcards (`*.example.com`, `cdn-*.**`)
- **L4 dispatching** — SNI-based TCP pass-through alongside HTTP on the same port
- **Load balancing** — round-robin, random, least-connections with connection tracking
- **Plugin middleware** — auth, response modification, L4 gating via Go SDK
- **Dynamic targets** — plugin-based service discovery with grouped targeting
- **Hot reload** — zero-downtime config changes with file watcher
- **WebSocket** — transparent proxy with session pinning
- **TLS** — multi-cert SNI, directory-based cert loading
- **HTTP/2** — full-duplex h2c upstream support

## Config

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

| Action | Description |
|--------|-------------|
| `proxy` | Reverse proxy with WebSocket, load balancing, custom headers |
| `static` | Fixed response with status, headers, template variables |
| `serve` | File server — directory or single file (SPA) |
| `pass` | L4 TCP pass-through — raw TLS relay |
| `drop` | Silently close the connection |

## Plugins

Extend prox with auth, response modification, and L4 gating via the [Go SDK](https://dortanes.github.io/prox/plugins#sdk):

```go
p := sdk.New()
p.OnRequest(func(req *sdk.Request) *sdk.Response {
    if req.Header("Authorization") == "" {
        return sdk.Deny(401, "Unauthorized")
    }
    return sdk.Allow()
})
p.Run()
```

```json5
{
  plugins: {
    auth: { path: "./plugins/auth.go" },
  },
  services: {
    web: {
      routes: [
        {
          match: { domain: "*.example.com", path: "/api/*" },
          plugins: ["auth"],
          plugin_timeout: "2s",
          action: { type: "proxy", upstream: "localhost:3000" },
        }
      ]
    }
  }
}
```

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

## License

MIT
