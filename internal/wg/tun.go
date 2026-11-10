package wg

import (
	"os"
	"sync"

	"golang.zx2c4.com/wireguard/tun"

	"mpquic/internal/ipstack"
)

// netTun is the tun.Device of the WireGuard device: the packets of the
// peers go to the userspace network stack, and its packets to the peers.
type netTun struct {
	st        *ipstack.Stack
	events    chan tun.Event
	closeOnce sync.Once
}

func newNetTun(st *ipstack.Stack) *netTun {
	t := &netTun{st: st, events: make(chan tun.Event, 1)}
	t.events <- tun.EventUp
	return t
}

func (t *netTun) File() *os.File           { return nil }
func (t *netTun) Name() (string, error)    { return "mpquic", nil }
func (t *netTun) MTU() (int, error)        { return t.st.MTU(), nil }
func (t *netTun) Events() <-chan tun.Event { return t.events }
func (t *netTun) BatchSize() int           { return 32 }

func (t *netTun) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	var p []byte
	select {
	case p = <-t.st.Out():
	case <-t.st.Done():
		return 0, os.ErrClosed
	}
	n := 0
	for {
		sizes[n] = copy(bufs[n][offset:], p)
		n++
		if n == len(bufs) {
			return n, nil
		}
		select {
		case p = <-t.st.Out():
		default:
			return n, nil
		}
	}
}

func (t *netTun) Write(bufs [][]byte, offset int) (int, error) {
	for _, b := range bufs {
		t.st.Inject(b[offset:])
	}
	return len(bufs), nil
}

func (t *netTun) Close() error {
	t.closeOnce.Do(func() {
		t.st.Close()
		close(t.events)
	})
	return nil
}
