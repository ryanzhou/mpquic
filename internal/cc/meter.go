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
	probeGate  atomic.Pointer[ProbeGate]
	sentPkts   atomic.Uint64
	lostPkts   atomic.Uint64
	rateCap    atomic.Uint64 // bytes/s, 0 = none
	lossTol    atomic.Uint64 // math.Float64bits
	restart    atomic.Bool
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

// SetRateCap bounds the controller's sending rate to bytesPerSec (0 removes
// the bound); see bbr.Sender.RateCap.
func (m *Meter) SetRateCap(bytesPerSec uint64) { m.rateCap.Store(bytesPerSec) }

// RateCap returns what SetRateCap last set.
func (m *Meter) RateCap() uint64 { return m.rateCap.Load() }

// SetLossTolerance sets the share of loss (0..1) that the controller does
// not take for congestion; see bbr.Sender.LossTolerance.
func (m *Meter) SetLossTolerance(t float64) { m.lossTol.Store(math.Float64bits(t)) }

// SetAppLimited tells the controller whether the application currently has
// nothing to send on this connection.
func (m *Meter) SetAppLimited(v bool) {
	if m.appLimited.Load() != v {
		m.appLimited.Store(v)
	}
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
	m.s.AppLimitedHint = m.appLimited.Load
	m.s.ProbeRTTGate = m.gate
	m.s.RateCap = func() bbr.Bandwidth { return bbr.Bandwidth(m.rateCap.Load()) * bbr.BytesPerSecond }
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

		PacketsSent: m.sentPkts.Load(),
		PacketsLost: m.lostPkts.Load(),
	}
}

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
	for _, p := range acked {
		a += p.BytesAcked
	}
	for _, p := range lost {
		l += p.BytesLost
	}
	m.lostPkts.Add(uint64(len(lost)))
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
