# Deployment

## Docker

```yaml
# docker-compose.yml
services:
  prox:
    image: ghcr.io/dortanes/prox:latest
    ports:
      - "443:443"
      - "8080:8080"
    volumes:
      - ./config/:/etc/prox/config/
      - ./certs/:/etc/prox/certs/
    command: ["serve", "-config", "/etc/prox/config/"]
```

Or with `docker run`:

```bash
docker run -v ./config.json5:/etc/prox/config.json5 -p 8080:8080 ghcr.io/dortanes/prox
```

## Hot Reload

Config changes are picked up automatically via file watcher, or manually via `kill -HUP`. Both L4 and L7 routes are swapped atomically — in-flight connections finish with the old config, new connections use the new one. Invalid configs are rejected silently.

All loaded files are watched — editing a nested service fragment or route include triggers a full reload.

```
prox serve -config config.json5          # watcher enabled by default
prox serve -config config.json5 -watch=false
```

## L4 Dispatching

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

!!! note
    `drop` routes work at L7 without the dispatcher (the connection hangs until timeout). When the dispatcher is already active due to `pass` routes, `drop` routes with domain patterns also participate in L4 matching — closing connections before TLS handshake.

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

A single listener can mix L4 (TCP pass-through) and L7 (HTTP) routes seamlessly. Hot reload updates both L4 and L7 routes atomically.
