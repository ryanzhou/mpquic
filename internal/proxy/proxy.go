package proxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"mpquic/internal/mp"
)

// Opener is the side of a multipath connection that originates proxied
// streams: *mp.Client normally, *mp.Server in reverse mode.
type Opener interface {
	OpenStream() (*mp.Stream, error)
	Session() *mp.Session // nil if no session is established
}

// Local accepts TCP connections (SOCKS5, or a fixed tunnel target) and
// forwards them over the multipath connection.
type Local struct {
	Client Opener
	Log    *slog.Logger
	// Tunnel, if non-empty, makes every connection go to this host:port
	// (ss-tunnel mode) instead of speaking SOCKS5.
	Tunnel string
	// UDP enables SOCKS5 UDP ASSOCIATE.
	UDP *UDPLocal
}

// Serve accepts connections on ln until it is closed.
func (l *Local) Serve(ln net.Listener) error {
	var tunnelAddr []byte
	if l.Tunnel != "" {
		a, err := EncodeAddr(l.Tunnel)
		if err != nil {
			return err
		}
		tunnelAddr = a
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			return err
		}
		go l.handle(c, tunnelAddr)
	}
}

func (l *Local) handle(c net.Conn, tunnelAddr []byte) {
	setNoDelay(c)
	raw, target := tunnelAddr, l.Tunnel
	if raw == nil {
		c.SetDeadline(time.Now().Add(30 * time.Second))
		cmd, r, t, err := socksHandshake(c, l.UDP != nil)
		if err != nil {
			l.Log.Debug("socks handshake failed", "from", c.RemoteAddr().String(), "err", err)
			c.Close()
			return
		}
		c.SetDeadline(time.Time{})
		if cmd == cmdUDPAssociate {
			l.UDP.associate(c)
			return
		}
		raw, target = r, t
	}
	st, err := l.Client.OpenStream()
	if err != nil {
		l.Log.Debug("cannot open stream", "target", target, "err", err)
		c.Close()
		return
	}
	if _, err := st.Write(raw); err != nil {
		st.Reset(mp.ResetGeneric)
		st.Close()
		c.Close()
		return
	}
	if tunnelAddr == nil {
		// Reply immediately (like ss-local); connect errors surface as a reset.
		if _, err := c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
			st.Reset(mp.ResetGeneric)
			st.Close()
			c.Close()
			return
		}
	}
	l.Log.Debug("connect", "target", target, "stream", st.ID())
	Pipe(c, st)
}

// RemoteOptions configure the remote's outbound TCP connections.
type RemoteOptions struct {
	// DialTimeout bounds DNS resolution plus connection setup (default 10s).
	DialTimeout time.Duration
	// RaceConnect races up to 5 staggered connection attempts per target
	// (see raceDialer) and keeps the first that connects.
	RaceConnect bool
}

// RemoteHandler returns a stream handler for the server side: it reads the
// target address, dials it and relays data.
func RemoteHandler(log *slog.Logger, opt RemoteOptions) func(*mp.Stream) {
	if opt.DialTimeout <= 0 {
		opt.DialTimeout = 10 * time.Second
	}
	d := &net.Dialer{Timeout: opt.DialTimeout, KeepAlive: 30 * time.Second}
	var race *raceDialer
	if opt.RaceConnect {
		race = newRaceDialer()
	}
	return func(st *mp.Stream) {
		target, _, err := ReadAddr(st)
		if err != nil {
			st.Reset(mp.ResetGeneric)
			st.Close()
			return
		}
		start := time.Now()
		var c net.Conn
		var extra []any
		if race != nil {
			ctx, cancel := context.WithTimeout(context.Background(), opt.DialTimeout)
			var rs raceStats
			c, rs, err = race.dialContext(ctx, target)
			cancel()
			extra = []any{"attempts", rs.attempts, "winner", rs.winner}
		} else {
			c, err = d.Dial("tcp", target)
		}
		dial := time.Since(start).Round(time.Millisecond)
		if err != nil {
			log.Debug("dial failed", append([]any{"target", target, "err", err, "after", dial}, extra...)...)
			st.Reset(mp.ResetDialFailed)
			st.Close()
			return
		}
		setNoDelay(c)
		log.Debug("connect", append([]any{"target", target, "stream", st.ID(), "dial", dial}, extra...)...)
		Pipe(c, st)
	}
}

func setNoDelay(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
}

var bufPool = sync.Pool{New: func() any { b := make([]byte, 32<<10); return &b }}

// Pipe relays data between a TCP connection and a stream in both directions
// with half-close propagation, then closes both. Besides *net.TCPConn, c may
// implement CloseWrite() error (half-close) and Abort() (close with a reset).
func Pipe(c net.Conn, st *mp.Stream) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		bp := bufPool.Get().(*[]byte)
		_, err := io.CopyBuffer(st, readerOnly{c}, *bp)
		bufPool.Put(bp)
		if err != nil {
			st.Reset(mp.ResetGeneric)
			return
		}
		st.CloseWrite()
	}()
	bp := bufPool.Get().(*[]byte)
	_, err := io.CopyBuffer(writerOnly{c}, st, *bp)
	bufPool.Put(bp)
	if err != nil {
		// Peer reset or local write error: abort both sides.
		st.Reset(mp.ResetGeneric)
		if tc, ok := c.(*net.TCPConn); ok {
			tc.SetLinger(0)
		} else if a, ok := c.(interface{ Abort() }); ok {
			a.Abort()
		}
		c.Close()
	} else if cw, ok := c.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}
	<-done
	c.Close()
	st.Close()
}

type readerOnly struct{ io.Reader }
type writerOnly struct{ io.Writer }
