package proxy

import (
	"encoding/binary"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apernet/quic-go/quicvarint"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"

	"mpquic/internal/mp"
)

// ICMP echo (ping) relaying, used by the packet front-ends (WireGuard, TUN). Echo requests
// and replies travel as datagrams on the reserved association 0:
//
//	0:varint  ATYP IP PORT(=0)  flow:u16  seq:u16  data
//
// The address is the target (request) or the replying host (reply). flow
// identifies an echo flow of the local side; the remote keeps one ICMP
// socket (or raw-socket identifier) per flow and replies on the same flow.
// Older remotes treat the datagram as UDP to port 0, which the OS rejects.
//
// The remote sends the pings with an unprivileged ICMP socket where the OS
// allows it (Linux with net.ipv4.ping_group_range, macOS) and falls back to
// a raw socket (root / CAP_NET_RAW, Windows administrator).

const (
	icmpAssoc = 0
	icmpIdle  = time.Minute
)

// EchoFlows is the number of echo flow ids of one receiver of echo replies.
const EchoFlows = 1 << 12

// EchoReceiver registers recv for the body of the ICMP echo datagrams from
// the remote that belong to its flows: the EchoFlows flow ids from base.
// Several front-ends (the WireGuard listener, the TUN device) can so relay
// pings over one session. release ends the registration.
func (u *UDPLocal) EchoReceiver(recv func(b []byte)) (base uint16, release func(), err error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for i := range u.echo {
		if u.echo[i] == nil {
			u.echo[i] = recv
			return uint16(i * EchoFlows), func() {
				u.mu.Lock()
				u.echo[i] = nil
				u.mu.Unlock()
			}, nil
		}
	}
	return 0, nil, errors.New("too many receivers of ICMP echo replies")
}

// AppendEcho appends an ICMP echo datagram to b. hdrLen is the length of
// everything before data.
func AppendEcho(b []byte, ip netip.Addr, flow, seq uint16, data []byte) (msg []byte, hdrLen int) {
	b = quicvarint.Append(b, icmpAssoc)
	b = AppendAddrPort(b, netip.AddrPortFrom(ip, 0))
	b = binary.BigEndian.AppendUint16(b, flow)
	b = binary.BigEndian.AppendUint16(b, seq)
	return append(b, data...), len(b)
}

// ParseEcho parses an ICMP echo datagram after its association id.
func ParseEcho(b []byte) (ip netip.Addr, flow, seq uint16, data []byte, ok bool) {
	ap, n, ok := ParseAddrPort(b)
	if !ok || len(b) < n+4 {
		return netip.Addr{}, 0, 0, nil, false
	}
	return ap.Addr(), binary.BigEndian.Uint16(b[n:]), binary.BigEndian.Uint16(b[n+2:]), b[n+4:], true
}

// AppendAddrPort appends ap as a SOCKS5-format address (ATYP IP PORT).
func AppendAddrPort(b []byte, ap netip.AddrPort) []byte {
	ip := ap.Addr().Unmap()
	if ip.Is4() {
		b = append(b, atypIPv4)
	} else {
		b = append(b, atypIPv6)
	}
	b = append(b, ip.AsSlice()...)
	return binary.BigEndian.AppendUint16(b, ap.Port())
}

// ParseAddrPort parses a SOCKS5-format IPv4 or IPv6 address at the start of
// b and returns it with its length. Domain names are rejected.
func ParseAddrPort(b []byte) (netip.AddrPort, int, bool) {
	if len(b) < 1 {
		return netip.AddrPort{}, 0, false
	}
	var n int
	switch b[0] {
	case atypIPv4:
		n = 1 + 4
	case atypIPv6:
		n = 1 + 16
	default:
		return netip.AddrPort{}, 0, false
	}
	if len(b) < n+2 {
		return netip.AddrPort{}, 0, false
	}
	ip, _ := netip.AddrFromSlice(b[1:n])
	return netip.AddrPortFrom(ip, binary.BigEndian.Uint16(b[n:])), n + 2, true
}

// icmpRemote sends the pings of the local side and relays the replies.
type icmpRemote struct {
	log *slog.Logger
	// listen opens ICMP sockets (see UDPRemote.SetICMPListener).
	listen func(network, addr string) (net.PacketConn, error)

	mu       sync.Mutex
	flows    map[echoKey]*echoFlow
	raw      [2]*rawPinger // per family (0: IPv4, 1: IPv6), opened on demand
	noRaw    [2]bool       // raw socket unavailable
	noDgram  [2]bool       // unprivileged ICMP sockets unavailable
	reported [2]bool       // warned that pings cannot be sent
}

type echoKey struct {
	s    *mp.Session
	flow uint16
	v6   bool
}

type echoFlow struct {
	key   echoKey
	conn  net.PacketConn // this flow's unprivileged socket, or nil (raw)
	rawID uint16         // echo identifier on the shared raw socket
	last  atomic.Int64
}

type rawPinger struct {
	conn   net.PacketConn
	byID   map[uint16]*echoFlow // guarded by icmpRemote.mu
	nextID uint16
}

func newICMPRemote(log *slog.Logger) *icmpRemote {
	return &icmpRemote{log: log, flows: make(map[echoKey]*echoFlow), listen: listenICMP}
}

func listenICMP(network, addr string) (net.PacketConn, error) {
	c, err := icmp.ListenPacket(network, addr)
	if err != nil {
		return nil, err // not a typed nil
	}
	return c, nil
}

// SetICMPListener replaces the function that opens ICMP sockets
// (icmp.ListenPacket with "udp4"/"udp6" for unprivileged sockets, else
// "ip4:icmp"/"ip6:ipv6-icmp"). For tests; call before any datagram arrives.
func (u *UDPRemote) SetICMPListener(listen func(network, addr string) (net.PacketConn, error)) {
	u.icmp.mu.Lock()
	u.icmp.listen = listen
	u.icmp.mu.Unlock()
}

func family(v6 bool) int {
	if v6 {
		return 1
	}
	return 0
}

func (u *icmpRemote) handle(s *mp.Session, b []byte) {
	ip, flow, seq, data, ok := ParseEcho(b)
	if !ok {
		return
	}
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsUnspecified() || ip.IsMulticast() {
		return
	}
	k := echoKey{s, flow, ip.Is6()}
	u.mu.Lock()
	f := u.flows[k]
	if f == nil {
		f = u.newFlowLocked(k)
	}
	var raw *rawPinger
	if f != nil && f.conn == nil {
		raw = u.raw[family(k.v6)]
	}
	u.mu.Unlock()
	if f == nil {
		return
	}
	f.last.Store(time.Now().UnixNano())
	var typ icmp.Type = ipv4.ICMPTypeEcho
	if k.v6 {
		typ = ipv6.ICMPTypeEchoRequest
	}
	// Marshal computes the ICMPv4 checksum; the OS computes ICMPv6 ones.
	// Unprivileged sockets replace the identifier with their own.
	m := icmp.Message{Type: typ, Body: &icmp.Echo{ID: int(f.rawID), Seq: int(seq), Data: data}}
	wb, err := m.Marshal(nil)
	if err != nil {
		return
	}
	if f.conn != nil {
		_, err = f.conn.WriteTo(wb, &net.UDPAddr{IP: ip.AsSlice()})
	} else {
		_, err = raw.conn.WriteTo(wb, &net.IPAddr{IP: ip.AsSlice()})
	}
	if err != nil {
		u.log.Debug("icmp send", "target", ip, "err", err)
	}
}

// newFlowLocked opens the socket of a new flow: an unprivileged ICMP socket
// if possible, else an identifier on the shared raw socket.
func (u *icmpRemote) newFlowLocked(k echoKey) *echoFlow {
	fam := family(k.v6)
	network, laddr := "udp4", "0.0.0.0"
	if k.v6 {
		network, laddr = "udp6", "::"
	}
	if !u.noDgram[fam] {
		c, err := u.listen(network, laddr)
		if err == nil {
			f := &echoFlow{key: k, conn: c}
			u.flows[k] = f
			go u.readLoop(c, k.v6, func(uint16) *echoFlow { return f })
			return f
		}
		u.log.Debug("unprivileged ICMP socket unavailable, trying a raw socket", "network", network, "err", err)
		u.noDgram[fam] = true
	}
	rp := u.rawLocked(k.v6)
	if rp == nil {
		return nil
	}
	for range 1 << 16 {
		rp.nextID++
		if rp.byID[rp.nextID] == nil {
			f := &echoFlow{key: k, rawID: rp.nextID}
			rp.byID[f.rawID] = f
			u.flows[k] = f
			return f
		}
	}
	return nil
}

func (u *icmpRemote) rawLocked(v6 bool) *rawPinger {
	fam := family(v6)
	if u.raw[fam] != nil || u.noRaw[fam] {
		return u.raw[fam]
	}
	network, laddr := "ip4:icmp", "0.0.0.0"
	if v6 {
		network, laddr = "ip6:ipv6-icmp", "::"
	}
	c, err := u.listen(network, laddr)
	if err != nil {
		u.noRaw[fam] = true
		if !u.reported[fam] {
			u.reported[fam] = true
			u.log.Warn("cannot relay ICMP echo (ping): no unprivileged ICMP socket and no raw socket (Linux: allow it with sysctl net.ipv4.ping_group_range, or grant CAP_NET_RAW)", "network", network, "err", err)
		}
		return nil
	}
	rp := &rawPinger{conn: c, byID: make(map[uint16]*echoFlow), nextID: uint16(time.Now().UnixNano())}
	u.raw[fam] = rp
	go u.readLoop(c, v6, func(id uint16) *echoFlow {
		u.mu.Lock()
		defer u.mu.Unlock()
		return rp.byID[id]
	})
	return rp
}

// readLoop relays the echo replies received on c to their flows.
func (u *icmpRemote) readLoop(c net.PacketConn, v6 bool, lookup func(id uint16) *echoFlow) {
	proto, reply := 1, icmp.Type(ipv4.ICMPTypeEchoReply)
	if v6 {
		proto, reply = 58, ipv6.ICMPTypeEchoReply
	}
	buf := make([]byte, 65536)
	for {
		n, peer, err := c.ReadFrom(buf)
		if err != nil {
			return
		}
		m, err := icmp.ParseMessage(proto, buf[:n])
		if err != nil || m.Type != reply {
			continue
		}
		echo, ok := m.Body.(*icmp.Echo)
		if !ok {
			continue
		}
		f := lookup(uint16(echo.ID))
		if f == nil || f.key.s.Context().Err() != nil {
			continue
		}
		var from netip.Addr
		switch a := peer.(type) {
		case *net.UDPAddr:
			from, _ = netip.AddrFromSlice(a.IP)
		case *net.IPAddr:
			from, _ = netip.AddrFromSlice(a.IP)
		}
		if !from.IsValid() {
			continue
		}
		f.last.Store(time.Now().UnixNano())
		msg, hdr := AppendEcho(nil, from.Unmap(), f.key.flow, uint16(echo.Seq), echo.Data)
		f.key.s.SendDatagram(msg, hdr)
	}
}

func (u *icmpRemote) reap(now int64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for k, f := range u.flows {
		if now-f.last.Load() > int64(icmpIdle) || k.s.Context().Err() != nil {
			if f.conn != nil {
				f.conn.Close()
			} else if rp := u.raw[family(k.v6)]; rp != nil {
				delete(rp.byID, f.rawID)
			}
			delete(u.flows, k)
		}
	}
}
