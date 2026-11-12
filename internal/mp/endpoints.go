package mp

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apernet/quic-go"

	"mpquic/internal/cc"
)

// Client is the QUIC dialer: it maintains one session to a listener over many
// paths (one per configured address; typically the direct address plus UDP
// relays). Normally the client is the proxy's local side and opens streams;
// in reverse mode it is the remote side and accepts them (Options.StreamHandler).
type Client struct {
	opt     *Options
	key     []byte
	tlsConf *tls.Config
	qconf   *quic.Config
	profile string
	sess    *Session
	servers []string
	wg      sync.WaitGroup

	mtuWarned atomic.Bool
}

// NewClient creates a client and starts dialing all servers.
func NewClient(servers []string, opt *Options) (*Client, error) {
	if len(servers) == 0 {
		return nil, errors.New("no server addresses")
	}
	o := opt.withDefaults()
	prof, err := cc.ParseProfile(o.Congestion)
	if err != nil {
		return nil, err
	}
	var id [16]byte
	rand.Read(id[:])
	c := &Client{
		opt: o, key: deriveKey(o.Password), tlsConf: clientTLSConfig(o.SNI),
		qconf: quicConfig(o), profile: prof, servers: servers,
	}
	c.sess = newSession(o, id, true, o.StreamHandler)
	for i, addr := range servers {
		c.wg.Add(1)
		go c.dialLoop(uint64(i), addr)
	}
	return c, nil
}

// Session returns the multipath session.
func (c *Client) Session() *Session { return c.sess }

// OpenStream opens a new multipath stream.
func (c *Client) OpenStream() (*Stream, error) { return c.sess.OpenStream() }

// Close shuts the client down.
func (c *Client) Close() {
	c.sess.Close()
	c.wg.Wait()
}

func (c *Client) dialLoop(index uint64, addr string) {
	defer c.wg.Done()
	backoff := 500 * time.Millisecond
	for c.sess.ctx.Err() == nil {
		start := time.Now()
		err := c.runPath(index, addr)
		if c.sess.ctx.Err() != nil {
			return
		}
		if time.Since(start) > 30*time.Second {
			backoff = 500 * time.Millisecond
		}
		c.opt.Logger.Debug("path ended; redialing", "path", addr, "err", errString(err), "in", backoff)
		select {
		case <-time.After(backoff):
		case <-c.sess.ctx.Done():
			return
		}
		backoff *= 2
		if backoff > 5*time.Second {
			backoff = 5 * time.Second
		}
	}
}

func (c *Client) runPath(index uint64, addr string) error {
	raddr, err := c.opt.resolveUDP(c.sess.ctx, addr)
	if err != nil {
		return err
	}
	network := "udp4"
	if raddr.IP.To4() == nil {
		network = "udp6"
	}
	udp, err := c.opt.listenUDP(c.sess.ctx, network, "")
	if err != nil {
		return err
	}
	tr := &quic.Transport{Conn: udp}
	defer udp.Close()
	defer tr.Close()

	ctx, cancel := context.WithTimeout(c.sess.ctx, c.opt.HandshakeTimeout)
	defer cancel()
	qconf, tooSmall := pathQUICConfig(c.qconf, c.opt, raddr)
	if tooSmall && !c.mtuWarned.Swap(true) {
		c.opt.Logger.Warn("mtu is too small for QUIC on this path", "path", addr, "mtu", c.opt.MTU,
			"min", minPacketSize+(c.opt.MTU-packetSizeFor(c.opt.MTU, raddr)))
	}
	conn, err := tr.Dial(ctx, raddr, c.tlsConf, qconf)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	meter := cc.Install(conn, c.profile)
	ctrl, err := conn.OpenStreamSync(ctx)
	if err != nil {
		conn.CloseWithError(errCodeShutdown, "")
		return err
	}
	exp, err := exporterOf(conn)
	if err != nil {
		conn.CloseWithError(errCodeShutdown, "")
		return err
	}
	if err := writeHello(ctrl, c.key, exp, c.sess.id, index); err != nil {
		conn.CloseWithError(errCodeShutdown, "")
		return err
	}
	br := bufio.NewReaderSize(ctrl, 64<<10)
	type res struct {
		n   [8]byte
		err error
	}
	ch := make(chan res, 1)
	go func() { n, err := readHelloOK(br, c.key, exp, c.sess.id); ch <- res{n, err} }()
	var r res
	select {
	case r = <-ch:
	case <-ctx.Done():
		r.err = ctx.Err()
	}
	if r.err != nil {
		conn.CloseWithError(errCodeAuth, "")
		return fmt.Errorf("handshake: %w", r.err)
	}
	p := newPath(c.sess, addr, index, conn, meter, ctrl, br)
	if err := c.sess.addPath(p, &r.n); err != nil {
		conn.CloseWithError(errCodeShutdown, "")
		return err
	}
	return p.run()
}

// Server is the QUIC listener: it accepts paths and groups them into
// sessions. Normally it is the proxy's remote side (with a stream handler);
// in reverse mode it is the local side and opens streams via OpenStream.
type Server struct {
	opt      *Options
	key      []byte
	tlsConf  *tls.Config
	qconf    *quic.Config
	profile  string
	acceptFn func(*Stream)

	mu        sync.Mutex
	sessions  map[[16]byte]*Session
	listeners []*quic.Listener
	closed    bool
}

// NewServer creates a server; handler (or Options.StreamHandler if handler is
// nil) is called in its own goroutine for every incoming stream. With no
// handler, incoming streams are refused.
func NewServer(opt *Options, handler func(*Stream)) (*Server, error) {
	o := opt.withDefaults()
	if handler == nil {
		handler = o.StreamHandler
	}
	prof, err := cc.ParseProfile(o.Congestion)
	if err != nil {
		return nil, err
	}
	tc, err := serverTLSConfig(o.CertFile, o.KeyFile)
	if err != nil {
		return nil, err
	}
	s := &Server{
		opt: o, key: deriveKey(o.Password), tlsConf: tc, qconf: quicConfig(o),
		profile: prof, acceptFn: handler, sessions: make(map[[16]byte]*Session),
	}
	if o.MTU > 0 {
		// The packet size depends on the client's address family.
		base := quicConfig(o)
		s.qconf.GetConfigForClient = func(info *quic.ClientInfo) (*quic.Config, error) {
			qc, tooSmall := pathQUICConfig(base, o, info.RemoteAddr)
			if tooSmall {
				o.Logger.Debug("mtu is too small for QUIC on this path", "peer", info.RemoteAddr.String(), "mtu", o.MTU)
			}
			return qc, nil
		}
	}
	go s.reaper()
	return s, nil
}

// ListenAndServe listens on a UDP address and serves until Close.
func (s *Server) ListenAndServe(addr string) error {
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return err
	}
	udp, err := s.opt.listenUDP(context.Background(), "udp", ua.String())
	if err != nil {
		return err
	}
	return s.Serve(udp)
}

// Serve serves on an existing UDP socket.
func (s *Server) Serve(udp net.PacketConn) error {
	tr := &quic.Transport{Conn: udp}
	ln, err := tr.Listen(s.tlsConf, s.qconf)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.listeners = append(s.listeners, ln)
	s.mu.Unlock()
	s.opt.Logger.Info("listening", "addr", udp.LocalAddr().String())
	for {
		conn, err := ln.Accept(context.Background())
		if err != nil {
			return err
		}
		go s.handleConn(conn)
	}
}

// Close stops all listeners and sessions.
func (s *Server) Close() {
	s.mu.Lock()
	s.closed = true
	ls := s.listeners
	ss := make([]*Session, 0, len(s.sessions))
	for _, x := range s.sessions {
		ss = append(ss, x)
	}
	s.mu.Unlock()
	for _, l := range ls {
		l.Close()
	}
	for _, x := range ss {
		x.Close()
	}
}

func (s *Server) handleConn(conn *quic.Conn) {
	meter := cc.Install(conn, s.profile)
	ctx, cancel := context.WithTimeout(context.Background(), s.opt.HandshakeTimeout)
	defer cancel()
	ctrl, err := conn.AcceptStream(ctx)
	if err != nil {
		conn.CloseWithError(errCodeAuth, "")
		return
	}
	exp, err := exporterOf(conn)
	if err != nil {
		conn.CloseWithError(errCodeAuth, "")
		return
	}
	br := bufio.NewReaderSize(ctrl, 64<<10)
	type res struct {
		h   *hello
		err error
	}
	ch := make(chan res, 1)
	go func() { h, err := readHello(br, s.key, exp); ch <- res{h, err} }()
	var r res
	select {
	case r = <-ch:
	case <-ctx.Done():
		r.err = ctx.Err()
	}
	if r.err != nil {
		s.opt.Logger.Warn("rejected path", "from", conn.RemoteAddr().String(), "err", r.err)
		conn.CloseWithError(errCodeAuth, "")
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		conn.CloseWithError(errCodeShutdown, "")
		return
	}
	sess := s.sessions[r.h.session]
	var stale []*Session
	if sess == nil {
		sess = newSession(s.opt, r.h.session, false, s.acceptFn)
		if s.opt.SingleSession {
			for id, old := range s.sessions {
				if old.ActivePaths() == 0 {
					delete(s.sessions, id)
					stale = append(stale, old)
				}
			}
		}
		s.sessions[r.h.session] = sess
		sess.log.Info("new session", "from", conn.RemoteAddr().String())
	}
	s.mu.Unlock()
	for _, old := range stale {
		old.log.Info("session superseded by a new session from the peer")
		old.Close()
	}
	if err := writeHelloOK(ctrl, s.key, exp, r.h.session, sess.nonce); err != nil {
		conn.CloseWithError(errCodeShutdown, "")
		return
	}
	name := fmt.Sprintf("#%d@%s", r.h.index, conn.RemoteAddr())
	p := newPath(sess, name, r.h.index, conn, meter, ctrl, br)
	if err := sess.addPath(p, nil); err != nil {
		conn.CloseWithError(errCodeShutdown, "")
		return
	}
	p.run()
}

func (s *Server) reaper() {
	for {
		time.Sleep(5 * time.Second)
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return
		}
		for id, sess := range s.sessions {
			sess.mu.Lock()
			idle := len(sess.paths) == 0 && time.Since(sess.noPathAt) > s.opt.SessionTimeout
			sess.mu.Unlock()
			if idle {
				delete(s.sessions, id)
				sess.log.Info("session expired")
				go sess.Close()
			}
		}
		s.mu.Unlock()
	}
}

// Stats returns per-session path statistics.
func (s *Server) Stats() map[string][]PathStat {
	s.mu.Lock()
	ss := make(map[string]*Session, len(s.sessions))
	for id, x := range s.sessions {
		ss[fmt.Sprintf("%x", id[:4])] = x
	}
	s.mu.Unlock()
	out := make(map[string][]PathStat, len(ss))
	for k, x := range ss {
		out[k] = x.Stats()
	}
	return out
}

// Session returns the best established session: the one with the most active
// paths, preferring the newest. It returns nil if there is none.
func (s *Server) Session() *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	var best *Session
	bestN := -1
	for _, x := range s.sessions {
		n := x.ActivePaths()
		if n > bestN || n == bestN && best != nil && x.created.After(best.created) {
			best, bestN = x, n
		}
	}
	return best
}

// ErrNoSession is returned by Server.OpenStream before any peer connected.
var ErrNoSession = errors.New("no peer connected")

// OpenStream opens a stream on the best session (reverse mode, where the
// listener is the side that originates proxied connections).
func (s *Server) OpenStream() (*Stream, error) {
	sess := s.Session()
	if sess == nil {
		return nil, ErrNoSession
	}
	return sess.OpenStream()
}
