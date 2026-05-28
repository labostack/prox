# Getting Started

Install prox, create a minimal configuration, and start proxying traffic.

## Installation

Install the latest release via `go install`:

```bash
go install github.com/dortanes/prox/cmd/prox@latest
```

To build from source:

```bash
git clone https://github.com/dortanes/prox.git
cd prox
go build -o prox ./cmd/prox
```

## Minimal Configuration

Create a `config.json5` file with a single service and action:

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

This configuration listens on port 8080 and proxies all requests to `localhost:3000`.

## Validate

Validate configuration before starting the server. This is recommended as a CI/CD step.

```bash
prox validate -config config.json5
# ✅ configuration is valid: config.json5 (1 file(s))
```

## Start the Server

```bash
prox serve -config config.json5
```

To enable debug-level logging:

```bash
prox serve -config config.json5 -log-level debug
```

## Hot Reload

Configuration changes are detected automatically while the server is running. Editing `config.json5` triggers an atomic reload — in-flight connections complete with the previous configuration, new connections use the updated one.

Manual reload via signal:

```bash
kill -HUP $(pgrep prox)
```

Invalid configurations are rejected gracefully. The server continues running with the last valid configuration.

## Directory Mode

Configuration can be split across multiple files in a directory. Each `.json5` file defines a separate service, with the filename (without extension) used as the service name.

```bash
mkdir config
# Create config/web.json5, config/api.json5, etc.
prox serve -config ./config/
```

See [Configuration](configuration/index.md) for the full configuration reference.

## CLI Reference

```
prox <command> [flags]

Commands:
  serve      Start the proxy server
  build      Compile plugin sources defined in config
  validate   Validate configuration (CI/CD)
  version    Print version
  help       Show help

Flags:
  -config string      Config file or directory (default "config.json5")
  -log-level string   debug, info, warn, error (default "info")
  -watch              Auto-reload on file change (default true)
```
