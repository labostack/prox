# Routing

Routes are evaluated in declaration order — the first match wins.

```json5
{
  match: {
    domain: "*.example.com", // optional, segment glob
    path: "/api/*", // optional if domain set
    methods: ["GET", "POST"], // optional, empty = all
  },
  set: { port: "8080" }, // optional, route-level variables for templates
  speed: { download_mbps: 50 }, // optional, bandwidth throttling
  action: "proxy_to_backend", // string ref to actions map
}
```

Route-level variables defined in `set` are available as `{key}` placeholders in the action's upstream template. This allows multiple routes to share a single action with different parameters.

At least one of `domain` or `path` must be specified. Omit `match` entirely for a **catch-all** route:

```json5
// Catch-all — matches any request not handled by previous routes.
{ action: { type: "drop" } }
```

## Domain Matching

Domain patterns use segment-based glob matching:

- `*` matches **exactly one** domain label (like wildcard SSL certificates)
- `cdn-*`, `*-prod` — partial wildcards match a label with a fixed prefix or suffix
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

Domain matching is **case-insensitive**. Ports are stripped automatically (`example.com:443` → `example.com`).

Domain patterns are also used for [L4 dispatching](../deployment.md#l4-dispatching) — the SNI hostname from the TLS ClientHello is matched against the same patterns.

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

## Inline Actions

Instead of referencing a named action, an action definition may be inlined directly:

```json5
{
  match: { path: "/health" },
  action: {
    type: "static",
    status: 200,
    body_ref: { text: "OK" }, // inline resource
  },
}
```

## Route Includes

Routes can be loaded from external files. A string path in the `routes` array references an external file whose routes are spliced in place, preserving order.

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

**Bare array** — routes only:

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

Inline routes and includes may be mixed freely:

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
