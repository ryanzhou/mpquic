package wg

import (
	"context"
	"os"
	"sync"

	"golang.zx2c4.com/wireguard/tun"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

const nicID = 1

// netTun is the tun.Device of the WireGuard device: a userspace gVisor
// network stack in promiscuous mode, so it terminates TCP connections to any
// address. intercept sees every packet from the peers first; packets it
// consumes (ICMP echo) bypass gVisor. Packets to the peers come from gVisor
// and from send (relayed UDP and ICMP).
type netTun struct {
	stack     *stack.Stack
	ep        *channel.Endpoint
	mtu       int
	intercept func(pkt []byte) bool
	out       chan []byte
	events    chan tun.Event
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
}

const outQueue = 1024

func newNetTun(mtu int, intercept func([]byte) bool) (*netTun, error) {
	s := stack.New(stack.Options{
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
		if err := s.SetTransportProtocolOption(tcp.ProtocolNumber, o); err != nil {
			s.Close()
			return nil, wrapErr("tcp option", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	t := &netTun{
		stack:     s,
		ep:        channel.New(512, uint32(mtu), ""),
		mtu:       mtu,
		intercept: intercept,
		out:       make(chan []byte, outQueue),
		events:    make(chan tun.Event, 1),
		ctx:       ctx,
		cancel:    cancel,
	}
	if err := s.CreateNIC(nicID, t.ep); err != nil {
		t.Close()
		return nil, wrapErr("create NIC", err)
	}
	// Accept packets to any address, and send from any address.
	if err := s.SetPromiscuousMode(nicID, true); err != nil {
		t.Close()
		return nil, wrapErr("promiscuous mode", err)
	}
	if err := s.SetSpoofing(nicID, true); err != nil {
		t.Close()
		return nil, wrapErr("spoofing", err)
	}
	s.SetRouteTable([]tcpip.Route{
		{Destination: header.IPv4EmptySubnet, NIC: nicID},
		{Destination: header.IPv6EmptySubnet, NIC: nicID},
	})
	t.events <- tun.EventUp
	go t.pump()
	return t, nil
}

func ptr[T any](v T) *T { return &v }

type tcpipError struct {
	what string
	err  tcpip.Error
}

func (e *tcpipError) Error() string { return e.what + ": " + e.err.String() }

func wrapErr(what string, err tcpip.Error) error { return &tcpipError{what, err} }

// pump moves gVisor's outgoing packets to the queue read by WireGuard.
func (t *netTun) pump() {
	for {
		pkt := t.ep.ReadContext(t.ctx)
		if pkt == nil {
			return
		}
		v := pkt.ToView()
		b := v.ToSlice()
		v.Release()
		pkt.DecRef()
		select {
		case t.out <- b:
		case <-t.ctx.Done():
			return
		}
	}
}

// send queues packets for the peers, dropping them if the queue is full
// (as a congested link would).
func (t *netTun) send(pkts ...[]byte) {
	for _, p := range pkts {
		select {
		case t.out <- p:
		default:
		}
	}
}

func (t *netTun) File() *os.File           { return nil }
func (t *netTun) Name() (string, error)    { return "mpquic", nil }
func (t *netTun) MTU() (int, error)        { return t.mtu, nil }
func (t *netTun) Events() <-chan tun.Event { return t.events }
func (t *netTun) BatchSize() int           { return 32 }

func (t *netTun) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	var p []byte
	select {
	case p = <-t.out:
	case <-t.ctx.Done():
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
		case p = <-t.out:
		default:
			return n, nil
		}
	}
}

func (t *netTun) Write(bufs [][]byte, offset int) (int, error) {
	for _, b := range bufs {
		p := b[offset:]
		if len(p) == 0 || t.intercept != nil && t.intercept(p) {
			continue
		}
		var proto tcpip.NetworkProtocolNumber
		switch p[0] >> 4 {
		case 4:
			proto = header.IPv4ProtocolNumber
		case 6:
			proto = header.IPv6ProtocolNumber
		default:
			continue
		}
		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(p)})
		t.ep.InjectInbound(proto, pkt)
		pkt.DecRef()
	}
	return len(bufs), nil
}

func (t *netTun) Close() error {
	t.closeOnce.Do(func() {
		t.cancel()
		t.stack.Close()
		t.ep.Close()
		close(t.events)
	})
	return nil
}
