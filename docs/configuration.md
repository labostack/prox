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
      routes: [...]            // required, at least one
    },
  },
}
```

### File reference

A string value loads the service from an external JSON5 file:

```json5
{
  services: {
    web: './web.json5',      // load ServiceFragment from file
    api: './api.json5',
  },
}
```

Relative paths are resolved from the **directory of the parent config file**.

### Directory reference

A string pointing to a directory loads all `.json5` files from it. Each file becomes a service named after its filename (without extension):

```json5
{
  services: {
    _microservices: './services/',
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
    domain: "*.example.com",       // optional, segment glob
    path: "/api/*",                // optional if domain set
    methods: ["GET", "POST"],      // optional, empty = all
  },
  action: "proxy_to_backend",      // string ref to actions map
}
```

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

| Pattern | Matches | Does not match |
|---|---|---|
| `example.com` | `example.com` | `sub.example.com` |
| `*.example.com` | `sub.example.com` | `example.com`, `a.b.example.com` |
| `*.test.example.com` | `api.test.example.com` | `test.example.com` |
| `test.*.example.com` | `test.staging.example.com` | `test.example.com` |
| `*.*.example.com` | `a.b.example.com` | `a.example.com`, `a.b.c.example.com` |
| `cdn-*.example.com` | `cdn-us.example.com`, `cdn-eu.example.com` | `cdn.example.com`, `web-us.example.com` |
| `*-prod.example.com` | `api-prod.example.com` | `api-staging.example.com` |
| `*.storage.**` | `cdn.storage.example.com`, `cdn.storage.a.b.c` | `storage.example.com`, `cdn.storage` |
| `cdn-*.**` | `cdn-us.example.com`, `cdn-eu.myapp.dev` | `cdn.example.com` |

Domain matching is **case-insensitive** and ports are stripped automatically (`example.com:443` → `example.com`).

Domain patterns are also used for [L4 dispatching](#l4-dispatching-pass-routes) — the SNI hostname from the TLS ClientHello is matched against the same patterns.

```json5
// Virtual hosting — one listener, multiple domains.
{
  services: {
    gateway: {
      listen: ":443",
      tls: true, tls_cert: "cert.pem", tls_key: "key.pem",
      routes: [
        { match: { domain: "api.example.com", path: "/v1/*" }, action: "api" },
        { match: { domain: "*.cdn.example.com" },              action: "cdn" },
        { match: { domain: "*.example.com", path: "/*" },      action: "site" },
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

## Actions

### `proxy` — Reverse Proxy

| Field      | Type   | Required | Description                           |
| ---------- | ------ | -------- | ------------------------------------- |
| `type`     | string | ✓        | `"proxy"`                             |
| `upstream` | string | ✓        | `"host:port"` or `"http://host:port"` |
| `timeout`  | string |          | `"5s"`, `"30s"`, `"1m"`               |
| `headers`  | object |          | Extra headers to send to upstream     |

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

### `static` — Static Response

| Field      | Type            | Required | Description                                 |
| ---------- | --------------- | -------- | ------------------------------------------- |
| `type`     | string          | ✓        | `"static"`                                  |
| `status`   | int             | ✓        | HTTP status code                            |
| `headers`  | object          |          | Response headers                            |
| `body_ref` | string / object |          | Ref to resource or inline `{ text: "..." }` / `{ json: {...} }` |

#### Template variables

Static response bodies can contain `{variable}` placeholders that are interpolated at request time:

| Variable | Description | Example |
|---|---|---|
| `{domain}` | Actual request host (no port) | `sub.example.com` |
| `{domain.pattern}` | Domain pattern from config | `*.example.com` |
| `{match.domain}` | Captured `*` wildcard value(s) | `sub` |
| `{match.glob}` | Captured `**` glob suffix | `example.com` |
| `{path}` | Actual request path | `/api/users` |
| `{match.path}` | Path pattern from config | `/api/*` |
| `{method}` | HTTP method | `GET` |
| `{host}` | Full Host header (with port) | `sub.example.com:443` |

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

| Field      | Type   | Required | Description                           |
| ---------- | ------ | -------- | ------------------------------------- |
| `type`     | string | ✓        | `"pass"`                              |
| `upstream` | string | ✓        | `"host:port"` — TCP dial address      |

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

| Field  | Type   | Description |
|--------|--------|-------------|
| `text` | string | Raw text content |
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
- No duplicate action/resource names across files
- No circular file references
- Reports **all** issues at once
