// Package cc is the congestion controller of a path's QUIC connection: it
// paces at the rate the session gives the path, bounds what the path has in
// flight (follower), and counts what became of the packets of each of the
// session's rounds (Meter), which is what the session's rate controller
// works from (internal/mp/rate.go).
package cc

import (
	"sync/atomic"
	"time"

	"github.com/apernet/quic-go"
	"github.com/apernet/quic-go/congestion"
	"github.com/apernet/quic-go/monotime"
)

// Metrics is a snapshot of a path's controller.
type Metrics struct {
	Rate     uint64 // the rate the path paces at (bytes/s)
	Cwnd     uint64 // what it may have in flight (bytes)
	InFlight uint64 // bytes
	// packets sent / declared lost so far (quic-go only counts losses
	// itself when its built-in controller is used)
	PacketsSent, PacketsLost uint64
}

// Meter wraps a path's follower and publishes its state, and the path's
// counters, through atomics. All CongestionControl methods are invoked from
// the quic-go connection goroutine.
type Meter struct {
	s      *follower
	rate   atomic.Uint64 // bytes/s (see SetRate)
	share  atomic.Uint64 // bytes/s (see SetRate)
	round  atomic.Int64
	rounds [meterRounds]meterRound

	cwnd     atomic.Uint64
	inflight atomic.Uint64

	sentPkts atomic.Uint64
	lostPkts atomic.Uint64
	acked    atomic.Uint64
	blocked  atomic.Bool  // in flight reached the window
	baseRTT  atomic.Int64 // see SetBaseRTT
}

// Rounds: what became of the packets sent in each of the session's rounds.
// Every acknowledgement and loss is counted for the round its packet was
// sent in, however much later it is known, so what a round delivered and
// lost is exact (the session reads it a round or two later) and does not
// depend on when the sender was sending or idle.

const meterRounds = 16

type meterRound struct {
	id                atomic.Int64 // round, +1 (0: unused)
	first             atomic.Int64 // its first packet number
	sent, acked, lost atomic.Uint64
	lastAck           atomic.Int64 // when its latest acknowledgement came (the connection's clock; 0: none)
}

// RoundStat is what became of the packets a path sent in a round.
type RoundStat struct {
	Sent, Acked, Lost uint64 // bytes; acknowledged and declared lost so far
	LastAck           int64  // when the latest acknowledgement came (ns on the connection's clock; 0: none)
}

// Rate is the rate (bytes/s) at which the round's packets were delivered:
// what was acknowledged over the round's length dt, or over the time that
// passed between the last acknowledgement of the round before (prev) and
// the last of this one, if that was longer: the packets queued, and that is
// the rate at which the queue let them through.
func (r RoundStat) Rate(prev RoundStat, dt time.Duration) float64 {
	if dt <= 0 {
		return 0
	}
	if prev.LastAck > 0 && r.LastAck > prev.LastAck {
		dt = max(dt, time.Duration(r.LastAck-prev.LastAck))
	}
	return float64(r.Acked) / dt.Seconds()
}

// StartRound begins round id: packets sent from now on belong to it.
func (m *Meter) StartRound(id int64) { m.round.Store(id) }

// Acked returns the bytes acknowledged so far.
func (m *Meter) Acked() uint64 { return m.acked.Load() }

// TakeBlocked reports whether the path had as much in flight as it may
// have since the last call: the path was not delivering what it was given.
func (m *Meter) TakeBlocked() bool { return m.blocked.Swap(false) }

// Blocked is TakeBlocked without taking.
func (m *Meter) Blocked() bool { return m.blocked.Load() }

// RoundStats returns what is known of the packets sent in round id. Rounds
// older than a dozen are forgotten.
func (m *Meter) RoundStats(id int64) (st RoundStat) {
	if id < 0 {
		return
	}
	r := &m.rounds[id%meterRounds]
	if r.id.Load() != id+1 {
		return // nothing was sent in it
	}
	return RoundStat{r.sent.Load(), r.acked.Load(), r.lost.Load(), r.lastAck.Load()}
}

// roundSent counts a packet for the current round (connection goroutine).
func (m *Meter) roundSent(pn int64, n uint64) {
	id := m.round.Load()
	r := &m.rounds[id%meterRounds]
	if r.id.Load() != id+1 {
		r.sent.Store(0)
		r.acked.Store(0)
		r.lost.Store(0)
		r.lastAck.Store(0)
		r.first.Store(pn)
		r.id.Store(id + 1)
	}
	r.sent.Add(n)
}

// roundOf returns the round in which packet pn was sent (connection
// goroutine), or nil if it is forgotten.
func (m *Meter) roundOf(pn int64) *meterRound {
	cur := m.round.Load()
	for id := cur; id >= 0 && id > cur-meterRounds; id-- {
		r := &m.rounds[id%meterRounds]
		if r.id.Load() == id+1 && r.first.Load() <= pn {
			return r
		}
	}
	return nil
}

var _ congestion.CongestionControlEx = (*Meter)(nil)

// Install replaces the connection's congestion controller with a follower
// of the session's rate (SetRate).
func Install(conn *quic.Conn) *Meter {
	// Seed with the connection's own initial packet size (see quic-go's
	// Conn.InitialPacketSize): it is what quic-go sends until path MTU
	// discovery reports more, and all it sends with a fixed MTU.
	m := &Meter{}
	m.s = newFollower(m, conn.InitialPacketSize())
	conn.SetCongestionControl(m)
	return m
}

// NewFollower returns a Meter that is not attached to a connection (tests).
func NewFollower() *Meter {
	m := &Meter{}
	m.s = newFollower(m, congestion.InitialPacketSize)
	return m
}

// SetRate sets the rate the path paces at, and the rate its window is for:
// its share of the session rate as that was measured (bytes/s). The two
// differ while the session sends less than its rate for a round (a drain),
// and before the session has measured a rate (share 0: the initial window).
func (m *Meter) SetRate(pace, share uint64) {
	m.rate.Store(pace)
	m.share.Store(share)
}

// SetBaseRTT tells the follower the path's round-trip time without queues,
// which bounds what it may have in flight (0: the connection's minimum).
func (m *Meter) SetBaseRTT(d time.Duration) {
	if m.baseRTT.Load() != int64(d) {
		m.baseRTT.Store(int64(d))
	}
}

// Rate returns the rate set with SetRate.
func (m *Meter) Rate() uint64 { return m.rate.Load() }

// Snapshot returns the latest metrics.
func (m *Meter) Snapshot() Metrics {
	return Metrics{
		Rate:        uint64(m.s.rate()),
		Cwnd:        m.cwnd.Load(),
		InFlight:    m.inflight.Load(),
		PacketsSent: m.sentPkts.Load(),
		PacketsLost: m.lostPkts.Load(),
	}
}

func (m *Meter) publish() { m.cwnd.Store(uint64(m.s.GetCongestionWindow())) }

func (m *Meter) SetRTTStatsProvider(p congestion.RTTStatsProvider) {
	m.s.SetRTTStatsProvider(p)
	m.publish()
}
func (m *Meter) TimeUntilSend(b congestion.ByteCount) monotime.Time { return m.s.TimeUntilSend(b) }
func (m *Meter) HasPacingBudget(now monotime.Time) bool             { return m.s.HasPacingBudget(now) }
func (m *Meter) OnPacketSent(t monotime.Time, inflight congestion.ByteCount, pn congestion.PacketNumber, n congestion.ByteCount, rtx bool) {
	m.s.OnPacketSent(t, inflight, pn, n, rtx)
	m.inflight.Store(uint64(inflight + n))
	m.sentPkts.Add(1)
	if rtx { // ACK-only packets are never acknowledged
		m.roundSent(int64(pn), uint64(n))
	}
}
func (m *Meter) CanSend(b congestion.ByteCount) bool { return m.s.CanSend(b) }
func (m *Meter) MaybeExitSlowStart()                 { m.s.MaybeExitSlowStart() }
func (m *Meter) OnPacketAcked(pn congestion.PacketNumber, acked, prior congestion.ByteCount, t monotime.Time) {
	m.s.OnPacketAcked(pn, acked, prior, t)
}
func (m *Meter) OnCongestionEvent(pn congestion.PacketNumber, lost, prior congestion.ByteCount) {
	m.s.OnCongestionEvent(pn, lost, prior)
}
func (m *Meter) OnCongestionEventEx(prior congestion.ByteCount, t monotime.Time, acked []congestion.AckedPacketInfo, lost []congestion.LostPacketInfo) {
	m.s.OnCongestionEventEx(prior, t, acked, lost)
	var a, l congestion.ByteCount
	at := max(int64(t), 1) // the connection's clock: only differences count
	var last *meterRound
	for _, p := range acked {
		a += p.BytesAcked
		if r := m.roundOf(int64(p.PacketNumber)); r != nil {
			r.acked.Add(uint64(p.BytesAcked))
			if r != last {
				last = r
				r.lastAck.Store(at)
			}
		}
	}
	for _, p := range lost {
		l += p.BytesLost
		if r := m.roundOf(int64(p.PacketNumber)); r != nil {
			r.lost.Add(uint64(p.BytesLost))
		}
	}
	if len(lost) > 0 {
		m.lostPkts.Add(uint64(len(lost)))
	}
	if a > 0 {
		m.acked.Add(uint64(a))
	}
	if prior > a+l {
		m.inflight.Store(uint64(prior - a - l))
	} else {
		m.inflight.Store(0)
	}
	m.publish()
}
func (m *Meter) OnRetransmissionTimeout(r bool) { m.s.OnRetransmissionTimeout(r); m.publish() }
func (m *Meter) SetMaxDatagramSize(s congestion.ByteCount) {
	m.s.SetMaxDatagramSize(s)
	m.publish()
}
func (m *Meter) InSlowStart() bool                         { return m.s.InSlowStart() }
func (m *Meter) InRecovery() bool                          { return m.s.InRecovery() }
func (m *Meter) GetCongestionWindow() congestion.ByteCount { return m.s.GetCongestionWindow() }
