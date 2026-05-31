# Configuration

prox uses [JSON5](https://json5.org) for configuration — a superset of JSON that supports comments, trailing commas, and unquoted keys.

## Structure

Every configuration file contains up to four top-level sections:

```
┌───────────────────────────────────────────────────────────────┐
│                          config.json5                         │
├──────────────┬──────────────┬───────────────┬─────────────────┤
│   services   │   plugins    │    actions    │    resources    │
│   (WHAT)     │(MIDDLEWARE)  │    (HOW)      │   (WITH WHAT)   │
│              │              │               │                 │
│  listen addr │  name        │  type: proxy  │  inline text    │
│  tls on/off  │   └ path     │  type: static │                 │
│  routes[]    │   └ autostart│  type: serve  │                 │
│   └ match    │              │  timeout      │                 │
│   └ plugins ─│──► ref       │               │                 │
│   └ speed    │              │               │                 │
│   └ action ──│──────────────│──► ref ───────│──► body_ref     │
└──────────────┴──────────────┴───────────────┴─────────────────┘
```

Configuration is reference-based. Routes point to actions by name, and actions point to resources by name. Both actions and resources may also be inlined directly when a definition is not reused.

## Services

A service defines a listener with routing rules. Services can be defined inline or loaded from external files.

### Inline

```json5
{
  services: {
    my_site: {
      listen: ":8080",         // required
      tls: true,               // optional, default: false
      tls_cert: "/path/cert",  // required if tls: true (unless acme is set)
      tls_key: "/path/key",    // required if tls: true (single-file mode)
      acme: {},                // optional, automatic certificates via ACME
      h2: false,               // optional, default: true. Set false for WebSocket
      config: {},              // optional, per-service tuning
      routes: [...]            // required, at least one
    },
  },
}
```

### `h2` — HTTP/2 on TLS listeners

Controls whether the TLS listener advertises HTTP/2 via ALPN negotiation.

| Value   | Description                                         |
| ------- | --------------------------------------------------- |
| `true`  | HTTP/2 enabled (default when TLS is on)             |
| `false` | HTTP/1.1 only — required for WebSocket support      |

Go's HTTP/2 implementation strips `Connection` and `Upgrade` hop-by-hop headers from incoming requests, preventing WebSocket upgrade detection. Set `h2: false` on any TLS service that handles WebSocket connections.

!!! note
    The `h2` option controls the **client-facing** listener protocol. The **upstream** protocol is controlled separately by the [`proto`](actions.md#upstream-protocol-proto) field in the action configuration.

This setting has no effect on non-TLS services (HTTP/2 requires TLS for ALPN negotiation).

For manual certificate setup, automatic ACME certificates, and advanced TLS options, see [TLS & Certificates](../tls.md).

### Service config

The optional `config` block tunes HTTP server timeouts and proxy transport behavior for a specific service. All values fall back to built-in defaults when omitted.

Durations accept strings (`"5s"`, `"1m30s"`, `"5m"`) or numbers (interpreted as seconds).

| Field                      | Default | Description                                                                 |
| -------------------------- | ------- | --------------------------------------------------------------------------- |
| `read_timeout`             | `10s`   | Maximum time to read the full request (headers + body)                      |
| `write_timeout`            | `30s`   | Maximum time to write the full response                                     |
| `idle_timeout`             | `120s`  | Keep-alive idle timeout before closing the connection                       |
| `response_header_timeout`  | `30s`   | Maximum time to wait for upstream response headers                          |
| `flush_interval`           | `0`     | How often to flush buffered proxy response data. `-1` = flush immediately   |
| `dial_timeout`             | action  | TCP dial timeout (defaults to the action's `timeout`)                       |
| `keep_alive`               | `30s`   | TCP keep-alive interval                                                     |
| `max_idle_conns`           | `4096`  | Maximum idle connections in the connection pool                              |
| `max_idle_conns_per_host`  | `4096`  | Maximum idle connections per upstream host                                   |
| `read_buffer_size`         | `32768` | Read buffer size in bytes for proxy transport (32 KB)                       |
| `write_buffer_size`        | `32768` | Write buffer size in bytes for proxy transport (32 KB)                      |
| `tls_handshake_timeout`    | `10s`   | TLS handshake deadline for HTTPS upstreams                                  |
| `h2_read_idle_timeout`     | `30s`   | HTTP/2: send ping after this idle period                                    |
| `h2_ping_timeout`          | `15s`   | HTTP/2: deadline for ping response                                          |
| `max_connections`          | `0`     | Maximum concurrent connections. `0` = unlimited                             |

!!! tip
    For streaming protocols (SSE, long-lived HTTP, long-polling), set `flush_interval` to `-1` and increase `read_timeout`, `write_timeout`, and `response_header_timeout` to accommodate long-lived connections.

```json5
// Streaming-friendly gateway — long timeouts, immediate flushing.
{
  services: {
    gateway: {
      listen: ":443",
      tls: true,
      tls_cert: "/etc/prox/certs/",
      config: {
        read_timeout: "5m",
        write_timeout: "5m",
        response_header_timeout: "5m",
        flush_interval: -1,
      },
      routes: [...]
    },
  },
}
```

### File reference

A string value loads the service definition from an external JSON5 file:

```json5
{
  services: {
    web: "./web.json5", // load ServiceFragment from file
    api: "./api.json5",
  },
}
```

Relative paths are resolved from the **directory of the parent config file**.

### Directory reference

A string pointing to a directory loads all `.json5` files from it. Each file becomes a service named after its filename (without extension):

```json5
{
  services: {
    _microservices: "./services/",
    //  services/web.json5 → service "web"
    //  services/api.json5 → service "api"
  },
}
```

Non-`.json5` files and subdirectories are ignored.

### Directory mode (CLI)

A directory path may also be passed directly to `-config`:

```bash
prox serve -config ./config/
```

Every `.json5` file in the directory is treated as a service fragment. No root config file is required.

## Service Fragments

A service fragment is the file format for external service definitions. It contains a service definition with optional local `actions` and `resources`:

```json5
// web.json5
{
  listen: ":8080",
  routes: [
    { match: { path: "/health" }, action: "health" },
    { match: { path: "/*" }, action: "frontend" },
  ],

  // Local actions — merged into the global pool.
  actions: {
    frontend: { type: "serve", root: "./public" },
  },

  // Local resources — merged into the global pool.
  resources: {
    banner: { text: "Welcome!" },
  },
}
```

**Merge rules:**

- Actions and resources from fragments are merged into the global pool alongside definitions from the root config.
- Duplicate names across any files produce a **validation error** — rename to avoid collisions.
- Global actions from the root config are accessible to all services (e.g., a shared `health` action).
- Fragments cannot reference other files — only one level of nesting is supported.

## Validation

Validate configuration before deploying:

```bash
prox validate -config config.json5
prox validate -config ./config/      # works with directories too
```

The validator checks:

- All action references resolve
- All resource references resolve
- Required fields are present
- HTTP methods are valid
- Path patterns are well-formed
- Domain patterns are well-formed (at most one `*` per segment, at least 2 segments, `**` only at end)
- At least one of `path` or `domain` in each route (or omit `match` for catch-all)
- TLS cert/key are provided when TLS is enabled
- `pass` routes require a `domain` (SNI matching) and cannot use `path` or `methods`
- `pass` actions require an `upstream`
- Balancer type is valid (`roundrobin`, `random`, or `leastconn`)
- Balancer targets are non-empty and unique (empty allowed with plugins)
- Balancer is only used with `proxy` or `pass` actions
- Action upstream contains `{target}` when a balancer is used
- Speed config values are non-negative, at least one direction > 0
- Plugin paths are non-empty
- No duplicate action/resource names across files
- No circular file references
- Reports **all** issues at once
