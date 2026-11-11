package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strconv"
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
// Register, and dispatches ICMP echo replies (association 0) to the
// front-end whose echo flow they belong to (EchoReceiver).
type UDPLocal struct {
	Client Opener
	Log    *slog.Logger
	// Reliable relays every association over a stream (see reliable.go)
	// instead of QUIC DATAGRAMs. Set it before registering associations.
	Reliable bool

	mu     sync.Mutex
	assocs map[uint64]*localAssoc
	nextID atomic.Uint64
	echo   [1 << 16 / EchoFlows]func(b []byte)
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
		if _, flow, _, _, ok := ParseEcho(rest); ok {
			u.mu.Lock()
			recv := u.echo[flow/EchoFlows]
			u.mu.Unlock()
			if recv != nil {
				recv(rest)
			}
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

	mu     sync.RWMutex
	assocs map[assocKey]*remoteAssoc
	icmp   *icmpRemote
	// sockets that could not be opened, associations closed to make room
	failed, evicted rateLog
}

type assocKey struct {
	s  *mp.Session
	id uint64
}

// remoteAssoc is the remote side of an association: one UDP socket.
type remoteAssoc struct {
	pc   *net.UDPConn
	last atomic.Int64
	done chan struct{} // closed with the socket
	once sync.Once

	// Targets given by name are resolved by a goroutine of the association
	// (see sendToName).
	nmu     sync.Mutex
	names   map[string]nameEntry
	lookups chan nameDgram
}

type nameEntry struct {
	ap    netip.AddrPort // invalid: the lookup failed
	until time.Time
}

type nameDgram struct {
	target  string
	payload []byte
}

const (
	// dgramReadBuf is the read buffer of a socket whose datagrams travel
	// as QUIC DATAGRAMs: larger than anything a QUIC packet (at most 1452
	// bytes) can carry, so a datagram that fills it is dropped as too large
	// anyway. A 64 KB buffer per association was 128 KB of stack each.
	dgramReadBuf = 2048
	// maxRemoteAssocs bounds the associations (sockets) of a remote. Beyond
	// it, and when the system refuses another socket, the associations idle
	// for longest are closed: a port scan or a flood of flows through the
	// tunnel must not take the file descriptors that TCP connections need.
	maxRemoteAssocs = 8192

	nameTTL      = time.Minute
	nameFailTTL  = 5 * time.Second
	nameTimeout  = 5 * time.Second
	maxNames     = 256
	lookupsQueue = 16
)

func (a *remoteAssoc) close() {
	a.once.Do(func() {
		a.pc.Close()
		close(a.done)
	})
}

func NewUDPRemote(log *slog.Logger) *UDPRemote {
	u := &UDPRemote{Log: log, assocs: make(map[assocKey]*remoteAssoc), icmp: newICMPRemote(log)}
	go u.reaper()
	return u
}

// HandleDatagram is the mp.Options.DatagramHandler for the server. It runs
// on the path's receive loop, which also carries the session's pings and
// acknowledgements: it must not block.
func (u *UDPRemote) HandleDatagram(s *mp.Session, b []byte) {
	r := bytes.NewReader(b)
	id, err := quicvarint.Read(r)
	if err != nil {
		return
	}
	k := assocKey{s, id}
	rest := b[len(b)-r.Len():]
	if id == icmpAssoc {
		u.icmp.handle(s, rest)
		return
	}
	if len(rest) == 0 { // the local released the association
		u.mu.Lock()
		if a := u.assocs[k]; a != nil {
			a.close()
			delete(u.assocs, k)
		}
		u.mu.Unlock()
		return
	}
	al := addrLen(rest)
	if al < 0 {
		return
	}
	u.mu.RLock()
	a := u.assocs[k]
	u.mu.RUnlock()
	if a == nil {
		if a = u.newAssoc(s, k); a == nil {
			return
		}
	}
	a.last.Store(time.Now().UnixNano())
	if ap, _, ok := ParseAddrPort(rest); ok {
		a.pc.WriteToUDPAddrPort(rest[al:], ap)
		return
	}
	target, _, err := ReadAddr(bytes.NewReader(rest[:al]))
	if err != nil {
		return
	}
	a.sendToName(u.Log, target, rest[al:])
}

// newAssoc opens the socket of a new association.
func (u *UDPRemote) newAssoc(s *mp.Session, k assocKey) *remoteAssoc {
	u.mu.Lock()
	defer u.mu.Unlock()
	if a := u.assocs[k]; a != nil {
		return a
	}
	if len(u.assocs) >= maxRemoteAssocs {
		u.evictLocked()
	}
	pc, err := net.ListenUDP("udp", nil)
	if err != nil && len(u.assocs) > 0 {
		// e.g. out of file descriptors: make room and try once more
		u.evictLocked()
		pc, err = net.ListenUDP("udp", nil)
	}
	if err != nil {
		u.failed.note(u.Log, "cannot open a socket for a UDP association: "+err.Error())
		return nil
	}
	a := &remoteAssoc{pc: pc, done: make(chan struct{})}
	a.last.Store(time.Now().UnixNano())
	u.assocs[k] = a
	go u.readLoop(s, k.id, a)
	return a
}

// evictLocked closes the eighth of the associations that has been idle for
// longest.
func (u *UDPRemote) evictLocked() {
	if len(u.assocs) == 0 {
		return
	}
	last := make([]int64, 0, len(u.assocs))
	for _, a := range u.assocs {
		last = append(last, a.last.Load())
	}
	slices.Sort(last)
	limit := last[len(last)/8]
	n := 0
	for k, a := range u.assocs {
		if a.last.Load() <= limit {
			a.close()
			delete(u.assocs, k)
			n++
		}
	}
	u.evicted.note(u.Log, fmt.Sprintf("too many UDP associations: closed the %d idle for longest", n))
}

// sendToName sends payload to a target given by name. The lookup must not
// hold up the caller (see HandleDatagram): a goroutine of the association
// does it, with the datagrams that wait for it, and the answer is kept for
// a while, as is a failure.
func (a *remoteAssoc) sendToName(log *slog.Logger, target string, payload []byte) {
	a.nmu.Lock()
	if e, ok := a.names[target]; ok && time.Now().Before(e.until) {
		a.nmu.Unlock()
		if e.ap.IsValid() {
			a.pc.WriteToUDPAddrPort(payload, e.ap)
		}
		return
	}
	if a.lookups == nil {
		a.lookups = make(chan nameDgram, lookupsQueue)
		go a.resolver(log)
	}
	ch := a.lookups
	a.nmu.Unlock()
	select {
	case ch <- nameDgram{target, bytes.Clone(payload)}:
	default: // the lookups are slower than the datagrams arrive: drop
	}
}

func (a *remoteAssoc) resolver(log *slog.Logger) {
	for {
		var d nameDgram
		select {
		case <-a.done:
			return
		case d = <-a.lookups:
		}
		a.nmu.Lock()
		e, ok := a.names[d.target]
		a.nmu.Unlock()
		if !ok || !time.Now().Before(e.until) {
			ap, err := lookupUDP(d.target)
			e = nameEntry{ap: ap, until: time.Now().Add(nameTTL)}
			if err != nil {
				log.Debug("udp resolve", "target", d.target, "err", err)
				e.until = time.Now().Add(nameFailTTL)
			}
			a.nmu.Lock()
			if a.names == nil || len(a.names) >= maxNames {
				a.names = make(map[string]nameEntry)
			}
			a.names[d.target] = e
			a.nmu.Unlock()
		}
		if e.ap.IsValid() {
			a.pc.WriteToUDPAddrPort(d.payload, e.ap)
		}
	}
}

// testLookup replaces the resolver in tests.
var testLookup atomic.Pointer[func(ctx context.Context, network, host string) ([]netip.Addr, error)]

// lookupUDP resolves "name:port" like net.ResolveUDPAddr (the first IPv4
// address, else the first), within nameTimeout.
func lookupUDP(target string) (netip.AddrPort, error) {
	host, ps, err := net.SplitHostPort(target)
	if err != nil {
		return netip.AddrPort{}, err
	}
	port, err := strconv.ParseUint(ps, 10, 16)
	if err != nil {
		return netip.AddrPort{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), nameTimeout)
	defer cancel()
	lookup := net.DefaultResolver.LookupNetIP
	if f := testLookup.Load(); f != nil {
		lookup = *f
	}
	ips, err := lookup(ctx, "ip", host)
	if err != nil {
		return netip.AddrPort{}, err
	}
	if len(ips) == 0 {
		return netip.AddrPort{}, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	ip := ips[0]
	for _, c := range ips {
		if c.Unmap().Is4() {
			ip = c
			break
		}
	}
	return netip.AddrPortFrom(ip.Unmap(), uint16(port)), nil
}

func (u *UDPRemote) readLoop(s *mp.Session, id uint64, a *remoteAssoc) {
	hdr := quicvarint.Append(nil, id)
	var buf [dgramReadBuf]byte
	for {
		n, from, err := a.pc.ReadFromUDPAddrPort(buf[:])
		if err != nil {
			return
		}
		a.last.Store(time.Now().UnixNano())
		msg := make([]byte, 0, len(hdr)+19+n)
		msg = append(msg, hdr...)
		msg = AppendAddrPort(msg, from)
		al := len(msg)
		msg = append(msg, buf[:n]...)
		if err := s.SendDatagram(msg, al); err != nil && s.Context().Err() != nil {
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
				a.close()
				delete(u.assocs, k)
			}
		}
		u.mu.Unlock()
		u.icmp.reap(now)
	}
}
