package mp

import (
	"log/slog"
	"time"
)

// Options configure both ends of a multipath session.
type Options struct {
	Password string
	// Window is the per-stream flow-control / reorder window in bytes. It must
	// exceed the aggregate bandwidth-delay product of all paths plus the
	// delay skew between the fastest and slowest path.
	Window int64
	// ChunkSize is the maximum scheduling unit (bytes). Smaller chunks give
	// finer-grained scheduling at the cost of per-chunk overhead.
	ChunkSize int
	// Congestion selects the per-path congestion controller.
	Congestion string
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
	// StatsInterval logs per-path statistics periodically if > 0.
	StatsInterval time.Duration
	// TLS certificate for the server (optional; self-signed if empty).
	CertFile, KeyFile string
	// SNI sent by the client (optional).
	SNI    string
	Logger *slog.Logger
	// DatagramHandler receives unreliable datagrams sent by the peer with
	// Session.SendDatagram (used for UDP relaying). Nil drops them.
	DatagramHandler func(s *Session, b []byte)
}

func (o *Options) withDefaults() *Options {
	c := *o
	if c.Window <= 0 {
		c.Window = 16 << 20
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
const initialPeerWindow = 4 << 20
