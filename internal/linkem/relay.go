package linkem

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Relay forwards UDP datagrams from clients on Listen to Target, creating
// one upstream socket per client address (like socat's fork mode), and
// applies Fwd (client->target) and Rev (target->client) impairments.
type Relay struct {
	Fwd, Rev *Link
	// SharedFwd / SharedRev, if set, are applied before Fwd / Rev. They can
	// be shared by several relays to emulate a common bottleneck in front of
	// all paths, e.g. the uplink of the sending endpoint.
	SharedFwd, SharedRev *Link
	// SharedFwdOut / SharedRevOut, if set, are applied after Fwd / Rev: a
	// common bottleneck behind all paths, e.g. the downlink of the receiving
	// endpoint. What it drops has already crossed the relays.
	SharedFwdOut, SharedRevOut *Link
	Timeout                    time.Duration
	Log                        *slog.Logger

	target string
	lc     *net.UDPConn
	mu     sync.Mutex
	flows  map[string]*flow
	closed bool

	// the target's address (see targetAddr)
	tmu       sync.Mutex
	taddr     *net.UDPAddr
	tfixed    bool      // the target is an IP address
	tat       time.Time // when taddr was resolved
	tretry    time.Time // no new lookup before
	resolving bool
	warned    atomic.Int64 // last "relay upstream" warning (unix nanoseconds)
}

type flow struct {
	up   *net.UDPConn
	from *net.UDPAddr
	last atomic.Int64 // unix nanoseconds
}

// targetTTL is how long the address of a named target is used before it
// is looked up again.
var targetTTL = 5 * time.Second

const (
	targetRetry   = time.Second // the next lookup after a failed one
	targetTimeout = 5 * time.Second
)

// testResolve replaces the resolver in tests.
var testResolve atomic.Pointer[func(ctx context.Context, hostport string) (*net.UDPAddr, error)]

// NewRelay binds listen and prepares to forward to target.
func NewRelay(listen, target string, fwd, rev Spec) (*Relay, error) {
	la, err := net.ResolveUDPAddr("udp", listen)
	if err != nil {
		return nil, err
	}
	if _, _, err := net.SplitHostPort(target); err != nil {
		return nil, err
	}
	lc, err := net.ListenUDP("udp", la)
	if err != nil {
		return nil, err
	}
	lc.SetReadBuffer(8 << 20)
	lc.SetWriteBuffer(8 << 20)
	r := &Relay{
		Fwd: NewLink(fwd, time.Now().UnixNano()), Rev: NewLink(rev, time.Now().UnixNano()+1),
		Timeout: 120 * time.Second, Log: slog.Default(),
		target: target, lc: lc, flows: make(map[string]*flow),
	}
	if ap, err := netip.ParseAddrPort(target); err == nil {
		r.taddr, r.tfixed = net.UDPAddrFromAddrPort(ap), true
	} else {
		r.targetAddr() // start the first lookup
	}
	return r, nil
}

// targetAddr returns the target's address without waiting: a named target
// is looked up in the background, every few seconds while flows are
// created, so DNS changes are followed and a resolver that is slow or down
// holds up neither the packets of established flows nor, as long as an
// earlier answer exists, new ones. It returns nil until the first answer.
func (r *Relay) targetAddr() *net.UDPAddr {
	r.tmu.Lock()
	defer r.tmu.Unlock()
	if !r.tfixed && !r.resolving {
		now := time.Now()
		if (r.taddr == nil || now.Sub(r.tat) > targetTTL) && !now.Before(r.tretry) {
			r.resolving = true
			go r.resolveTarget()
		}
	}
	return r.taddr
}

func (r *Relay) resolveTarget() {
	ctx, cancel := context.WithTimeout(context.Background(), targetTimeout)
	resolve := resolveUDP
	if f := testResolve.Load(); f != nil {
		resolve = *f
	}
	addr, err := resolve(ctx, r.target)
	cancel()
	r.tmu.Lock()
	r.resolving = false
	if err == nil {
		r.taddr, r.tat = addr, time.Now()
	} else {
		r.tretry = time.Now().Add(targetRetry)
	}
	r.tmu.Unlock()
	if err != nil {
		r.Log.Warn("relay target", "target", r.target, "err", err)
	}
}

// resolveUDP is net.ResolveUDPAddr with a context.
func resolveUDP(ctx context.Context, hostport string) (*net.UDPAddr, error) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return nil, err
	}
	p, err := net.DefaultResolver.LookupPort(ctx, "udp", port)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	ip := ips[0]
	for _, c := range ips { // like net.ResolveUDPAddr: IPv4 first
		if c.Unmap().Is4() {
			ip = c
			break
		}
	}
	return net.UDPAddrFromAddrPort(netip.AddrPortFrom(ip.Unmap(), uint16(p))), nil
}

// Addr returns the listening address.
func (r *Relay) Addr() *net.UDPAddr { return r.lc.LocalAddr().(*net.UDPAddr) }

// SetDown takes the relay down (drops all packets both ways) or up.
func (r *Relay) SetDown(down bool) {
	r.Fwd.SetDown(down)
	r.Rev.SetDown(down)
}

// Close stops the relay.
func (r *Relay) Close() {
	r.mu.Lock()
	r.closed = true
	for _, f := range r.flows {
		f.up.Close()
	}
	r.mu.Unlock()
	r.lc.Close()
	r.Fwd.Close()
	r.Rev.Close()
}

// Serve runs the relay until Close.
func (r *Relay) Serve() error {
	go r.reaper()
	buf := make([]byte, 65536)
	for {
		n, from, err := r.lc.ReadFromUDP(buf)
		if err != nil {
			r.mu.Lock()
			c := r.closed
			r.mu.Unlock()
			if c {
				return nil
			}
			return err
		}
		f, err := r.flowFor(from)
		if err != nil {
			// at most one warning per second: this is per packet
			if now, last := time.Now().UnixNano(), r.warned.Load(); now-last > int64(time.Second) && r.warned.CompareAndSwap(last, now) {
				r.Log.Warn("relay upstream", "err", err)
			}
			continue
		}
		up := f.up
		out := func(b []byte) { up.Write(b) }
		if l := r.SharedFwdOut; l != nil {
			last := out
			out = func(b []byte) { l.Send(b, last) }
		}
		if r.SharedFwd != nil {
			r.SharedFwd.Send(buf[:n], func(b []byte) { r.Fwd.Send(b, out) })
		} else {
			r.Fwd.Send(buf[:n], out)
		}
	}
}

var errNoTarget = errors.New("the target's address is not known yet")

func (r *Relay) flowFor(from *net.UDPAddr) (*flow, error) {
	k := from.String()
	r.mu.Lock()
	defer r.mu.Unlock()
	if f := r.flows[k]; f != nil {
		f.last.Store(time.Now().UnixNano())
		return f, nil
	}
	ta := r.targetAddr()
	if ta == nil {
		return nil, errNoTarget
	}
	up, err := net.DialUDP("udp", nil, ta)
	if err != nil {
		return nil, err
	}
	up.SetReadBuffer(8 << 20)
	up.SetWriteBuffer(8 << 20)
	f := &flow{up: up, from: from}
	f.last.Store(time.Now().UnixNano())
	r.flows[k] = f
	r.Log.Debug("relay flow", "from", k, "via", up.LocalAddr().String())
	go func() {
		b := make([]byte, 65536)
		for {
			n, err := up.Read(b)
			if err != nil {
				// The socket is connected, so an ICMP "port unreachable"
				// from the target (it is restarting) comes back as an error
				// of the next call: that is not the end of the flow, or the
				// client would be forwarded but never hear back.
				if errors.Is(err, syscall.ECONNREFUSED) {
					continue
				}
				if !errors.Is(err, net.ErrClosed) {
					// anything else: start over with the client's next packet
					r.mu.Lock()
					if r.flows[k] == f {
						delete(r.flows, k)
					}
					r.mu.Unlock()
					up.Close()
				}
				return
			}
			f.last.Store(time.Now().UnixNano())
			out := func(p []byte) { r.lc.WriteToUDP(p, from) }
			if l := r.SharedRevOut; l != nil {
				last := out
				out = func(p []byte) { l.Send(p, last) }
			}
			if r.SharedRev != nil {
				r.SharedRev.Send(b[:n], func(p []byte) { r.Rev.Send(p, out) })
			} else {
				r.Rev.Send(b[:n], out)
			}
		}
	}()
	return f, nil
}

func (r *Relay) reaper() {
	for {
		time.Sleep(time.Second * 5)
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			return
		}
		for k, f := range r.flows {
			if time.Since(time.Unix(0, f.last.Load())) > r.Timeout {
				f.up.Close()
				delete(r.flows, k)
			}
		}
		r.mu.Unlock()
	}
}
