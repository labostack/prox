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
| Hybrid | All | Both | Full middleware + discovery |

## Configuration

Add a `plugins` array to any route:

```json5
{
  match: { domain: "*.example.com", path: "/api/*" },
  plugins: ["./plugins/auth.go"],
  plugin_timeout: "2s",   // optional (default: 5s)
  action: { type: "proxy", upstream: "localhost:3000" },
}
```

### Fields

- `plugins` — list of plugin paths (binary, `.go` source, or Go package directory)
- `plugin_timeout` — per-request timeout for plugin hook calls (default: 5s)

### Rules

- Plugin paths are resolved relative to the config file's directory
- Absolute paths are used as-is
- **`.go` source files are compiled automatically** — no manual build step needed
- Pre-compiled binaries are used as-is (must be executable)
- A `balancer` is required only when using target discovery (not for auth-only plugins)
- Multiple plugins can be attached to a single route (sequential execution, short-circuit on deny)

### Auto-compilation

Prox can compile plugin sources automatically — no manual build step needed.

**Single file** — path ends in `.go`:

```
plugins: ["./plugins/auth.go"]  →  go build -o ./plugins/auth
```

**Directory** — path points to a Go package directory:

```
plugins: ["./plugins/auth/"]  →  go build -o ./plugins/auth
```

Compiled binaries are placed next to the source. Rebuilds are **skipped** if the binary is newer than the source (mtime check).

## Lifecycle

```
1. prox starts → spawns plugin process
2. prox sends "configure" for each bound route
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

Plugin stderr is forwarded to prox's logger at `debug` level. Use it for diagnostics.

## Performance

| Operation | Overhead |
|---|---|
| Request routing (no hooks) | **None** — unchanged hot path |
| `on_request` hook | ~50-100μs per call (Unix socket + msgpack) |
| `on_response` hook | ~50-100μs per call |
| `on_connect` hook (L4) | ~50-100μs per call |
| `set_targets` push | O(1) atomic pointer store |
| Plugin crash | Targets freeze, hooks fail open, auto-restart |
