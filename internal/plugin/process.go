package plugin

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Process wraps a single plugin subprocess.
type Process struct {
	name    string
	path    string
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	scanner *bufio.Scanner
	mu      sync.Mutex // protects writes to stdin
	pushCh  chan Push   // incoming pushes from the plugin
	done    chan struct{}
}

// startProcess spawns the plugin binary and wires up stdin/stdout.
// If the path ends in ".go", the source is compiled automatically.
func startProcess(name, path string) (*Process, error) {
	binPath, err := resolvePluginBinary(path)
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(binPath)

	// Isolate plugin process group so terminal signals don't kill it.
	setProcessGroup(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("creating stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, fmt.Errorf("creating stdout pipe: %w", err)
	}

	// Forward plugin stderr to prox's logger.
	cmd.Stderr = &logWriter{plugin: name}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting plugin %q: %w", path, err)
	}

	p := &Process{
		name:    name,
		path:    path,
		cmd:     cmd,
		stdin:   stdin,
		scanner: bufio.NewScanner(stdout),
		pushCh:  make(chan Push, 32),
		done:    make(chan struct{}),
	}

	// Read stdout in background — dispatches pushes to pushCh.
	go p.readLoop()

	return p, nil
}

// resolvePluginBinary returns the path to an executable binary.
//
// Supported plugin path formats:
//   - Pre-compiled binary: used as-is
//   - ".go" source file: compiled to a sibling binary (./plugins/resolver.go → ./plugins/resolver)
//   - Directory: compiled as a Go package (./plugins/resolver/ → ./plugins/resolver)
//
// Compilation runs from the source's directory so go.mod and third-party
// imports are resolved naturally by the Go toolchain.
func resolvePluginBinary(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("plugin path %q: %w", path, err)
	}

	if info.IsDir() {
		return buildPluginDir(path)
	}

	if strings.HasSuffix(path, ".go") {
		return buildPluginFile(path, info)
	}

	// Pre-compiled binary.
	return path, nil
}

// buildPluginFile compiles a .go source file's package into a sibling binary.
// Builds the entire package directory (not just the single file) so that
// all .go files in the package are included.
func buildPluginFile(path string, srcInfo os.FileInfo) (string, error) {
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
		return binPath, nil
	}

	slog.Info("building plugin",
		"source", filepath.Base(path),
		"output", filepath.Base(binPath),
	)

	absBin, _ := filepath.Abs(binPath)
	if err := runBuild(dir, "-o", absBin, "."); err != nil {
		return "", err
	}

	return binPath, nil
}

// buildPluginDir compiles a Go package directory into a binary named after the directory.
func buildPluginDir(dir string) (string, error) {
	absDir, _ := filepath.Abs(dir)
	binPath := absDir // ./plugins/resolver/ → ./plugins/resolver (binary)

	newestMod := newestGoFile(absDir)
	if newestMod == nil {
		return "", fmt.Errorf("plugin dir %q contains no .go files", dir)
	}

	if skipBuild(binPath, newestMod.ModTime()) {
		return binPath, nil
	}

	slog.Info("building plugin",
		"source", filepath.Base(dir)+"/",
		"output", filepath.Base(binPath),
	)

	if err := runBuild(absDir, "-o", binPath, "."); err != nil {
		return "", err
	}

	return binPath, nil
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
			slog.Info("retrying plugin build", "dir", dir)
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

// Send writes a JSON request to the plugin's stdin.
func (p *Process) Send(req Request) error {
	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshaling request: %w", err)
	}
	data = append(data, '\n')

	p.mu.Lock()
	defer p.mu.Unlock()

	if _, err := p.stdin.Write(data); err != nil {
		return fmt.Errorf("writing to plugin stdin: %w", err)
	}
	return nil
}

// Pushes returns the channel of incoming push messages.
func (p *Process) Pushes() <-chan Push {
	return p.pushCh
}

// Done returns a channel that closes when the process exits.
func (p *Process) Done() <-chan struct{} {
	return p.done
}

// Stop terminates the plugin process gracefully.
func (p *Process) Stop() {
	p.stdin.Close()

	// Wait for the process to exit (readLoop will close done).
	<-p.done
}

// readLoop reads line-delimited JSON from the plugin's stdout.
func (p *Process) readLoop() {
	defer close(p.done)
	defer close(p.pushCh)

	for p.scanner.Scan() {
		line := p.scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var push Push
		if err := json.Unmarshal(line, &push); err != nil {
			slog.Warn("plugin sent invalid JSON",
				"plugin", p.name,
				"err", err,
				"line", string(line),
			)
			continue
		}

		// Ignore non-push messages (e.g. configure responses).
		if push.Method == "" {
			continue
		}

		select {
		case p.pushCh <- push:
		default:
			slog.Warn("plugin push buffer full",
				"plugin", p.name,
				"method", push.Method,
			)
		}
	}

	if err := p.scanner.Err(); err != nil {
		slog.Debug("plugin stdout closed",
			"plugin", p.name,
			"err", err,
		)
	}

	// Wait for process to fully exit.
	_ = p.cmd.Wait()
}

// logWriter forwards plugin stderr lines to slog.
// Each line is logged individually as the message to keep output readable.
type logWriter struct {
	plugin string
}

func (w *logWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(string(p), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		slog.Debug("[" + w.plugin + "] " + line)
	}
	return len(p), nil
}
