// Package wg is the WireGuard front-end of the local side: a userspace
// WireGuard server whose peers' traffic is proxied transparently over the
// multipath connection. TCP connections are terminated by a gVisor network
// stack and relayed as streams (like SOCKS5 CONNECT), UDP datagrams travel
// as UDP associations (full-cone: one association per peer socket) and ICMP
// echo requests are relayed to the remote, which sends the pings.
package wg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"

	"mpquic/internal/mp"
	"mpquic/internal/proxy"
)

// DefaultMTU is the default MTU of the WireGuard interface (as wg-quick).
const DefaultMTU = 1420

// Peer is a WireGuard peer (client).
type Peer struct {
	PublicKey    string
	PresharedKey string   // optional
	AllowedIPs   []string // source addresses of the peer; default: all
}

// Options configure a Server.
type Options struct {
	// Listen is the UDP address of the WireGuard endpoint: "host:port" or
	// ":port" (all interfaces).
	Listen     string
	PrivateKey string
	Peers      []Peer
	MTU        int // default DefaultMTU
	Opener     proxy.Opener
	// UDP relays UDP and ICMP echo; nil rejects UDP (ICMP port
	// unreachable) and drops pings.
	UDP *proxy.UDPLocal
	Log *slog.Logger
}

// Server is a running WireGuard front-end.
type Server struct {
	opt  Options
	log  *slog.Logger
	tun  *netTun
	dev  *device.Device
	pub  Key
	port int
	done chan struct{}

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

const (
	udpIdle  = 2 * time.Minute
	dnsIdle  = 15 * time.Second // flows that only did DNS
	echoIdle = 30 * time.Second
)

// ParseListen normalizes a WireGuard listen address: "PORT", ":PORT" or
// "HOST:PORT".
func ParseListen(s string) (host string, port int, err error) {
	s = strings.TrimSpace(s)
	if p, e := strconv.Atoi(s); e == nil {
		host, port = "", p
	} else {
		h, ps, e := net.SplitHostPort(s)
		if e != nil {
			return "", 0, fmt.Errorf("WireGuard listen address %q: want [HOST:]PORT", s)
		}
		p, e := strconv.Atoi(ps)
		if e != nil {
			return "", 0, fmt.Errorf("WireGuard listen address %q: bad port", s)
		}
		host, port = h, p
	}
	if port < 0 || port > 65535 {
		return "", 0, fmt.Errorf("WireGuard listen port %d out of range", port)
	}
	if host != "" {
		if _, e := netip.ParseAddr(host); e != nil {
			return "", 0, fmt.Errorf("WireGuard listen address %q: host must be an IP address", s)
		}
	}
	return host, port, nil
}

// Check validates the options that do not depend on the runtime.
func (o *Options) Check() error {
	if _, _, err := ParseListen(o.Listen); err != nil {
		return err
	}
	if o.PrivateKey == "" {
		return errors.New("WireGuard private key is required")
	}
	if _, err := ParseKey(o.PrivateKey); err != nil {
		return fmt.Errorf("WireGuard private key: %v", err)
	}
	if len(o.Peers) == 0 {
		return errors.New("WireGuard needs at least one peer public key")
	}
	for i, p := range o.Peers {
		if _, err := ParseKey(p.PublicKey); err != nil {
			return fmt.Errorf("WireGuard peer %d public key: %v", i+1, err)
		}
		if p.PresharedKey != "" {
			if _, err := ParseKey(p.PresharedKey); err != nil {
				return fmt.Errorf("WireGuard peer %d preshared key: %v", i+1, err)
			}
		}
		if len(p.AllowedIPs) == 0 && len(o.Peers) > 1 {
			return fmt.Errorf("WireGuard peer %d: with several peers, every peer needs its allowed IPs (its tunnel addresses)", i+1)
		}
		for _, a := range p.AllowedIPs {
			if _, err := parsePrefix(a); err != nil {
				return fmt.Errorf("WireGuard peer %d: %v", i+1, err)
			}
		}
	}
	if o.MTU != 0 && (o.MTU < 1280 || o.MTU > 9000) {
		return fmt.Errorf("WireGuard MTU %d: must be between 1280 and 9000", o.MTU)
	}
	return nil
}

func parsePrefix(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if p, err := netip.ParsePrefix(s); err == nil {
		return p.Masked(), nil
	}
	if a, err := netip.ParseAddr(s); err == nil {
		return netip.PrefixFrom(a, a.BitLen()), nil
	}
	return netip.Prefix{}, fmt.Errorf("allowed IP %q: want an address or CIDR prefix", s)
}

// Start starts the WireGuard server.
func Start(opt Options) (*Server, error) {
	if err := opt.Check(); err != nil {
		return nil, err
	}
	if opt.MTU == 0 {
		opt.MTU = DefaultMTU
	}
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	priv, _ := ParseKey(opt.PrivateKey)
	host, port, _ := ParseListen(opt.Listen)
	s := &Server{
		opt:      opt,
		log:      opt.Log,
		pub:      priv.PublicKey(),
		done:     make(chan struct{}),
		udpFlows: make(map[netip.AddrPort]*udpFlow),
		echoes:   make(map[echoKey]*echoFlow),
		byFlow:   make(map[uint16]*echoFlow),
	}
	t, err := newNetTun(opt.MTU, s.intercept)
	if err != nil {
		return nil, err
	}
	s.tun = t
	fwd := tcp.NewForwarder(t.stack, 0, 1024, s.handleTCP)
	t.stack.SetTransportProtocolHandler(tcp.ProtocolNumber, fwd.HandlePacket)
	t.stack.SetTransportProtocolHandler(udp.ProtocolNumber, s.handleUDP)

	if opt.UDP != nil {
		opt.UDP.ICMP = s.echoReply
	}
	var bind conn.Bind
	if a, err := netip.ParseAddr(host); err == nil && !a.IsUnspecified() {
		bind = &addrBind{addr: a}
	} else {
		bind = conn.NewDefaultBind()
	}
	logf := func(lvl slog.Level) func(string, ...any) {
		return func(f string, args ...any) {
			s.log.Log(context.Background(), lvl, "wireguard: "+fmt.Sprintf(f, args...))
		}
	}
	s.dev = device.NewDevice(t, bind, &device.Logger{Verbosef: logf(slog.LevelDebug), Errorf: logf(slog.LevelDebug)})
	var uapi strings.Builder
	fmt.Fprintf(&uapi, "private_key=%s\nlisten_port=%d\n", priv.hex(), port)
	for _, p := range opt.Peers {
		pk, _ := ParseKey(p.PublicKey)
		fmt.Fprintf(&uapi, "public_key=%s\n", pk.hex())
		if p.PresharedKey != "" {
			psk, _ := ParseKey(p.PresharedKey)
			fmt.Fprintf(&uapi, "preshared_key=%s\n", psk.hex())
		}
		allowed := p.AllowedIPs
		if len(allowed) == 0 {
			allowed = []string{"0.0.0.0/0", "::/0"}
		}
		for _, a := range allowed {
			pfx, _ := parsePrefix(a)
			fmt.Fprintf(&uapi, "allowed_ip=%s\n", pfx)
		}
	}
	if err := s.dev.IpcSet(uapi.String()); err != nil {
		s.dev.Close()
		return nil, fmt.Errorf("WireGuard: %w", err)
	}
	if err := s.dev.Up(); err != nil {
		s.dev.Close()
		return nil, fmt.Errorf("WireGuard listener %s: %w", opt.Listen, err)
	}
	s.port = port
	if cfg, err := s.dev.IpcGet(); err == nil {
		for _, l := range strings.Split(cfg, "\n") {
			if v, ok := strings.CutPrefix(l, "listen_port="); ok {
				s.port, _ = strconv.Atoi(v)
			}
		}
	}
	go s.reaper()
	return s, nil
}

// PublicKey returns the server's public key (for the peers' configuration).
func (s *Server) PublicKey() string { return s.pub.String() }

// Port returns the UDP port the server listens on.
func (s *Server) Port() int { return s.port }

// Close stops the server.
func (s *Server) Close() {
	select {
	case <-s.done:
		return
	default:
	}
	close(s.done)
	s.dev.Close() // closes the tun too
	s.mu.Lock()
	flows := s.udpFlows
	s.udpFlows = map[netip.AddrPort]*udpFlow{}
	s.mu.Unlock()
	for _, f := range flows {
		s.opt.UDP.Unregister(f.id, true)
	}
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

func (s *Server) handleTCP(r *tcp.ForwarderRequest) {
	id := r.ID()
	dst := netip.AddrPortFrom(toAddr(id.LocalAddress), id.LocalPort)
	if !relayable(dst.Addr()) {
		r.Complete(true)
		return
	}
	st, err := s.opt.Opener.OpenStream()
	if err != nil {
		s.log.Debug("wireguard: cannot open stream", "target", dst, "err", err)
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
	s.log.Debug("wireguard connect", "from", netip.AddrPortFrom(toAddr(id.RemoteAddress), id.RemotePort), "target", dst, "stream", st.ID())
	proxy.Pipe(abortConn{gonet.NewTCPConn(&wq, ep), ep}, st)
}

func (s *Server) handleUDP(id stack.TransportEndpointID, pkt *stack.PacketBuffer) bool {
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
		s.tun.send(unreachable(dst.Addr(), src.Addr(), packetBytes(pkt)))
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
		s.log.Debug("wireguard: udp send", "target", dst, "err", err)
	}
	return true
}

// packetTooBig tells the client that the tunnel dropped its datagram as too
// large, so that its path MTU discovery can adapt (like a router would).
func (s *Server) packetTooBig(pkt *stack.PacketBuffer, client, target netip.Addr, maxPayload int) {
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
	s.tun.send(tooBig(target, client, orig, mtu))
}

// packetBytes returns the IP packet of a parsed packet buffer.
func packetBytes(pkt *stack.PacketBuffer) []byte {
	b := append([]byte(nil), pkt.NetworkHeader().Slice()...)
	b = append(b, pkt.TransportHeader().Slice()...)
	return append(b, pkt.Data().AsRange().ToSlice()...)
}

func (s *Server) udpFlow(src netip.AddrPort) *udpFlow {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f := s.udpFlows[src]; f != nil {
		return f
	}
	f := &udpFlow{client: src}
	f.id = s.opt.UDP.Register(func(msg []byte) { s.udpReply(f, msg) })
	s.udpFlows[src] = f
	s.log.Debug("wireguard udp flow", "from", src, "id", f.id)
	return f
}

func (s *Server) udpReply(f *udpFlow, msg []byte) {
	from, n, ok := proxy.ParseAddrPort(msg)
	if !ok {
		return
	}
	from = netip.AddrPortFrom(from.Addr().Unmap(), from.Port())
	if from.Addr().Is4() != f.client.Addr().Is4() {
		return
	}
	f.last.Store(time.Now().UnixNano())
	s.tun.send(udpPackets(from, f.client, msg[n:], s.opt.MTU)...)
}

// intercept handles the ICMP echo requests from the peers.
func (s *Server) intercept(p []byte) bool {
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
		for range 1 << 16 {
			s.nextFlow++
			if s.byFlow[s.nextFlow] == nil {
				f = &echoFlow{echoKey: k, flow: s.nextFlow}
				s.echoes[k] = f
				s.byFlow[f.flow] = f
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

// echoReply receives an echo reply from the remote (proxy.UDPLocal.ICMP).
func (s *Server) echoReply(b []byte) {
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
	s.tun.send(echoReply(f.target, f.client, f.id, seq, data))
}

func (s *Server) reaper() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.done:
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
