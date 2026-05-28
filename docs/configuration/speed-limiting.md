# Speed Limiting

Per-route bandwidth throttling with independent download and upload limits. Each route can cap throughput from upstream to client (download) and from client to upstream (upload) separately.

## Configuration

Add a `speed` block to any route:

```json5
{
  match: { path: "/api/*" },
  speed: {
    download_mbps: 50,
    upload_mbps: 10,
  },
  action: { type: "proxy", upstream: "localhost:3000" },
}
```

| Field           | Type   | Default | Description                                                       |
| --------------- | ------ | ------- | ----------------------------------------------------------------- |
| `download_mbps` | number | `0`     | Upstream → client bandwidth cap in Mbps. `0` = unlimited          |
| `upload_mbps`   | number | `0`     | Client → upstream bandwidth cap in Mbps. `0` = unlimited          |
| `shared`        | bool   | `false` | When true, all connections on the route share the bandwidth budget |

At least one of `download_mbps` or `upload_mbps` must be greater than zero.

## Per-Connection Mode

The default mode. Each connection receives its own independent bandwidth budget — two simultaneous downloads each get the full configured rate.

```json5
{
  match: { path: "/api/*" },
  speed: { download_mbps: 50, upload_mbps: 10 },
  action: { type: "proxy", upstream: "localhost:3000" },
}
```

!!! tip
    Fractional values are supported — use `0.5` for 500 Kbps.

## Shared Mode

All connections on the route share a single bandwidth bucket. This caps the total bandwidth consumed by a backend regardless of the number of connected clients.

```json5
{
  match: { path: "/downloads/*" },
  speed: { download_mbps: 100, shared: true },
  action: { type: "proxy", upstream: "localhost:3000" },
}
```

!!! note
    In shared mode, individual connections are not capped — they compete for the shared budget.

## Plugin-Driven Speed Limits

[Plugins](../plugins/index.md) can set speed limits dynamically via the push API or per-connection in the `on_request` hook.

### Push API

Set route-wide defaults that apply to all new connections:

```go
// By route ID:
p.SetSpeedLimit("web:0", sdk.SpeedLimit{DownloadMbps: 50, UploadMbps: 10})

// By action — targets all routes using the given action:
p.SetActionSpeedLimit("proxy", sdk.SpeedLimit{DownloadMbps: 100})
```

### Per-Connection

Set limits for a single connection in the `on_request` hook:

```go
p.OnRequest(func(req *sdk.Request) *sdk.Response {
    return sdk.Allow(sdk.WithSpeedLimit(10, 5)) // 10 Mbps down, 5 Mbps up
})
```

### Group Speed Limiting

By default, plugin speed limits are per-connection — each connection gets an independent bandwidth budget. With `GroupKey`, all connections sharing the same key share a single bandwidth pool.

This addresses the multi-connection bypass problem: clients that open multiple parallel connections per session effectively multiply their bandwidth cap. With grouping, total throughput across all connections stays within the configured limit.

```go
p.OnRequest(func(req *sdk.Request) *sdk.Response {
    userID := authenticate(req)
    // All connections from the same user share one 50 Mbps budget.
    return sdk.Allow(sdk.WithSpeedLimit(50, 50, userID))
})
```

- When `GroupKey` is empty — per-connection limiting (backward compatible)
- When `GroupKey` is set — all connections with the same key share a single bandwidth pool
- Group buckets are cleaned up automatically when all connections in the group close

## Limit Resolution

When multiple sources set speed limits (config, plugin push, plugin response), the **most restrictive** (lowest non-zero) value wins per direction.

| Source                            | Scope                                    | Set When         |
| --------------------------------- | ---------------------------------------- | ---------------- |
| Config `speed`                    | Route-level                              | Startup / reload |
| Plugin push                       | Route-level                              | Any time         |
| Plugin response                   | Single connection                        | Per-request      |
| Plugin response (grouped)         | All connections with same GroupKey        | Per-request      |

!!! note
    A zero value from any source means "no limit from this source" — it does not override limits set by other sources.

## Protocol Support

Speed limiting operates transparently across all proxy modes:

- **HTTP** — throttles response body (download) and request body (upload)
- **WebSocket** — throttles both directions of the bidirectional relay
- **gRPC** — throttles the HTTP/2 data stream
- **Streaming (SSE)** — throttles chunked response data

!!! tip
    Speed limiting adds zero overhead on routes without `speed` configuration — no extra allocations, no wrapper objects, no context values.
