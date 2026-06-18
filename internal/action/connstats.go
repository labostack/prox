package action

import (
	"context"
	"io"
	"sync/atomic"
)

// ConnStats accumulates byte counts from hijacked connection relays.
// Used only for WebSocket and CONNECT paths where ResponseCapture
// is bypassed after hijack.
type ConnStats struct {
	BytesRx int64 // atomic: client → upstream
	BytesTx int64 // atomic: upstream → client
}

// AddRx atomically adds to the received byte counter.
func (cs *ConnStats) AddRx(n int64) { atomic.AddInt64(&cs.BytesRx, n) }

// AddTx atomically adds to the transmitted byte counter.
func (cs *ConnStats) AddTx(n int64) { atomic.AddInt64(&cs.BytesTx, n) }

// LoadRx atomically loads the received byte counter.
func (cs *ConnStats) LoadRx() int64 { return atomic.LoadInt64(&cs.BytesRx) }

// LoadTx atomically loads the transmitted byte counter.
func (cs *ConnStats) LoadTx() int64 { return atomic.LoadInt64(&cs.BytesTx) }

type connStatsKey struct{}

// NewConnStatsContext returns a context carrying the given ConnStats.
func NewConnStatsContext(ctx context.Context, cs *ConnStats) context.Context {
	return context.WithValue(ctx, connStatsKey{}, cs)
}

// ConnStatsFromContext extracts ConnStats from the context, or nil.
func ConnStatsFromContext(ctx context.Context) *ConnStats {
	cs, _ := ctx.Value(connStatsKey{}).(*ConnStats)
	return cs
}

// CountingReader wraps an io.ReadCloser and counts bytes read.
// Not safe for concurrent use — r.Body is read by a single goroutine.
type CountingReader struct {
	rc io.ReadCloser
	N  int64
}

// Read implements io.Reader with byte counting.
func (cr *CountingReader) Read(p []byte) (int, error) {
	if cr.rc == nil {
		return 0, io.EOF
	}
	nr, err := cr.rc.Read(p)
	cr.N += int64(nr)
	return nr, err
}

// Close implements io.Closer.
func (cr *CountingReader) Close() error {
	if cr.rc == nil {
		return nil
	}
	return cr.rc.Close()
}

// Reset reinitializes the CountingReader for reuse from a pool.
func (cr *CountingReader) Reset(rc io.ReadCloser) {
	cr.rc = rc
	cr.N = 0
}
