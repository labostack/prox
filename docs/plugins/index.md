# Plugins

Plugins are external executables that extend prox at runtime. They dynamically manage balancer targets, authorize HTTP requests, modify upstream responses, and gate L4 TCP connections.

## Overview

- Plugins communicate with prox over **stdin/stdout** (JSON) for lifecycle events and **Unix sockets** (msgpack) for request-response hooks.
- Each plugin runs as a **child process**, automatically restarted on crash.
- The [Go SDK](sdk.md) handles all transport details — plugin authors register callbacks only.
- Target updates are applied **atomically** with zero lock contention on the data plane.

## Modes

| Mode | Hooks | Transport | Use Case |
|------|-------|-----------|----------|
| Push-only | `OnConfigure` | stdin/stdout | Target discovery, DNS resolution |
| Request-response | `OnRequest`, `OnResponse`, `OnConnect` | Unix socket (msgpack) | Auth, rate limiting, header injection |
| Fire-and-forget | `OnDisconnect` | Unix socket (msgpack) | Connection statistics, usage tracking |
| Hybrid | All | Both | Full middleware + discovery |

## Building Plugins

Plugins written as `.go` source files or Go package directories must be compiled with `prox build` before starting the server. Pre-compiled binaries are used as-is and must be executable.

```bash
# Compile all plugins defined in config
prox build -config config.json5
```

This compiles each plugin source and places the binary alongside the source file. Rebuilds are **skipped** if the binary is newer than the source (mtime check).

**Single file** — path ends in `.go`:

```json5
plugins: {
  auth: { path: "./plugins/auth.go" }  // → builds ./plugins/auth
}
```

**Directory** — path points to a Go package:

```json5
plugins: {
  auth: { path: "./plugins/auth/" }  // → builds ./plugins/auth/auth
}
```

!!! tip
    Run `prox build` in CI/CD pipelines or Dockerfile build stages. `prox serve` expects pre-compiled binaries and returns an error if plugin source has not been compiled.

## Configuration

Plugins are defined in the global `plugins` block and attached at three levels: route, service, or action.

### Route-Level

Plugins apply only to the specific route:

```json5
routes: [
  {
    match: { path: "/api/*" },
    plugins: ["auth"],
    action: "api",
  }
]
```

### Service-Level

Plugins apply to **all routes** in the service:

```json5
services: {
  web: {
    listen: ":443",
    plugins: ["auth"],
    routes: [
      { match: { path: "/api/*" }, action: "api" },
      { match: { path: "/ws" }, action: "proxy" },  // also gets "auth"
    ]
  }
}
```

### Action-Level

Plugins apply to **all routes** that reference the action:

```json5
actions: {
  api: {
    type: "proxy",
    upstream: "localhost:3000",
    plugins: ["ratelimit"],
  }
}
```

### Merge Order

Plugins from all levels are merged per route in this order: **service → action → route**. Duplicates are removed (first occurrence wins). The effective list determines execution order — service-level plugins run first, then action-level, then route-level.

### Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `plugins` | `[]string` | — | Plugin aliases or literal paths. Available on routes, services, and actions. |
| `plugin_timeout` | `duration` | `5s` | Per-request timeout for plugin hook calls. Route-level only. |
| `autostart` | `bool` | `false` | Start plugin at proxy startup without route bindings. |

### Rules

- Plugin paths accept `.go` source files or Go package directories — compile with `prox build` before starting the server.
- A `balancer` is required only when using target discovery (not for auth-only plugins).
- Multiple plugins attach to a single route with sequential execution; deny verdicts short-circuit the chain.
- Plugins with `autostart: true` spawn at startup without route bindings — suitable for background routines, metrics, and health monitors.

## Lifecycle

```
1. prox starts → spawns plugin process
2. prox sends "configure" for each bound route (or empty route for autostart plugins)
3. Plugin optionally sends "ready" with socket path and hooks
4. prox connects to the Unix socket (connection pool)
5. Plugin pushes "set_targets" whenever data changes
6. For each request: prox calls hooks over socket, plugin responds
7. On config reload → prox sends new "configure"
8. On prox shutdown → stdin closes → plugin exits
```

### Crash Recovery

If a plugin process exits unexpectedly:

1. Targets **freeze** at the last known state.
2. Request-response hooks **fail open** (requests pass through).
3. prox restarts the plugin with **exponential backoff** (1s → 2s → 4s → … → 30s max).
4. After restart, prox re-sends `configure` for all bound routes.
5. The backoff resets after a successful message.

### Stderr

Plugin stderr is forwarded to the prox logger at `debug` level, tagged with the plugin alias from config:

```
05:28:48 DBG [auth] 2026/05/28 05:28:48 token validated for user-abc123
```

The tag resolves from the `plugins` registry key. For raw path references (not registered in `plugins`), the binary basename is used.

## Performance

| Operation | Overhead |
|-----------|----------|
| Request routing (no hooks) | **None** — unchanged hot path |
| `on_request` hook | ~50–100μs per call (Unix socket + msgpack) |
| `on_response` hook | ~50–100μs per call |
| `on_connect` hook (L4) | ~50–100μs per call |
| `set_targets` push | O(1) atomic pointer store |
| Plugin crash | Targets freeze, hooks fail open, auto-restart |
