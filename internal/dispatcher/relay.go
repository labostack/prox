package dispatcher

import (
	"io"
	"net"
	"sync"
)

// Relay copies data bidirectionally between two connections.
// It blocks until both directions finish (EOF or error).
// Caller is responsible for closing both connections.
func Relay(client, upstream net.Conn) (rxBytes, txBytes int64) {
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		rxBytes, _ = io.Copy(upstream, client)
		// Signal to upstream that no more data is coming.
		if tc, ok := upstream.(halfCloser); ok {
			_ = tc.CloseWrite()
		}
	}()

	go func() {
		defer wg.Done()
		txBytes, _ = io.Copy(client, upstream)
		if tc, ok := client.(halfCloser); ok {
			_ = tc.CloseWrite()
		}
	}()

	wg.Wait()
	return
}

// halfCloser is implemented by connections that support half-close
// (e.g. *net.TCPConn). This signals EOF to the peer without closing
// the read side, enabling graceful shutdown of bidirectional streams.
type halfCloser interface {
	CloseWrite() error
}
