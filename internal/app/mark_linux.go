package app

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// socketMark returns the mp.Options.Control that sets the firewall mark of
// a socket (SO_MARK; needs CAP_NET_ADMIN), or nil for no mark.
func socketMark(mark uint32) func(network, address string, c syscall.RawConn) error {
	if mark == 0 {
		return nil
	}
	return func(_, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK, int(mark))
		}); err != nil {
			return err
		}
		if serr != nil {
			return &os.SyscallError{Syscall: "setsockopt SO_MARK (--fwmark needs root or CAP_NET_ADMIN)", Err: serr}
		}
		return nil
	}
}
