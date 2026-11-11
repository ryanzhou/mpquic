package cc

import (
	"time"

	"github.com/apernet/quic-go/congestion"
	"github.com/apernet/quic-go/monotime"

	"mpquic/internal/cc/common"
)

// follower is the congestion controller of a path whose rate is decided
// elsewhere: by the session, for all its paths together (see Meter.SetRate).
// It has no estimate, start-up or probing of its own. It paces at the rate
// it is given and bounds what is in flight, so a path that stops delivering
// stops sending. (Hysteria's Brutal does the same with a configured rate.)
type follower struct {
	m     *Meter
	rtt   congestion.RTTStatsProvider
	pacer *common.Pacer
	mds   congestion.ByteCount
}

const (
	// followMinRate is the least a path paces at (bytes/s): the pacer must
	// never see zero, and the session's keepalives and acknowledgements
	// must get out.
	followMinRate = 8 << 10
	// followInitial is the rate before the session has assigned one.
	followInitial = 64 << 10
	// A path may have this much in flight: twice what its rate sends in a
	// round trip, and at least what it sends in followSlack on top of one
	// (short paths: acknowledgements come in bunches).
	followSlack     = 50 * time.Millisecond
	followMinWindow = 16 // packets
)

func newFollower(m *Meter, mds congestion.ByteCount) *follower {
	f := &follower{m: m, mds: mds}
	f.pacer = common.NewPacer(f.rate)
	f.pacer.SetMaxDatagramSize(mds)
	return f
}

func (f *follower) rate() congestion.ByteCount {
	r := f.m.rate.Load()
	if r == 0 {
		r = followInitial
	}
	return congestion.ByteCount(max(r, followMinRate))
}

func (f *follower) window() congestion.ByteCount {
	var rtt time.Duration
	if f.rtt != nil {
		rtt = f.rtt.MinRTT()
	}
	if rtt <= 0 {
		return 2 * followMinWindow * f.mds
	}
	d := max(2*rtt, rtt+followSlack)
	return max(congestion.ByteCount(float64(f.rate())*d.Seconds()), followMinWindow*f.mds)
}

func (f *follower) SetRTTStatsProvider(p congestion.RTTStatsProvider) { f.rtt = p }
func (f *follower) TimeUntilSend(congestion.ByteCount) monotime.Time  { return f.pacer.TimeUntilSend() }
func (f *follower) HasPacingBudget(now monotime.Time) bool {
	return f.pacer.Budget(now) >= f.mds
}
func (f *follower) OnPacketSent(t monotime.Time, _ congestion.ByteCount, _ congestion.PacketNumber, n congestion.ByteCount, _ bool) {
	f.pacer.SentPacket(t, n)
}
func (f *follower) CanSend(inFlight congestion.ByteCount) bool {
	if inFlight < f.window() {
		return true
	}
	f.m.blocked.Store(true)
	return false
}
func (f *follower) MaybeExitSlowStart() {}
func (f *follower) OnPacketAcked(congestion.PacketNumber, congestion.ByteCount, congestion.ByteCount, monotime.Time) {
}
func (f *follower) OnCongestionEvent(congestion.PacketNumber, congestion.ByteCount, congestion.ByteCount) {
}
func (f *follower) OnCongestionEventEx(congestion.ByteCount, monotime.Time, []congestion.AckedPacketInfo, []congestion.LostPacketInfo) {
}
func (f *follower) OnRetransmissionTimeout(bool) {}
func (f *follower) SetMaxDatagramSize(s congestion.ByteCount) {
	f.mds = s
	f.pacer.SetMaxDatagramSize(s)
}
func (f *follower) InSlowStart() bool                         { return false }
func (f *follower) InRecovery() bool                          { return false }
func (f *follower) GetCongestionWindow() congestion.ByteCount { return f.window() }

func (f *follower) ModeName() string          { return "follow" }
func (f *follower) BandwidthEstimate() uint64 { return uint64(f.rate()) }
func (f *follower) PacingRateBytes() uint64   { return uint64(f.rate()) }
func (f *follower) Restart()                  {}
func (f *follower) MinRTT() time.Duration {
	if f.rtt == nil {
		return 0
	}
	return f.rtt.MinRTT()
}
