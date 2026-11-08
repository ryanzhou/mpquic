package proxy

import (
	"bytes"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apernet/quic-go/quicvarint"

	"mpquic/internal/mp"
)

// UDP relaying (SOCKS5 UDP ASSOCIATE, the UDP tunnel, WireGuard peers).
//
// Every association gets a session-unique id. Datagrams travel as
//
//	assocID:varint  SOCKS5-address  payload
//
// using QUIC DATAGRAM frames on the lowest-latency path (unreliable, like UDP;
// datagrams that do not fit in a packet are dropped). In reliable mode (see
// reliable.go) each association is a stream instead. On the remote side each
// association owns one UDP socket, so replies from any peer are relayed back
// (full-cone).

const udpIdle = 2 * time.Minute

// UDPLocal handles the local side of UDP associations. Besides SOCKS5 UDP
// ASSOCIATE it serves other front-ends (the WireGuard listener) through
// Register, and dispatches ICMP echo replies (association 0) to ICMP.
type UDPLocal struct {
	Client Opener
	Log    *slog.Logger
	// ICMP receives the body of ICMP echo datagrams (see icmp.go).
	ICMP func(b []byte)
	// Reliable relays every association over a stream (see reliable.go)
	// instead of QUIC DATAGRAMs. Set it before registering associations.
	Reliable bool

	mu     sync.Mutex
	assocs map[uint64]*localAssoc
	nextID atomic.Uint64
	// reliable mode: datagrams dropped (congestion), streams refused
	drops, refused rateLog
}

type localAssoc struct {
	deliver func(msg []byte)
	rel     *relAssoc // reliable mode
}

func NewUDPLocal(c Opener, log *slog.Logger) *UDPLocal {
	return &UDPLocal{Client: c, Log: log, assocs: make(map[uint64]*localAssoc)}
}

// Register allocates an association whose datagrams from the remote are
// passed to deliver as "ATYP ADDR PORT DATA" (the peer's address). deliver
// runs on the receive path and must not keep msg.
func (u *UDPLocal) Register(deliver func(msg []byte)) uint64 {
	id := u.nextID.Add(1)
	a := &localAssoc{deliver: deliver}
	if u.Reliable {
		a.rel = newRelAssoc(u, id, deliver)
	}
	u.mu.Lock()
	u.assocs[id] = a
	u.mu.Unlock()
	return id
}

// Unregister removes an association. If notify is set, the remote is told to
// release its socket now instead of when it idles out (a reliable
// association's stream is always closed).
func (u *UDPLocal) Unregister(id uint64, notify bool) {
	u.mu.Lock()
	a := u.assocs[id]
	delete(u.assocs, id)
	u.mu.Unlock()
	if a != nil && a.rel != nil {
		a.rel.close()
		return
	}
	if notify {
		if sess := u.Client.Session(); sess != nil {
			// An association id alone closes it; older remotes ignore it.
			sess.SendDatagram(quicvarint.Append(nil, id), 0)
		}
	}
}

// Send relays payload to target (a SOCKS5-format address) on association id.
// It does not keep target or payload, and does not block.
func (u *UDPLocal) Send(id uint64, target, payload []byte) error {
	if u.Reliable {
		u.mu.Lock()
		a := u.assocs[id]
		u.mu.Unlock()
		if a == nil {
			return net.ErrClosed
		}
		return a.rel.send(target, payload)
	}
	sess := u.Client.Session()
	if sess == nil {
		return mp.ErrNoSession
	}
	msg := quicvarint.Append(make([]byte, 0, 8+len(target)+len(payload)), id)
	hdr := len(msg) + len(target)
	msg = append(msg, target...)
	msg = append(msg, payload...)
	return sess.SendDatagram(msg, hdr)
}

// HandleDatagram is the mp.Options.DatagramHandler for the client.
func (u *UDPLocal) HandleDatagram(_ *mp.Session, b []byte) {
	r := bytes.NewReader(b)
	id, err := quicvarint.Read(r)
	if err != nil {
		return
	}
	rest := b[len(b)-r.Len():]
	if id == icmpAssoc {
		if u.ICMP != nil {
			u.ICMP(rest)
		}
		return
	}
	u.mu.Lock()
	a := u.assocs[id]
	u.mu.Unlock()
	if a != nil && a.rel == nil {
		a.deliver(rest)
	}
}

// associate serves one UDP ASSOCIATE request on control connection c.
func (u *UDPLocal) associate(c net.Conn) {
	defer c.Close()
	la := c.LocalAddr().(*net.TCPAddr)
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: la.IP})
	if err != nil {
		c.Write([]byte{5, 1, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer pc.Close()
	bound := pc.LocalAddr().(*net.UDPAddr)
	rep, _ := EncodeAddr(bound.String())
	if _, err := c.Write(append([]byte{5, 0, 0}, rep...)); err != nil {
		return
	}
	var client atomic.Pointer[net.UDPAddr]
	id := u.Register(func(rest []byte) {
		ca := client.Load()
		if ca == nil {
			return
		}
		pkt := make([]byte, 0, 3+len(rest))
		pkt = append(pkt, 0, 0, 0) // RSV RSV FRAG
		pkt = append(pkt, rest...) // ATYP ADDR PORT DATA
		pc.WriteToUDP(pkt, ca)
	})
	defer u.Unregister(id, true)
	u.Log.Debug("udp associate", "id", id, "bind", bound.String())
	// The association lives as long as the TCP control connection.
	go func() {
		io.Copy(io.Discard, c)
		pc.Close()
	}()
	buf := make([]byte, 65536)
	for {
		n, from, err := pc.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if n < 4 || buf[2] != 0 { // fragmentation not supported
			continue
		}
		al := addrLen(buf[3:n])
		if al < 0 {
			continue
		}
		if ca := client.Load(); ca == nil || !ca.IP.Equal(from.IP) || ca.Port != from.Port {
			client.Store(from)
		}
		if err := u.Send(id, buf[3:3+al], buf[3+al:n]); err != nil {
			u.Log.Debug("udp send", "err", err)
		}
	}
}

// UDPRemote handles the remote side of UDP associations.
type UDPRemote struct {
	Log *slog.Logger

	mu     sync.Mutex
	assocs map[assocKey]*remoteAssoc
	icmp   *icmpRemote
}

type assocKey struct {
	s  *mp.Session
	id uint64
}

type remoteAssoc struct {
	pc    *net.UDPConn
	last  atomic.Int64
	cache sync.Map // string -> *net.UDPAddr
}

func NewUDPRemote(log *slog.Logger) *UDPRemote {
	u := &UDPRemote{Log: log, assocs: make(map[assocKey]*remoteAssoc), icmp: newICMPRemote(log)}
	go u.reaper()
	return u
}

// HandleDatagram is the mp.Options.DatagramHandler for the server.
func (u *UDPRemote) HandleDatagram(s *mp.Session, b []byte) {
	r := bytes.NewReader(b)
	id, err := quicvarint.Read(r)
	if err != nil {
		return
	}
	k := assocKey{s, id}
	if id == icmpAssoc {
		u.icmp.handle(s, b[len(b)-r.Len():])
		return
	}
	if r.Len() == 0 { // the local released the association
		u.mu.Lock()
		if a := u.assocs[k]; a != nil {
			a.pc.Close()
			delete(u.assocs, k)
		}
		u.mu.Unlock()
		return
	}
	target, _, err := ReadAddr(r)
	if err != nil {
		return
	}
	payload := b[len(b)-r.Len():]
	u.mu.Lock()
	a := u.assocs[k]
	if a == nil {
		pc, err := net.ListenUDP("udp", nil)
		if err != nil {
			u.mu.Unlock()
			return
		}
		a = &remoteAssoc{pc: pc}
		u.assocs[k] = a
		go u.readLoop(s, id, a)
	}
	u.mu.Unlock()
	a.last.Store(time.Now().UnixNano())
	var ua *net.UDPAddr
	if v, ok := a.cache.Load(target); ok {
		ua = v.(*net.UDPAddr)
	} else {
		ua, err = net.ResolveUDPAddr("udp", target)
		if err != nil {
			u.Log.Debug("udp resolve", "target", target, "err", err)
			return
		}
		a.cache.Store(target, ua)
	}
	a.pc.WriteToUDP(payload, ua)
}

func (u *UDPRemote) readLoop(s *mp.Session, id uint64, a *remoteAssoc) {
	hdr := quicvarint.Append(nil, id)
	buf := make([]byte, 65536)
	for {
		n, from, err := a.pc.ReadFromUDP(buf)
		if err != nil {
			return
		}
		a.last.Store(time.Now().UnixNano())
		ip := from.IP
		if v4 := ip.To4(); v4 != nil {
			ip = v4
		}
		addr, _ := EncodeAddr((&net.UDPAddr{IP: ip, Port: from.Port}).String())
		msg := make([]byte, 0, len(hdr)+len(addr)+n)
		msg = append(msg, hdr...)
		msg = append(msg, addr...)
		msg = append(msg, buf[:n]...)
		if err := s.SendDatagram(msg, len(hdr)+len(addr)); err != nil && s.Context().Err() != nil {
			return
		}
	}
}

func (u *UDPRemote) reaper() {
	for {
		time.Sleep(10 * time.Second)
		now := time.Now().UnixNano()
		u.mu.Lock()
		for k, a := range u.assocs {
			if now-a.last.Load() > int64(udpIdle) || k.s.Context().Err() != nil {
				a.pc.Close()
				delete(u.assocs, k)
			}
		}
		u.mu.Unlock()
		u.icmp.reap(now)
	}
}
