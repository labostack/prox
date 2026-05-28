package plugin

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// BuildPlugins compiles all plugin sources into binaries.
// Each path can be a .go file, a directory containing Go source, or
// a pre-compiled binary (skipped). Returns the first error encountered.
func BuildPlugins(paths []string) error {
	seen := make(map[string]struct{})
	for _, path := range paths {
		abs, _ := filepath.Abs(path)
		if _, ok := seen[abs]; ok {
			continue
		}
		seen[abs] = struct{}{}

		if err := buildPlugin(path); err != nil {
			return err
		}
	}
	return nil
}

// buildPlugin compiles a single plugin from source.
func buildPlugin(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		// Path doesn't exist — try appending ".go" (e.g. "plugins/auth/main" → "plugins/auth/main.go").
		goPath := path + ".go"
		goInfo, goErr := os.Stat(goPath)
		if goErr != nil {
			// Also try treating as a directory that hasn't been created yet.
			// Check if the parent directory contains .go files.
			dir := filepath.Dir(path)
			dirInfo, dirErr := os.Stat(dir)
			if dirErr == nil && dirInfo.IsDir() {
				return buildPluginDir(dir)
			}
			return fmt.Errorf("plugin path %q: %w", path, err)
		}
		return buildPluginFile(goPath, goInfo)
	}

	if info.IsDir() {
		return buildPluginDir(path)
	}

	if strings.HasSuffix(path, ".go") {
		return buildPluginFile(path, info)
	}

	// Pre-compiled binary — nothing to build.
	slog.Debug("plugin already compiled", "path", path)
	return nil
}

// buildPluginFile compiles a .go source file's package into a sibling binary.
// Builds the entire package directory (not just the single file) so that
// all .go files in the package are included.
func buildPluginFile(path string, srcInfo os.FileInfo) error {
	binPath := strings.TrimSuffix(path, ".go")

	absPath, _ := filepath.Abs(path)
	dir := filepath.Dir(absPath)

	// Check the newest .go file in the directory for mtime comparison,
	// since the entire package is compiled.
	newestMod := newestGoFile(dir)
	if newestMod == nil {
		newestMod = srcInfo
	}

	if skipBuild(binPath, newestMod.ModTime()) {
		return nil
	}

	slog.Info("building plugin",
		"source", filepath.Base(path),
		"output", filepath.Base(binPath),
	)

	absBin, _ := filepath.Abs(binPath)
	return runBuild(dir, "-o", absBin, ".")
}

// buildPluginDir compiles a Go package directory into a binary named after the directory.
func buildPluginDir(dir string) error {
	absDir, _ := filepath.Abs(dir)
	binPath := absDir // ./plugins/resolver/ → ./plugins/resolver (binary)

	newestMod := newestGoFile(absDir)
	if newestMod == nil {
		return fmt.Errorf("plugin dir %q contains no .go files", dir)
	}

	if skipBuild(binPath, newestMod.ModTime()) {
		return nil
	}

	slog.Info("building plugin",
		"source", filepath.Base(dir)+"/",
		"output", filepath.Base(binPath),
	)

	return runBuild(absDir, "-o", binPath, ".")
}

// newestGoFile returns the os.FileInfo of the most recently modified .go file
// in the directory. Returns nil if the directory contains no .go files.
func newestGoFile(dir string) os.FileInfo {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var newest os.FileInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if newest == nil || info.ModTime().After(newest.ModTime()) {
			newest = info
		}
	}
	return newest
}

// skipBuild returns true if the binary exists and is newer than srcMod.
func skipBuild(binPath string, srcMod time.Time) bool {
	binInfo, err := os.Stat(binPath)
	if err != nil {
		return false
	}
	if binInfo.ModTime().After(srcMod) {
		slog.Debug("plugin up to date",
			"plugin", filepath.Base(binPath),
		)
		return true
	}
	return false
}

// runBuild runs the 'go build' command. If it fails and a go.mod file is present
// in the plugin directory, it attempts to resolve missing dependencies by running
// 'go mod tidy' and then retries the build.
func runBuild(dir string, args ...string) error {
	cmd := exec.Command("go", append([]string{"build"}, args...)...)
	cmd.Dir = dir
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout

	err := cmd.Run()
	if err == nil {
		return nil
	}

	goModPath := filepath.Join(dir, "go.mod")
	if _, statErr := os.Stat(goModPath); statErr == nil {
		slog.Warn("plugin build failed, resolving dependencies", "dir", dir, "err", err)
		if tidyErr := tidyPluginDir(dir); tidyErr != nil {
			slog.Warn("plugin dependency resolution failed", "dir", dir, "err", tidyErr)
		} else {
			slog.Debug("retrying plugin build", "dir", dir)
			cmdRetry := exec.Command("go", append([]string{"build"}, args...)...)
			cmdRetry.Dir = dir
			cmdRetry.Stderr = os.Stderr
			cmdRetry.Stdout = os.Stdout
			if retryErr := cmdRetry.Run(); retryErr == nil {
				return nil
			}
		}
	}

	return fmt.Errorf("building plugin: %w", err)
}

// tidyPluginDir runs `go mod tidy` in the plugin directory if go.mod is present.
func tidyPluginDir(dir string) error {
	slog.Debug("tidying plugin dependencies", "dir", dir)
	cmd := exec.Command("go", "mod", "tidy")
	cmd.Dir = dir
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	return cmd.Run()
}
