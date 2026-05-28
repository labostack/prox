package watcher

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestWatch_DetectsChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json5")

	if err := os.WriteFile(path, []byte("v1"), 0644); err != nil {
		t.Fatal(err)
	}

	// Wait a moment to ensure mtime differs on next write.
	time.Sleep(50 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var called atomic.Int32
	go Watch(ctx, []string{path}, func() {
		called.Add(1)
	})

	// Let the watcher take an initial snapshot.
	time.Sleep(100 * time.Millisecond)

	// Modify the file.
	if err := os.WriteFile(path, []byte("v2-changed"), 0644); err != nil {
		t.Fatal(err)
	}

	// Wait for at least one poll cycle.
	time.Sleep(2 * time.Second)

	if called.Load() == 0 {
		t.Error("expected onChange to be called after file modification")
	}
}

func TestWatch_NoChangeNoCalls(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stable.json5")

	if err := os.WriteFile(path, []byte("stable"), 0644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	var called atomic.Int32
	go Watch(ctx, []string{path}, func() {
		called.Add(1)
	})

	// Wait a couple of poll cycles without changing the file.
	time.Sleep(2500 * time.Millisecond)
	cancel()

	if called.Load() != 0 {
		t.Errorf("expected no onChange calls, got %d", called.Load())
	}
}

func TestWatch_NoFiles(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Should return immediately without blocking.
	done := make(chan struct{})
	go func() {
		Watch(ctx, nil, func() {})
		close(done)
	}()

	select {
	case <-done:
		// OK — returned immediately.
	case <-time.After(2 * time.Second):
		t.Error("Watch with no files should return immediately")
	}
}

func TestWatch_NonExistentFile(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Should return immediately (no valid files to watch).
	done := make(chan struct{})
	go func() {
		Watch(ctx, []string{"/nonexistent/path.json5"}, func() {})
		close(done)
	}()

	select {
	case <-done:
		// OK.
	case <-time.After(2 * time.Second):
		t.Error("Watch with non-existent files should return immediately")
	}
}

func TestSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snap.txt")

	if err := os.WriteFile(path, []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}

	s, err := snapshot(path)
	if err != nil {
		t.Fatal(err)
	}

	if s.size != 5 {
		t.Errorf("expected size 5, got %d", s.size)
	}
	if s.modTime.IsZero() {
		t.Error("expected non-zero modTime")
	}
}
