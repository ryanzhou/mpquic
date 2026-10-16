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
	"time"

	"github.com/apernet/quic-go"

	"mpquic/internal/cc"
	"mpquic/internal/cc/bbr"
)

// Client maintains one session to a remote instance over many paths (one per
// configured server address; typically the direct address plus UDP relays).
type Client struct {
	opt     *Options
	key     []byte
	tlsConf *tls.Config
	qconf   *quic.Config
	profile bbr.Profile
	sess    *Session
	servers []string
	wg      sync.WaitGroup
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
	c.sess = newSession(o, id, true, nil)
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
	raddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return err
	}
	network := "udp4"
	if raddr.IP.To4() == nil {
		network = "udp6"
	}
	udp, err := net.ListenUDP(network, nil)
	if err != nil {
		return err
	}
	tr := &quic.Transport{Conn: udp}
	defer udp.Close()
	defer tr.Close()

	ctx, cancel := context.WithTimeout(c.sess.ctx, c.opt.HandshakeTimeout)
	defer cancel()
	conn, err := tr.Dial(ctx, raddr, c.tlsConf, c.qconf)
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

// Server accepts paths from clients and groups them into sessions.
type Server struct {
	opt      *Options
	key      []byte
	tlsConf  *tls.Config
	qconf    *quic.Config
	profile  bbr.Profile
	acceptFn func(*Stream)

	mu        sync.Mutex
	sessions  map[[16]byte]*Session
	listeners []*quic.Listener
	closed    bool
}

// NewServer creates a server; handler is called (in its own goroutine) for
// every new incoming stream.
func NewServer(opt *Options, handler func(*Stream)) (*Server, error) {
	o := opt.withDefaults()
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
	go s.reaper()
	return s, nil
}

// ListenAndServe listens on a UDP address and serves until Close.
func (s *Server) ListenAndServe(addr string) error {
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return err
	}
	udp, err := net.ListenUDP("udp", ua)
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
	if sess == nil {
		sess = newSession(s.opt, r.h.session, false, s.acceptFn)
		s.sessions[r.h.session] = sess
		sess.log.Info("new session", "from", conn.RemoteAddr().String())
	}
	s.mu.Unlock()
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
