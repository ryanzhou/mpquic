package bbr

import (
	"math/rand"
	"sort"
	"testing"
	"time"

	"github.com/apernet/quic-go/congestion"
	"github.com/apernet/quic-go/monotime"
)

// A small deterministic network simulator: one bulk sender (always has
// data), a FIFO bottleneck with a time-varying rate and a drop-tail buffer,
// fixed propagation delay, optional random loss and ACK jitter.

type fakeRTT struct{ min time.Duration }

func (f fakeRTT) MinRTT() time.Duration        { return f.min }
func (f fakeRTT) LatestRTT() time.Duration     { return f.min }
func (f fakeRTT) SmoothedRTT() time.Duration   { return f.min }
func (f fakeRTT) MeanDeviation() time.Duration { return 0 }
func (f fakeRTT) MaxAckDelay() time.Duration   { return 0 }
func (f fakeRTT) PTO(bool) time.Duration       { return 3 * f.min }
func (f fakeRTT) UpdateRTT(_, _ time.Duration) {}
func (f fakeRTT) SetMaxAckDelay(time.Duration) {}
func (f fakeRTT) SetInitialRTT(time.Duration)  {}

type fakeClock struct{ t monotime.Time }

func (c *fakeClock) Now() monotime.Time { return c.t }

type simConfig struct {
	profile  Profile
	duration time.Duration
	rtt      time.Duration                // propagation round trip
	rate     func(time.Duration) float64  // bottleneck rate, bytes/s
	buffer   time.Duration                // drop-tail buffer at the initial rate
	loss     float64                      // random loss before the bottleneck
	jitter   time.Duration                // ACK aggregation: ACKs are released in bursts every jitter (Wi-Fi/cellular slots)
	gate     func(enter bool) bool        // ProbeRTTGate
	onStep   func(time.Duration, *Sender) // called every 10 ms

	flows     int                         // bulk BBR flows sharing the bottleneck (default 1)
	flowStart []time.Duration             // start time of each flow
	cross     func(time.Duration) float64 // unresponsive cross traffic, bytes/s
	reno      []bool                      // flows that use Reno instead of BBR
	outage    func(time.Duration) bool    // link down (everything sent is dropped)
	setup     func(i int, s *Sender)      // called for each flow's sender before the start
}

const simBucket = 100 * time.Millisecond

type simResult struct {
	delivered []float64       // bytes delivered per 100 ms bucket (flow 0)
	queue     []time.Duration // max queueing delay of flow 0's RTT samples per bucket
	s         *Sender         // flow 0

	senders       []*Sender
	flowDelivered [][]float64
}

// flowGoodput returns flow i's delivered rate (bytes/s) in [from, to).
func (r simResult) flowGoodput(i int, from, to time.Duration) float64 {
	return simResult{delivered: r.flowDelivered[i]}.goodput(from, to)
}

// goodput returns the delivered rate (bytes/s) in [from, to).
func (r simResult) goodput(from, to time.Duration) float64 {
	var sum float64
	for i := int(from / simBucket); i < int(to/simBucket) && i < len(r.delivered); i++ {
		sum += r.delivered[i]
	}
	return sum / (to - from).Seconds()
}

// queued returns how long within [from, to) the queueing delay exceeded d.
func (r simResult) queued(from, to, d time.Duration) time.Duration {
	var n time.Duration
	for i := int(from / simBucket); i < int(to/simBucket) && i < len(r.queue); i++ {
		if r.queue[i] > d {
			n += simBucket
		}
	}
	return n
}

// medianQueue returns the median per-bucket queueing delay in [from, to).
func (r simResult) medianQueue(from, to time.Duration) time.Duration {
	var q []time.Duration
	for i := int(from / simBucket); i < int(to/simBucket) && i < len(r.queue); i++ {
		q = append(q, r.queue[i])
	}
	sort.Slice(q, func(i, j int) bool { return q[i] < q[j] })
	return q[len(q)/2]
}

func runSim(cfg simConfig) simResult {
	const (
		dt   = 100 * time.Microsecond
		size = congestion.ByteCount(1200)
	)
	rng := rand.New(rand.NewSource(1))
	start := monotime.Now()
	clk := &fakeClock{t: start}

	type pkt struct {
		flow   int // -1: cross traffic
		pn     congestion.PacketNumber
		sentAt time.Duration
		ackAt  time.Duration
	}
	type flow struct {
		s        *Sender
		startAt  time.Duration
		acks     []pkt // in delivery order
		dropped  []pkt // not delivered, not yet declared lost
		inflight congestion.ByteCount
		pn       congestion.PacketNumber

		// probe timeout (QUIC PTO): when no ACK arrives, a probe packet is
		// sent regardless of the window; its ACK lets the packet threshold
		// declare the earlier ones lost (e.g. after an outage)
		lastAckAt, lastProbeAt time.Duration
		srtt, rttvar           time.Duration // RFC 6298 / 9002
		ptoCount               int

		// Reno (loss-based competitor)
		reno           bool
		cwnd, ssthresh float64
		recoveryEnd    congestion.PacketNumber
	}
	nflows := max(cfg.flows, 1)
	flows := make([]*flow, nflows)
	nb := int(cfg.duration/simBucket) + 1
	res := simResult{queue: make([]time.Duration, nb)}
	for i := range flows {
		s := NewBbrSender(clk, size, cfg.profile)
		s.SetRTTStatsProvider(fakeRTT{cfg.rtt})
		if i == 0 {
			s.ProbeRTTGate = cfg.gate
		}
		if cfg.setup != nil {
			cfg.setup(i, s)
		}
		f := &flow{s: s, pn: 1, cwnd: 10 * float64(size), ssthresh: 1e12}
		f.reno = i < len(cfg.reno) && cfg.reno[i]
		if i < len(cfg.flowStart) {
			f.startAt = cfg.flowStart[i]
		}
		flows[i] = f
		res.senders = append(res.senders, s)
		res.flowDelivered = append(res.flowDelivered, make([]float64, nb))
	}
	res.s = flows[0].s
	res.delivered = res.flowDelivered[0]

	var (
		queue       []pkt
		qBytes      congestion.ByteCount
		credit      float64
		crossCredit float64
	)
	bufBytes := congestion.ByteCount(cfg.rate(0) * cfg.buffer.Seconds())
	var now time.Duration
	enqueue := func(p pkt) bool {
		if qBytes+size > bufBytes || cfg.outage != nil && cfg.outage(now) {
			return false
		}
		queue = append(queue, p)
		qBytes += size
		return true
	}

	for now = 0; now < cfg.duration; now += dt {
		clk.t = start.Add(now)
		bucket := int(now / simBucket)

		// unresponsive cross traffic
		if cfg.cross != nil {
			crossCredit += cfg.cross(now) * dt.Seconds()
			for crossCredit >= float64(size) {
				crossCredit -= float64(size)
				enqueue(pkt{flow: -1})
			}
		}

		// bottleneck
		credit += cfg.rate(now) * dt.Seconds()
		for len(queue) > 0 && credit >= float64(size) {
			p := queue[0]
			queue = queue[1:]
			qBytes -= size
			credit -= float64(size)
			if p.flow < 0 {
				continue
			}
			p.ackAt = now + cfg.rtt
			if cfg.jitter > 0 {
				// release at the next slot boundary: extra delay uniform in [0, jitter)
				p.ackAt = (p.ackAt + cfg.jitter - 1) / cfg.jitter * cfg.jitter
			}
			f := flows[p.flow]
			f.acks = append(f.acks, p)
			res.flowDelivered[p.flow][bucket] += float64(size)
		}
		if len(queue) == 0 {
			credit = min(credit, float64(size))
		}

		for i, f := range flows {
			if now < f.startAt {
				continue
			}
			// ACKs
			var acked []congestion.AckedPacketInfo
			var lost []congestion.LostPacketInfo
			for len(f.acks) > 0 && f.acks[0].ackAt <= now {
				p := f.acks[0]
				f.acks = f.acks[1:]
				acked = append(acked, congestion.AckedPacketInfo{PacketNumber: p.pn, BytesAcked: size, ReceivedTime: clk.t})
				if i == 0 {
					res.queue[bucket] = max(res.queue[bucket], now-p.sentAt-cfg.rtt)
				}
				if r := now - p.sentAt; f.srtt == 0 {
					f.srtt, f.rttvar = r, r/2
				} else {
					f.rttvar = (3*f.rttvar + (f.srtt - r).Abs()) / 4
					f.srtt = (7*f.srtt + r) / 8
				}
				f.lastAckAt, f.ptoCount = now, 0
			}
			if len(acked) > 0 {
				largest := acked[len(acked)-1].PacketNumber
				for len(f.dropped) > 0 && f.dropped[0].pn+3 <= largest {
					lost = append(lost, congestion.LostPacketInfo{PacketNumber: f.dropped[0].pn, BytesLost: size})
					f.dropped = f.dropped[1:]
				}
			}
			if len(acked)+len(lost) > 0 {
				prior := f.inflight
				f.inflight -= size * congestion.ByteCount(len(acked)+len(lost))
				if !f.reno {
					f.s.OnCongestionEventEx(prior, clk.t, acked, lost)
				}
			}
			if f.reno {
				for range acked {
					if f.cwnd < f.ssthresh {
						f.cwnd += float64(size)
					} else {
						f.cwnd += float64(size) * float64(size) / f.cwnd
					}
				}
				if len(lost) > 0 && lost[0].PacketNumber > f.recoveryEnd {
					f.cwnd = max(f.cwnd/2, 2*float64(size))
					f.ssthresh = f.cwnd
					f.recoveryEnd = f.pn
				}
				for f.inflight+size <= congestion.ByteCount(f.cwnd) {
					f.inflight += size
					if rng.Float64() < cfg.loss || !enqueue(pkt{flow: i, pn: f.pn, sentAt: now}) {
						f.dropped = append(f.dropped, pkt{pn: f.pn, sentAt: now})
					}
					f.pn++
				}
				continue
			}

			// sender
			// PTO = srtt + 4 rttvar + max_ack_delay (25 ms), backed off;
			// before the first RTT sample from quic-go's initial RTT (100 ms)
			srtt, rttvar := f.srtt, f.rttvar
			if srtt == 0 {
				srtt, rttvar = 100*time.Millisecond, 50*time.Millisecond
			}
			pto := (srtt + max(4*rttvar, time.Millisecond) + 25*time.Millisecond) << min(f.ptoCount, 6)
			if f.inflight > 0 && now-max(f.lastAckAt, f.lastProbeAt) > pto {
				f.s.OnPacketSent(clk.t, f.inflight, f.pn, size, true)
				f.inflight += size
				if !enqueue(pkt{flow: i, pn: f.pn, sentAt: now}) {
					f.dropped = append(f.dropped, pkt{pn: f.pn, sentAt: now})
				}
				f.pn++
				f.lastProbeAt = now
				f.ptoCount++
			}
			for f.s.CanSend(f.inflight) && f.s.HasPacingBudget(clk.t) {
				f.s.OnPacketSent(clk.t, f.inflight, f.pn, size, true)
				f.inflight += size
				if rng.Float64() < cfg.loss || !enqueue(pkt{flow: i, pn: f.pn, sentAt: now}) {
					f.dropped = append(f.dropped, pkt{pn: f.pn, sentAt: now})
				}
				f.pn++
			}
		}
		if cfg.onStep != nil && now%(10*time.Millisecond) == 0 {
			cfg.onStep(now, flows[0].s)
		}
	}
	return res
}

func mbit(m float64) float64 { return m * 1e6 / 8 }

// Single bulk flow on a deep buffer: BBR must keep the queue small (it used
// to hold a whole BDP, doubling the RTT for everything sharing the path).
func TestSimSteadyStateQueue(t *testing.T) {
	for _, p := range []Profile{ProfileStandard, ProfileConservative} {
		r := runSim(simConfig{profile: p, duration: 15 * time.Second, rtt: 50 * time.Millisecond,
			rate: func(time.Duration) float64 { return mbit(20) }, buffer: 500 * time.Millisecond})
		util := r.goodput(3*time.Second, 15*time.Second) / mbit(20)
		mq := r.medianQueue(3*time.Second, 15*time.Second)
		t.Logf("%s: utilization %.1f%%, median queue %v, stale resets %d", p, util*100, mq, r.s.StaleResets)
		if util < 0.9 {
			t.Errorf("%s: utilization %.1f%% < 90%%", p, util*100)
		}
		if mq > 20*time.Millisecond {
			t.Errorf("%s: median queueing delay %v > 20ms", p, mq)
		}
		if r.s.StaleResets != 0 {
			t.Errorf("%s: %d spurious stale-estimate resets", p, r.s.StaleResets)
		}
	}
}

// The bottleneck rate drops 8x (40 -> 5 Mbit/s) for 6 s on a 2 s buffer.
// BBRv1 kept pacing at the old rate for 10 (queue-inflated) round trips:
// ~0.8 s of queue for the whole low phase.
func TestSimRateDrop(t *testing.T) {
	rate := func(now time.Duration) float64 {
		if now >= 6*time.Second && now < 12*time.Second {
			return mbit(5)
		}
		return mbit(40)
	}
	r := runSim(simConfig{profile: ProfileStandard, duration: 20 * time.Second, rtt: 50 * time.Millisecond,
		rate: rate, buffer: 2 * time.Second})
	bloat := r.queued(6*time.Second, 12*time.Second, 50*time.Millisecond)
	low := r.goodput(7*time.Second, 12*time.Second) / mbit(5)
	high := r.goodput(17*time.Second, 20*time.Second) / mbit(40)
	t.Logf("queue > 50ms for %v of the 6 s low phase; utilization low %.0f%% recovered %.0f%%; stale resets %d",
		bloat, low*100, high*100, r.s.StaleResets)
	if bloat > 1500*time.Millisecond {
		t.Errorf("queue stayed above 50ms for %v after the rate drop (want <= 1.5s)", bloat)
	}
	if low < 0.85 {
		t.Errorf("utilization after the drop %.0f%% < 85%%", low*100)
	}
	if high < 0.7 {
		t.Errorf("utilization after the rate recovered %.0f%% < 70%%", high*100)
	}
}

// A moderate drop (40 -> 28 Mbit/s) does not trip the stale-estimate reset
// (delivery stays above 3/4 of the estimate); the queue-aware filter time
// base must still expire the old estimate quickly.
func TestSimModerateRateDrop(t *testing.T) {
	rate := func(now time.Duration) float64 {
		if now >= 6*time.Second {
			return mbit(28)
		}
		return mbit(40)
	}
	r := runSim(simConfig{profile: ProfileStandard, duration: 14 * time.Second, rtt: 50 * time.Millisecond,
		rate: rate, buffer: 2 * time.Second})
	bloat := r.queued(6*time.Second, 14*time.Second, 25*time.Millisecond)
	util := r.goodput(8*time.Second, 14*time.Second) / mbit(28)
	t.Logf("queue > 25ms for %v after the drop; utilization %.0f%%; stale resets %d", bloat, util*100, r.s.StaleResets)
	if bloat > 1500*time.Millisecond {
		t.Errorf("queue stayed above 25ms for %v after the rate drop (want <= 1.5s)", bloat)
	}
	if util < 0.9 {
		t.Errorf("utilization %.0f%% < 90%%", util*100)
	}
}

// ACK aggregation (Wi-Fi, cellular: ACKs arrive in bursts, which looks like
// jitter of up to the slot length) and random loss must neither trip the
// stale-estimate reset nor cost throughput. The 40 ms / 5 ms case also guards
// the standard profile against overestimate avoidance, which collapses there.
func TestSimNoFalseStaleResets(t *testing.T) {
	cases := []struct {
		name   string
		loss   float64
		jitter time.Duration
		rtt    time.Duration
	}{
		{"aggr20ms-rtt30ms", 0, 20 * time.Millisecond, 30 * time.Millisecond},
		{"aggr40ms-rtt5ms", 0, 40 * time.Millisecond, 5 * time.Millisecond},
		{"aggr20ms-rtt50ms", 0, 20 * time.Millisecond, 50 * time.Millisecond},
		{"loss2%-rtt150ms", 0.02, 0, 150 * time.Millisecond},
		{"loss5%-aggr10ms-rtt100ms", 0.05, 10 * time.Millisecond, 100 * time.Millisecond},
	}
	for _, c := range cases {
		r := runSim(simConfig{profile: ProfileStandard, duration: 15 * time.Second, rtt: c.rtt,
			rate: func(time.Duration) float64 { return mbit(50) }, buffer: 200 * time.Millisecond,
			loss: c.loss, jitter: c.jitter})
		util := r.goodput(4*time.Second, 15*time.Second) / mbit(50)
		t.Logf("%s: utilization %.0f%%, stale resets %d", c.name, util*100, r.s.StaleResets)
		if r.s.StaleResets != 0 {
			t.Errorf("%s: %d spurious stale-estimate resets", c.name, r.s.StaleResets)
		}
		if util < 0.85 {
			t.Errorf("%s: utilization %.0f%% < 85%%", c.name, util*100)
		}
	}
}

// The ProbeRTT gate is held only while in PROBE_RTT, and it is used.
func TestSimProbeGateHeldOnlyWhileProbing(t *testing.T) {
	held, probes := false, 0
	gate := func(enter bool) bool {
		if enter && !held {
			probes++
		}
		held = enter
		return true
	}
	runSim(simConfig{profile: ProfileStandard, duration: 40 * time.Second, rtt: 50 * time.Millisecond,
		rate: func(time.Duration) float64 { return mbit(10) }, buffer: 200 * time.Millisecond, gate: gate,
		onStep: func(now time.Duration, s *Sender) {
			if held && s.mode != bbrModeProbeRtt {
				t.Fatalf("%v: gate held in mode %s", now, s.ModeName())
			}
		}})
	if probes < 2 {
		t.Errorf("PROBE_RTT entered %d times in 40 s, want >= 2", probes)
	}
}

// Regression: on a mostly idle (interactive) path every packet is sent with
// nothing in flight, so min_rtt expires on an ACK that exits quiescence and
// PROBE_RTT is skipped. The gate taken for it used to be kept forever, which
// stopped every other path of the session from ever probing again.
func TestProbeGateReleasedWhenProbeSkipped(t *testing.T) {
	clk := &fakeClock{t: monotime.Now()}
	b := NewBbrSender(clk, 1200, ProfileStandard)
	b.SetRTTStatsProvider(fakeRTT{50 * time.Millisecond})
	held, taken := false, 0
	b.ProbeRTTGate = func(enter bool) bool {
		if enter && !held {
			taken++
		}
		held = enter
		return true
	}
	pn := congestion.PacketNumber(1)
	for i := 0; i < 400; i++ { // 20 s, one packet per RTT
		b.OnPacketSent(clk.t, 0, pn, 1200, true)
		clk.t = clk.t.Add(50 * time.Millisecond)
		b.OnCongestionEventEx(1200, clk.t, []congestion.AckedPacketInfo{{PacketNumber: pn, BytesAcked: 1200, ReceivedTime: clk.t}}, nil)
		pn++
		if held && b.mode != bbrModeProbeRtt {
			t.Fatalf("after %v: gate held while in %s", time.Duration(i+1)*50*time.Millisecond, b.ModeName())
		}
	}
	if taken == 0 {
		t.Fatalf("min_rtt never expired in 20 s")
	}
}

// Two flows share a deep-buffered bottleneck (e.g. two paths of a session
// behind the same uplink), the second joining later: the stale-estimate reset
// must not make them starve each other or leave the link idle.
func TestSimSharedBottleneck(t *testing.T) {
	r := runSim(simConfig{profile: ProfileStandard, duration: 25 * time.Second, rtt: 50 * time.Millisecond,
		rate: func(time.Duration) float64 { return mbit(20) }, buffer: time.Second,
		flows: 2, flowStart: []time.Duration{0, 3 * time.Second}})
	a := r.flowGoodput(0, 10*time.Second, 25*time.Second) / mbit(20)
	b := r.flowGoodput(1, 10*time.Second, 25*time.Second) / mbit(20)
	mq := r.medianQueue(10*time.Second, 25*time.Second)
	t.Logf("shares %.0f%% + %.0f%%, median queue %v, stale resets %d/%d", a*100, b*100, mq,
		r.senders[0].StaleResets, r.senders[1].StaleResets)
	if a+b < 0.9 {
		t.Errorf("utilization %.0f%% < 90%%", (a+b)*100)
	}
	if min(a, b) < 0.25 {
		t.Errorf("unfair shares %.0f%% / %.0f%%", a*100, b*100)
	}
	if mq > 100*time.Millisecond {
		t.Errorf("median queueing delay %v > 100ms", mq)
	}
}

// Three flows share a bottleneck (three paths of a session behind one
// uplink). Uncoupled, each probes for the whole link and together they keep
// its buffer full. With each estimate capped at a third of the link (what
// mpquic's session rate converges to), they leave STARTUP at their share
// and keep the queue short, at the same utilisation.
func TestSimRateCapSharedBottleneck(t *testing.T) {
	run := func(capped bool) (float64, time.Duration, *Sender) {
		cfg := simConfig{profile: ProfileStandard, duration: 15 * time.Second, rtt: 40 * time.Millisecond,
			rate: func(time.Duration) float64 { return mbit(60) }, buffer: 100 * time.Millisecond, flows: 3}
		if capped {
			cfg.setup = func(_ int, s *Sender) {
				s.RateCap = func() Bandwidth { return Bandwidth(mbit(20)) * BytesPerSecond }
			}
		}
		r := runSim(cfg)
		var u float64
		for i := range 3 {
			u += r.flowGoodput(i, 3*time.Second, 15*time.Second) / mbit(60)
		}
		return u, r.medianQueue(3*time.Second, 15*time.Second), r.senders[0]
	}
	u0, q0, _ := run(false)
	u1, q1, s := run(true)
	t.Logf("uncoupled: utilization %.0f%%, median queue %v; capped: %.0f%%, %v", u0*100, q0, u1*100, q1)
	if u1 < 0.9 {
		t.Errorf("capped utilization %.0f%% < 90%%", u1*100)
	}
	if q1 > 10*time.Millisecond || q1 >= q0 {
		t.Errorf("capped median queue %v (uncoupled %v)", q1, q0)
	}
	if bw := s.BandwidthEstimate(); float64(bw) > mbit(20)+1 {
		t.Errorf("estimate %.1f Mbit/s above the cap", float64(bw)*8/1e6)
	}
}

// A capped flow sends at its cap and does not probe above it on its own
// (the session probes on all capped paths at once, with RateGain): on a link
// with room to spare, three flows capped at 20 Mbit/s deliver 60 Mbit/s with
// gain 1 and a quarter more with gain 1.25.
func TestSimRateGain(t *testing.T) {
	run := func(gain float64) float64 {
		r := runSim(simConfig{profile: ProfileStandard, duration: 12 * time.Second, rtt: 40 * time.Millisecond,
			rate: func(time.Duration) float64 { return mbit(120) }, buffer: 100 * time.Millisecond, flows: 3,
			setup: func(_ int, s *Sender) {
				s.RateCap = func() Bandwidth { return Bandwidth(mbit(20)) * BytesPerSecond }
				s.RateGain = func() float64 { return gain }
			}})
		var u float64
		for i := range 3 {
			u += r.flowGoodput(i, 4*time.Second, 12*time.Second)
		}
		return u * 8 / 1e6
	}
	g1, g125, g0 := run(1), run(1.25), run(0)
	t.Logf("goodput with gain 1: %.1f, 1.25: %.1f, unset: %.1f Mbit/s", g1, g125, g0)
	if g1 < 57 || g1 > 61 {
		t.Errorf("gain 1: %.1f Mbit/s, want the caps' 60 (no probing above a cap)", g1)
	}
	if g125 < 1.18*g1 || g125 > 1.3*g1 {
		t.Errorf("gain 1.25: %.1f Mbit/s, want 1.25 times the %.1f with gain 1", g125, g1)
	}
	if g0 < 57 || g0 > 61 {
		t.Errorf("gain unset: %.1f Mbit/s, want 60", g0)
	}
}

// Unresponsive cross traffic takes 60% of the link for 7 s: the flow must
// yield without bloating the queue, keep using what is left, and take the
// whole link back afterwards.
func TestSimCrossTraffic(t *testing.T) {
	cross := func(now time.Duration) float64 {
		if now >= 5*time.Second && now < 12*time.Second {
			return mbit(12)
		}
		return 0
	}
	r := runSim(simConfig{profile: ProfileStandard, duration: 22 * time.Second, rtt: 50 * time.Millisecond,
		rate: func(time.Duration) float64 { return mbit(20) }, buffer: time.Second, cross: cross})
	during := r.goodput(7*time.Second, 12*time.Second) / mbit(8)
	bloat := r.queued(5*time.Second, 12*time.Second, 100*time.Millisecond)
	after := r.goodput(17*time.Second, 22*time.Second) / mbit(20)
	t.Logf("share of the remaining 8 Mbit/s %.0f%%, queue > 100ms for %v, afterwards %.0f%%, stale resets %d",
		during*100, bloat, after*100, r.s.StaleResets)
	if during < 0.8 {
		t.Errorf("used %.0f%% of the capacity left by the cross traffic", during*100)
	}
	if bloat > 1500*time.Millisecond {
		t.Errorf("queue above 100ms for %v", bloat)
	}
	if after < 0.85 {
		t.Errorf("utilization after the cross traffic stopped %.0f%%", after*100)
	}
}

// BBR against a loss-based (Reno) flow. In deep buffers BBRv1 is window-
// limited by the queue Reno builds and gets a minority share; the stale-
// estimate reset must not turn that into starvation. In a shallow buffer
// BBRv1 used to take ~90%; holding the drain phase makes it share.
func TestSimRenoCompetition(t *testing.T) {
	for _, c := range []struct {
		buffer   time.Duration
		minShare float64
	}{
		{50 * time.Millisecond, 0.2},
		{200 * time.Millisecond, 0.15},
		{time.Second, 0.1},
		{3 * time.Second, 0.1},
	} {
		r := runSim(simConfig{profile: ProfileStandard, duration: 40 * time.Second, rtt: 50 * time.Millisecond,
			rate: func(time.Duration) float64 { return mbit(20) }, buffer: c.buffer,
			flows: 2, reno: []bool{false, true}})
		a := r.flowGoodput(0, 10*time.Second, 40*time.Second) / mbit(20)
		b := r.flowGoodput(1, 10*time.Second, 40*time.Second) / mbit(20)
		t.Logf("buffer %v: bbr %.0f%%, reno %.0f%%, stale resets %d", c.buffer, a*100, b*100, r.s.StaleResets)
		if a < c.minShare {
			t.Errorf("buffer %v: bbr starved (%.0f%% < %.0f%%)", c.buffer, a*100, c.minShare*100)
		}
		if a+b < 0.9 {
			t.Errorf("buffer %v: utilization %.0f%%", c.buffer, (a+b)*100)
		}
	}
}

// A rate that keeps flapping (cellular, Wi-Fi). BBRv1 kept the high estimate
// through the low phases (good throughput, but the buffer full all the time:
// median queue 220-420 ms here). The stale reset must keep queues short, and
// the quick re-probing after it must find the capacity again when it returns.
func TestSimFlappingRate(t *testing.T) {
	for _, c := range []struct {
		hi, lo    float64
		rtt, buf  time.Duration
		period    time.Duration
		loss      float64
		minUtil   float64
		maxMedian time.Duration
	}{
		{40, 5, 50 * time.Millisecond, 2 * time.Second, 3 * time.Second, 0, 0.8, 40 * time.Millisecond},
		{40, 5, 50 * time.Millisecond, 2 * time.Second, 8 * time.Second, 0, 0.85, 30 * time.Millisecond},
		{50, 10, 30 * time.Millisecond, time.Second, 5 * time.Second, 0, 0.85, 30 * time.Millisecond},
		{100, 20, 150 * time.Millisecond, time.Second, 3 * time.Second, 0, 0.6, 250 * time.Millisecond},
		// a relay path that collapses now and then, with random loss
		{40, 2, 20 * time.Millisecond, 100 * time.Millisecond, 3 * time.Second, 0.03, 0.75, 30 * time.Millisecond},
	} {
		rate := func(now time.Duration) float64 {
			if (now/c.period)%2 == 1 {
				return mbit(c.lo)
			}
			return mbit(c.hi)
		}
		var capacity float64
		for now := 5 * time.Second; now < 60*time.Second; now += 10 * time.Millisecond {
			capacity += rate(now) * 0.01 * (1 - c.loss)
		}
		r := runSim(simConfig{profile: ProfileStandard, duration: 60 * time.Second, rtt: c.rtt, rate: rate, buffer: c.buf, loss: c.loss})
		util := r.goodput(5*time.Second, 60*time.Second) * 55 / capacity
		mq := r.medianQueue(5*time.Second, 60*time.Second)
		t.Logf("%v<->%v Mbit/s every %v, rtt %v, loss %v: utilization %.0f%%, median queue %v, stale resets %d",
			c.hi, c.lo, c.period, c.rtt, c.loss, util*100, mq, r.s.StaleResets)
		if util < c.minUtil {
			t.Errorf("%v<->%v/%v/%v: utilization %.0f%% < %.0f%%", c.hi, c.lo, c.period, c.rtt, util*100, c.minUtil*100)
		}
		if mq > c.maxMedian {
			t.Errorf("%v<->%v/%v/%v: median queue %v > %v", c.hi, c.lo, c.period, c.rtt, mq, c.maxMedian)
		}
	}
}
