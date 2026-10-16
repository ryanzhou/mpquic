package linkem

import (
	"log/slog"
	"net"
	"sync"
	"time"
)

// Relay forwards UDP datagrams from clients on Listen to Target, creating
// one upstream socket per client address (like socat's fork mode), and
// applies Fwd (client->target) and Rev (target->client) impairments.
type Relay struct {
	Fwd, Rev *Link
	Timeout  time.Duration
	Log      *slog.Logger

	target *net.UDPAddr
	lc     *net.UDPConn
	mu     sync.Mutex
	flows  map[string]*flow
	closed bool
}

type flow struct {
	up   *net.UDPConn
	from *net.UDPAddr
	last time.Time
}

// NewRelay binds listen and prepares to forward to target.
func NewRelay(listen, target string, fwd, rev Spec) (*Relay, error) {
	la, err := net.ResolveUDPAddr("udp", listen)
	if err != nil {
		return nil, err
	}
	ta, err := net.ResolveUDPAddr("udp", target)
	if err != nil {
		return nil, err
	}
	lc, err := net.ListenUDP("udp", la)
	if err != nil {
		return nil, err
	}
	lc.SetReadBuffer(8 << 20)
	lc.SetWriteBuffer(8 << 20)
	return &Relay{
		Fwd: NewLink(fwd, time.Now().UnixNano()), Rev: NewLink(rev, time.Now().UnixNano()+1),
		Timeout: 120 * time.Second, Log: slog.Default(),
		target: ta, lc: lc, flows: make(map[string]*flow),
	}, nil
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
			r.Log.Warn("relay upstream", "err", err)
			continue
		}
		up := f.up
		r.Fwd.Send(buf[:n], func(b []byte) { up.Write(b) })
	}
}

func (r *Relay) flowFor(from *net.UDPAddr) (*flow, error) {
	k := from.String()
	r.mu.Lock()
	defer r.mu.Unlock()
	if f := r.flows[k]; f != nil {
		f.last = time.Now()
		return f, nil
	}
	up, err := net.DialUDP("udp", nil, r.target)
	if err != nil {
		return nil, err
	}
	up.SetReadBuffer(8 << 20)
	up.SetWriteBuffer(8 << 20)
	f := &flow{up: up, from: from, last: time.Now()}
	r.flows[k] = f
	r.Log.Debug("relay flow", "from", k, "via", up.LocalAddr().String())
	go func() {
		b := make([]byte, 65536)
		for {
			n, err := up.Read(b)
			if err != nil {
				return
			}
			r.mu.Lock()
			f.last = time.Now()
			r.mu.Unlock()
			r.Rev.Send(b[:n], func(p []byte) { r.lc.WriteToUDP(p, from) })
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
			if time.Since(f.last) > r.Timeout {
				f.up.Close()
				delete(r.flows, k)
			}
		}
		r.mu.Unlock()
	}
}
