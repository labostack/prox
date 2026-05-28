# prox

A modular reverse proxy with config-driven routing, load balancing, L4/L7 dispatching, hot reload, and plugin middleware.

[![CI](https://github.com/dortanes/prox/actions/workflows/ci.yml/badge.svg)](https://github.com/dortanes/prox/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/dortanes/prox.svg)](https://pkg.go.dev/github.com/dortanes/prox)
[![Go Report Card](https://goreportcard.com/badge/github.com/dortanes/prox)](https://goreportcard.com/report/github.com/dortanes/prox)
[![Release](https://img.shields.io/github/v/release/dortanes/prox?logo=github&color=blue)](https://github.com/dortanes/prox/releases)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

**[Documentation](https://dortanes.github.io/prox)** · [Getting Started](https://dortanes.github.io/prox/getting-started) · [Configuration](https://dortanes.github.io/prox/configuration/) · [Plugins](https://dortanes.github.io/prox/plugins/) · [Deployment](https://dortanes.github.io/prox/deployment)

---

## Install

```bash
go install github.com/dortanes/prox/cmd/prox@latest
```

## Usage

```bash
prox serve -config config.json5
```

Minimal configuration:

```json5
{
  services: {
    web: {
      listen: ":8080",
      routes: [
        { match: { domain: "api.example.com", path: "/v1/*" }, action: "api" },
        { match: { domain: "*.example.com" }, action: "site" },
      ],
    },
  },
  actions: {
    api:  { type: "proxy", upstream: "localhost:3000" },
    site: { type: "serve", root: "./public" },
  },
}
```

## Features

- **JSON5 routing** — services, routes, actions, and resources
- **Domain wildcards** — `*.example.com`, `cdn-*.**`
- **L4 + L7** — SNI-based TCP pass-through alongside HTTP on the same port
- **Load balancing** — round-robin, random, least-connections
- **Speed limiting** — per-route, per-connection, or shared bandwidth caps
- **Plugin middleware** — auth, response modification, service discovery via [Go SDK](https://dortanes.github.io/prox/plugins/sdk)
- **Hot reload** — zero-downtime config swap via file watcher or SIGHUP
- **WebSocket & HTTP/2** — transparent proxying with h2c upstream support
- **TLS** — multi-cert SNI with directory-based certificate loading

## Performance

~88K req/s — 2.87 ms avg latency (HTTP/1.1, no TLS, single node).

| Proxy | Req/s | Avg | P99 |
|-------|------:|----:|----:|
| HAProxy | 90,080 | 2.78 ms | 4.12 ms |
| **prox** | **88,643** | **2.87 ms** | **3.96 ms** |
| Nginx | 87,768 | 2.90 ms | 3.74 ms |
| Traefik | 82,737 | 3.08 ms | 5.84 ms |

<details>
<summary>Details</summary>

Apple M4 Pro, 12-core, 24 GB · `wrk -t4 -c256 -d10s` · Go upstream (200 OK, 2 bytes) · `bash bench/run.sh`
</details>

## License

[MIT](LICENSE)
