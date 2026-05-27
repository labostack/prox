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
