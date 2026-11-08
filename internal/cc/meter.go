// Package cc wires congestion controllers into quic-go connections and exposes
// their internal state (bandwidth estimate, cwnd, bytes in flight) to the
// multipath scheduler in a goroutine-safe way.
package cc

import (
	"fmt"
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
	// AckedBytes counts bytes acknowledged so far.
	AckedBytes uint64
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

	appLimited atomic.Bool
	probeGate  atomic.Pointer[ProbeGate]
	sentPkts   atomic.Uint64
	lostPkts   atomic.Uint64
	stale      atomic.Uint64
	rawBw      atomic.Uint64
	acked      atomic.Uint64
	rateCap    atomic.Uint64 // bytes/s, 0 = none
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

// SetRateCap bounds the controller's bandwidth estimate to bytesPerSec (0
// removes the bound); see bbr.Sender.RateCap.
func (m *Meter) SetRateCap(bytesPerSec uint64) { m.rateCap.Store(bytesPerSec) }

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

// Install replaces the connection's congestion controller with a metered BBR.
// It returns nil (keeping quic-go's built-in CUBIC) for ProfileCubic.
func Install(conn *quic.Conn, profile bbr.Profile) *Meter {
	if profile == ProfileCubic {
		return nil
	}
	// Seed with the connection's own initial packet size (see quic-go's
	// Conn.InitialPacketSize): it is what quic-go sends until path MTU
	// discovery reports more, and all it sends with a fixed MTU.
	m := &Meter{s: bbr.NewBbrSender(bbr.DefaultClock{}, conn.InitialPacketSize(), profile)}
	m.s.AppLimitedHint = m.appLimited.Load
	m.s.ProbeRTTGate = m.gate
	m.s.RateCap = func() bbr.Bandwidth { return bbr.Bandwidth(m.rateCap.Load()) * bbr.BytesPerSecond }
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
		StaleResets:  m.stale.Load(),
		RawBandwidth: m.rawBw.Load(),
		AckedBytes:   m.acked.Load(),
	}
}

func (m *Meter) modeName() string {
	if v, ok := m.mode.Load().(string); ok {
		return v
	}
	return ""
}

func (m *Meter) publish() {
	m.mode.Store(m.s.ModeName())
	m.bw.Store(m.s.BandwidthEstimate())
	m.rawBw.Store(m.s.RawBandwidthEstimate())
	m.cwnd.Store(uint64(m.s.GetCongestionWindow()))
	m.stale.Store(m.s.StaleResets)
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
