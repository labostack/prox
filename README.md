# prox

Modular reverse proxy with config-driven routing, load balancing, L4/L7 dispatching, hot reload, and zero dependencies.

[![CI](https://github.com/dortanes/prox/actions/workflows/ci.yml/badge.svg)](https://github.com/dortanes/prox/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/go-%E2%89%A5%201.23-brightgreen.svg)](https://golang.org/)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

## Quick Start

```bash
go install github.com/dortanes/prox/cmd/prox@latest

prox serve -config config.json5
```

Or build from source:

```bash
go build -o prox ./cmd/prox
```

## Config

Three sections: **services** (listeners), **actions** (handlers), **resources** (data).

```json5
{
  services: {
    web: {
      listen: ":8080",
      routes: [
        { match: { domain: "api.example.com", path: "/v1/*" }, action: "api" },
        { match: { domain: "*.example.com", path: "/*" }, action: "site" },
        { match: { path: "/*" }, action: "default" },
      ],
    },
  },
  actions: {
    api: { type: "proxy", upstream: "localhost:3000", timeout: "5s" },
    site: { type: "serve", root: "./public" },
    default: { type: "static", status: 404, body_ref: { text: "Not found" } },
  },
}
```

Routes are evaluated in order, first match wins. Match criteria include [domain](docs/configuration.md#domain-matching) patterns (`*.example.com`, `test.*.example.com`) and path patterns (`/health`, `/api/*`). Omit `match` for a [catch-all](docs/configuration.md#routes) route. Actions and resources can be referenced by name or [inlined](docs/configuration.md#inline-actions). Services can be [split into separate files](docs/configuration.md#file-reference) or loaded from a [config directory](docs/configuration.md#directory-mode-cli). Routes can be [included from external files](docs/configuration.md#route-includes) to keep configs modular. Routes can use a [balancer](docs/configuration.md#load-balancing) to distribute traffic across multiple upstream targets. Each service has an optional [`config`](docs/configuration.md#service-config) block for tuning timeouts and streaming behavior.

### Action Types

| Type     | Description                                                                                                                |
| -------- | -------------------------------------------------------------------------------------------------------------------------- |
| `proxy`  | Reverse proxy with [WebSocket support](docs/configuration.md#websocket-support), [load balancing](docs/configuration.md#load-balancing), configurable timeout and [custom headers](docs/configuration.md#proxy--reverse-proxy) |
| `static` | Fixed response with status, headers, and optional body with [template variables](docs/configuration.md#template-variables) |
| `serve`  | File server — directory with auto `index.html`, or single file (SPA)                                                       |
| `pass`   | L4 TCP pass-through — [relay raw TLS to upstream](docs/configuration.md#pass--l4-tcp-pass-through) without termination     |
| `drop`   | Silently [close the connection](docs/configuration.md#drop--drop-connection) — useful as a catch-all fallback               |

See [docs/configuration.md](docs/configuration.md) for the full reference.

## Load Balancing

Routes can distribute traffic across multiple upstreams. The balancer selects a target per request (L7) or per connection (L4 `pass`), available as `{target}` in the action's `upstream`.

```json5
{
  match: { domain: "*.**", path: "/ws" },
  balancer: {
    type: "roundrobin",   // or "random", "leastconn"
    targets: [
      "10.0.1.1:3505",
      "10.0.1.2:3505",
      "10.0.1.3:3505",
    ],
  },
  action: {
    type: "proxy",
    upstream: "{target}",
  },
}
```

WebSocket connections through balanced routes are pinned to the selected target for the entire session. See [docs/configuration.md#load-balancing](docs/configuration.md#load-balancing) for details.

## Plugins

Plugins are external executables that dynamically manage balancer targets at runtime. They communicate with prox over stdin/stdout using line-delimited JSON. Plugins run as sidecar processes — they never touch the request hot path.

```json5
{
  match: { domain: "*.**", path: "/ws" },
  plugins: ["./plugins/resolver"],
  balancer: { type: "leastconn" },
  set: { port: "8080" },
  action: "dynamic_proxy",
}
```

On startup, prox sends a `configure` message with the route's match criteria. The plugin responds with `set_targets` pushes — either a flat list or grouped by key (e.g., location prefix). Grouped targets enable per-request routing based on domain wildcard captures.

Route-level variables (`set`) and action `fallback` allow sharing a single action across multiple routes with different parameters and graceful degradation.

**Lifecycle**: auto-restart on crash with exponential backoff, graceful shutdown on SIGTERM, reconfigure on hot reload.

See [docs/plugins.md](docs/plugins.md) for the protocol specification and authoring guide.

## L4 Dispatching

A single listener can mix L4 (TCP pass-through) and L7 (HTTP) routes. The dispatcher peeks the TLS ClientHello for the SNI hostname, walks routes in config order, and dispatches:

- **`pass` routes** — raw TCP relay to upstream, no TLS termination
- **L7 routes** — TLS termination, then HTTP routing as usual

The dispatcher activates automatically when any route uses `type: "pass"`. No configuration flags needed.

```json5
{
  services: {
    gateway: {
      listen: ":443",
      tls: true,
      tls_cert: "/etc/prox/certs/",
      routes: [
        // L4: relay raw TLS to backend (no termination)
        {
          match: { domain: "*.fun.example.com" },
          action: { type: "pass", upstream: "10.0.0.5:1022" },
        },

        // L7: terminate TLS, then handle HTTP
        { match: { domain: "*.example.com" }, action: "app" },
      ],
    },
  },
  actions: {
    app: { type: "proxy", upstream: "localhost:3000" },
  },
}
```

## Hot Reload

Config changes are picked up automatically via file watcher, or manually via `kill -HUP`. Both L4 and L7 routes are swapped atomically — in-flight connections finish with the old config, new connections use the new one. Invalid configs are rejected silently.

All loaded files are watched — editing a nested service fragment triggers a full reload.

```
prox serve -config config.json5          # watcher enabled by default
prox serve -config config.json5 -watch=false
```

## CLI

```
prox <command> [flags]

  serve      Start the proxy server
  validate   Validate config (exit 0 = valid, 1 = invalid)
  version    Print version

  -config    Path to config file or directory (default "config.json5")
  -log-level debug | info | warn | error (default "info")
  -watch     Auto-reload on file change (default true)
```

## Docker

```yaml
# docker-compose.yml
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

Or with `docker run`:

```bash
docker run -v ./config.json5:/etc/prox/config.json5 -p 8080:8080 ghcr.io/dortanes/prox
```

## License

MIT
