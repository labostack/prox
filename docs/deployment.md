# Deployment

Production deployment patterns for prox, including containerized operation, configuration reloading, and L4/L7 mixed-mode dispatching.

## Docker

Run prox with Docker Compose:

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

Standalone container:

```bash
docker run -v ./config.json5:/etc/prox/config.json5 -p 8080:8080 ghcr.io/dortanes/prox
```

## Hot Reload

Configuration changes are detected automatically via file watcher. Manual reload is also supported via `SIGHUP`. Both L4 and L7 routes are swapped atomically — in-flight connections complete with the previous configuration, new connections use the updated one. Invalid configurations are rejected silently.

All loaded files are watched. Editing a nested service fragment or route include triggers a full reload.

```
prox serve -config config.json5            # watcher enabled by default
prox serve -config config.json5 -watch=false
```

## L4 Dispatching

When a service contains any `pass` routes, prox activates an L4 dispatcher on that listener. The dispatcher intercepts raw TCP connections **before** TLS termination:

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
    Without the dispatcher, `drop` routes operate at L7 — the connection hangs until timeout. When the dispatcher is active (due to `pass` routes), `drop` routes with domain patterns also participate in L4 matching, closing connections before the TLS handshake.

**Route order matters.** The dispatcher evaluates all routes — not just `pass` routes — in configuration order. The first domain match wins.

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

A single listener can mix L4 (TCP pass-through) and L7 (HTTP) routes. Hot reload updates both L4 and L7 routes atomically.
