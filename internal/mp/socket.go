package mp

import (
	"context"
	"net"
	"net/netip"
	"strconv"
)

// listenUDP opens a UDP socket of the multipath connection (see
// Options.Control).
func (o *Options) listenUDP(ctx context.Context, network, addr string) (*net.UDPConn, error) {
	lc := net.ListenConfig{Control: o.Control}
	pc, err := lc.ListenPacket(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	return pc.(*net.UDPConn), nil
}

// resolveUDP is net.ResolveUDPAddr whose DNS queries, with Options.Control,
// go out on sockets it has seen: a name is then looked up by Go's own
// resolver, not the system's.
func (o *Options) resolveUDP(ctx context.Context, addr string) (*net.UDPAddr, error) {
	if o.Control == nil {
		return net.ResolveUDPAddr("udp", addr)
	}
	host, ps, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if _, err := netip.ParseAddr(host); err == nil || host == "" {
		return net.ResolveUDPAddr("udp", addr)
	}
	port, err := strconv.ParseUint(ps, 10, 16)
	if err != nil {
		return nil, &net.AddrError{Err: "invalid port", Addr: addr}
	}
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		d := net.Dialer{Control: o.Control}
		return d.DialContext(ctx, network, address)
	}}
	ips, err := r.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	// like net.ResolveUDPAddr: the first IPv4 address, else the first
	ip := ips[0]
	for _, a := range ips {
		if a.Unmap().Is4() {
			ip = a
			break
		}
	}
	return net.UDPAddrFromAddrPort(netip.AddrPortFrom(ip.Unmap(), uint16(port))), nil
}
