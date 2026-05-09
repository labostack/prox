# Getting Started

## Install

```bash
go install github.com/dortanes/prox/cmd/prox@latest
```

Or build from source:

```bash
git clone https://github.com/dortanes/prox.git
cd prox
go build -o prox ./cmd/prox
```

## Create a config

Create `config.json5`:

```json5
{
  services: {
    web: {
      listen: ":8080",
      routes: [
        {
          match: { path: "/*" },
          action: "proxy",
        },
      ],
    },
  },
  actions: {
    proxy: {
      type: "proxy",
      upstream: "localhost:3000",
      timeout: "10s",
    },
  },
}
```

## Validate

```bash
prox validate -config config.json5
# ✅ configuration is valid: config.json5 (1 file(s))
```

## Run

```bash
prox serve -config config.json5
```

With debug logging:

```bash
prox serve -config config.json5 -log-level debug
```

## Hot Reload

Edit `config.json5` while the server is running — changes are picked up automatically.

Or send SIGHUP:

```bash
kill -HUP $(pgrep prox)
```

Invalid configs are rejected gracefully — the server keeps running with the last valid config.

## Directory Mode

Instead of a single config file, you can use a directory of service fragments:

```bash
mkdir config
# Create config/web.json5, config/api.json5, etc.
prox serve -config ./config/
```

Each `.json5` file becomes a service (name = filename without extension).

## CLI Reference

```
prox <command> [flags]

Commands:
  serve      Start the proxy server
  validate   Validate configuration (CI/CD)
  version    Print version
  help       Show help

Flags:
  -config string      Config file or directory (default "config.json5")
  -log-level string   debug, info, warn, error (default "info")
  -watch              Auto-reload on file change (default true)
```
