package proxy

import (
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

// ServeTunnel relays the datagrams arriving on pc to target (UDP tunnel mode,
// like ss-tunnel -u) until pc is closed. Every client address is an
// association; replies go back to it from pc.
func (u *UDPLocal) ServeTunnel(pc *net.UDPConn, target string) error {
	raw, err := EncodeAddr(target)
	if err != nil {
		return err
	}
	type flow struct {
		id   uint64
		last atomic.Int64
	}
	var mu sync.Mutex
	flows := make(map[netip.AddrPort]*flow)
	done := make(chan struct{})
	defer func() {
		close(done)
		mu.Lock()
		for _, f := range flows {
			u.Unregister(f.id, true)
		}
		mu.Unlock()
	}()
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
			}
			old := time.Now().Add(-udpIdle).UnixNano()
			mu.Lock()
			for a, f := range flows {
				if f.last.Load() < old {
					delete(flows, a)
					u.Unregister(f.id, true)
				}
			}
			mu.Unlock()
		}
	}()
	buf := make([]byte, 65536)
	for {
		n, from, err := pc.ReadFromUDPAddrPort(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			// e.g. Windows reports ICMP port unreachable for an earlier
			// reply as a read error
			u.Log.Debug("udp tunnel read", "err", err)
			continue
		}
		mu.Lock()
		f := flows[from]
		if f == nil {
			f = &flow{}
			f.id = u.Register(func(msg []byte) {
				f.last.Store(time.Now().UnixNano())
				if al := addrLen(msg); al >= 0 {
					pc.WriteToUDPAddrPort(msg[al:], from)
				}
			})
			flows[from] = f
			u.Log.Debug("udp tunnel", "from", from.String(), "target", target, "id", f.id)
		}
		mu.Unlock()
		f.last.Store(time.Now().UnixNano())
		if err := u.Send(f.id, raw, buf[:n]); err != nil {
			u.Log.Debug("udp send", "target", target, "err", err)
		}
	}
}
