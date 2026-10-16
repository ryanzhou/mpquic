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
}

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
	}
	return "", fmt.Errorf("unsupported congestion control %q (want bbr, bbr-aggressive, bbr-conservative)", s)
}

// Install replaces the connection's congestion controller with a metered BBR.
func Install(conn *quic.Conn, profile bbr.Profile) *Meter {
	size := conn.InitialPacketSize()
	if g := bbr.GetInitialPacketSize(conn.RemoteAddr()); g < size {
		size = g
	}
	m := &Meter{s: bbr.NewBbrSender(bbr.DefaultClock{}, size, profile)}
	m.s.AppLimitedHint = m.appLimited.Load
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
