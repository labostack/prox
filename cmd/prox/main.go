// prox is a modular reverse proxy with config-driven routing.
//
// Usage:
//
//	prox serve    -config config.json5
//	prox validate -config config.json5
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/dortanes/prox/internal/config"
	"github.com/dortanes/prox/internal/server"
	"github.com/dortanes/prox/internal/watcher"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "serve":
		os.Exit(runServe(os.Args[2:]))
	case "validate":
		os.Exit(runValidate(os.Args[2:]))
	case "version":
		fmt.Printf("prox %s\n", version)
		os.Exit(0)
	case "help", "-h", "--help":
		printUsage()
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `prox — modular reverse proxy

Usage:
  prox <command> [flags]

Commands:
  serve      Start the proxy server
  validate   Validate configuration (for CI/CD pipelines)
  version    Print version
  help       Show this help

Flags (serve, validate):
  -config string      Path to config file or directory (default "config.json5")
  -log-level string   Log level: debug, info, warn, error (default "info")
  -watch              Watch config files for changes and auto-reload (default true)

`)
}

// runServe starts the proxy with the given config.
func runServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	configPath := fs.String("config", "config.json5", "path to config file or directory")
	logLevel := fs.String("log-level", "info", "log level: debug, info, warn, error")
	watchEnabled := fs.Bool("watch", true, "watch config files for changes and auto-reload")
	_ = fs.Parse(args)

	initLogger(*logLevel)

	slog.Info("loading configuration", "path", *configPath)

	result, err := config.LoadFile(*configPath)
	if err != nil {
		slog.Error("configuration error", "error", err)
		if config.IsValidationError(err) {
			fmt.Fprintf(os.Stderr, "\n%s\n", err)
		}
		return 1
	}

	cfg := result.Config

	slog.Info("configuration loaded",
		"services", len(cfg.Services),
		"actions", len(cfg.Actions),
		"resources", len(cfg.Resources),
		"files", len(result.Paths),
	)

	group, err := server.Build(cfg)
	if err != nil {
		slog.Error("server build error", "error", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT,
		syscall.SIGTERM,
	)
	defer stop()

	reloadCh := make(chan struct{}, 1)

	sighupCh := make(chan os.Signal, 1)
	signal.Notify(sighupCh, syscall.SIGHUP)
	go func() {
		for range sighupCh {
			slog.Info("SIGHUP received, scheduling reload")
			triggerReload(reloadCh)
		}
	}()

	if *watchEnabled {
		go watcher.Watch(ctx, result.Paths, func() {
			triggerReload(reloadCh)
		})
		slog.Info("config file watcher enabled", "files", len(result.Paths))
	}

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-reloadCh:
				performReload(*configPath, group)
			}
		}
	}()

	if err := group.ListenAndServe(ctx); err != nil {
		slog.Error("server error", "error", err)
		return 1
	}

	slog.Info("prox stopped")
	return 0
}

func triggerReload(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
		// Already a reload pending — skip duplicate.
	}
}

func performReload(path string, group *server.Group) {
	slog.Info("reloading configuration", "path", path)

	result, err := config.LoadFile(path)
	if err != nil {
		slog.Error("reload failed: invalid config, keeping current",
			"path", path,
			"error", err,
		)
		return
	}

	if err := group.Reload(result.Config); err != nil {
		slog.Error("reload failed: could not apply config, keeping current",
			"error", err,
		)
		return
	}
}

// runValidate checks the config and exits with 0 (valid) or 1 (invalid).
func runValidate(args []string) int {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	configPath := fs.String("config", "config.json5", "path to config file or directory")
	_ = fs.Parse(args)

	result, err := config.LoadFile(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ %s\n", err)
		return 1
	}

	fmt.Fprintf(os.Stdout, "✅ configuration is valid: %s (%d file(s))\n",
		*configPath, len(result.Paths))
	return 0
}

// initLogger sets up structured logging with the given level.
func initLogger(level string) {
	var lvl slog.Level

	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: lvl,
	})
	slog.SetDefault(slog.New(handler))
}
