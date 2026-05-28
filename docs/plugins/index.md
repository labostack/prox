# Plugins

Plugins are external executables that extend prox at runtime. They can dynamically manage balancer targets, authorize HTTP requests, modify upstream responses, and gate L4 TCP connections.

## Overview

- Plugins communicate with prox over **stdin/stdout** (JSON) for lifecycle events and **Unix sockets** (msgpack) for request-response hooks
- Each plugin runs as a **child process**, automatically restarted on crash
- The [Go SDK](sdk.md) handles all transport details — plugin authors just register callbacks
- Target updates are applied **atomically** — zero lock contention on the data plane

## Modes

| Mode | Hooks | Transport | Use Case |
|------|-------|-----------|----------|
| Push-only | `OnConfigure` | stdin/stdout | Target discovery, DNS resolver |
| Request-response | `OnRequest`, `OnResponse`, `OnConnect` | Unix socket (msgpack) | Auth, rate limiting, header injection |
| Fire-and-forget | `OnDisconnect` | Unix socket (msgpack) | Connection statistics, usage tracking |
| Hybrid | All | Both | Full middleware + discovery |

## Configuration

Define your plugins in the global `plugins` block. Attach them at three levels:

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

Plugins from all levels are merged per route: **service → action → route**. Duplicates are removed (first occurrence wins). The effective list determines execution order — service-level plugins run first, then action-level, then route-level.

### Fields

- `plugins` — list of plugin aliases (or literal paths) — available on route, service, and action
- `plugin_timeout` — per-request timeout for plugin hook calls (default: 5s) — route-level only
- `autostart` — start plugin at proxy startup without route bindings (default: false)

### Rules

- Define `plugins` globally to map easy-to-read aliases to absolute or relative paths
- Plugin paths are resolved relative to the config file's directory where they are defined
- You can still define a raw path string (e.g. `"./plugins/auth.go"`) directly in a `plugins` array
- **`.go` source files are compiled automatically** — no manual build step needed
- Pre-compiled binaries are used as-is (must be executable)
- A `balancer` is required only when using target discovery (not for auth-only plugins)
- Multiple plugins can be attached to a single route (sequential execution, short-circuit on deny)
- Plugins with `autostart: true` are spawned at startup without route bindings — useful for background routines, metrics, health monitors

### Auto-compilation

Prox can compile plugin sources automatically — no manual build step needed.

**Single file** — path ends in `.go`:

```json5
  plugins: {
    auth: { path: "./plugins/auth.go" } // → go build -o ./plugins/auth
  }
```

**Directory** — path points to a Go package directory:

```json5
  plugins: {
    auth: { path: "./plugins/auth/" } // → go build -o ./plugins/auth
  }
```

Compiled binaries are placed next to the source. Rebuilds are **skipped** if the binary is newer than the source (mtime check).

### Dependency Resolution

If a plugin compilation fails and a `go.mod` file is present in its directory, prox will automatically run `go mod tidy` to resolve any missing third-party dependencies (such as the `github.com/dortanes/prox/sdk`), update the `go.sum` file, and retry the build. This ensures seamless support for Go Workspaces (`go.work`) during local development while guaranteeing flawless automated builds in clean, containerized, or production environments.

## Lifecycle

```
1. prox starts → spawns plugin process
2. prox sends "configure" for each bound route (or empty route for autostart plugins)
3. plugin optionally sends "ready" with socket path and hooks
4. prox connects to the Unix socket (connection pool)
5. plugin pushes "set_targets" whenever data changes
6. for each request: prox calls hooks over socket, plugin responds
7. on config reload → prox sends new "configure"
8. on prox shutdown → stdin is closed → plugin should exit
```

### Crash Recovery

If a plugin process exits unexpectedly:

1. Targets **freeze** at the last known state
2. Request-response hooks **fail open** (requests are allowed through)
3. Prox restarts the plugin with **exponential backoff** (1s → 2s → 4s → ... → 30s max)
4. After restart, prox re-sends `configure` for all bound routes
5. The backoff resets after a successful message

### Stderr

Plugin stderr is forwarded to prox's logger at `debug` level, tagged with the plugin's alias from config:

```
05:28:48 DBG [auth] 2026/05/28 05:28:48 token validated for user-abc123
```

The tag name is resolved from the `plugins` registry key. For raw path references (not registered in `plugins`), the binary basename is used.

## Performance

| Operation | Overhead |
|---|---|
| Request routing (no hooks) | **None** — unchanged hot path |
| `on_request` hook | ~50-100μs per call (Unix socket + msgpack) |
| `on_response` hook | ~50-100μs per call |
| `on_connect` hook (L4) | ~50-100μs per call |
| `set_targets` push | O(1) atomic pointer store |
| Plugin crash | Targets freeze, hooks fail open, auto-restart |
