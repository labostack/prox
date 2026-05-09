# prox

Modular reverse proxy with config-driven routing, hot reload, and near-zero dependencies.

[![CI](https://github.com/dortanes/prox/actions/workflows/ci.yml/badge.svg)](https://github.com/dortanes/prox/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/go-%E2%89%A5%201.23-brightgreen.svg)](https://golang.org/)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

## Quick Start

```bash
go install github.com/dortanes/prox/cmd/prox@latest

prox serve -config config.json5
```

Or build from source:

```bash
go build -o prox ./cmd/prox
```

## Config

Three sections: **services** (listeners), **actions** (handlers), **resources** (data).

```json5
{
  services: {
    web: {
      listen: ":8080",
      routes: [
        { match: { path: "/api/*" }, action: "backend" },
        { match: { path: "/*" },     action: "static" },
      ],
    },
  },
  actions: {
    backend: { type: "proxy", upstream: "localhost:3000", timeout: "5s" },
    static:  { type: "serve", root: "./public" },
  },
}
```

Routes are evaluated in order, first match wins. Paths support exact (`/health`) and wildcard (`/api/*`) matching. Actions and resources can be referenced by name or [inlined](docs/configuration.md#inline-actions). Services can be [split into separate files](docs/configuration.md#file-reference) or loaded from a [config directory](docs/configuration.md#directory-mode-cli).

### Action Types

| Type | Description |
|------|-------------|
| `proxy` | Reverse proxy to upstream (`host:port`) with configurable timeout |
| `static` | Fixed response with status, headers, and optional body from resources |
| `serve` | File server — directory with auto `index.html`, or single file (SPA) |

See [docs/configuration.md](docs/configuration.md) for the full reference.

## Hot Reload

Config changes are picked up automatically via file watcher, or manually via `kill -HUP`. Routes and actions are swapped atomically — in-flight requests finish with the old config, new requests use the new one. Invalid configs are rejected silently.

All loaded files are watched — editing a nested service fragment triggers a full reload.

```
prox serve -config config.json5          # watcher enabled by default
prox serve -config config.json5 -watch=false
```

## CLI

```
prox <command> [flags]

  serve      Start the proxy server
  validate   Validate config (exit 0 = valid, 1 = invalid)
  version    Print version

  -config    Path to config file or directory (default "config.json5")
  -log-level debug | info | warn | error (default "info")
  -watch     Auto-reload on file change (default true)
```

## Docker

```yaml
# docker-compose.yml
services:
  prox:
    image: ghcr.io/dortanes/prox:latest
    ports:
      - "8080:8080"
    volumes:
      - ./config/:/etc/prox/config/
    command: ["serve", "-config", "/etc/prox/config/"]
```

Or with `docker run`:

```bash
docker run -v ./config.json5:/etc/prox/config.json5 -p 8080:8080 ghcr.io/dortanes/prox
```

## License

MIT
