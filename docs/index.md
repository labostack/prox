# prox

prox is a modular reverse proxy for HTTP and raw TCP traffic. It provides config-driven routing, multi-protocol dispatching, load balancing, plugin middleware, and zero-downtime hot reload — all from a single binary.

## Capabilities

**Config-driven routing** — Define services, routes, and actions in JSON5. Match requests by domain pattern, path prefix, HTTP method, or headers. Organize configuration as a single file or a directory of service fragments.

**L4/L7 dispatching** — Run HTTP reverse proxying and raw TCP pass-through on the same listener. SNI-based routing directs TLS connections to upstream servers without termination, while HTTP traffic is routed at L7 with full header inspection.

**Load balancing** — Distribute traffic across upstream targets using round-robin, random, or least-connections strategies. Health checks and connection tracking are built in.

**Speed limiting** — Throttle bandwidth per route with per-connection or shared budgets. Configurable read and write rates apply independently.

**Plugin middleware** — Extend request handling with plugins built using the [Go SDK](plugins/sdk.md). Plugins support authentication, header injection, response modification, L4 connection gating, and dynamic target discovery. Plugins are compiled with `prox build` and communicate over IPC.

**Hot reload** — Edit configuration while the server is running. Changes are applied atomically — in-flight connections complete with the previous configuration, new connections use the updated one.

**TLS** — Multi-certificate SNI with directory-based certificate loading. Certificates are matched automatically by domain.

**WebSocket** — Transparent proxying with session pinning to upstream targets.

## Action Types

Routes resolve to actions. Each action type defines how a matched request is handled.

| Type | Description |
|------|-------------|
| `proxy` | Reverse proxy to upstream servers. Supports WebSocket, load balancing, and custom headers. |
| `static` | Return a fixed response with configurable status code, headers, and template variables. |
| `serve` | Serve files from a directory or a single file. Supports SPA mode. |
| `pass` | L4 TCP pass-through. Relays the raw TLS connection to an upstream without termination. |
| `drop` | Silently close the connection. |

See [Actions](configuration/actions.md) for full configuration reference.

## Plugin Hooks

Plugins attach to lifecycle hooks to intercept traffic at different layers.

| Hook | Level | Description |
|------|-------|-------------|
| `OnRequest` | L7 | Inspect and authorize HTTP requests. Inject or modify headers. |
| `OnResponse` | L7 | Modify upstream response headers and status codes. |
| `OnConnect` | L4 | Gate raw TCP connections on `pass` routes before relay. |
| `OnDisconnect` | L7 | Receive connection statistics (bytes transferred, duration). |
| `OnConfigure` | — | Lifecycle hook for initialization and dynamic target discovery. |

See [Plugins](plugins/index.md) for architecture details and [SDK Reference](plugins/sdk.md) for the Go API.

## Next Steps

| Topic | Description |
|-------|-------------|
| [Getting Started](getting-started.md) | Installation, first configuration, and CLI reference. |
| [Configuration](configuration/index.md) | Services, routes, actions, resources, and TLS setup. |
| [Routing](configuration/routing.md) | Domain patterns, path matching, method filters, and header conditions. |
| [Plugins](plugins/index.md) | Plugin architecture, SDK, and IPC protocol. |
| [Deployment](deployment.md) | Docker, hot reload, and L4 dispatching in production. |
