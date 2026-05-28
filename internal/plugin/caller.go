package plugin

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/vmihailenco/msgpack/v5"
)

const (
	defaultPoolSize    = 16
	defaultCallTimeout = 5 * time.Second
	maxFrameSize       = 1 << 20 // 1 MB
)

// Caller manages a connection pool to a plugin's Unix socket.
// Each connection handles one request at a time — prox grabs a
// connection, writes a request frame, reads the response, and
// returns the connection to the pool. This gives natural concurrency
// without ID correlation or multiplexing.
type Caller struct {
	socketPath string
	pool       chan net.Conn
	timeout    time.Duration
	mu         sync.Mutex
	closed     bool
}

// NewCaller creates a Caller that connects to the given Unix socket.
func NewCaller(socketPath string, poolSize int, timeout time.Duration) *Caller {
	if poolSize <= 0 {
		poolSize = defaultPoolSize
	}
	if timeout <= 0 {
		timeout = defaultCallTimeout
	}
	return &Caller{
		socketPath: socketPath,
		pool:       make(chan net.Conn, poolSize),
		timeout:    timeout,
	}
}

// CallRequest sends an on_request hook and returns the plugin's verdict.
func (c *Caller) CallRequest(ctx context.Context, req *RequestInfo) (*AuthorizeResult, error) {
	envBytes, err := MarshalEnvelope(HookTypeRequest, req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	respBytes, err := c.call(ctx, envBytes)
	if err != nil {
		return nil, err
	}

	var result AuthorizeResult
	if err := msgpack.Unmarshal(respBytes, &result); err != nil {
		return nil, fmt.Errorf("unmarshal authorize result: %w", err)
	}
	return &result, nil
}

// CallResponse sends an on_response hook and returns response modifications.
func (c *Caller) CallResponse(ctx context.Context, req *RequestInfo, resp *UpstreamResponseInfo) (*ResponseModResult, error) {
	pair := &ResponsePair{Req: *req, Resp: *resp}
	envBytes, err := MarshalEnvelope(HookTypeResponse, pair)
	if err != nil {
		return nil, fmt.Errorf("marshal response pair: %w", err)
	}

	respBytes, err := c.call(ctx, envBytes)
	if err != nil {
		return nil, err
	}

	var result ResponseModResult
	if err := msgpack.Unmarshal(respBytes, &result); err != nil {
		return nil, fmt.Errorf("unmarshal response mod: %w", err)
	}
	return &result, nil
}

// CallConnect sends an on_connect hook and returns the plugin's verdict.
func (c *Caller) CallConnect(ctx context.Context, conn *ConnInfo) (*ConnResult, error) {
	envBytes, err := MarshalEnvelope(HookTypeConnect, conn)
	if err != nil {
		return nil, fmt.Errorf("marshal connect: %w", err)
	}

	respBytes, err := c.call(ctx, envBytes)
	if err != nil {
		return nil, err
	}

	var result ConnResult
	if err := msgpack.Unmarshal(respBytes, &result); err != nil {
		return nil, fmt.Errorf("unmarshal connect result: %w", err)
	}
	return &result, nil
}

// Fire sends a frame without waiting for a response (fire-and-forget).
// Used for disconnect notifications. The connection is returned to the pool
// after writing; the plugin's read loop simply advances to the next frame.
func (c *Caller) Fire(frame []byte) error {
	conn, err := c.getConn()
	if err != nil {
		return err
	}
	_ = conn.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
	if err := writeFrame(conn, frame); err != nil {
		_ = conn.SetWriteDeadline(time.Time{})
		c.discardConn(conn)
		return err
	}
	_ = conn.SetWriteDeadline(time.Time{})
	c.putConn(conn)
	return nil
}

// Close drains the connection pool and closes all connections.
func (c *Caller) Close() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()

	close(c.pool)
	for conn := range c.pool {
		conn.Close()
	}
}

// call performs the actual request-response over a pooled connection.
func (c *Caller) call(ctx context.Context, envBytes []byte) ([]byte, error) {
	conn, err := c.getConn()
	if err != nil {
		return nil, fmt.Errorf("get connection: %w", err)
	}

	// Apply context deadline to the connection.
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(c.timeout)
	}
	if err := conn.SetDeadline(deadline); err != nil {
		c.discardConn(conn)
		return nil, fmt.Errorf("set deadline: %w", err)
	}

	// Write length-prefixed frame.
	if err := writeFrame(conn, envBytes); err != nil {
		c.discardConn(conn)
		return nil, fmt.Errorf("write frame: %w", err)
	}

	// Read length-prefixed response.
	resp, err := readFrame(conn)
	if err != nil {
		c.discardConn(conn)
		return nil, fmt.Errorf("read frame: %w", err)
	}

	// Clear deadline and return to pool.
	_ = conn.SetDeadline(time.Time{})
	c.putConn(conn)

	return resp, nil
}

func (c *Caller) getConn() (net.Conn, error) {
	// Try to grab an idle connection.
	select {
	case conn := <-c.pool:
		return conn, nil
	default:
	}

	// Create a new connection.
	return net.DialTimeout("unix", c.socketPath, c.timeout)
}

func (c *Caller) putConn(conn net.Conn) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		conn.Close()
		return
	}
	c.mu.Unlock()

	select {
	case c.pool <- conn:
	default:
		// Pool is full — discard.
		conn.Close()
	}
}

func (c *Caller) discardConn(conn net.Conn) {
	conn.Close()
}

// --- Frame I/O (shared with SDK) ---

func writeFrame(w io.Writer, payload []byte) error {
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(payload)))
	if _, err := w.Write(lenBuf[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

func readFrame(r io.Reader) ([]byte, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return nil, err
	}

	n := binary.BigEndian.Uint32(lenBuf[:])
	if n > maxFrameSize {
		return nil, fmt.Errorf("frame too large: %d bytes", n)
	}

	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}
