package cc

import (
	"time"

	"github.com/apernet/quic-go/congestion"
	"github.com/apernet/quic-go/monotime"

	"mpquic/internal/cc/common"
)

// follower is the congestion controller of a path. The path's rate is not
// its decision: the session decides it for all its paths together (see
// Meter.SetRate), and the follower has no start-up or probing of its own.
// It does two things:
//
//   - It paces at the rate it is given. (Hysteria's Brutal paces at a
//     configured rate in the same way.)
//   - It bounds what is in flight to what the path's share of the session
//     rate needs over two round trips without a queue. This is BBR's
//     window: a path that carries its share has one round trip's worth in
//     flight, so the second is room for a queue of one round trip (and for
//     acknowledgements that come in bunches). A path that carries less
//     than it is given (a slow relay, a bottleneck whose capacity dropped)
//     queues that much and no more, and a path that stops delivering stops
//     sending. The session hears when the window held the path back
//     (Meter.TakeBlocked).
//
// The window is for the share, not for the rate of the moment (as BBR's is
// for its bandwidth estimate, not its pacing rate): a round that sends less
// to drain a queue must not find its window shrunk below what is in flight.
// And it is for what the path is given, not for what it delivers: a window
// that follows the delivery rate shrinks when the path delivers less because
// the window holds it back, and so shrinks itself.
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
	// On a short path the second round trip is too little room:
	// acknowledgements come every few milliseconds at best, and in bunches.
	// The window is at least what the rate needs over one round trip and
	// followSlack (quic-go's maximum ACK delay), and never less than
	// followMinWindow packets (quic-go's initial window).
	followSlack     = 25 * time.Millisecond
	followMinWindow = 32
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

// baseRTT is the path's round trip without queues: the session's figure if
// it has one (the lowest of the last ten seconds). The connection's minimum
// is for its lifetime, and a path whose round trip got longer (a new route)
// would be held back by it for good.
func (f *follower) baseRTT() time.Duration {
	rtt := time.Duration(f.m.baseRTT.Load())
	if rtt <= 0 && f.rtt != nil {
		rtt = f.rtt.MinRTT()
	}
	return rtt
}

func (f *follower) window() congestion.ByteCount {
	floor := followMinWindow * f.mds
	rtt := f.baseRTT()
	if rtt <= 0 {
		return floor
	}
	w := float64(f.m.share.Load()) * max(2*rtt, rtt+followSlack).Seconds()
	return max(congestion.ByteCount(w), floor)
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
