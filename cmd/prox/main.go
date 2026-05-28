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
	"runtime/debug"
	"syscall"

	"github.com/dortanes/prox/internal/config"
	"github.com/dortanes/prox/internal/logger"
	"github.com/dortanes/prox/internal/plugin"
	"github.com/dortanes/prox/internal/server"
	"github.com/dortanes/prox/internal/watcher"
)

var version = "dev"

func init() {
	if version != "dev" {
		return
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		version = info.Main.Version
	}
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "serve":
		os.Exit(runServe(os.Args[2:]))
	case "build":
		os.Exit(runBuild(os.Args[2:]))
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
  build      Compile plugin sources into binaries
  validate   Validate configuration (for CI/CD pipelines)
  version    Print version
  help       Show this help

Flags (serve, build, validate):
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

	// Priority: LOG_LEVEL env > -log-level flag > config file > "info"
	level := *logLevel
	if envLevel := os.Getenv("LOG_LEVEL"); envLevel != "" {
		level = envLevel
	}

	// Early init — console only (config not loaded yet).
	if err := logger.Setup(logger.Config{Level: level}); err != nil {
		fmt.Fprintf(os.Stderr, "logger init error: %v\n", err)
		return 1
	}
	defer logger.Close()

	slog.Debug("loading configuration", "path", *configPath)

	result, err := config.LoadFile(*configPath)
	if err != nil {
		slog.Error("config load failed", "err", err)
		if config.IsValidationError(err) {
			fmt.Fprintf(os.Stderr, "\n%s\n", err)
		}
		return 1
	}

	cfg := result.Config

	// Re-init logger with full config (file handlers).
	logCfg := logger.Config{Level: level}
	if cfg.Logging != nil {
		logCfg.ErrorLog = cfg.Logging.ErrorLog
		// Config level is lowest priority.
		if level == "info" && cfg.Logging.Level != "" {
			logCfg.Level = cfg.Logging.Level
		}
	}
	if err := logger.Setup(logCfg); err != nil {
		slog.Error("logger setup error", "err", err)
		return 1
	}

	// Setup access logging.
	if err := setupAccessLogging(cfg); err != nil {
		slog.Error("access log setup error", "err", err)
		return 1
	}

	slog.Debug("config loaded",
		"services", len(cfg.Services),
		"actions", len(cfg.Actions),
		"resources", len(cfg.Resources),
		"files", len(result.Paths),
	)

	group, err := server.Build(cfg)
	if err != nil {
		slog.Error("server build failed", "err", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT,
		syscall.SIGTERM,
	)
	defer stop()

	// Second interrupt forces immediate exit.
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh // first one is already handled above
		<-sigCh // second one = force exit
		slog.Warn("forced shutdown")
		os.Exit(1)
	}()

	reloadCh := make(chan struct{}, 1)

	sighupCh := make(chan os.Signal, 1)
	signal.Notify(sighupCh, syscall.SIGHUP)
	go func() {
		for range sighupCh {
			slog.Info("SIGHUP received")
			// Reopen log files for rotation support.
			if err := logger.ReopenFiles(); err != nil {
				slog.Error("failed to reopen log files", "err", err)
			}
			triggerReload(reloadCh)
		}
	}()

	if *watchEnabled {
		go watcher.Watch(ctx, result.Paths, func() {
			triggerReload(reloadCh)
		})
		slog.Debug("file watcher started", "files", len(result.Paths))
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
		slog.Error("server failed", "err", err)
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
	slog.Info("reloading config", "path", path)

	result, err := config.LoadFile(path)
	if err != nil {
		slog.Error("reload failed",
			"path", path,
			"err", err,
		)
		return
	}

	// Reconfigure access logging for new routes.
	if err := setupAccessLogging(result.Config); err != nil {
		slog.Error("reload failed", "err", err)
		return
	}

	if err := group.Reload(result.Config); err != nil {
		slog.Error("reload failed",
			"err", err,
		)
		return
	}
}

// setupAccessLogging configures global and per-route access log files from the config.
func setupAccessLogging(cfg *config.Config) error {
	globalPath := ""
	if cfg.Logging != nil {
		globalPath = cfg.Logging.AccessLog
	}

	routePaths := make(map[string]string)
	for name, svc := range cfg.Services {
		for i, route := range svc.Routes {
			// Route-level access_log takes priority over action-level.
			path := route.AccessLog
			if path == "" {
				path = resolveActionAccessLog(route, cfg)
			}
			if path != "" {
				routePaths[fmt.Sprintf("%s:%d", name, i)] = path
			}
		}
	}

	return logger.SetupAccess(globalPath, routePaths)
}

// resolveActionAccessLog returns the access_log value from the action
// referenced by the route (inline or named). Returns "" if unset.
func resolveActionAccessLog(route *config.Route, cfg *config.Config) string {
	if route.Action.Inline != nil {
		return route.Action.Inline.AccessLog
	}
	if route.Action.Name != "" {
		if act, ok := cfg.Actions[route.Action.Name]; ok {
			return act.AccessLog
		}
	}
	return ""
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

// runBuild compiles all plugin sources referenced in the configuration.
func runBuild(args []string) int {
	fs := flag.NewFlagSet("build", flag.ExitOnError)
	configPath := fs.String("config", "config.json5", "path to config file or directory")
	_ = fs.Parse(args)

	if err := logger.Setup(logger.Config{Level: "info"}); err != nil {
		fmt.Fprintf(os.Stderr, "logger init error: %v\n", err)
		return 1
	}
	defer logger.Close()

	result, err := config.LoadFile(*configPath)
	if err != nil {
		slog.Error("config load failed", "err", err)
		return 1
	}

	// Collect all unique plugin paths.
	var paths []string
	for _, p := range result.Config.Plugins {
		if p.Path != "" {
			paths = append(paths, p.Path)
		}
	}

	if len(paths) == 0 {
		slog.Info("no plugins to build")
		return 0
	}

	if err := plugin.BuildPlugins(paths); err != nil {
		slog.Error("plugin build failed", "err", err)
		return 1
	}

	slog.Info("plugins built", "count", len(paths))
	return 0
}


