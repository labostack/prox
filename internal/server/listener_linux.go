//go:build linux

package server

import (
	"context"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// reusePortListen creates a net.Listener with SO_REUSEPORT and TCP_DEFER_ACCEPT enabled.
func reusePortListen(network, addr string) (net.Listener, error) {
	lc := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			var opterr error
			err := c.Control(func(fd uintptr) {
				opterr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
				if opterr != nil {
					return
				}
				// 15 seconds wait timeout for incoming data before accepting anyway
				opterr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_DEFER_ACCEPT, 15)
			})
			if err != nil {
				return err
			}
			return opterr
		},
	}
	return lc.Listen(context.Background(), network, addr)
}
