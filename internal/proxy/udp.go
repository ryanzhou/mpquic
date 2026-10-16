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

// UDP relaying (SOCKS5 UDP ASSOCIATE).
//
// Every association gets a session-unique id. Datagrams travel as
//
//	assocID:varint  SOCKS5-address  payload
//
// using QUIC DATAGRAM frames on the lowest-latency path (unreliable, like UDP).
// On the remote side each association owns one UDP socket, so replies from any
// peer are relayed back (full-cone).

const udpIdle = 2 * time.Minute

// UDPLocal handles the local side of UDP associations.
type UDPLocal struct {
	Client *mp.Client
	Log    *slog.Logger

	mu     sync.Mutex
	assocs map[uint64]*localAssoc
	nextID atomic.Uint64
}

type localAssoc struct {
	id     uint64
	pc     *net.UDPConn
	client atomic.Pointer[net.UDPAddr]
}

func NewUDPLocal(c *mp.Client, log *slog.Logger) *UDPLocal {
	return &UDPLocal{Client: c, Log: log, assocs: make(map[uint64]*localAssoc)}
}

// HandleDatagram is the mp.Options.DatagramHandler for the client.
func (u *UDPLocal) HandleDatagram(_ *mp.Session, b []byte) {
	r := bytes.NewReader(b)
	id, err := quicvarint.Read(r)
	if err != nil {
		return
	}
	u.mu.Lock()
	a := u.assocs[id]
	u.mu.Unlock()
	if a == nil {
		return
	}
	ca := a.client.Load()
	if ca == nil {
		return
	}
	rest := b[len(b)-r.Len():]
	pkt := make([]byte, 0, 3+len(rest))
	pkt = append(pkt, 0, 0, 0) // RSV RSV FRAG
	pkt = append(pkt, rest...) // ATYP ADDR PORT DATA
	a.pc.WriteToUDP(pkt, ca)
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
	a := &localAssoc{id: u.nextID.Add(1), pc: pc}
	u.mu.Lock()
	u.assocs[a.id] = a
	u.mu.Unlock()
	defer func() {
		u.mu.Lock()
		delete(u.assocs, a.id)
		u.mu.Unlock()
	}()
	u.Log.Debug("udp associate", "id", a.id, "bind", bound.String())
	// The association lives as long as the TCP control connection.
	go func() {
		io.Copy(io.Discard, c)
		pc.Close()
	}()
	hdr := quicvarint.Append(nil, a.id)
	buf := make([]byte, 65536)
	for {
		n, from, err := pc.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if n < 4 || buf[2] != 0 { // fragmentation not supported
			continue
		}
		if ca := a.client.Load(); ca == nil || !ca.IP.Equal(from.IP) || ca.Port != from.Port {
			a.client.Store(from)
		}
		msg := make([]byte, 0, len(hdr)+n-3)
		msg = append(msg, hdr...)
		msg = append(msg, buf[3:n]...)
		if err := u.Client.Session().SendDatagram(msg); err != nil {
			u.Log.Debug("udp send", "err", err)
		}
	}
}

// UDPRemote handles the remote side of UDP associations.
type UDPRemote struct {
	Log *slog.Logger

	mu     sync.Mutex
	assocs map[assocKey]*remoteAssoc
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
	u := &UDPRemote{Log: log, assocs: make(map[assocKey]*remoteAssoc)}
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
	target, _, err := ReadAddr(r)
	if err != nil {
		return
	}
	payload := b[len(b)-r.Len():]
	k := assocKey{s, id}
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
		if err := s.SendDatagram(msg); err != nil && s.Context().Err() != nil {
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
	}
}
