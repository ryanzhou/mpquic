package wg

import (
	"net"
	"net/netip"
	"sync"

	"golang.zx2c4.com/wireguard/conn"
)

// addrBind is a conn.Bind on one UDP socket bound to a specific address.
// (conn.NewDefaultBind, used for wildcard listen addresses, always binds to
// all interfaces.)
type addrBind struct {
	addr netip.Addr

	mu sync.Mutex
	c  *net.UDPConn
}

type endpoint netip.AddrPort

func (e endpoint) ClearSrc()           {}
func (e endpoint) SrcToString() string { return "" }
func (e endpoint) DstToString() string { return netip.AddrPort(e).String() }
func (e endpoint) DstToBytes() []byte  { b, _ := netip.AddrPort(e).MarshalBinary(); return b }
func (e endpoint) DstIP() netip.Addr   { return netip.AddrPort(e).Addr() }
func (e endpoint) SrcIP() netip.Addr   { return netip.Addr{} }

func (b *addrBind) Open(port uint16) ([]conn.ReceiveFunc, uint16, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.c != nil {
		return nil, 0, conn.ErrBindAlreadyOpen
	}
	c, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(netip.AddrPortFrom(b.addr, port)))
	if err != nil {
		return nil, 0, err
	}
	b.c = c
	recv := func(bufs [][]byte, sizes []int, eps []conn.Endpoint) (int, error) {
		n, ap, err := c.ReadFromUDPAddrPort(bufs[0])
		if err != nil {
			return 0, err
		}
		sizes[0] = n
		eps[0] = endpoint(netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()))
		return 1, nil
	}
	return []conn.ReceiveFunc{recv}, uint16(c.LocalAddr().(*net.UDPAddr).Port), nil
}

func (b *addrBind) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.c == nil {
		return nil
	}
	err := b.c.Close()
	b.c = nil
	return err
}

func (b *addrBind) SetMark(uint32) error { return nil }

func (b *addrBind) Send(bufs [][]byte, ep conn.Endpoint) error {
	b.mu.Lock()
	c := b.c
	b.mu.Unlock()
	if c == nil {
		return net.ErrClosed
	}
	e, ok := ep.(endpoint)
	if !ok {
		return conn.ErrWrongEndpointType
	}
	for _, buf := range bufs {
		if _, err := c.WriteToUDPAddrPort(buf, netip.AddrPort(e)); err != nil {
			return err
		}
	}
	return nil
}

func (b *addrBind) ParseEndpoint(s string) (conn.Endpoint, error) {
	ap, err := netip.ParseAddrPort(s)
	if err != nil {
		return nil, err
	}
	return endpoint(netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())), nil
}

func (b *addrBind) BatchSize() int { return 1 }
