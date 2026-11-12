package cc

import (
	"time"

	"github.com/apernet/quic-go/congestion"
	"github.com/apernet/quic-go/monotime"

	"mpquic/internal/cc/common"
)

// follower is the congestion controller of a path whose rate is decided
// elsewhere: by the session, for all its paths together (see Meter.SetRate).
// It has no start-up or probing of its own, and its rate is not its
// decision. It paces at the rate it is given, and it bounds what is in
// flight to what the path itself delivers in two round trips, so a path
// that delivers less than it is given (a slow relay) queues one round trip's
// worth and no more, and a path that stops delivering stops sending.
// (Hysteria's Brutal paces at a configured rate in the same way; the window
// is BBR's.)
type follower struct {
	m     *Meter
	rtt   congestion.RTTStatsProvider
	pacer *common.Pacer
	mds   congestion.ByteCount

	// The path's delivery rate: bytes acknowledged per second, the highest
	// of the last followWindows measuring windows in which the path had
	// data to send (a window in which it had none says nothing).
	winAt    monotime.Time
	winBytes congestion.ByteCount
	winIdle  bool
	rates    [followWindows]float64
}

const (
	// followMinRate is the least a path paces at (bytes/s): the pacer must
	// never see zero, and the session's keepalives and acknowledgements
	// must get out.
	followMinRate = 8 << 10
	// followInitial is the rate before the session has assigned one.
	followInitial = 64 << 10
	// A path may have in flight what it delivers in two round trips, and at
	// least what it delivers in followSlack on top of one (short paths:
	// acknowledgements come in bunches), and never less than
	// followMinWindow packets (the start, and a path that carried little).
	followSlack     = 25 * time.Millisecond
	followMinWindow = 32 // packets
	followWindows   = 4
	// The window bounds a path that delivers less than this much of its
	// rate (see window).
	followCarries = 0.9
	// A measuring window is a round trip long, and at least this.
	followMinMeasure = 25 * time.Millisecond
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
// it has one. The connection's minimum is for its lifetime, and a path
// whose round trip got longer (a new route, a queue that is not ours) would
// be held back by it.
func (f *follower) baseRTT() time.Duration {
	rtt := time.Duration(f.m.baseRTT.Load())
	if rtt <= 0 && f.rtt != nil {
		rtt = f.rtt.MinRTT()
	}
	return rtt
}

// delivered is the path's delivery rate (bytes/s, 0: not known yet).
func (f *follower) delivered() float64 {
	var r float64
	for _, v := range f.rates {
		r = max(r, v)
	}
	return r
}

// onAcked counts n bytes acknowledged at t.
func (f *follower) onAcked(t monotime.Time, n congestion.ByteCount) {
	if f.winAt == 0 {
		f.winAt = t
	}
	f.winBytes += n
	f.winIdle = f.winIdle || f.m.appLimited.Load()
	d := t.Sub(f.winAt)
	w := max(f.baseRTT(), followMinMeasure)
	if d < w {
		// More than is known shows before the window is over (a start-up:
		// the rate doubles every round trip, and so must the window).
		if r := float64(f.winBytes) / d.Seconds(); d >= w/2 && r > f.rates[0] {
			f.rates[0] = r
		}
		return
	}
	// A window in which the path had nothing to send for a while only
	// counts if it shows more than is known.
	if r := float64(f.winBytes) / d.Seconds(); !f.winIdle || r > f.delivered() {
		copy(f.rates[1:], f.rates[:])
		f.rates[0] = r
	}
	f.winAt, f.winBytes, f.winIdle = t, 0, false
}

func (f *follower) window() congestion.ByteCount {
	floor := followMinWindow * f.mds
	rtt := f.baseRTT()
	if rtt <= 0 {
		return floor
	}
	d := max(2*rtt, rtt+followSlack)
	del := f.delivered()
	w := del * d.Seconds()
	// A path that delivers what it is given is not held back, however long
	// its round trips are: they also grow with queues that are not this
	// path's, or not this direction's (the acknowledgements of a download
	// behind an upload's queue), and a window of two base round trips
	// would then keep the path below its rate.
	if r := float64(f.rate()); del >= followCarries*r && f.rtt != nil {
		w = max(w, r*(f.rtt.SmoothedRTT()+d).Seconds())
	}
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
func (f *follower) OnCongestionEventEx(_ congestion.ByteCount, t monotime.Time, acked []congestion.AckedPacketInfo, _ []congestion.LostPacketInfo) {
	var n congestion.ByteCount
	for _, p := range acked {
		n += p.BytesAcked
	}
	if n > 0 {
		f.onAcked(t, n)
	}
}
func (f *follower) OnRetransmissionTimeout(bool) {}
func (f *follower) SetMaxDatagramSize(s congestion.ByteCount) {
	f.mds = s
	f.pacer.SetMaxDatagramSize(s)
}
func (f *follower) InSlowStart() bool                         { return false }
func (f *follower) InRecovery() bool                          { return false }
func (f *follower) GetCongestionWindow() congestion.ByteCount { return f.window() }
