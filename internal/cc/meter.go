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

// Meter wraps a BBR sender and publishes its state through atomics. All
// CongestionControl methods are invoked from the quic-go connection goroutine.
type Meter struct {
	s      *bbr.Sender
	hasRTT atomic.Bool

	bw       atomic.Uint64
	cwnd     atomic.Uint64
	inflight atomic.Uint64
	minRTT   atomic.Int64
	pacing   atomic.Uint64
	mode     atomic.Value
	lastMode string // of publish (the connection's goroutine)

	appLimited atomic.Bool
	hadNoData  atomic.Bool // ... at some moment since the controller last asked
	probeGate  atomic.Pointer[ProbeGate]
	sentPkts   atomic.Uint64
	lostPkts   atomic.Uint64
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
	lossTol    atomic.Uint64 // math.Float64bits
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

// SetLossTolerance sets the share of loss (0..1) that the controller does
// not take for congestion; see bbr.Sender.LossTolerance.
func (m *Meter) SetLossTolerance(t float64) { m.lossTol.Store(math.Float64bits(t)) }

// SetAppLimited tells the controller whether the application currently has
// nothing to send on this connection. Every moment without data counts,
// however short: the controller is told at its next packet or
// acknowledgement that there was one (appLimitedHint), as a TCP sender
// notes the moment its send buffer runs empty. A path that the scheduler
// feeds with gaps would otherwise have its lower delivery rate taken for
// what it carries.
func (m *Meter) SetAppLimited(v bool) {
	if v {
		m.hadNoData.Store(true)
	}
	if m.appLimited.Load() != v {
		m.appLimited.Store(v)
	}
}

func (m *Meter) appLimitedHint() bool {
	had := m.hadNoData.Load() && m.hadNoData.Swap(false)
	return m.appLimited.Load() || had
}

var _ congestion.CongestionControlEx = (*Meter)(nil)

// Congestion settings: BBR (version 3) on every path, or quic-go's built-in
// CUBIC for comparison. "bbr-aggressive" and "bbr-conservative" named
// variants of the earlier BBRv1 controller and now mean "bbr".
const (
	ProfileBBR   = "bbr"
	ProfileCubic = "cubic"
)

// ParseProfile validates a congestion setting.
func ParseProfile(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "bbr", "bbr-standard", "bbr-aggressive", "bbr-conservative":
		return ProfileBBR, nil
	case "cubic":
		return ProfileCubic, nil
	}
	return "", fmt.Errorf("unsupported congestion control %q (bbr, cubic)", s)
}

// Install replaces the connection's congestion controller with a metered BBR.
// It returns nil (keeping quic-go's built-in CUBIC) for ProfileCubic.
func Install(conn *quic.Conn, profile string) *Meter {
	if profile == ProfileCubic {
		return nil
	}
	// Seed with the connection's own initial packet size (see quic-go's
	// Conn.InitialPacketSize): it is what quic-go sends until path MTU
	// discovery reports more, and all it sends with a fixed MTU.
	m := &Meter{s: bbr.NewBbrSender(bbr.DefaultClock{}, conn.InitialPacketSize())}
	m.s.AppLimitedHint = m.appLimitedHint
	m.s.ProbeRTTGate = m.gate
	m.s.RateCap = func() bbr.Bandwidth { return bbr.Bandwidth(m.rateCap.Load()) * bbr.BytesPerSecond }
	m.s.RateGain = func() float64 { return math.Float64frombits(m.rateGain.Load()) }
	m.s.LossTolerance = func() float64 { return math.Float64frombits(m.lossTol.Load()) }
	conn.SetCongestionControl(m)
	return m
}

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
	for _, p := range acked {
		a += p.BytesAcked
	}
	for _, p := range lost {
		l += p.BytesLost
	}
	if len(lost) > 0 {
		m.lostPkts.Add(uint64(len(lost)))
		m.lostBytes.Add(uint64(l))
	}
	if a > 0 {
		now := int64(Mono(time.Now()))
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
