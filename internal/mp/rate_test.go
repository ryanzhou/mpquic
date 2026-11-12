package mp

import (
	"math"
	"testing"
	"time"

	"github.com/apernet/quic-go/congestion"
	"github.com/apernet/quic-go/monotime"

	"mpquic/internal/cc"
)

// rateSim is a fluid model of a session's paths: every path sends at the
// rate the session gives it (as far as the application has data and the
// path's window allows), through a bottleneck of its own if it has one and
// then, half a round trip later, through one that all share: a queue with a
// buffer, or a policer with a burst allowance. What a path sent in a step is
// acknowledged, or known to be lost, another half round trip later (plus
// the time it queued). The paths' round trips differ, so what they send in
// the same moment reaches the common bottleneck at different times.
type rateSim struct {
	s        *Session
	now      time.Time
	el       time.Duration
	capacity float64       // common bottleneck (bytes/s), 0 = none
	buffer   float64       // ... its buffer (seconds of capacity), for a queue
	bucket   float64       // ... or its burst allowance (seconds of capacity), for a policer
	tokens   float64       // ... and what is left of that (bytes)
	q        float64       // ... or what is queued (bytes)
	demand   float64       // what the application sends at most (bytes/s), 0 = all it can
	extra    time.Duration // delay that is not the session's: a queue in the ACK direction
	paths    []*simPath

	sent, lost, dlv float64          // totals since reset
	peak            float64          // highest send rate over a second since reset (bytes/s)
	win             []float64        // what the last hundred ticks sent
	winSum          float64          // ... and this one so far
	qsum            time.Duration    // the common queue's delay, summed over the ticks since reset
	ticks           int              // ... and their number
	trace           func(x *rateSim) // called every tick, if set
}

type simPath struct {
	base     time.Duration
	own      float64 // own bottleneck (bytes/s), 0 = none
	ownBuf   float64 // ... its buffer (seconds)
	q        float64
	loss     float64 // random loss
	pn       int64
	inflight float64
	transit  []simAck
	pend     []simAck
	rtt      simRTT
	sent     float64       // since reset
	qsum     time.Duration // its own queue\'s delay, summed over the steps it sent in since reset
	qn       int
}

// simAck is what a path sent in a step, on its way: to the common
// bottleneck (transit), then back as an acknowledgement (pend).
type simAck struct {
	at          time.Duration // when it arrives
	sent        time.Duration
	pn          int64
	acked, lost float64
}

type simRTT struct{ min, latest time.Duration }

func (r *simRTT) MinRTT() time.Duration        { return r.min }
func (r *simRTT) LatestRTT() time.Duration     { return r.latest }
func (r *simRTT) SmoothedRTT() time.Duration   { return r.latest }
func (r *simRTT) MeanDeviation() time.Duration { return 0 }
func (r *simRTT) MaxAckDelay() time.Duration   { return 0 }
func (r *simRTT) PTO(bool) time.Duration       { return 2 * r.latest }
func (r *simRTT) UpdateRTT(_, _ time.Duration) {}
func (r *simRTT) SetMaxAckDelay(time.Duration) {}
func (r *simRTT) SetInitialRTT(time.Duration)  {}

func mbit(x float64) float64 { return x * 1e6 / 8 }

// newRateSim: a common queue of capacity with a buffer of buffer seconds
// (capacity 0: none), and a path per base round-trip time.
func newRateSim(capacity, buffer float64, base ...time.Duration) *rateSim {
	s := &Session{opt: &Options{}, log: testLogger(), dialer: true}
	x := &rateSim{s: s, now: time.Now(), capacity: capacity, buffer: buffer}
	for i, r := range base {
		sp := &simPath{base: r}
		p := &Path{sess: s, id: i + 1, meter: cc.NewFollower(), state: stateActive}
		p.meter.SetRTTStatsProvider(&sp.rtt)
		s.paths = append(s.paths, p)
		x.paths = append(x.paths, sp)
	}
	return x
}

// policer makes the common bottleneck drop what exceeds its rate, after a
// burst of burst seconds of that rate.
func (x *rateSim) policer(burst float64) *rateSim {
	x.buffer, x.bucket, x.tokens = 0, burst, burst*x.capacity
	return x
}

// own gives the paths bottlenecks of their own (bytes/s) with buffers of buf seconds.
func (x *rateSim) own(buf float64, rates ...float64) *rateSim {
	for i, r := range rates {
		x.paths[i].own, x.paths[i].ownBuf = r, buf
	}
	return x
}

// simStep is the model's resolution: acknowledgements arrive all the time,
// not only when the session looks.
const simStep = time.Millisecond

func (x *rateSim) run(d time.Duration) {
	dt := simStep.Seconds()
	for end := x.el + d; x.el < end; x.el += simStep {
		x.now = x.now.Add(simStep)
		t := monotime.Time(int64(time.Hour) + int64(x.el))
		var total float64
		for _, p := range x.s.paths {
			if p.state == stateActive {
				total += float64(max(p.meter.Rate(), 1))
			}
		}
		// The paths send, each through its own bottleneck. What gets
		// through reaches the common one half a round trip later.
		var tick float64
		for i, p := range x.s.paths {
			sp := x.paths[i]
			if p.state != stateActive {
				continue
			}
			rate := float64(p.meter.Rate())
			if rate == 0 {
				rate = 64 << 10
			}
			want := rate * dt
			if x.demand > 0 { // what the application has, shared out as the rates are
				want = min(want, x.demand*dt*rate/total)
			}
			if !p.meter.CanSend(congestion.ByteCount(sp.inflight)) {
				want = 0
			}
			if want < 1 {
				continue
			}
			drop := want * sp.loss
			adm := want - drop
			delay := x.extra
			if sp.own > 0 {
				sp.q += adm
				sp.q -= min(sp.q, sp.own*dt)
				if limit := sp.ownBuf * sp.own; sp.q > limit {
					d := min(sp.q-limit, adm)
					sp.q -= d
					adm -= d
					drop += d
				}
				delay += time.Duration(sp.q / sp.own * float64(time.Second))
				sp.qsum += time.Duration(sp.q / sp.own * float64(time.Second))
				sp.qn++
			}
			// two "packets": what gets through, and what does not
			p.meter.OnPacketSent(t, congestion.ByteCount(sp.inflight), congestion.PacketNumber(sp.pn), congestion.ByteCount(adm), true)
			p.meter.OnPacketSent(t, congestion.ByteCount(sp.inflight+adm), congestion.PacketNumber(sp.pn+1), congestion.ByteCount(drop), true)
			sp.transit = append(sp.transit, simAck{at: x.el + delay + sp.base/2, pn: sp.pn, acked: adm, lost: drop, sent: x.el})
			sp.pn += 2
			sp.inflight += want
			sp.sent += want
			x.sent += want
			tick += want
		}
		// The common bottleneck takes what arrives now.
		var in float64
		for _, sp := range x.paths {
			for _, a := range sp.transit {
				if a.at > x.el {
					break
				}
				in += a.acked
			}
		}
		var over float64
		var qd time.Duration
		if x.capacity > 0 {
			if x.bucket > 0 {
				x.tokens = min(x.tokens+x.capacity*dt, x.bucket*x.capacity)
				pass := min(in, x.tokens)
				x.tokens -= pass
				over = in - pass
			} else {
				x.q += in
				x.q -= min(x.q, x.capacity*dt)
				if limit := x.buffer * x.capacity; x.q > limit {
					over = min(x.q-limit, in)
					x.q -= over
				}
				qd = time.Duration(x.q / x.capacity * float64(time.Second))
			}
			x.qsum += qd
		}
		x.ticks++
		for i, p := range x.s.paths {
			sp := x.paths[i]
			for len(sp.transit) > 0 && sp.transit[0].at <= x.el {
				a := sp.transit[0]
				sp.transit = sp.transit[1:]
				if in > 0 {
					d := over * a.acked / in
					a.acked -= d
					a.lost += d
				}
				a.at = x.el + qd + sp.base/2
				x.lost += a.lost
				x.dlv += a.acked
				sp.pend = append(sp.pend, a)
			}
			for len(sp.pend) > 0 && sp.pend[0].at <= x.el {
				a := sp.pend[0]
				sp.pend = sp.pend[1:]
				var acked []congestion.AckedPacketInfo
				var lost []congestion.LostPacketInfo
				if a.acked >= 1 {
					acked = append(acked, congestion.AckedPacketInfo{PacketNumber: congestion.PacketNumber(a.pn), BytesAcked: congestion.ByteCount(a.acked)})
				}
				if a.lost >= 1 {
					lost = append(lost, congestion.LostPacketInfo{PacketNumber: congestion.PacketNumber(a.pn + 1), BytesLost: congestion.ByteCount(a.lost)})
				}
				p.meter.OnCongestionEventEx(congestion.ByteCount(sp.inflight), t, acked, lost)
				sp.inflight -= a.acked + a.lost
				rtt := x.el - a.sent
				sp.rtt.latest = rtt
				if sp.rtt.min == 0 || rtt < sp.rtt.min {
					sp.rtt.min = rtt
				}
				p.latestRTT, p.srtt = rtt, rtt
			}
		}
		x.winSum += tick
		if x.el%tickInterval != 0 {
			continue
		}
		x.win = append(x.win, x.winSum)
		x.winSum = 0
		if len(x.win) > 100 {
			x.win = x.win[1:]
		}
		var w float64
		for _, v := range x.win {
			w += v
		}
		x.peak = max(x.peak, w/(float64(len(x.win))*tickInterval.Seconds()))
		x.s.couplingTickLocked(x.now)
		if x.trace != nil {
			x.trace(x)
		}
	}
}

// reset starts the totals anew.
func (x *rateSim) reset() {
	x.sent, x.lost, x.dlv, x.peak, x.qsum, x.ticks = 0, 0, 0, 0, 0, 0
	x.win = x.win[:0]
	for _, sp := range x.paths {
		sp.sent, sp.qsum, sp.qn = 0, 0, 0
	}
}

func (x *rateSim) lossShare() float64 { return x.lost / max(x.sent, 1) }
func (x *rateSim) meanQueue() time.Duration {
	return x.qsum / time.Duration(max(x.ticks, 1))
}

// goodput is the delivery rate since reset (bytes/s) over d.
func (x *rateSim) goodput(d time.Duration) float64 { return x.dlv / d.Seconds() }

func (x *rateSim) bw() float64 { return x.s.cp.bw }

// within fails the test unless lo <= v/ref <= hi.
func within(t *testing.T, what string, v, ref, lo, hi float64) {
	t.Helper()
	t.Logf("%s = %.1f Mbit/s (%.3f of %.1f)", what, v*8/1e6, v/ref, ref*8/1e6)
	if r := v / ref; r < lo || r > hi || math.IsNaN(r) {
		t.Errorf("%s = %.1f Mbit/s: %.3f of %.1f, want %.2f..%.2f", what, v*8/1e6, r, ref*8/1e6, lo, hi)
	}
}

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

// One path behind a queue: the rate is the queue's, and the queue is short.
func TestRateSingleQueue(t *testing.T) {
	x := newRateSim(mbit(100), 0.1, ms(40))
	x.run(5 * time.Second)
	within(t, "B", x.bw(), mbit(100), 0.93, 1.02)
	x.reset()
	x.run(10 * time.Second)
	within(t, "goodput", x.goodput(10*time.Second), mbit(100), 0.9, 1.0)
	if q := x.meanQueue(); q > ms(15) {
		t.Errorf("mean queue %v", q)
	}
	if l := x.lossShare(); l > 0.002 {
		t.Errorf("loss %.2f%%", 100*l)
	}
}

// The scenario the controller was written for: five relays of 200 Mbit/s,
// round trips of 150 to 250 ms, and a policer of 500 Mbit/s in front of the
// receiver. Per-path controllers each found 200 and sent twice what came
// through; the session finds 500, loses little and does not burst.
func TestRatePolicedDownlink(t *testing.T) {
	x := newRateSim(mbit(500), 0, ms(150), ms(170), ms(200), ms(220), ms(250)).policer(0.05)
	x.own(0.1, mbit(200), mbit(200), mbit(200), mbit(200), mbit(200))
	x.run(3 * time.Second)
	// the start-up is over: at most one round trip at twice the capacity
	if x.peak > 1.5*mbit(500) {
		t.Errorf("start-up: %.0f Mbit/s over a second", x.peak*8/1e6)
	}
	x.run(7 * time.Second)
	x.reset()
	x.run(50 * time.Second)
	// (B is the rate at which the policer lets the session's packets pass:
	// its rate, not a margin below it.)
	within(t, "B", x.bw(), mbit(500), 0.93, 1.02)
	within(t, "goodput", x.goodput(50*time.Second), mbit(500), 0.92, 1.0)
	if l := x.lossShare(); l > 0.01 {
		t.Errorf("loss %.2f%%", 100*l)
	}
	if x.peak > 1.25*mbit(500) { // never more than a probe's rate
		t.Errorf("peak %.0f Mbit/s over a second", x.peak*8/1e6)
	}
}

// A policer that lets a second's worth pass in a burst: probes get through
// for a while, and the loss comes later. It must stay rare all the same.
func TestRatePolicerLargeBurst(t *testing.T) {
	x := newRateSim(mbit(100), 0, ms(30), ms(50), ms(80)).policer(1)
	x.run(20 * time.Second)
	x.reset()
	x.run(100 * time.Second)
	within(t, "goodput", x.goodput(100*time.Second), mbit(100), 0.9, 1.01)
	if l := x.lossShare(); l > 0.035 {
		t.Errorf("loss %.2f%%", 100*l)
	}
}

// Three paths behind one queue share its rate, whatever the buffer.
func TestRateSharedQueue(t *testing.T) {
	for _, buf := range []float64{0.01, 0.05, 0.3} {
		x := newRateSim(mbit(100), buf, ms(20), ms(50), ms(80))
		x.run(8 * time.Second)
		x.reset()
		x.run(20 * time.Second)
		within(t, "goodput", x.goodput(20*time.Second), mbit(100), 0.9, 1.0)
		if q := x.meanQueue(); q > ms(20) {
			t.Errorf("buffer %v s: mean queue %v", buf, q)
		}
		if l := x.lossShare(); l > 0.01 {
			t.Errorf("buffer %v s: loss %.2f%%", buf, 100*l)
		}
	}
}

// Paths with bottlenecks of their own and nothing in common: each carries
// what its own takes.
func TestRateIndependentPaths(t *testing.T) {
	x := newRateSim(0, 0, ms(20), ms(40), ms(80)).own(0.1, mbit(50), mbit(50), mbit(50))
	x.run(8 * time.Second)
	x.reset()
	x.run(20 * time.Second)
	within(t, "goodput", x.goodput(20*time.Second), mbit(150), 0.9, 1.0)

	x = newRateSim(0, 0, ms(20), ms(40), ms(80)).own(0.1, mbit(10), mbit(50), mbit(100))
	x.run(20 * time.Second)
	x.reset()
	x.run(20 * time.Second)
	within(t, "goodput (10+50+100)", x.goodput(20*time.Second), mbit(160), 0.85, 1.0)
	within(t, "slow path", x.paths[0].sent/20, mbit(10), 0.8, 1.15)
	if l := x.lossShare(); l > 0.01 {
		t.Errorf("loss %.2f%%", 100*l)
	}
}

// Random loss is not congestion: the rate stays what the queue takes.
func TestRateRandomLoss(t *testing.T) {
	for _, loss := range []float64{0.01, 0.05, 0.15} {
		x := newRateSim(mbit(100), 0.05, ms(20), ms(40), ms(60), ms(90))
		for _, sp := range x.paths {
			sp.loss = loss
		}
		x.run(15 * time.Second)
		x.reset()
		x.run(30 * time.Second)
		within(t, "goodput", x.goodput(30*time.Second), mbit(100), 0.85, 1.0)
	}
	// loss on one path only does not hold the others back
	x := newRateSim(mbit(100), 0.05, ms(20), ms(40), ms(60))
	x.paths[1].loss = 0.1
	x.run(15 * time.Second)
	x.reset()
	x.run(30 * time.Second)
	within(t, "goodput (one lossy path)", x.goodput(30*time.Second), mbit(100), 0.85, 1.0)
}

// The capacity drops and comes back: the rate follows within seconds.
func TestRateCapacityChange(t *testing.T) {
	for _, policed := range []bool{false, true} {
		x := newRateSim(mbit(100), 0.1, ms(20), ms(50), ms(80))
		if policed {
			x.policer(0.05)
		}
		x.run(10 * time.Second)
		x.capacity = mbit(40)
		x.run(3 * time.Second)
		x.reset()
		x.run(7 * time.Second)
		within(t, "after the drop: B", x.bw(), mbit(40), 0.85, 1.05)
		if l := x.lossShare(); l > 0.02 {
			t.Errorf("policed=%v: loss %.2f%% after the drop", policed, 100*l)
		}
		x.capacity = mbit(100)
		x.run(40 * time.Second)
		within(t, "after the rise: B", x.bw(), mbit(100), 0.85, 1.02)
	}
}

// An application that sends little leaves the rate alone, and gets all of
// it when it sends more, without a burst.
func TestRateAppLimited(t *testing.T) {
	x := newRateSim(mbit(100), 0.05, ms(20), ms(50), ms(80))
	x.run(8 * time.Second)
	b := x.bw()
	x.demand = mbit(10)
	x.run(30 * time.Second)
	within(t, "B while idle", x.bw(), b, 0.95, 1.05)
	x.demand = 0
	x.reset()
	x.run(10 * time.Second)
	within(t, "goodput afterwards", x.goodput(10*time.Second), mbit(100), 0.9, 1.0)
	if x.peak > 1.3*mbit(100) {
		t.Errorf("peak %.0f Mbit/s over a second", x.peak*8/1e6)
	}
	// from the start: little data must not end the start-up low
	x = newRateSim(mbit(100), 0.05, ms(20), ms(50), ms(80))
	x.demand = mbit(5)
	x.run(10 * time.Second)
	x.demand = 0
	x.run(5 * time.Second)
	x.reset()
	x.run(10 * time.Second)
	within(t, "goodput after a slow start", x.goodput(10*time.Second), mbit(100), 0.9, 1.0)
}

// A configured limit is a ceiling for the session, not per path.
func TestRateLimit(t *testing.T) {
	x := newRateSim(mbit(100), 0.05, ms(20), ms(50), ms(80))
	x.s.opt.MaxSendRate = uint64(mbit(60))
	x.run(8 * time.Second)
	x.reset()
	x.run(10 * time.Second)
	within(t, "goodput", x.goodput(10*time.Second), mbit(60), 0.93, 1.0)
	if x.peak > 1.02*mbit(60) {
		t.Errorf("peak %.1f Mbit/s over a second", x.peak*8/1e6)
	}
	if q := x.meanQueue(); q > ms(1) {
		t.Errorf("mean queue %v", q)
	}
}

// The start-up doubles every round trip or so: 100 Mbit/s at 50 ms within
// a second and a half.
func TestRateStartup(t *testing.T) {
	x := newRateSim(mbit(100), 0.05, ms(50), ms(50))
	x.run(1500 * time.Millisecond)
	x.reset()
	x.run(500 * time.Millisecond)
	within(t, "goodput", x.goodput(500*time.Millisecond), mbit(100), 0.8, 1.05)
}

// A queue in the other direction delays the acknowledgements of every path.
// That is not this direction's congestion.
func TestRateReverseQueue(t *testing.T) {
	x := newRateSim(mbit(100), 0.05, ms(20), ms(50), ms(80))
	x.run(8 * time.Second)
	x.extra = ms(60)
	x.run(10 * time.Second)
	x.reset()
	x.run(20 * time.Second)
	within(t, "goodput", x.goodput(20*time.Second), mbit(100), 0.85, 1.0)
}

// A path that dies leaves its share to the others.
func TestRatePathLoss(t *testing.T) {
	x := newRateSim(mbit(100), 0.05, ms(20), ms(50), ms(80))
	x.run(8 * time.Second)
	x.s.paths[0].state = stateDead
	x.run(3 * time.Second)
	x.reset()
	x.run(10 * time.Second)
	within(t, "goodput", x.goodput(10*time.Second), mbit(100), 0.9, 1.0)
}

// A line whose capacity flaps between 100 and 40 Mbit/s every four seconds.
func TestRateFlap(t *testing.T) {
	x := newRateSim(mbit(100), 0.1, ms(20), ms(50), ms(80))
	x.run(8 * time.Second)
	x.reset()
	for i := 0; i < 10; i++ {
		x.capacity = mbit(40)
		x.run(4 * time.Second)
		x.capacity = mbit(100)
		x.run(4 * time.Second)
	}
	// (Each rise is found by probes, over a second or so of the four.)
	within(t, "goodput", x.goodput(80*time.Second), mbit(70), 0.8, 1.0)
	if l := x.lossShare(); l > 0.04 { // each drop overflows the buffer for a few rounds
		t.Errorf("loss %.2f%%", 100*l)
	}
	// (After a drop the paths' windows hold what queues to a round trip's
	// worth until B has followed: after two lossy rounds, or when the old
	// rate has aged out of the last ten. Half the time is spent at 40.)
	if q := x.meanQueue(); q > ms(50) {
		t.Errorf("mean queue %v", q)
	}
}

// One relay is slower than its share of the line: it carries what it can,
// and the others the rest.
func TestRateSlowRelay(t *testing.T) {
	x := newRateSim(mbit(100), 0.05, ms(20), ms(50), ms(80)).own(0.1, mbit(20))
	x.run(15 * time.Second)
	x.reset()
	x.run(30 * time.Second)
	within(t, "goodput", x.goodput(30*time.Second), mbit(100), 0.9, 1.0)
	within(t, "slow relay", x.paths[0].sent/30, mbit(20), 0.75, 1.1)
	// It has the shortest round trip, so small transfers go there: its
	// queue is their latency. (A path that is held to its own rate sends
	// what its relay takes, so what queues there in a probe stays until
	// the next drain: on average well under the round trip, 20 ms here,
	// that the path's window allows.)
	if q := x.paths[0].qsum / time.Duration(max(x.paths[0].qn, 1)); q > ms(15) {
		t.Errorf("the slow relay's queue: %v on average", q)
	}
	if l := x.lossShare(); l > 0.01 {
		t.Errorf("loss %.2f%%", 100*l)
	}
}

// Independent paths, one of them lossy.
func TestRateIndependentLossy(t *testing.T) {
	x := newRateSim(0, 0, ms(20), ms(50), ms(80)).own(0.1, mbit(50), mbit(30), mbit(20))
	x.paths[1].loss = 0.02
	x.run(20 * time.Second)
	x.reset()
	x.run(30 * time.Second)
	within(t, "goodput", x.goodput(30*time.Second), mbit(100), 0.85, 1.0)
}

// Relays with policers of their own (not the line): each path is held to
// what its relay lets pass.
func TestRatePolicedRelays(t *testing.T) {
	x := newRateSim(0, 0, ms(20), ms(50), ms(80)).own(0, mbit(30), mbit(60), mbit(90))
	x.run(30 * time.Second)
	x.reset()
	x.run(60 * time.Second)
	within(t, "goodput", x.goodput(60*time.Second), mbit(180), 0.8, 1.0)
	if l := x.lossShare(); l > 0.03 {
		t.Errorf("loss %.2f%%", 100*l)
	}
}

// The other direction of a download: little to send (acknowledgements), and
// every path's round trips are long because of the download's queue. That
// queue is not this direction's. It must not end the start-up or cut the
// rate, or the acknowledgements and pings are throttled to what happened to
// be sent so far.
func TestRateThinDirection(t *testing.T) {
	x := newRateSim(mbit(100), 0.05, ms(16), ms(40), ms(70), ms(120))
	x.demand = mbit(1)
	for i, sp := range x.paths { // and the paths lose
		sp.loss = []float64{0.2, 0.1, 0.05, 0.01}[i]
	}
	x.run(2 * time.Second)
	for i := 0; i < 40; i++ { // the foreign queue comes and goes
		x.extra = ms(10 + 10*(i%5))
		x.run(250 * time.Millisecond)
	}
	if x.s.cp.phase != cpStartup {
		t.Errorf("start-up ended at B = %.2f Mbit/s with 1 Mbit/s to send", x.bw()*8/1e6)
	}
	var total float64
	for _, p := range x.s.paths {
		total += float64(p.meter.Rate())
	}
	if total < mbit(2) {
		t.Errorf("the paths may send %.2f Mbit/s together", total*8/1e6)
	}
	// and when there is data, it gets the line
	x.demand, x.extra = 0, 0
	for _, sp := range x.paths {
		sp.loss = 0
	}
	x.run(2 * time.Second)
	x.reset()
	x.run(5 * time.Second)
	within(t, "goodput", x.goodput(5*time.Second), mbit(100), 0.9, 1.0)
}

// Two relays whose rates flap on their own (50/10 Mbit/s every 3 s, 20/60
// every 4 s): each path's limit follows its relay down and up again. (With
// every path held to a limit, the session's rate is the sum of the limits,
// so a limit only comes back up if the try for more has rate of its own to
// try with.)
func TestRateFlappingRelays(t *testing.T) {
	x := newRateSim(0, 0, ms(20), ms(60)).own(0.1, mbit(50), mbit(20))
	x.run(6 * time.Second)
	x.reset()
	var capacity float64
	for i := 0; i < 120; i++ { // 60 s in half seconds
		a, b := mbit(50), mbit(20)
		if i/6%2 == 1 {
			a = mbit(10)
		}
		if i/8%2 == 1 {
			b = mbit(60)
		}
		x.paths[0].own, x.paths[1].own = a, b
		capacity += (a + b) / 120
		x.run(500 * time.Millisecond)
	}
	// (A path's rate follows its relay down after two lossy rounds, or
	// when the higher rates have aged out of its last ten rounds, and up
	// by probes that ask for twice as much more each time.)
	within(t, "goodput", x.goodput(60*time.Second), capacity, 0.5, 1.0)
	if l := x.lossShare(); l > 0.1 { // (a relay drops to a fifth every few seconds)
		t.Errorf("loss %.2f%%", 100*l)
	}
}
