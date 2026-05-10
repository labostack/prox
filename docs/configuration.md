# Configuration

prox uses [JSON5](https://json5.org) for configuration — a superset of JSON with comments, trailing commas, and unquoted keys.

## Structure

Every config has three sections:

```
┌──────────────────────────────────────────────────────┐
│                    config.json5                      │
├──────────────┬──────────────┬────────────────────────┤
│   services   │   actions    │      resources         │
│   (WHAT)     │   (HOW)      │      (WITH WHAT)       │
│              │              │                        │
│  listen addr │  type: proxy │  inline text           │
│  tls on/off  │  type: static│                        │
│  routes[]    │  type: serve │                        │
│   └ match    │  timeout     │                        │
│   └ action ──│──► ref ──────│──► body_ref → resource │
└──────────────┴──────────────┴────────────────────────┘
```

**Key concept:** everything is reference-based. Routes point to actions by name, actions point to resources by name. But you can also inline them directly when a definition isn't reused.

## Services

A service is a listener with routing rules. Services can be defined inline or loaded from external files.

### Inline

```json5
{
  services: {
    my_site: {
      listen: ":8080",         // required
      tls: true,               // optional, default: false
      tls_cert: "/path/cert",  // required if tls: true
      tls_key: "/path/key",    // required if tls: true
      config: {},              // optional, per-service tuning
      routes: [...]            // required, at least one
    },
  },
}
```

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

> [!TIP]
> For streaming protocols (SSE, long-lived HTTP, long-polling), set `flush_interval` to `-1` and increase `read_timeout`, `write_timeout`, and `response_header_timeout` to accommodate long-lived connections.

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

A string value loads the service from an external JSON5 file:

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

You can also pass a directory directly to `-config`:

```bash
prox serve -config ./config/
```

Every `.json5` file in the directory is treated as a service fragment. No root config file is needed.

## Service Fragments

A service fragment is the file format for external service definitions. It's a service definition with optional local `actions` and `resources`:

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
- Duplicate names across any files are a **validation error** — rename to avoid collisions.
- Global actions from the root config are accessible by all services (e.g. a shared `health` action).
- Fragments cannot reference other files — only one level of nesting.

## Routes

Routes are evaluated in order — first match wins.

```json5
{
  match: {
    domain: "*.example.com", // optional, segment glob
    path: "/api/*", // optional if domain set
    methods: ["GET", "POST"], // optional, empty = all
  },
  set: { port: "8080" }, // optional, route-level variables for templates
  action: "proxy_to_backend", // string ref to actions map
}
```

Route-level variables defined in `set` are available as `{key}` placeholders in the action's upstream template. This allows multiple routes to share a single action with different parameters.

At least one of `domain` or `path` must be specified. Omit `match` entirely for a **catch-all** route:

```json5
// Catch-all — matches any request not handled by previous routes.
{ action: { type: "drop" } }
```

### Domain matching

Domain patterns use segment-based glob matching:

- `*` matches **exactly one** domain label (like wildcard SSL certificates)
- `cdn-*`, `*-prod` — partial wildcards match a label with a fixed prefix/suffix
- `**` matches **one or more** domain labels (only valid as the last segment)

| Pattern              | Matches                                        | Does not match                          |
| -------------------- | ---------------------------------------------- | --------------------------------------- |
| `example.com`        | `example.com`                                  | `sub.example.com`                       |
| `*.example.com`      | `sub.example.com`                              | `example.com`, `a.b.example.com`        |
| `*.test.example.com` | `api.test.example.com`                         | `test.example.com`                      |
| `test.*.example.com` | `test.staging.example.com`                     | `test.example.com`                      |
| `*.*.example.com`    | `a.b.example.com`                              | `a.example.com`, `a.b.c.example.com`    |
| `cdn-*.example.com`  | `cdn-us.example.com`, `cdn-eu.example.com`     | `cdn.example.com`, `web-us.example.com` |
| `*-prod.example.com` | `api-prod.example.com`                         | `api-staging.example.com`               |
| `*.storage.**`       | `cdn.storage.example.com`, `cdn.storage.a.b.c` | `storage.example.com`, `cdn.storage`    |
| `cdn-*.**`           | `cdn-us.example.com`, `cdn-eu.myapp.dev`       | `cdn.example.com`                       |

Domain matching is **case-insensitive** and ports are stripped automatically (`example.com:443` → `example.com`).

Domain patterns are also used for [L4 dispatching](#l4-dispatching-pass-routes) — the SNI hostname from the TLS ClientHello is matched against the same patterns.

```json5
// Virtual hosting — one listener, multiple domains.
{
  services: {
    gateway: {
      listen: ":443",
      tls: true,
      tls_cert: "cert.pem",
      tls_key: "key.pem",
      routes: [
        { match: { domain: "api.example.com", path: "/v1/*" }, action: "api" },
        { match: { domain: "*.cdn.example.com" }, action: "cdn" },
        { match: { domain: "*.example.com", path: "/*" }, action: "site" },
      ],
    },
  },
}
```

### Inline actions

Instead of referencing a named action, you can define one inline:

```json5
{
  match: { path: "/health" },
  action: {
    type: "static",
    status: 200,
    body_ref: { text: "OK" }, // inline resource too!
  },
}
```

### Route includes

Routes can be loaded from external files. Use a string path instead of a route object in the `routes` array — the referenced file's routes are spliced in place, preserving order.

```json5
{
  listen: ":443",
  tls: true,
  tls_cert: "./certs/",
  routes: [
    "./routes/realtime.json5", // routes from file (spliced in order)
    "./routes/fallback.json5", // another include
  ],
}
```

Route include files support two formats:

**Bare array** — just the routes:

```json5
// routes/realtime.json5
[
  {
    match: { domain: "*.**", path: "/ws" },
    action: { type: "proxy", upstream: "localhost:3505" },
  },
  {
    match: { domain: "*.**", path: "/grpc" },
    action: { type: "proxy", upstream: "localhost:3506" },
  },
]
```

**Object wrapper** — routes inside a `routes` key:

```json5
// routes/realtime.json5
{
  routes: [
    {
      match: { domain: "*.**", path: "/ws" },
      action: { type: "proxy", upstream: "localhost:3505" },
    },
  ],
}
```

You can mix inline routes and includes freely:

```json5
{
  listen: ":443",
  routes: [
    "./routes/realtime.json5",
    { match: { path: "/health" }, action: { type: "static", status: 200 } },
    "./routes/fallback.json5",
  ],
}
```

Relative paths are resolved from the **directory of the parent config file**. Included files are tracked by the file watcher — editing a route include triggers a hot reload. Circular references are detected and rejected.

## Load Balancing

Routes can distribute requests across multiple upstream targets using a `balancer`. The balancer selects a target per request (L7) or per connection (L4), and the selected address is available as `{target}` in the action's `upstream` field.

```json5
{
  match: { domain: "*.**", path: "/ws" },
  balancer: {
    type: "roundrobin",
    targets: ["10.0.1.1:3505", "10.0.1.2:3505", "10.0.1.3:3505"],
  },
  action: {
    type: "proxy",
    upstream: "{target}",
  },
}
```

### Balancer config

| Field     | Type     | Required | Description                                                      |
| --------- | -------- | -------- | ---------------------------------------------------------------- |
| `type`    | string   | ✓        | Balancing strategy: `"roundrobin"`, `"random"`, or `"leastconn"` |
| `targets` | string[] | ✓        | List of upstream addresses                                       |

### Balancer types

| Type         | Description                                                           |
| ------------ | --------------------------------------------------------------------- |
| `roundrobin` | Distributes requests evenly in order across targets. Lock-free, O(1). |
| `random`     | Selects a target at random each time.                                 |
| `leastconn`  | Routes to the target with the fewest active connections.              |

### The `{target}` template

The `{target}` placeholder in `upstream` is replaced with the address selected by the balancer. You can use it standalone or embedded in a URL:

```json5
// Bare target — becomes http://10.0.1.1:3505
upstream: "{target}"

// Embedded — becomes http://10.0.1.1:3505/api/v1
upstream: "http://{target}/api/v1"
```

**Constraints:**

- Balancers are only supported with `proxy` and `pass` action types
- The action's `upstream` **must** contain `{target}` when a balancer is used
- Targets must be non-empty and unique within a balancer (unless [plugins](plugins.md) populate them)

### L4 pass-through with balancing

Balancers also work with `pass` routes for L4 TCP load balancing. Each new connection gets a target from the balancer:

```json5
{
  match: { domain: "*.cdn.example.com" },
  balancer: {
    type: "roundrobin",
    targets: ["10.0.0.5:443", "10.0.0.6:443"],
  },
  action: {
    type: "pass",
    upstream: "{target}",
  },
}
```

### WebSocket support

WebSocket connections through a balanced route work transparently — the balancer selects the target before the upgrade handshake, and the entire WebSocket session stays pinned to that target.

### Connection tracking (`leastconn`)

The `leastconn` strategy tracks active connections per target. A connection is counted from the moment the balancer selects it until the request completes (HTTP response sent, WebSocket closed, or TCP relay finished). This works across both L7 proxy routes and L4 pass routes.

```json5
// Route to the server with the fewest active connections.
{
  match: { domain: "*.**", path: "/ws" },
  balancer: {
    type: "leastconn",
    targets: ["10.0.1.1:3505", "10.0.1.2:3505", "10.0.1.3:3505"],
  },
  action: {
    type: "proxy",
    upstream: "{target}",
  },
}
```

### Dynamic targets with plugins

Balancer targets can be populated dynamically by [plugins](plugins.md). Set `targets` to an empty array and attach a plugin that pushes targets at runtime:

```json5
{
  match: { domain: "*.**", path: "/ws" },
  plugins: ["./plugins/resolver"],
  balancer: {
    type: "leastconn",
    targets: [],   // plugin will populate
  },
  action: {
    type: "proxy",
    upstream: "{target}",
  },
}
```

See [docs/plugins.md](plugins.md) for the full plugin protocol and authoring guide.

## Actions

### `proxy` — Reverse Proxy

| Field      | Type   | Required | Description                                              |
| ---------- | ------ | -------- | -------------------------------------------------------- |
| `type`     | string | ✓        | `"proxy"`                                                |
| `upstream` | string | ✓        | `"host:port"`, `"http://host:port"`, or template         |
| `timeout`  | string |          | `"5s"`, `"30s"`, `"1m"`                                  |
| `headers`  | object |          | Extra headers to send to upstream                        |
| `fallback` | string |          | Named action to invoke when the primary action fails     |

The upstream field supports template placeholders: `{target}` (from the route's balancer) and any key from the route's `set` field. For example, `"{target}:{port}"` resolves both the balancer target and a route-level variable.

The `fallback` action is invoked when no balancer target is available or the upstream is unreachable. This enables graceful degradation without returning 502.

```json5
// Route with variables and shared action.
{
  match: { domain: "*.**", path: "/ws" },
  plugins: ["./plugins/resolver"],
  balancer: { type: "leastconn" },
  set: { port: "8080" },
  action: "dynamic_proxy",
},

// Shared action definition.
actions: {
  dynamic_proxy: {
    type: "proxy",
    upstream: "{target}:{port}",
    fallback: "default_backend",
  },
  default_backend: {
    type: "proxy",
    upstream: "https://fallback.internal",
  },
}
```

Headers are injected into every request forwarded to the upstream. This is useful for setting a custom `Host` header, authentication tokens, or any other headers the upstream requires.

```json5
{
  match: { domain: "*.**" },
  action: {
    type: "proxy",
    upstream: "https://backend.internal",
    headers: {
      Host: "public.example.com",
      "X-Forwarded-Proto": "https",
    },
  },
}
```

#### WebSocket support

WebSocket connections are detected and handled automatically — no configuration needed. When a client sends an `Upgrade: websocket` request, prox:

1. Dials the upstream directly via TCP
2. Forwards the full HTTP upgrade handshake (including all configured `headers`)
3. Establishes a bidirectional tunnel after the `101 Switching Protocols` response
4. Relays frames transparently until either side closes

This works with any WebSocket library or protocol (RFC 6455). The `timeout` setting applies to the initial upstream dial.

```json5
// WebSocket-capable proxy — no extra config needed.
{
  match: { domain: "ws.example.com", path: "/ws/*" },
  action: {
    type: "proxy",
    upstream: "localhost:8080",
    timeout: "10s",
  },
}
```

If the upstream rejects the upgrade (e.g. returns 403), the rejection response is forwarded to the client as-is.

### `static` — Static Response

| Field      | Type            | Required | Description                                                     |
| ---------- | --------------- | -------- | --------------------------------------------------------------- |
| `type`     | string          | ✓        | `"static"`                                                      |
| `status`   | int             | ✓        | HTTP status code                                                |
| `headers`  | object          |          | Response headers                                                |
| `body_ref` | string / object |          | Ref to resource or inline `{ text: "..." }` / `{ json: {...} }` |

#### Template variables

Static response bodies can contain `{variable}` placeholders that are interpolated at request time:

| Variable           | Description                    | Example               |
| ------------------ | ------------------------------ | --------------------- |
| `{domain}`         | Actual request host (no port)  | `sub.example.com`     |
| `{domain.pattern}` | Domain pattern from config     | `*.example.com`       |
| `{match.domain}`   | Captured `*` wildcard value(s) | `sub`                 |
| `{match.glob}`     | Captured `**` glob suffix      | `example.com`         |
| `{path}`           | Actual request path            | `/api/users`          |
| `{match.path}`     | Path pattern from config       | `/api/*`              |
| `{method}`         | HTTP method                    | `GET`                 |
| `{host}`           | Full Host header (with port)   | `sub.example.com:443` |

For multiple `*` wildcards, captured values are joined with `.` — e.g. pattern `*.*.example.com` matching `a.b.example.com` gives `{match.domain}` = `a.b`. The `**` glob suffix is captured separately into `{match.glob}` — e.g. pattern `*.storage.**` matching `cdn.storage.example.com` gives `{match.domain}` = `cdn` and `{match.glob}` = `example.com`.

```json5
{
  match: { domain: "test.*.example.com" },
  action: {
    type: "static",
    status: 200,
    headers: { "Content-Type": "text/plain" },
    body_ref: { text: "Env: {match.domain}, full host: {domain}" },
  },
}
// GET http://test.staging.example.com/ → "Env: staging, full host: test.staging.example.com"
```

Bodies without `{` are served as-is with no overhead.

### `serve` — File Server

Serves files from a directory or a single file.

| Field  | Type   | Required | Description                                |
| ------ | ------ | -------- | ------------------------------------------ |
| `type` | string | ✓        | `"serve"`                                  |
| `root` | string | ✗†       | Directory to serve (e.g. `"./public"`)     |
| `file` | string | ✗†       | Single file to serve (e.g. `"./app.html"`) |

† Exactly one of `root` or `file` is required.

**Directory mode** (`root`):

- Automatically serves `index.html` for directory requests
- `GET /` → `root/index.html`
- `GET /css/app.css` → `root/css/app.css`
- Directory listings are disabled (404 if no `index.html`)
- Route prefix is stripped automatically: route `/static/*` with root `./public` maps `/static/app.css` → `./public/app.css`

**File mode** (`file`):

- Always serves the same file regardless of the request path
- Useful for SPA fallbacks

```json5
// Directory serving
{
  match: { path: "/*" },
  action: {
    type: "serve",
    root: "./public",
  },
}

// Single file
{
  match: { path: "/app/*" },
  action: {
    type: "serve",
    file: "./dist/index.html",  // SPA fallback
  },
}
```

### `pass` — L4 TCP Pass-through

Relays raw TCP connections to an upstream without TLS termination. The proxy peeks the TLS ClientHello to extract the SNI hostname for routing, then forwards all bytes (including the ClientHello) to the upstream. The upstream handles TLS directly.

| Field      | Type   | Required | Description                      |
| ---------- | ------ | -------- | -------------------------------- |
| `type`     | string | ✓        | `"pass"`                         |
| `upstream` | string | ✓        | `"host:port"` — TCP dial address |

**Constraints:**

- `pass` routes **must** have a `domain` pattern (SNI matching)
- `pass` routes **cannot** use `path` or `methods` (these are HTTP-level concepts — not available before TLS termination)

### `drop` — Drop Connection

Silently closes the connection without sending any response. Useful as a catch-all to reject unknown domains or unwanted traffic.

| Field  | Type   | Required | Description |
| ------ | ------ | -------- | ----------- |
| `type` | string | ✓        | `"drop"`    |

At L7 (HTTP), the TCP connection is hijacked and closed immediately — no HTTP response is sent. At L4 (when combined with `pass` routes), the raw TCP connection is closed before TLS handshake.

```json5
// Reject all unmatched domains.
{ action: { type: "drop" } }
```

## L4 Dispatching (pass routes)

When a service has any `pass` routes, prox automatically activates an L4 dispatcher. The dispatcher intercepts raw TCP connections **before** TLS termination:

```
Client → :443 TCP
  → Peek SNI from TLS ClientHello (5s timeout)
  → Walk routes in config order
    → First match is "pass" → raw TCP relay to upstream
    → First match is "drop" → close connection (when dispatcher is active)
    → First match is L7     → TLS termination → HTTP routing
    → No match              → TLS termination → HTTP 404
```

> **Note:** `drop` routes work at L7 without the dispatcher (the connection hangs until timeout). When the dispatcher is already active due to `pass` routes, `drop` routes with domain patterns also participate in L4 matching — closing connections before TLS handshake.

**Route order matters.** The dispatcher walks all routes (not just `pass` routes) in config order. The first domain match wins:

```json5
{
  services: {
    gateway: {
      listen: ":443",
      tls: true,
      tls_cert: "/etc/prox/certs/",
      routes: [
        // L4: *.cdn.example.com → raw TCP relay (no TLS termination)
        {
          match: { domain: "*.cdn.example.com" },
          action: { type: "pass", upstream: "10.0.0.5:3504" },
        },

        // L7: everything else on *.example.com → TLS termination + HTTP
        {
          match: { domain: "*.example.com" },
          action: "web_app",
        },
      ],
    },
  },
}
```

Hot reload updates both L4 and L7 routes atomically.

## Resources

Named, reusable content blobs referenced by actions via `body_ref`.

| Field  | Type   | Description                                  |
| ------ | ------ | -------------------------------------------- |
| `text` | string | Raw text content                             |
| `json` | any    | JSON value — auto-marshaled to a JSON string |

Use `text` for plain strings, `json` for structured data (avoids manual escaping).

```json5
{
  resources: {
    greeting: {
      text: "Hello, World!",
    },
    health: {
      json: { status: "ok", version: "1.0" },
    },
  },
}
```

Inline resources work the same way:

```json5
{
  match: { path: "/health" },
  action: {
    type: "static",
    status: 200,
    headers: { "Content-Type": "application/json" },
    body_ref: { json: { status: "ok" } },
  },
}
```

## Validation

Validate before deploying:

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
- Plugins require a balancer on the same route
- Plugin paths are non-empty
- No duplicate action/resource names across files
- No circular file references
- Reports **all** issues at once
