# prox

Modular reverse proxy with config-driven routing, load balancing, L4/L7 dispatching, hot reload, and plugin middleware.

[![CI](https://github.com/dortanes/prox/actions/workflows/ci.yml/badge.svg)](https://github.com/dortanes/prox/actions/workflows/ci.yml) [![Go Reference](https://pkg.go.dev/badge/github.com/dortanes/prox.svg)](https://pkg.go.dev/github.com/dortanes/prox) [![Go Report Card](https://goreportcard.com/badge/github.com/dortanes/prox)](https://goreportcard.com/report/github.com/dortanes/prox) [![GitHub Release](https://img.shields.io/github/v/release/dortanes/prox?logo=github&color=blue)](https://github.com/dortanes/prox/releases) [![Go Version](https://img.shields.io/badge/go-%E2%89%A5%201.25-brightgreen.svg)](https://golang.org/) [![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

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

## Features

- **Config-driven routing** — JSON5 config with services, actions, and resources
- **Domain matching** — segment wildcards (`*.example.com`, `cdn-*.**`)
- **L4 dispatching** — SNI-based TCP pass-through alongside HTTP on the same port
- **Load balancing** — round-robin, random, least-connections with connection tracking
- **Speed limiting** — per-route bandwidth throttling with shared or per-connection budgets
- **Plugin middleware** — auth, response modification, L4 gating, connection stats via Go SDK
- **Dynamic targets** — plugin-based service discovery with grouped targeting
- **Hot reload** — zero-downtime config changes with file watcher
- **Logging** — colorized console, leveled output, file-based access/error logs
- **WebSocket** — transparent proxy with session pinning
- **TLS** — multi-cert SNI, directory-based cert loading
- **HTTP/2** — full-duplex h2c upstream support
- **Fully concurrent** — goroutine-per-connection across all CPU cores

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

Extend prox with auth, response modification, L4 gating, and connection statistics via the [Go SDK](https://dortanes.github.io/prox/plugins#sdk):

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
    ratelimit: { path: "./plugins/ratelimit" },
  },
  services: {
    web: {
      plugins: ["auth"],              // applies to all routes in this service
      routes: [
        {
          match: { path: "/api/*" },
          plugins: ["ratelimit"],     // route-specific (in addition to service-level)
          action: "api",
        }
      ]
    }
  },
  actions: {
    api: {
      type: "proxy",
      upstream: "localhost:3000",
      plugins: ["ratelimit"],         // applies to all routes using this action
    },
  },
}
```

Plugins are merged from all levels: **service → action → route** (deduplicated, first occurrence wins).

Plugins with `autostart: true` are spawned at proxy startup without requiring route bindings — useful for background routines, health monitors, metrics exporters, and other global tasks:

```json5
plugins: {
  routines: { path: "./plugins/routines", autostart: true },
}
```

## Speed Limiting

Per-route bandwidth throttling with independent download/upload limits. Supports two modes:

**Per-connection** — each connection gets its own bandwidth budget (default):

```json5
{
  match: { path: "/api/*" },
  speed: { download_mbps: 50, upload_mbps: 10 },
  action: { type: "proxy", upstream: "localhost:3000" },
}
```

**Shared** — all connections on the route share the bandwidth budget:

```json5
{
  match: { path: "/downloads/*" },
  speed: { download_mbps: 100, shared: true },
  action: { type: "proxy", upstream: "localhost:3000" },
}
```

Plugins can also set speed limits dynamically — per-route via push or per-connection in `on_request`:

```go
// Set route-wide default.
p.SetSpeedLimit("web:0", sdk.SpeedLimit{DownloadMbps: 50})

// Per-connection limit in on_request.
p.OnRequest(func(req *sdk.Request) *sdk.Response {
    return sdk.Allow(sdk.WithSpeedLimit(10, 5)) // 10 Mbps down, 5 Mbps up
})

// Grouped limit — all connections with the same key share a single budget.
p.OnRequest(func(req *sdk.Request) *sdk.Response {
    return sdk.Allow(sdk.WithSpeedLimit(50, 50, userID))
})
```

When multiple limits apply (config + plugin push + plugin response), the **most restrictive** value wins per direction. Speed limiting works with all proxy modes including WebSocket and gRPC.

## Logging

Colorized console output with structured key-value fields. Log level can be set via environment variable, CLI flag, or config file:

```bash
# Environment variable (highest priority)
LOG_LEVEL=debug prox serve

# CLI flag
prox serve -log-level debug
```

File-based logging with global and per-route access logs:

```json5
{
  logging: {
    level: "info",                           // overridden by LOG_LEVEL env
    access_log: "/var/log/prox/access.log",  // global access log (JSON lines)
    error_log: "/var/log/prox/error.log",    // warn/error level messages
  },
  services: {
    web: {
      routes: [
        {
          match: { path: "/api/*" },
          access_log: "/var/log/prox/api.log",  // per-route access log
          action: { type: "proxy", upstream: "localhost:3000" },
        },
      ],
    },
  },
}
```

Log files support rotation via `SIGHUP` — send the signal to reopen all log files after rotating them with tools like `logrotate`.

## Performance

**~88K requests/sec** with 2.8 ms average latency (HTTP/1.1 reverse proxy, no TLS, single node).

Comparison with popular proxies — same machine, same upstream, same load tool ([wrk](https://github.com/wg/wrk), 256 connections):

| Proxy | Req/s | Avg latency | P99 latency |
|-------|------:|------------:|------------:|
| HAProxy | 90,080 | 2.78 ms | 4.12 ms |
| **prox** | **88,643** | **2.87 ms** | **3.96 ms** |
| Nginx | 87,768 | 2.90 ms | 3.74 ms |
| Traefik | 82,737 | 3.08 ms | 5.84 ms |
| Caddy | 12,335 | 4.84 ms | 18.11 ms |

<details>
<summary>Benchmark details</summary>

- **Machine:** Apple M4 Pro (12-core), 24 GB RAM, macOS
- **Load:** `wrk -t4 -c256 -d10s`, 3 runs per proxy, best result used
- **Upstream:** Go HTTP server returning `200 OK` (2 bytes)
- **Config:** Minimal reverse proxy config, logging disabled, no TLS
- **Tuning:** `SO_REUSEPORT` enabled with multiple parallel acceptor loops (tuned to `PROX_WORKERS=2` on macOS to eliminate kqueue scheduler contention), platform-specific socket optimizations (like `TCP_DEFER_ACCEPT` on Linux to avoid waking worker threads until request data is ready to read), production Go compiler optimizations (`-ldflags="-s -w"`), and disabled background GC sweeps (`GOGC=off` for benchmark duration) to maximize raw scheduler throughput.
- **Reproduce:** `bash bench/run.sh` (requires `brew install wrk nginx haproxy caddy traefik`)
</details>

> Results depend on hardware, OS, and workload. Run `bench/run.sh` on your own machine for accurate numbers.

## License

MIT
