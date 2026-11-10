// Package ipstack terminates the IP traffic of the local side's packet
// front-ends (the WireGuard listener and the TUN device) and proxies it
// transparently over the multipath connection. TCP connections are
// terminated by a gVisor network stack and relayed as streams (like SOCKS5
// CONNECT), UDP datagrams travel as UDP associations (full-cone: one
// association per client socket) and ICMP echo requests are relayed to the
// remote, which sends the pings.
//
// A front-end hands the packets of its clients to Inject and sends the
// packets it reads from Out back to them.
package ipstack

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"

	"mpquic/internal/mp"
	"mpquic/internal/proxy"
)

const nicID = 1

// outQueue is the number of packets queued for the clients.
const outQueue = 1024

const (
	udpIdle  = 2 * time.Minute
	dnsIdle  = 15 * time.Second // flows that only did DNS
	echoIdle = 30 * time.Second
)

// Options configure a Stack.
type Options struct {
	// Name of the front-end in log messages, e.g. "wireguard".
	Name string
	// MTU of the link to the clients.
	MTU    int
	Opener proxy.Opener
	// UDP relays UDP and ICMP echo; nil rejects UDP (ICMP port
	// unreachable) and drops pings.
	UDP *proxy.UDPLocal
	Log *slog.Logger
}

// Stack is a userspace network stack in promiscuous mode, so it terminates
// TCP connections to any address. ICMP echo requests bypass gVisor. Packets
// to the clients come from gVisor and from the relayed UDP and ICMP.
type Stack struct {
	opt       Options
	log       *slog.Logger
	stack     *stack.Stack
	ep        *channel.Endpoint
	out       chan []byte
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once

	echoBase    uint16 // first echo flow id (see proxy.UDPLocal.EchoReceiver)
	echoRelease func()

	mu       sync.Mutex
	udpFlows map[netip.AddrPort]*udpFlow
	echoes   map[echoKey]*echoFlow
	byFlow   map[uint16]*echoFlow
	nextFlow uint16
}

type udpFlow struct {
	id     uint64
	client netip.AddrPort
	last   atomic.Int64
	nonDNS atomic.Bool // sent to a port other than 53
}

type echoKey struct {
	client, target netip.Addr
	id             uint16
}

type echoFlow struct {
	echoKey
	flow uint16
	last atomic.Int64
}

// New starts a stack.
func New(opt Options) (*Stack, error) {
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	if opt.Name == "" {
		opt.Name = "ipstack"
	}
	gs := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	opts := []tcpip.SettableTransportProtocolOption{
		ptr(tcpip.TCPSACKEnabled(true)),
		ptr(tcpip.CongestionControlOption("cubic")),
		ptr(tcpip.TCPModerateReceiveBufferOption(true)),
		// Classic SACK loss recovery (RFC 6675) instead of gVisor's default
		// RACK. After a retransmission timeout, the first ACK makes RACK
		// declare every older unacknowledged segment lost (they were sent
		// before the retransmission) and re-enter SACK recovery, which cuts
		// the window again: the sender then sends one or two segments per
		// timeout (~200 ms). A burst loss of 100+ KB, e.g. a UDP socket
		// buffer overflowing, stalled a connection for many seconds.
		ptr(tcpip.TCPRecovery(0)),
		&tcpip.TCPReceiveBufferSizeRangeOption{Min: tcp.MinBufferSize, Default: tcp.DefaultReceiveBufferSize, Max: 8 << 20},
		&tcpip.TCPSendBufferSizeRangeOption{Min: tcp.MinBufferSize, Default: tcp.DefaultSendBufferSize, Max: 8 << 20},
	}
	for _, o := range opts {
		if err := gs.SetTransportProtocolOption(tcp.ProtocolNumber, o); err != nil {
			gs.Close()
			return nil, wrapErr("tcp option", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Stack{
		opt:      opt,
		log:      opt.Log,
		stack:    gs,
		ep:       channel.New(512, uint32(opt.MTU), ""),
		out:      make(chan []byte, outQueue),
		ctx:      ctx,
		cancel:   cancel,
		udpFlows: make(map[netip.AddrPort]*udpFlow),
		echoes:   make(map[echoKey]*echoFlow),
		byFlow:   make(map[uint16]*echoFlow),
	}
	if err := gs.CreateNIC(nicID, s.ep); err != nil {
		s.Close()
		return nil, wrapErr("create NIC", err)
	}
	// Accept packets to any address, and send from any address.
	if err := gs.SetPromiscuousMode(nicID, true); err != nil {
		s.Close()
		return nil, wrapErr("promiscuous mode", err)
	}
	if err := gs.SetSpoofing(nicID, true); err != nil {
		s.Close()
		return nil, wrapErr("spoofing", err)
	}
	gs.SetRouteTable([]tcpip.Route{
		{Destination: header.IPv4EmptySubnet, NIC: nicID},
		{Destination: header.IPv6EmptySubnet, NIC: nicID},
	})
	fwd := tcp.NewForwarder(gs, 0, 1024, s.handleTCP)
	gs.SetTransportProtocolHandler(tcp.ProtocolNumber, fwd.HandlePacket)
	gs.SetTransportProtocolHandler(udp.ProtocolNumber, s.handleUDP)
	if opt.UDP != nil {
		base, release, err := opt.UDP.EchoReceiver(s.echoReply)
		if err != nil {
			s.Close()
			return nil, err
		}
		s.echoBase, s.echoRelease = base, release
	}
	go s.pump()
	go s.reaper()
	return s, nil
}

func ptr[T any](v T) *T { return &v }

type tcpipError struct {
	what string
	err  tcpip.Error
}

func (e *tcpipError) Error() string { return e.what + ": " + e.err.String() }

func wrapErr(what string, err tcpip.Error) error { return &tcpipError{what, err} }

// MTU returns the MTU of the link to the clients.
func (s *Stack) MTU() int { return s.opt.MTU }

// Out delivers the packets for the clients.
func (s *Stack) Out() <-chan []byte { return s.out }

// Done is closed when the stack is closed.
func (s *Stack) Done() <-chan struct{} { return s.ctx.Done() }

// Close stops the stack.
func (s *Stack) Close() {
	s.closeOnce.Do(func() {
		s.cancel()
		s.stack.Close()
		s.ep.Close()
		if s.echoRelease != nil {
			s.echoRelease()
		}
		s.mu.Lock()
		flows := s.udpFlows
		s.udpFlows = map[netip.AddrPort]*udpFlow{}
		s.mu.Unlock()
		for _, f := range flows {
			s.opt.UDP.Unregister(f.id, true)
		}
	})
}

// pump moves gVisor's outgoing packets to the queue read by the front-end.
func (s *Stack) pump() {
	for {
		pkt := s.ep.ReadContext(s.ctx)
		if pkt == nil {
			return
		}
		v := pkt.ToView()
		b := v.ToSlice()
		v.Release()
		pkt.DecRef()
		select {
		case s.out <- b:
		case <-s.ctx.Done():
			return
		}
	}
}

// send queues packets for the clients, dropping them if the queue is full
// (as a congested link would).
func (s *Stack) send(pkts ...[]byte) {
	for _, p := range pkts {
		select {
		case s.out <- p:
		default:
		}
	}
}

// Inject takes an IP packet from a client. p is not retained.
func (s *Stack) Inject(p []byte) {
	if len(p) == 0 || s.ctx.Err() != nil || s.intercept(p) {
		return
	}
	var proto tcpip.NetworkProtocolNumber
	switch p[0] >> 4 {
	case 4:
		proto = header.IPv4ProtocolNumber
	case 6:
		proto = header.IPv6ProtocolNumber
	default:
		return
	}
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(p)})
	s.ep.InjectInbound(proto, pkt)
	pkt.DecRef()
}

func toAddr(a tcpip.Address) netip.Addr {
	ip, _ := netip.AddrFromSlice(a.AsSlice())
	return ip
}

// relayable reports whether traffic to ip is forwarded to the remote.
func relayable(ip netip.Addr) bool {
	return ip.IsValid() && !ip.IsUnspecified() && !ip.IsMulticast() && ip != netip.AddrFrom4([4]byte{255, 255, 255, 255})
}

// abortConn is a gVisor TCP connection that proxy.Pipe can reset.
type abortConn struct {
	*gonet.TCPConn
	ep tcpip.Endpoint
}

func (c abortConn) Abort() { c.ep.Abort() }

func (s *Stack) handleTCP(r *tcp.ForwarderRequest) {
	id := r.ID()
	dst := netip.AddrPortFrom(toAddr(id.LocalAddress), id.LocalPort)
	if !relayable(dst.Addr()) {
		r.Complete(true)
		return
	}
	st, err := s.opt.Opener.OpenStream()
	if err != nil {
		s.log.Debug(s.opt.Name+": cannot open stream", "target", dst, "err", err)
		r.Complete(true)
		return
	}
	if _, err := st.Write(proxy.AppendAddrPort(nil, dst)); err != nil {
		st.Reset(mp.ResetGeneric)
		st.Close()
		r.Complete(true)
		return
	}
	var wq waiter.Queue
	ep, terr := r.CreateEndpoint(&wq)
	if terr != nil {
		st.Reset(mp.ResetGeneric)
		st.Close()
		r.Complete(true)
		return
	}
	r.Complete(false)
	ep.SocketOptions().SetDelayOption(false)
	s.log.Debug(s.opt.Name+" connect", "from", netip.AddrPortFrom(toAddr(id.RemoteAddress), id.RemotePort), "target", dst, "stream", st.ID())
	proxy.Pipe(abortConn{gonet.NewTCPConn(&wq, ep), ep}, st)
}

func (s *Stack) handleUDP(id stack.TransportEndpointID, pkt *stack.PacketBuffer) bool {
	hdr := header.UDP(pkt.TransportHeader().Slice())
	nh := pkt.Network()
	lenOK, csumOK := header.UDPValid(hdr, func() uint16 { return pkt.Data().Checksum() }, uint16(pkt.Data().Size()),
		pkt.NetworkProtocolNumber, nh.SourceAddress(), nh.DestinationAddress(), pkt.RXChecksumValidated)
	if !lenOK || !csumOK {
		return true
	}
	src := netip.AddrPortFrom(toAddr(id.RemoteAddress), id.RemotePort)
	dst := netip.AddrPortFrom(toAddr(id.LocalAddress), id.LocalPort)
	if !relayable(dst.Addr()) {
		return true
	}
	if s.opt.UDP == nil {
		s.send(unreachable(dst.Addr(), src.Addr(), packetBytes(pkt)))
		return true
	}
	f := s.udpFlow(src)
	f.last.Store(time.Now().UnixNano())
	if dst.Port() != 53 {
		f.nonDNS.Store(true)
	}
	err := s.opt.UDP.Send(f.id, proxy.AppendAddrPort(nil, dst), pkt.Data().AsRange().ToSlice())
	var tl *mp.DatagramTooLargeError
	if errors.As(err, &tl) {
		s.packetTooBig(pkt, src.Addr(), dst.Addr(), tl.Max)
	} else if err != nil {
		s.log.Debug(s.opt.Name+": udp send", "target", dst, "err", err)
	}
	return true
}

// packetTooBig tells the client that the tunnel dropped its datagram as too
// large, so that its path MTU discovery can adapt (like a router would).
func (s *Stack) packetTooBig(pkt *stack.PacketBuffer, client, target netip.Addr, maxPayload int) {
	nh := pkt.NetworkHeader().Slice()
	orig := packetBytes(pkt)
	mtu := maxPayload + 8 // UDP header
	if target.Is4() {
		if header.IPv4(nh).Flags()&header.IPv4FlagDontFragment == 0 {
			return
		}
		mtu += header.IPv4MinimumSize
	} else {
		mtu += header.IPv6MinimumSize
		if mtu < header.IPv6MinimumMTU {
			return
		}
	}
	s.send(tooBig(target, client, orig, mtu))
}

// packetBytes returns the IP packet of a parsed packet buffer.
func packetBytes(pkt *stack.PacketBuffer) []byte {
	b := append([]byte(nil), pkt.NetworkHeader().Slice()...)
	b = append(b, pkt.TransportHeader().Slice()...)
	return append(b, pkt.Data().AsRange().ToSlice()...)
}

func (s *Stack) udpFlow(src netip.AddrPort) *udpFlow {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f := s.udpFlows[src]; f != nil {
		return f
	}
	f := &udpFlow{client: src}
	f.id = s.opt.UDP.Register(func(msg []byte) { s.udpReply(f, msg) })
	s.udpFlows[src] = f
	s.log.Debug(s.opt.Name+" udp flow", "from", src, "id", f.id)
	return f
}

func (s *Stack) udpReply(f *udpFlow, msg []byte) {
	from, n, ok := proxy.ParseAddrPort(msg)
	if !ok {
		return
	}
	from = netip.AddrPortFrom(from.Addr().Unmap(), from.Port())
	if from.Addr().Is4() != f.client.Addr().Is4() {
		return
	}
	f.last.Store(time.Now().UnixNano())
	s.send(udpPackets(from, f.client, msg[n:], s.opt.MTU)...)
}

// intercept handles the ICMP echo requests from the clients.
func (s *Stack) intercept(p []byte) bool {
	e, isEcho, ok := parseEcho(p)
	if !isEcho {
		return false
	}
	if !ok || s.opt.UDP == nil || !relayable(e.dst) {
		return true
	}
	sess := s.opt.Opener.Session()
	if sess == nil {
		return true
	}
	k := echoKey{e.src, e.dst, e.id}
	s.mu.Lock()
	f := s.echoes[k]
	if f == nil {
		for range proxy.EchoFlows {
			s.nextFlow = (s.nextFlow + 1) % proxy.EchoFlows
			id := s.echoBase + s.nextFlow
			if s.byFlow[id] == nil {
				f = &echoFlow{echoKey: k, flow: id}
				s.echoes[k] = f
				s.byFlow[id] = f
				break
			}
		}
	}
	s.mu.Unlock()
	if f == nil {
		return true
	}
	f.last.Store(time.Now().UnixNano())
	msg, hdr := proxy.AppendEcho(nil, e.dst, f.flow, e.seq, e.data)
	sess.SendDatagram(msg, hdr)
	return true
}

// echoReply receives an echo reply from the remote.
func (s *Stack) echoReply(b []byte) {
	_, flow, seq, data, ok := proxy.ParseEcho(b)
	if !ok {
		return
	}
	s.mu.Lock()
	f := s.byFlow[flow]
	s.mu.Unlock()
	if f == nil {
		return
	}
	s.send(echoReply(f.target, f.client, f.id, seq, data))
}

func (s *Stack) reaper() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		now := time.Now().UnixNano()
		var expired []uint64
		s.mu.Lock()
		for k, f := range s.udpFlows {
			idle := udpIdle
			if !f.nonDNS.Load() {
				idle = dnsIdle
			}
			if now-f.last.Load() > int64(idle) {
				expired = append(expired, f.id)
				delete(s.udpFlows, k)
			}
		}
		for k, f := range s.echoes {
			if now-f.last.Load() > int64(echoIdle) {
				delete(s.echoes, k)
				delete(s.byFlow, f.flow)
			}
		}
		s.mu.Unlock()
		for _, id := range expired {
			s.opt.UDP.Unregister(id, true)
		}
	}
}
