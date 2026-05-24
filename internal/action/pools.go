package action

import (
	"bufio"
	"sync"
)

const copyBufSize = 32 * 1024

var (
	// bufioReaderPool reuses bufio.Reader instances for upstream response reading.
	bufioReaderPool = sync.Pool{
		New: func() any { return bufio.NewReaderSize(nil, 4096) },
	}

	// copyBufPool reuses 32KB byte slices for streaming body copies.
	copyBufPool = sync.Pool{
		New: func() any {
			b := make([]byte, copyBufSize)
			return &b
		},
	}
)

// proxyBufPool implements httputil.BufferPool to provide reusable buffers
// for ReverseProxy response copying. Without this, each proxied request
// allocates a new 32KB buffer.
type proxyBufPool struct{}

func (proxyBufPool) Get() []byte {
	bp := copyBufPool.Get().(*[]byte)
	return *bp
}

func (proxyBufPool) Put(b []byte) {
	copyBufPool.Put(&b)
}
