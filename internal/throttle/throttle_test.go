package throttle

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
	"time"
)

func TestNewBucketZero(t *testing.T) {
	if b := NewBucket(0); b != nil {
		t.Fatal("NewBucket(0) should return nil")
	}
	if b := NewBucket(-1); b != nil {
		t.Fatal("NewBucket(-1) should return nil")
	}
}

func TestNewBucketPositive(t *testing.T) {
	if b := NewBucket(1_000_000); b == nil {
		t.Fatal("NewBucket(1000000) should return non-nil")
	}
}

func TestWriterNil(t *testing.T) {
	if w := NewWriter(io.Discard, nil); w != nil {
		t.Fatal("NewWriter with nil bucket should return nil")
	}
}

func TestWriterRate(t *testing.T) {
	const (
		rate = 1024 * 1024 // 1 MB/s
		size = 256 * 1024  // 256 KB
	)

	bucket := NewBucket(rate)
	// Drain initial burst tokens so timing is driven purely by the rate.
	bucket.mu.Lock()
	bucket.tokens = 0
	bucket.last = time.Now().UnixNano()
	bucket.mu.Unlock()

	w := NewWriter(io.Discard, bucket)
	data := make([]byte, size)

	start := time.Now()
	if _, err := w.Write(data); err != nil {
		t.Fatalf("write error: %v", err)
	}
	elapsed := time.Since(start)

	// 256KB at 1MB/s ≈ 250ms. Allow 200ms lower bound with 50ms tolerance.
	const minExpected = 200 * time.Millisecond
	if elapsed < minExpected {
		t.Fatalf("write completed too fast: %v (expected >= %v)", elapsed, minExpected)
	}
}

func TestReaderRate(t *testing.T) {
	const (
		rate = 1024 * 1024 // 1 MB/s
		size = 256 * 1024  // 256 KB
	)

	bucket := NewBucket(rate)
	bucket.mu.Lock()
	bucket.tokens = 0
	bucket.last = time.Now().UnixNano()
	bucket.mu.Unlock()

	src := bytes.NewReader(make([]byte, size))
	r := NewReader(src, bucket)
	buf := make([]byte, size)

	start := time.Now()
	total := 0
	for total < size {
		n, err := r.Read(buf[total:])
		total += n
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read error: %v", err)
		}
	}
	elapsed := time.Since(start)

	const minExpected = 200 * time.Millisecond
	if elapsed < minExpected {
		t.Fatalf("read completed too fast: %v (expected >= %v)", elapsed, minExpected)
	}
}

func TestSharedBucket(t *testing.T) {
	const (
		rate = 1024 * 1024 // 1 MB/s shared
		size = 128 * 1024  // 128 KB each
	)

	bucket := NewBucket(rate)
	bucket.mu.Lock()
	bucket.tokens = 0
	bucket.last = time.Now().UnixNano()
	bucket.mu.Unlock()

	w1 := NewWriter(io.Discard, bucket)
	w2 := NewWriter(io.Discard, bucket)
	data := make([]byte, size)

	var wg sync.WaitGroup
	wg.Add(2)

	start := time.Now()
	go func() {
		defer wg.Done()
		_, _ = w1.Write(data)
	}()
	go func() {
		defer wg.Done()
		_, _ = w2.Write(data)
	}()
	wg.Wait()
	elapsed := time.Since(start)

	// 256KB combined at 1MB/s ≈ 250ms.
	const minExpected = 200 * time.Millisecond
	if elapsed < minExpected {
		t.Fatalf("shared writes completed too fast: %v (expected >= %v)", elapsed, minExpected)
	}
}

func TestMbpsToBytes(t *testing.T) {
	if v := MbpsToBytes(100); v != 12_500_000 {
		t.Fatalf("MbpsToBytes(100) = %d, want 12500000", v)
	}
	if v := MbpsToBytes(0.5); v != 62_500 {
		t.Fatalf("MbpsToBytes(0.5) = %d, want 62500", v)
	}
}

func TestContextRoundtrip(t *testing.T) {
	limits := &Limits{
		Download: []*Bucket{NewBucket(1000)},
		Upload:   []*Bucket{NewBucket(2000)},
	}

	ctx := NewContext(context.Background(), limits)
	got := FromContext(ctx)
	if got == nil {
		t.Fatal("FromContext returned nil")
	}
	if got != limits {
		t.Fatal("FromContext returned different Limits")
	}

	if v := FromContext(context.Background()); v != nil {
		t.Fatal("FromContext on empty context should return nil")
	}
}

func TestSetRate(t *testing.T) {
	b := NewBucket(1_000_000) // 1 MB/s
	if b == nil {
		t.Fatal("bucket should not be nil")
	}
	// SetRate to 500 KB/s and verify it throttles more.
	b.SetRate(500_000)
	b.mu.Lock()
	expectedRate := float64(500_000) / 1e9
	if b.rate != expectedRate {
		t.Fatalf("rate = %v, want %v", b.rate, expectedRate)
	}
	b.mu.Unlock()
}

func TestSetRateNilSafe(t *testing.T) {
	var b *Bucket
	b.SetRate(1000) // should not panic
}

func TestGroupRegistryAcquireRelease(t *testing.T) {
	g := NewGroupRegistry()

	// First acquire creates buckets.
	dl, ul := g.Acquire("user-1", 100_000, 50_000)
	if dl == nil || ul == nil {
		t.Fatal("Acquire should return non-nil buckets")
	}

	// Second acquire returns the same buckets.
	dl2, ul2 := g.Acquire("user-1", 100_000, 50_000)
	if dl2 != dl || ul2 != ul {
		t.Fatal("second Acquire should return same bucket pointers")
	}

	// Release twice — entry should be cleaned up.
	g.Release("user-1")
	g.mu.Lock()
	if _, ok := g.groups["user-1"]; !ok {
		t.Fatal("entry should still exist after first release")
	}
	g.mu.Unlock()

	g.Release("user-1")
	g.mu.Lock()
	if _, ok := g.groups["user-1"]; ok {
		t.Fatal("entry should be removed after all releases")
	}
	g.mu.Unlock()
}

func TestGroupRegistrySharedBudget(t *testing.T) {
	const (
		rate = 1024 * 1024 // 1 MB/s shared
		size = 128 * 1024  // 128 KB each
	)

	g := NewGroupRegistry()
	dl1, _ := g.Acquire("group-a", rate, 0)
	dl2, _ := g.Acquire("group-a", rate, 0)

	// Buckets must be the same object (shared).
	if dl1 != dl2 {
		t.Fatal("grouped connections should share the same bucket")
	}

	// Drain burst tokens for deterministic timing.
	dl1.mu.Lock()
	dl1.tokens = 0
	dl1.last = time.Now().UnixNano()
	dl1.mu.Unlock()

	w1 := NewWriter(io.Discard, dl1)
	w2 := NewWriter(io.Discard, dl2)
	data := make([]byte, size)

	var wg sync.WaitGroup
	wg.Add(2)

	start := time.Now()
	go func() {
		defer wg.Done()
		_, _ = w1.Write(data)
	}()
	go func() {
		defer wg.Done()
		_, _ = w2.Write(data)
	}()
	wg.Wait()
	elapsed := time.Since(start)

	// 256KB combined at 1MB/s ≈ 250ms.
	const minExpected = 200 * time.Millisecond
	if elapsed < minExpected {
		t.Fatalf("grouped writes completed too fast: %v (expected >= %v)", elapsed, minExpected)
	}

	g.Release("group-a")
	g.Release("group-a")
}

func TestGroupRegistryUpdateRate(t *testing.T) {
	g := NewGroupRegistry()

	dl, _ := g.Acquire("user-x", 1_000_000, 500_000)
	if dl == nil {
		t.Fatal("bucket should not be nil")
	}

	// Update rate.
	g.UpdateRate("user-x", 2_000_000, 0)

	dl.mu.Lock()
	expectedRate := float64(2_000_000) / 1e9
	if dl.rate != expectedRate {
		t.Fatalf("rate = %v, want %v", dl.rate, expectedRate)
	}
	dl.mu.Unlock()

	// UpdateRate on non-existent key is no-op.
	g.UpdateRate("nonexistent", 1_000_000, 1_000_000)

	g.Release("user-x")
}
