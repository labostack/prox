package action

import (
	"bufio"
	"net/http"
	"net/url"
	"sync"
)

const copyBufSize = 32 * 1024

// copyBuffer wraps a pre-allocated byte slice to prevent slice descriptor heap escapes.
type copyBuffer struct {
	bytes []byte
}

var (
	// bufioReaderPool reuses bufio.Reader instances for upstream response reading.
	bufioReaderPool = sync.Pool{
		New: func() any { return bufio.NewReaderSize(nil, 4096) },
	}

	// copyBufPool reuses 32KB byte slices for streaming body copies (fallback/ReverseProxy path).
	copyBufPool = sync.Pool{
		New: func() any {
			b := make([]byte, copyBufSize)
			return &b
		},
	}

	// fastCopyBufPool reuses copyBuffer structs for the zero-allocation fastStaticProxy path.
	fastCopyBufPool = sync.Pool{
		New: func() any {
			return &copyBuffer{bytes: make([]byte, copyBufSize)}
		},
	}

	// headerPool reuses http.Header maps on the fastStaticProxy path.
	headerPool = sync.Pool{
		New: func() any {
			return make(http.Header, 8)
		},
	}

	// urlPool reuses url.URL structs on the fastStaticProxy path.
	urlPool = sync.Pool{
		New: func() any {
			return new(url.URL)
		},
	}

	// requestPool reuses http.Request structs on the fastStaticProxy path.
	requestPool = sync.Pool{
		New: func() any {
			return new(http.Request)
		},
	}
)

// resetHeader clears all keys from the header map.
// In modern Go, this mapclear loop is optimized into a single fast runtime call.
func resetHeader(h http.Header) {
	for k := range h {
		delete(h, k)
	}
}

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
