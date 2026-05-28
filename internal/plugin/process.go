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
// The path must point to a pre-compiled binary, or a source path whose
// binary has been compiled via 'prox build'.
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

// resolvePluginBinary returns the path to a pre-compiled binary.
//
// Supported plugin path formats:
//   - Pre-compiled binary: used as-is
//   - ".go" source file: expected binary is the path without the .go extension
//   - Directory: expected binary is the directory path itself
//   - Bare path (no extension, not a dir): tries path.go → binary at path
//
// Source paths (.go files and directories) are NOT compiled at runtime.
// Run 'prox build' to compile plugins before starting the server.
func resolvePluginBinary(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		// Path doesn't exist — try path+".go" (e.g. "plugins/auth/main" → binary "plugins/auth/main").
		goPath := path + ".go"
		if _, goErr := os.Stat(goPath); goErr == nil {
			// Source exists at path.go — binary is expected at path (without .go).
			binPath := path
			if _, binErr := os.Stat(binPath); binErr != nil {
				return "", fmt.Errorf("plugin %q is not compiled — run 'prox build' first", filepath.Base(path))
			}
			return binPath, nil
		}
		// Try parent directory as a Go package.
		dir := filepath.Dir(path)
		if dirInfo, dirErr := os.Stat(dir); dirErr == nil && dirInfo.IsDir() {
			absDir, _ := filepath.Abs(dir)
			if _, binErr := os.Stat(absDir); binErr != nil {
				return "", fmt.Errorf("plugin %q is not compiled — run 'prox build' first", filepath.Base(dir))
			}
			return absDir, nil
		}
		return "", fmt.Errorf("plugin path %q: %w", path, err)
	}

	var binPath string
	switch {
	case info.IsDir():
		binPath, _ = filepath.Abs(path)
	case strings.HasSuffix(path, ".go"):
		binPath = strings.TrimSuffix(path, ".go")
	default:
		// Pre-compiled binary.
		return path, nil
	}

	// Verify the compiled binary exists.
	if _, err := os.Stat(binPath); err != nil {
		return "", fmt.Errorf("plugin %q is not compiled — run 'prox build' first", filepath.Base(path))
	}

	return binPath, nil
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
