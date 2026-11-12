package mp

import (
	"log/slog"
	"syscall"
	"time"
)

// Options configure both ends of a multipath session.
type Options struct {
	Password string
	// Control, if set, is called on every socket of the multipath
	// connection before it is used (the UDP sockets of the paths, and the
	// DNS queries for the servers' names), e.g. to set a firewall mark.
	Control func(network, address string, c syscall.RawConn) error
	// Window is the maximum per-stream flow-control / reorder window in
	// bytes. Each stream starts at 8 MB and auto-tunes up to Window when the
	// window limits throughput. A single stream's rate is bounded by about
	// Window / (slowest path RTT).
	Window int64
	// ChunkSize is the maximum scheduling unit (bytes). Smaller chunks give
	// finer-grained scheduling at the cost of per-chunk overhead.
	ChunkSize int
	// IdleTimeout resets proxied streams without activity (ss "timeout").
	IdleTimeout time.Duration
	// PathIdleTimeout is the QUIC idle timeout of a path connection.
	PathIdleTimeout  time.Duration
	HandshakeTimeout time.Duration
	// SuspectTimeout is the minimum time a ping may stay unanswered before a
	// path is considered suspect (no new data is scheduled on it and its
	// in-flight data is reinjected on other paths).
	SuspectTimeout time.Duration
	// PathDeadTimeout closes (and, on the client, re-dials) a path that has
	// been suspect for this long.
	PathDeadTimeout time.Duration
	// SessionTimeout (server): forget a session that has had no paths for this long.
	SessionTimeout time.Duration
	// AckDelay is the maximum time acknowledgements are coalesced.
	AckDelay time.Duration
	// NoDup disables opportunistic duplication of in-flight data onto idle
	// faster paths (it is on by default; it only uses otherwise idle capacity).
	NoDup bool
	// MaxSendRate / MaxRecvRate, if > 0, are ceilings for what the session
	// sends and receives over all paths together, in bytes/s: the known
	// rate of this endpoint's uplink and downlink. The receive limit is
	// announced to the peer, which then sends no faster. The session still
	// adapts below a limit; it never probes above it.
	MaxSendRate, MaxRecvRate uint64
	// MTU, if > 0, is the path MTU (IP packet size) of every path: QUIC
	// packets get a fixed size that fits it (UDP payload = MTU - 28 for IPv4
	// paths, - 48 for IPv6; 1200..1452) and path MTU discovery is off.
	// 0 starts at 1200-byte packets and discovers each path's MTU.
	MTU int
	// StatsInterval logs per-path statistics periodically if > 0.
	StatsInterval time.Duration
	// TLS certificate for the server (optional; self-signed if empty).
	CertFile, KeyFile string
	// SNI sent by the client (optional).
	SNI    string
	Logger *slog.Logger
	// StreamHandler, if set, accepts streams opened by the peer (it runs in
	// its own goroutine per stream). Normally only the remote sets it; in
	// reverse mode the remote is the QUIC dialer (Client) and still does.
	StreamHandler func(*Stream)
	// SingleSession (listener side): expect a single peer. When a new session
	// is established, older sessions without active paths are closed at once
	// (their peer evidently restarted) instead of waiting for SessionTimeout.
	SingleSession bool
	// DatagramHandler receives unreliable datagrams sent by the peer with
	// Session.SendDatagram (used for UDP relaying). Nil drops them.
	DatagramHandler func(s *Session, b []byte)
}

func (o *Options) withDefaults() *Options {
	c := *o
	if c.Window <= 0 {
		c.Window = 64 << 20
	}
	if c.Window < 256<<10 {
		c.Window = 256 << 10
	}
	if c.ChunkSize <= 0 {
		c.ChunkSize = 16 << 10
	}
	if c.ChunkSize < 1024 {
		c.ChunkSize = 1024
	}
	if c.ChunkSize > 256<<10 {
		c.ChunkSize = 256 << 10
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = 300 * time.Second
	}
	if c.PathIdleTimeout <= 0 {
		c.PathIdleTimeout = 30 * time.Second
	}
	if c.HandshakeTimeout <= 0 {
		c.HandshakeTimeout = 5 * time.Second
	}
	if c.SuspectTimeout <= 0 {
		c.SuspectTimeout = 600 * time.Millisecond
	}
	if c.PathDeadTimeout <= 0 {
		c.PathDeadTimeout = 6 * time.Second
	}
	if c.SessionTimeout <= 0 {
		c.SessionTimeout = 5 * time.Minute
	}
	if c.AckDelay <= 0 {
		c.AckDelay = 2 * time.Millisecond
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return &c
}

// initialPeerWindow is the window assumed before the first ACK arrives.
const initialPeerWindow = 8 << 20
