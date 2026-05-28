# Wire Protocol

This is a reference for the plugin wire protocol. Plugins built with the [Go SDK](sdk.md) handle this automatically — this page is for authors implementing plugins in other languages.

## Channels

Plugins use two communication channels:

| Channel | Format | Purpose |
|---------|--------|---------|
| stdin/stdout | Line-delimited JSON | Lifecycle events, target pushes |
| Unix socket | Length-prefixed msgpack | Request-response hooks |

## stdin/stdout — Lifecycle & Pushes

### Prox → Plugin: `configure`

Sent once per bound route after the plugin starts, and again on config reload. For autostart plugins with no route bindings, a single `configure` is sent with an empty `route_id` and no `match`.

```json
{
  "method": "configure",
  "params": {
    "route_id": "gateway:0",
    "match": { "domain": "*.**", "path": "/ws" }
  }
}
```

### Plugin → Prox: `ready`

Sent after `configure` to declare request-response capabilities. This instructs prox to connect to the plugin's Unix socket for hook calls.

```json
{
  "method": "ready",
  "params": {
    "socket": "/tmp/prox-p-12345.sock",
    "hooks": ["on_request", "on_response", "on_connect", "on_disconnect"]
  }
}
```

Valid hook names: `on_request`, `on_response`, `on_connect`, `on_disconnect`.

### Plugin → Prox: `set_targets`

Pushes new targets to route balancers. Sent whenever the target pool changes.

**By route ID:**

```json
{
  "method": "set_targets",
  "params": {
    "route_id": "gateway:0",
    "targets": ["10.0.1.1:3505", "10.0.1.2:3505"]
  }
}
```

**By action name** — updates all routes using the given action:

```json
{
  "method": "set_targets",
  "params": {
    "action": "dynamic_proxy",
    "targets": ["10.0.1.1:3505", "10.0.1.2:3505"]
  }
}
```

**Wildcard** — updates all routes with balancers:

```json
{
  "method": "set_targets",
  "params": {
    "route_id": "*",
    "targets": ["10.0.1.1:3505", "10.0.1.2:3505"]
  }
}
```

**Grouped mode** — compatible with any targeting method above:

```json
{
  "method": "set_targets",
  "params": {
    "action": "dynamic_proxy",
    "groups": {
      "de": ["de-node1.internal:8080", "de-node2.internal:8080"],
      "us": ["us-node1.internal:8080"]
    }
  }
}
```

**Field rules:**

| Field | Type | Description |
|-------|------|-------------|
| `route_id` | `string` | Target a specific route, or `"*"` for all routes |
| `action` | `string` | Target all routes using this action name |
| `targets` | `[]string` | Flat replacement list (not a diff). Previous targets are discarded. |
| `groups` | `map[string][]string` | Keyed replacement map. Each key gets its own sub-balancer. |

- Use `targets` **or** `groups`, not both in the same message.
- Use `route_id` **or** `action`, not both.

### Plugin → Prox: `set_speed`

Pushes speed limits to route connections. Supports the same targeting as `set_targets`.

```json
{
  "method": "set_speed",
  "params": {
    "route_id": "gateway:0",
    "download_mbps": 50,
    "upload_mbps": 10
  }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `route_id` | `string` | Target route, or `"*"` for all routes |
| `action` | `string` | Target all routes using this action |
| `download_mbps` | `number` | Download bandwidth cap in Mbps |
| `upload_mbps` | `number` | Upload bandwidth cap in Mbps |
| `group_key` | `string` | When set, updates the rate for active group buckets with this key |

## Unix Socket — Request-Response Hooks

The socket uses **length-prefixed msgpack frames**:

```
[4 bytes: payload length, big-endian][msgpack payload]
```

### Frame Structure

Each frame is an `Envelope` containing a hook type and the hook-specific data:

```
Envelope {
  hook: string    // "on_request", "on_response", "on_connect", "on_disconnect"
  data: bytes     // msgpack-encoded hook payload
}
```

### `on_request`

**Request payload:**

| Field | msgpack key | Type |
|-------|-------------|------|
| RouteID | `r` | `string` |
| Method | `m` | `string` |
| Path | `p` | `string` |
| Query | `q` | `string` |
| Domain | `d` | `string` |
| Host | `ho` | `string` |
| Proto | `pr` | `string` |
| RemoteAddr | `a` | `string` |
| ContentLength | `cl` | `int64` |
| Headers | `h` | `map[string]string` |
| Body | `bd` | `bytes` |
| MatchDomain | `md` | `string` |
| MatchGlob | `mg` | `string` |
| MatchPath | `mp` | `string` |
| Vars | `v` | `map[string]string` |
| Target | `tg` | `string` |

**Response payload:**

| Field | msgpack key | Type |
|-------|-------------|------|
| Allow | `ok` | `bool` |
| Drop | `dr` | `bool` |
| Fallback | `fb` | `bool` |
| Status | `s` | `int` |
| Body | `b` | `string` |
| Headers | `h` | `map[string]string` |
| SpeedLimit | `sp` | `object` |
| CleanQuery | `cq` | `bool` |
| RewritePath | `rp` | `string` |

**SpeedLimit object (`sp`):**

| Field | msgpack key | Type | Description |
|-------|-------------|------|-------------|
| DownloadMbps | `dl` | `float64` | Download bandwidth cap in Mbps |
| UploadMbps | `ul` | `float64` | Upload bandwidth cap in Mbps |
| GroupKey | `gk` | `string` | Aggregate key — connections with the same key share a single bandwidth pool |

### `on_response`

**Request payload** — a pair of request info and upstream response:

```
ResponsePair {
  req:  RequestInfo         // same as on_request payload
  resp: UpstreamResponse {
    status:  int               // msgpack key: "s"
    headers: map[string]string // msgpack key: "h"
  }
}
```

**Response payload:**

| Field | msgpack key | Type |
|-------|-------------|------|
| Status | `s` | `int` (0 = no change) |
| Headers | `h` | `map[string]string` (add/override) |
| Remove | `rm` | `[]string` (headers to remove) |

### `on_connect`

**Request payload:**

| Field | msgpack key | Type |
|-------|-------------|------|
| RouteID | `r` | `string` |
| Domain | `d` | `string` |
| RemoteAddr | `a` | `string` |
| MatchDomain | `md` | `string` |
| MatchGlob | `mg` | `string` |

**Response payload:**

| Field | msgpack key | Type |
|-------|-------------|------|
| Allow | `ok` | `bool` |

### `on_disconnect`

Fire-and-forget — **no response expected**. prox writes the frame and does not wait for a reply.

**Request payload:**

| Field | msgpack key | Type |
|-------|-------------|------|
| RouteID | `r` | `string` |
| Target | `tg` | `string` |
| RemoteAddr | `a` | `string` |
| BytesRx | `rx` | `int64` |
| BytesTx | `tx` | `int64` |
| DurationMs | `ms` | `int64` |

## Minimal Plugin Example (Bash)

```bash
#!/bin/bash
while IFS= read -r line; do
    method=$(echo "$line" | jq -r '.method')
    if [ "$method" = "configure" ]; then
        route_id=$(echo "$line" | jq -r '.params.route_id')
        echo '{"result":"ok"}'
        # Push initial targets
        echo "{\"method\":\"set_targets\",\"params\":{\"route_id\":\"$route_id\",\"targets\":[\"10.0.1.1:3505\"]}}"
    fi
done
```

!!! note
    Bash plugins support push-based target discovery only. Request-response hooks require a Unix socket server with msgpack framing — use the [Go SDK](sdk.md) or implement the socket protocol directly.
