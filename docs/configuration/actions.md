# Actions

Actions define the behavior executed when a route matches. Actions can be referenced by name or [inlined](routing.md#inline-actions) directly in a route.

## `proxy` — Reverse Proxy

| Field      | Type   | Required | Description                                              |
| ---------- | ------ | -------- | -------------------------------------------------------- |
| `type`     | string | ✓        | `"proxy"`                                                |
| `upstream` | string | ✓        | `"host:port"`, `"http://host:port"`, or template         |
| `timeout`  | string |          | `"5s"`, `"30s"`, `"1m"`                                  |
| `headers`  | object |          | Extra headers to send to upstream                        |
| `fallback` | string |          | Named action to invoke when the primary action fails     |
| `proto`    | string |          | Upstream protocol: `"h2"` for HTTP/2 cleartext (h2c)     |
| `stream`   | bool   |          | Use raw HTTP/1.1 tunnel for bidirectional streaming       |

The upstream field supports template placeholders: `{target}` (from the route's [balancer](load-balancing.md)) and any key from the route's `set` field. For example, `"{target}:{port}"` resolves both the balancer target and a route-level variable.

The `fallback` action is invoked when no balancer target is available or the upstream is unreachable, enabling graceful degradation without returning 502.

### Upstream protocol (`proto`)

Controls the HTTP protocol version used to communicate with the upstream server.

| Value | Description |
| ----- | ----------- |
| _(empty)_ | HTTP/1.1 (default) |
| `"h2"` | HTTP/2 cleartext (h2c) — HTTP/2 over plain TCP, no TLS |

HTTP/2 enables full-duplex streaming: the client can upload data while simultaneously receiving a response. This is required for protocols that use long-lived POST/GET pairs for bidirectional communication.

!!! tip
    Combine `proto: "h2"` with a service-level [`config`](index.md#service-config) that increases timeouts and enables immediate flushing for streaming workloads.

```json5
// HTTP/2 upstream — full-duplex streaming support.
{
  match: { domain: "*.**" },
  action: {
    type: "proxy",
    upstream: "localhost:3501",
    proto: "h2",
  },
}
```

### Streaming mode (`stream`)

When `stream` is `true`, the proxy bypasses `httputil.ReverseProxy` and establishes a raw HTTP/1.1 tunnel. The request is forwarded over a raw TCP connection with the body streamed in a background goroutine, and the response is flushed immediately.

!!! note
    Prefer `proto: "h2"` for bidirectional streaming when the upstream supports HTTP/2. Use `stream: true` only for HTTP/1.1 upstreams that require raw tunnel behavior.

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

### Headers

Headers are injected into every request forwarded to the upstream. This applies to custom `Host` headers, authentication tokens, or any other headers required by the upstream.

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

### WebSocket support

WebSocket connections are detected and handled automatically — no additional configuration is required. When a client sends an `Upgrade: websocket` request, prox:

1. Dials the upstream directly via TCP
2. Forwards the full HTTP upgrade handshake (including all configured `headers`)
3. Establishes a bidirectional tunnel after the `101 Switching Protocols` response
4. Relays raw bytes transparently until either side closes

This is compatible with any WebSocket library or protocol (RFC 6455). The proxy does not interpret WebSocket frames — raw bytes are relayed transparently. The `timeout` setting applies to the initial upstream dial.

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

If the upstream rejects the upgrade (e.g., returns 403), the rejection response is forwarded to the client as-is.

!!! note
    On TLS services, HTTP/2 is enabled by default. For WebSocket support on a TLS service, set [`h2: false`](index.md#h2--http2-on-tls-listeners) to force HTTP/1.1 — Go's HTTP/2 strips the `Connection` and `Upgrade` headers required for WebSocket detection.

## `static` — Static Response

| Field      | Type            | Required | Description                                                     |
| ---------- | --------------- | -------- | --------------------------------------------------------------- |
| `type`     | string          | ✓        | `"static"`                                                      |
| `status`   | int             | ✓        | HTTP status code                                                |
| `headers`  | object          |          | Response headers                                                |
| `body_ref` | string / object |          | Ref to resource or inline `{ text: "..." }` / `{ json: {...} }` |

### Template variables

Static response bodies support `{variable}` placeholders that are interpolated at request time:

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

For multiple `*` wildcards, captured values are joined with `.` — e.g., pattern `*.*.example.com` matching `a.b.example.com` produces `{match.domain}` = `a.b`. The `**` glob suffix is captured separately into `{match.glob}` — e.g., pattern `*.storage.**` matching `cdn.storage.example.com` produces `{match.domain}` = `cdn` and `{match.glob}` = `example.com`.

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

Bodies without `{` are served as-is with no interpolation overhead.

## `serve` — File Server

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

## `pass` — L4 TCP Pass-through

Relays raw TCP connections to an upstream without TLS termination. The proxy peeks the TLS ClientHello to extract the SNI hostname for routing, then forwards all bytes (including the ClientHello) to the upstream. The upstream handles TLS directly.

| Field      | Type   | Required | Description                      |
| ---------- | ------ | -------- | -------------------------------- |
| `type`     | string | ✓        | `"pass"`                         |
| `upstream` | string | ✓        | `"host:port"` — TCP dial address |

**Constraints:**

- `pass` routes **must** have a `domain` pattern (SNI matching)
- `pass` routes **cannot** use `path` or `methods` (these are HTTP-level concepts unavailable before TLS termination)

See [L4 Dispatching](../deployment.md#l4-dispatching) for details on how pass routes interact with L7 routes.

## `drop` — Drop Connection

Silently closes the connection without sending a response. Useful as a catch-all to reject unknown domains or unwanted traffic.

| Field  | Type   | Required | Description |
| ------ | ------ | -------- | ----------- |
| `type` | string | ✓        | `"drop"`    |

At L7 (HTTP), the TCP connection is hijacked and closed immediately — no HTTP response is sent. At L4 (when combined with `pass` routes), the raw TCP connection is closed before the TLS handshake.

```json5
// Reject all unmatched domains.
{ action: { type: "drop" } }
```

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

Inline resources follow the same format:

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
