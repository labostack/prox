package action

import (
	"net/http"
)

// Drop aborts the connection without sending any HTTP response.
// Uses http.ErrAbortHandler which is handled by Go's HTTP server:
//   - HTTP/1.1: connection is closed with no response written
//   - HTTP/2: stream is reset (RST_STREAM)
type Drop struct{}

func (d *Drop) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	panic(http.ErrAbortHandler)
}
