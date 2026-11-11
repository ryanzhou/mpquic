// Package cc wires congestion controllers into quic-go connections and exposes
// their internal state (bandwidth estimate, cwnd, bytes in flight) to the
// multipath scheduler in a goroutine-safe way.
package cc

import (
	"fmt"
	"math"
	"strings"
	"sync/atomic"
	"time"

	"github.com/apernet/quic-go"
	"github.com/apernet/quic-go/congestion"
	"github.com/apernet/quic-go/monotime"

	"mpquic/internal/cc/bbr"
)

// Metrics is a snapshot of congestion-controller state.
type Metrics struct {
	Bandwidth uint64 // bytes/s, 0 if unknown
	Cwnd      uint64 // bytes
	InFlight  uint64 // bytes
	MinRTT    time.Duration
	Pacing    uint64 // bytes/s
	Mode      string
	// packets sent / declared lost so far (quic-go only counts losses
	// itself when its built-in controller is used)
	PacketsSent, PacketsLost uint64
	// StaleResets counts bandwidth estimates found stale (rate drop with a
	// standing queue) and reset to the measured delivery rate.
	StaleResets uint64
	// RawBandwidth is the estimate without the rate cap (bytes/s).
	RawBandwidth uint64
	// AckedBytes / SentBytes count the bytes acknowledged and sent so far
	// in ack-eliciting packets (including retransmissions and control
	// traffic).
	AckedBytes, SentBytes uint64
	// LostBytes counts the bytes of packets declared lost.
	LostBytes uint64
	// AckedAt is when AckedBytes last grew (monotonic, since process start).
	AckedAt time.Duration
}

// sender is a path's congestion controller: a follower of the session's
// rate, or a BBR of its own.
type sender interface {
	congestion.CongestionControlEx
	ModeName() string
	BandwidthEstimate() uint64
	RawBandwidthEstimate() uint64
	PacingRateBytes() uint64
	MinRTT() time.Duration
	Restart()
}

// Meter wraps a path's congestion controller and publishes its state, and
// the path's counters, through atomics. All CongestionControl methods are
// invoked from the quic-go connection goroutine.
type Meter struct {
	s      sender
	rate   atomic.Uint64 // bytes/s, for a follower (see SetRate)
	round  atomic.Int64
	rounds [meterRounds]meterRound
	hasRTT atomic.Bool

	bw       atomic.Uint64
	cwnd     atomic.Uint64
	inflight atomic.Uint64
	minRTT   atomic.Int64
	pacing   atomic.Uint64
	mode     atomic.Value
	lastMode string // of publish (the connection's goroutine)

	appLimited atomic.Bool
	probeGate  atomic.Pointer[ProbeGate]
	sentPkts   atomic.Uint64
	lostPkts   atomic.Uint64
	stale      atomic.Uint64
	rawBw      atomic.Uint64
	acked      atomic.Uint64
	sent       atomic.Uint64
	lostBytes  atomic.Uint64
	ackedAt    atomic.Int64
	markAt     atomic.Int64 // pending mark (see Mark), 0 = none
	markAcked  atomic.Uint64
	markTime   atomic.Int64
	rateCap    atomic.Uint64 // bytes/s, 0 = none
	rateGain   atomic.Uint64 // math.Float64bits, 0 = none
	restart    atomic.Bool
	blocked    atomic.Bool // follower: in flight reached the window
}

var processStart = time.Now()

// Mono converts t to the clock of Metrics.AckedAt and Mark.
func Mono(t time.Time) time.Duration { return t.Sub(processStart) }

// Mark asks the meter to remember how many bytes were acknowledged as of
// at (and when the last of them was): the state before the first
// acknowledgement that arrives after at. The session uses it to measure
// what was delivered of the bytes sent up to a moment, which is known one
// round trip later.
func (m *Meter) Mark(at time.Duration) { m.markAt.Store(max(int64(at), 1)) }

// Marked returns the result of the last Mark. If nothing was acknowledged
// since at, that is the current state.
func (m *Meter) Marked() (acked uint64, at time.Duration) {
	if m.markAt.Load() != 0 {
		return m.acked.Load(), time.Duration(m.ackedAt.Load())
	}
	return m.markAcked.Load(), time.Duration(m.markTime.Load())
}

func (m *Meter) takeMark(now int64) {
	if t := m.markAt.Load(); t != 0 && now >= t {
		m.markAcked.Store(m.acked.Load())
		m.markTime.Store(m.ackedAt.Load())
		// not Store(0): a Mark set meanwhile (the session's next round)
		// must stay, or that round would read this one's result again
		m.markAt.CompareAndSwap(t, 0)
	}
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
	firstAck, lastAck atomic.Int64  // when its first and latest acknowledgements came (Mono; 0: none)
	firstBytes        atomic.Uint64 // what the first one acknowledged
}

// RoundStat is what became of the packets a path sent in a round.
type RoundStat struct {
	Sent, Acked, Lost uint64 // bytes; acknowledged and declared lost so far
	// The acknowledgements after the first came over AckSpan and were for
	// SpanBytes. Behind a queue that is longer than the round was, and
	// SpanBytes/AckSpan is the rate at which the queue let them through.
	AckSpan   time.Duration
	SpanBytes uint64
}

// Rate is the rate (bytes/s) at which the round's packets were delivered:
// what was acknowledged over the round's length dt, or over the time the
// acknowledgements took if that was longer (they queued).
func (r RoundStat) Rate(dt time.Duration) float64 {
	if dt <= 0 {
		return 0
	}
	if r.AckSpan > dt && r.SpanBytes > 0 {
		return float64(r.SpanBytes) / r.AckSpan.Seconds()
	}
	return float64(r.Acked) / dt.Seconds()
}

// StartRound begins round id: packets sent from now on belong to it.
func (m *Meter) StartRound(id int64) { m.round.Store(id) }

// Acked returns the bytes acknowledged so far.
func (m *Meter) Acked() uint64 { return m.acked.Load() }

// TakeBlocked reports whether a follower had as much in flight as it may
// have since the last call: the path was not delivering what it was given.
func (m *Meter) TakeBlocked() bool { return m.blocked.Swap(false) }

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
	st.Sent, st.Acked, st.Lost = r.sent.Load(), r.acked.Load(), r.lost.Load()
	if f, l, fb := r.firstAck.Load(), r.lastAck.Load(), r.firstBytes.Load(); l > f && st.Acked > fb {
		st.AckSpan, st.SpanBytes = time.Duration(l-f), st.Acked-fb
	}
	return
}

// roundSent counts a packet for the current round (connection goroutine).
func (m *Meter) roundSent(pn int64, n uint64) {
	id := m.round.Load()
	r := &m.rounds[id%meterRounds]
	if r.id.Load() != id+1 {
		r.sent.Store(0)
		r.acked.Store(0)
		r.lost.Store(0)
		r.firstAck.Store(0)
		r.lastAck.Store(0)
		r.firstBytes.Store(0)
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

// ProbeGate serialises BBR PROBE_RTT among the connections sharing it (the
// paths of one session): while one path has cut its window to 4 packets, the
// others keep carrying the traffic.
type ProbeGate struct {
	holder atomic.Pointer[Meter]
}

func (g *ProbeGate) enter(m *Meter) bool {
	return g.holder.CompareAndSwap(nil, m) || g.holder.Load() == m
}

// Release frees the gate if m holds it (e.g. when m's path is closed).
func (g *ProbeGate) Release(m *Meter) { g.holder.CompareAndSwap(m, nil) }

// SetProbeGate makes m take part in g.
func (m *Meter) SetProbeGate(g *ProbeGate) { m.probeGate.Store(g) }

func (m *Meter) gate(enter bool) bool {
	g := m.probeGate.Load()
	if g == nil {
		return true
	}
	if enter {
		return g.enter(m)
	}
	g.Release(m)
	return true
}

// SetRateCap bounds the controller's bandwidth estimate to bytesPerSec (0
// removes the bound); see bbr.Sender.RateCap.
func (m *Meter) SetRateCap(bytesPerSec uint64) { m.rateCap.Store(bytesPerSec) }

// SetRateGain sets the pacing gain that applies while the rate cap limits
// the estimate (1: none); see bbr.Sender.RateGain.
func (m *Meter) SetRateGain(g float64) { m.rateGain.Store(math.Float64bits(g)) }

// Restart asks the controller to re-enter STARTUP (on its next ACK) if it is
// in PROBE_BW; see bbr.Sender.Restart.
func (m *Meter) Restart() { m.restart.Store(true) }

// SetAppLimited tells the controller whether the application currently has
// nothing to send on this connection.
func (m *Meter) SetAppLimited(v bool) {
	if m.appLimited.Load() != v {
		m.appLimited.Store(v)
	}
}

var _ congestion.CongestionControlEx = (*Meter)(nil)

// ParseProfile validates a congestion setting such as "bbr", "bbr-aggressive".
func ParseProfile(s string) (bbr.Profile, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "", "bbr":
		return bbr.ProfileStandard, nil
	case "bbr-aggressive", "aggressive":
		return bbr.ProfileAggressive, nil
	case "bbr-conservative", "conservative":
		return bbr.ProfileConservative, nil
	case "cubic", "reno", "newreno":
		return ProfileCubic, nil
	}
	return "", fmt.Errorf("unsupported congestion control %q (want bbr, bbr-aggressive, bbr-conservative, cubic)", s)
}

// ProfileCubic keeps quic-go's built-in CUBIC controller (for comparison).
const ProfileCubic bbr.Profile = "cubic"

// Install replaces the connection's congestion controller. With follow, the
// path paces at the rate its session gives it (SetRate); otherwise it runs a
// BBR of its own with the given profile. It returns nil (keeping quic-go's
// built-in CUBIC) for ProfileCubic.
func Install(conn *quic.Conn, profile bbr.Profile, follow bool) *Meter {
	if profile == ProfileCubic {
		return nil
	}
	// Seed with the connection's own initial packet size (see quic-go's
	// Conn.InitialPacketSize): it is what quic-go sends until path MTU
	// discovery reports more, and all it sends with a fixed MTU.
	m := &Meter{}
	if follow {
		m.s = newFollower(m, conn.InitialPacketSize())
	} else {
		b := bbr.NewBbrSender(bbr.DefaultClock{}, conn.InitialPacketSize(), profile)
		b.AppLimitedHint = m.appLimited.Load
		b.ProbeRTTGate = m.gate
		b.RateCap = func() bbr.Bandwidth { return bbr.Bandwidth(m.rateCap.Load()) * bbr.BytesPerSecond }
		b.RateGain = func() float64 { return math.Float64frombits(m.rateGain.Load()) }
		m.s = b
	}
	conn.SetCongestionControl(m)
	return m
}

// NewFollower returns a Meter with a follower that is not attached to a
// connection (tests).
func NewFollower() *Meter {
	m := &Meter{}
	m.s = newFollower(m, congestion.InitialPacketSize)
	return m
}

// SetRate sets the rate a follower paces at (bytes/s).
func (m *Meter) SetRate(bytesPerSec uint64) {
	m.rate.Store(bytesPerSec)
	if m.Follows() { // what Snapshot reports must not wait for the next ACK
		m.bw.Store(bytesPerSec)
		m.rawBw.Store(bytesPerSec)
		m.pacing.Store(bytesPerSec)
	}
}

// Rate returns the rate set with SetRate.
func (m *Meter) Rate() uint64 { return m.rate.Load() }

// Follows reports whether the path's rate is the session's to set.
func (m *Meter) Follows() bool { _, ok := m.s.(*follower); return ok }

// Snapshot returns the latest metrics.
func (m *Meter) Snapshot() Metrics {
	return Metrics{
		Bandwidth: m.bw.Load(),
		Cwnd:      m.cwnd.Load(),
		InFlight:  m.inflight.Load(),
		MinRTT:    time.Duration(m.minRTT.Load()),
		Pacing:    m.pacing.Load(),
		Mode:      m.modeName(),

		PacketsSent:  m.sentPkts.Load(),
		PacketsLost:  m.lostPkts.Load(),
		StaleResets:  m.stale.Load(),
		RawBandwidth: m.rawBw.Load(),
		AckedBytes:   m.acked.Load(),
		SentBytes:    m.sent.Load(),
		LostBytes:    m.lostBytes.Load(),
		AckedAt:      time.Duration(m.ackedAt.Load()),
	}
}

// Inject overwrites the counters and the raw estimate that Snapshot
// reports. It is for tests of code that reads a Meter without a connection.
func (m *Meter) Inject(x Metrics) {
	if x.AckedBytes != m.acked.Load() {
		m.takeMark(int64(x.AckedAt))
	}
	m.sentPkts.Store(x.PacketsSent)
	m.lostPkts.Store(x.PacketsLost)
	m.acked.Store(x.AckedBytes)
	m.sent.Store(x.SentBytes)
	m.lostBytes.Store(x.LostBytes)
	m.ackedAt.Store(int64(x.AckedAt))
	m.rawBw.Store(x.RawBandwidth)
}

// RateCap and RateGain return what SetRateCap and SetRateGain last set.
func (m *Meter) RateCap() uint64   { return m.rateCap.Load() }
func (m *Meter) RateGain() float64 { return math.Float64frombits(m.rateGain.Load()) }

func (m *Meter) modeName() string {
	if v, ok := m.mode.Load().(string); ok {
		return v
	}
	return ""
}

func (m *Meter) publish() {
	// stored only when it changes: boxing the string allocates, and this
	// runs for every ACK
	if name := m.s.ModeName(); name != m.lastMode {
		m.lastMode = name
		m.mode.Store(name)
	}
	m.bw.Store(m.s.BandwidthEstimate())
	m.rawBw.Store(m.s.RawBandwidthEstimate())
	m.cwnd.Store(uint64(m.s.GetCongestionWindow()))
	if b, ok := m.s.(*bbr.Sender); ok {
		m.stale.Store(b.StaleResets)
	}
	if m.hasRTT.Load() {
		m.minRTT.Store(int64(m.s.MinRTT()))
		m.pacing.Store(m.s.PacingRateBytes())
	}
}

func (m *Meter) SetRTTStatsProvider(p congestion.RTTStatsProvider) {
	m.s.SetRTTStatsProvider(p)
	m.hasRTT.Store(true)
	m.publish()
}
func (m *Meter) TimeUntilSend(b congestion.ByteCount) monotime.Time { return m.s.TimeUntilSend(b) }
func (m *Meter) HasPacingBudget(now monotime.Time) bool             { return m.s.HasPacingBudget(now) }
func (m *Meter) OnPacketSent(t monotime.Time, inflight congestion.ByteCount, pn congestion.PacketNumber, n congestion.ByteCount, rtx bool) {
	m.s.OnPacketSent(t, inflight, pn, n, rtx)
	m.inflight.Store(uint64(inflight + n))
	m.sentPkts.Add(1)
	if rtx {
		m.sent.Add(uint64(n)) // ACK-only packets are never acknowledged
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
	if m.restart.Load() && m.restart.CompareAndSwap(true, false) {
		m.s.Restart()
	}
	m.s.OnCongestionEventEx(prior, t, acked, lost)
	var a, l congestion.ByteCount
	now := int64(Mono(time.Now()))
	var last *meterRound
	for _, p := range acked {
		a += p.BytesAcked
		if r := m.roundOf(int64(p.PacketNumber)); r != nil {
			r.acked.Add(uint64(p.BytesAcked))
			if r != last {
				last = r
				if r.firstAck.Load() == 0 {
					r.firstAck.Store(now)
				}
				r.lastAck.Store(now)
			}
			if r.firstAck.Load() == now {
				r.firstBytes.Add(uint64(p.BytesAcked))
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
		m.lostBytes.Add(uint64(l))
	}
	if a > 0 {
		m.takeMark(now)
		m.acked.Add(uint64(a))
		m.ackedAt.Store(now)
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
