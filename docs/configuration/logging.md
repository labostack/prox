# Logging

Structured console output with colorized levels, file-based access and error logs, per-route log overrides, and signal-based log rotation.

## Log Level

Control verbosity via configuration, CLI flag, or environment variable.

| Level   | Description                                              |
|---------|----------------------------------------------------------|
| `debug` | Verbose output — config loading, route matching details  |
| `info`  | Normal operation — startup, reload events, connections   |
| `warn`  | Recoverable issues — timeouts, retries                   |
| `error` | Failures — upstream errors, TLS handshake failures       |

```json5
{
  logging: {
    level: "info",
  },
}
```

### Priority Order

The log level is resolved in the following order (highest priority first):

1. `LOG_LEVEL` environment variable
2. `-log-level` CLI flag
3. `logging.level` in configuration file
4. Default: `info`

```bash
# Environment variable (highest priority)
LOG_LEVEL=debug prox serve

# CLI flag
prox serve -log-level debug
```

!!! tip
    The `LOG_LEVEL` environment variable overrides all other sources. This is useful for temporarily increasing verbosity without modifying the configuration file.

## Global Log Files

Configure file-based logging in the top-level `logging` block.

```json5
{
  logging: {
    level: "info",
    access_log: "/var/log/prox/access.log",
    error_log: "/var/log/prox/error.log",
  },
}
```

| Field        | Type   | Default | Description                                          |
|--------------|--------|---------|------------------------------------------------------|
| `level`      | string | `info`  | Minimum log level: `debug`, `info`, `warn`, `error`  |
| `access_log` | string | —       | Path to the global access log file (JSON lines)      |
| `error_log`  | string | —       | Path to the error log file (`warn` + `error` level)  |

Access logs are written in JSON lines format — one JSON object per request. The error log captures `warn` and `error` level messages from all components.

## Per-Route Access Log

Override the global access log for specific routes by setting `access_log` on the route definition.

```json5
{
  services: {
    web: {
      routes: [
        {
          match: { path: "/api/*" },
          access_log: "/var/log/prox/api.log",
          action: { type: "proxy", upstream: "localhost:3000" },
        },
        {
          match: { path: "/admin/*" },
          access_log: "/var/log/prox/admin.log",
          action: { type: "proxy", upstream: "localhost:3001" },
        },
      ],
    },
  },
}
```

Requests matching these routes are logged to their respective files instead of the global access log.

## Per-Action Access Log

Actions can also specify an `access_log`. This applies to all routes that reference the action.

```json5
{
  actions: {
    api: {
      type: "proxy",
      upstream: "localhost:3000",
      access_log: "/var/log/prox/api.log",
    },
  },
}
```

!!! important
    Route-level `access_log` takes priority over action-level `access_log`. When both are set, the route-level value is used.

## Disabling Access Logs

Suppress access logging for specific routes or actions by setting `access_log` to `"off"`.

Per-route:

```json5
{
  match: { path: "/tunnel/*" },
  access_log: "off",
  action: "transport",
}
```

Per-action:

```json5
{
  actions: {
    transport: {
      type: "proxy",
      upstream: "localhost:8443",
      stream: true,
      access_log: "off",
    },
  },
}
```

!!! tip
    Disable access logs on WebSocket tunnels, health-check endpoints, and other high-frequency routes that would otherwise produce excessive log volume.

## Log Rotation

Log files support rotation via the `SIGHUP` signal. When prox receives `SIGHUP`, all log file handles are closed and reopened, allowing external tools such as `logrotate` to rotate the files.

### Example logrotate Configuration

```
/var/log/prox/*.log {
    daily
    rotate 14
    compress
    missingok
    notifempty
    postrotate
        kill -HUP $(pidof prox) 2>/dev/null || true
    endscript
}
```

!!! note
    `SIGHUP` also triggers a configuration reload. Log file handles are reopened as part of the reload process.
