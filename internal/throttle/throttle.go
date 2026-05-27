// Package throttle implements token-bucket bandwidth limiting for proxy connections.
package throttle

import (
	"context"
	"io"
	"sync"
	"time"
)

// maxChunk is the largest write chunk passed through a throttled Writer.
const maxChunk = 32 * 1024

// Bucket is a goroutine-safe token-bucket rate limiter.
type Bucket struct {
	mu     sync.Mutex
	rate   float64 // tokens per nanosecond
	tokens float64
	max    float64
	last   int64 // UnixNano of last refill
}

// NewBucket creates a rate limiter allowing bytesPerSec throughput.
// Returns nil if bytesPerSec <= 0.
func NewBucket(bytesPerSec int64) *Bucket {
	if bytesPerSec <= 0 {
		return nil
	}
	// Burst = 2x max write chunk. Small enough to avoid initial-burst bypass
	// at high rates, large enough for smooth throughput at low rates.
	burst := int64(2 * maxChunk)
	if burst < 64*1024 {
		burst = 64 * 1024
	}
	return &Bucket{
		rate:   float64(bytesPerSec) / 1e9,
		tokens: float64(burst),
		max:    float64(burst),
		last:   time.Now().UnixNano(),
	}
}

// Wait consumes n tokens, sleeping if the bucket is empty.
func (b *Bucket) Wait(n int) {
	b.mu.Lock()
	now := time.Now().UnixNano()
	elapsed := now - b.last
	b.last = now
	b.tokens += float64(elapsed) * b.rate
	if b.tokens > b.max {
		b.tokens = b.max
	}
	b.tokens -= float64(n)
	deficit := b.tokens
	b.mu.Unlock()

	if deficit < 0 {
		time.Sleep(time.Duration(-deficit / b.rate))
	}
}

// Writer throttles writes through one or more Buckets.
type Writer struct {
	w       io.Writer
	buckets []*Bucket
}

// NewWriter wraps w with bandwidth limiting. Nil buckets are filtered out.
// Returns nil if no valid buckets remain (caller should use the original writer).
func NewWriter(w io.Writer, buckets ...*Bucket) *Writer {
	valid := make([]*Bucket, 0, len(buckets))
	for _, b := range buckets {
		if b != nil {
			valid = append(valid, b)
		}
	}
	if len(valid) == 0 {
		return nil
	}
	return &Writer{w: w, buckets: valid}
}

// Write splits p into chunks, throttling each through all buckets.
func (tw *Writer) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		chunk := len(p)
		if chunk > maxChunk {
			chunk = maxChunk
		}
		for _, b := range tw.buckets {
			b.Wait(chunk)
		}
		n, err := tw.w.Write(p[:chunk])
		total += n
		if err != nil {
			return total, err
		}
		p = p[chunk:]
	}
	return total, nil
}

// Reader throttles reads through one or more Buckets.
type Reader struct {
	r       io.Reader
	buckets []*Bucket
}

// NewReader wraps r with bandwidth limiting. Nil buckets are filtered out.
// Returns nil if no valid buckets remain (caller should use the original reader).
func NewReader(r io.Reader, buckets ...*Bucket) *Reader {
	valid := make([]*Bucket, 0, len(buckets))
	for _, b := range buckets {
		if b != nil {
			valid = append(valid, b)
		}
	}
	if len(valid) == 0 {
		return nil
	}
	return &Reader{r: r, buckets: valid}
}

// Read reads from the underlying reader, then throttles based on bytes read.
func (tr *Reader) Read(p []byte) (int, error) {
	n, err := tr.r.Read(p)
	if n > 0 {
		for _, b := range tr.buckets {
			b.Wait(n)
		}
	}
	return n, err
}

// SetRate updates the bucket's throughput rate.
// Safe to call concurrently with Wait.
func (b *Bucket) SetRate(bytesPerSec int64) {
	if b == nil || bytesPerSec <= 0 {
		return
	}
	b.mu.Lock()
	b.rate = float64(bytesPerSec) / 1e9
	b.mu.Unlock()
}

// MbpsToBytes converts megabits per second to bytes per second.
func MbpsToBytes(mbps float64) int64 {
	return int64(mbps * 1_000_000 / 8)
}

// groupEntry holds shared buckets and a reference count for one group key.
type groupEntry struct {
	download *Bucket
	upload   *Bucket
	refs     int32
}

// GroupRegistry manages shared bandwidth buckets keyed by a group identifier.
// All connections with the same group key share a single download/upload budget.
type GroupRegistry struct {
	mu     sync.Mutex
	groups map[string]*groupEntry
}

// NewGroupRegistry creates an empty group registry.
func NewGroupRegistry() *GroupRegistry {
	return &GroupRegistry{groups: make(map[string]*groupEntry)}
}

// Acquire returns shared download/upload buckets for the given key,
// creating them on first call. Each Acquire must be paired with Release.
// If rates differ from the existing entry, the bucket rates are updated.
func (g *GroupRegistry) Acquire(key string, dlRate, ulRate int64) (dl, ul *Bucket) {
	g.mu.Lock()
	defer g.mu.Unlock()

	e, ok := g.groups[key]
	if !ok {
		e = &groupEntry{
			download: NewBucket(dlRate),
			upload:   NewBucket(ulRate),
		}
		g.groups[key] = e
	} else {
		// Update rates if changed (e.g. plan upgrade/downgrade).
		if e.download != nil && dlRate > 0 {
			e.download.SetRate(dlRate)
		}
		if e.upload != nil && ulRate > 0 {
			e.upload.SetRate(ulRate)
		}
	}
	e.refs++
	return e.download, e.upload
}

// Release decrements the reference count for the given key.
// When the count reaches zero, the entry is removed.
func (g *GroupRegistry) Release(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	e, ok := g.groups[key]
	if !ok {
		return
	}
	e.refs--
	if e.refs <= 0 {
		delete(g.groups, key)
	}
}

// UpdateRate changes the download/upload rates for an existing group.
// Non-positive values are ignored. No-op if the group does not exist.
func (g *GroupRegistry) UpdateRate(key string, dlRate, ulRate int64) {
	g.mu.Lock()
	defer g.mu.Unlock()

	e, ok := g.groups[key]
	if !ok {
		return
	}
	if dlRate > 0 && e.download != nil {
		e.download.SetRate(dlRate)
	}
	if ulRate > 0 && e.upload != nil {
		e.upload.SetRate(ulRate)
	}
}

// Limits holds per-direction bandwidth buckets for a connection.
type Limits struct {
	Download []*Bucket
	Upload   []*Bucket
}

type ctxKey struct{}

// NewContext returns a child context carrying the given Limits.
func NewContext(ctx context.Context, l *Limits) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}

// FromContext extracts Limits from the context, or nil if not set.
func FromContext(ctx context.Context) *Limits {
	l, _ := ctx.Value(ctxKey{}).(*Limits)
	return l
}
