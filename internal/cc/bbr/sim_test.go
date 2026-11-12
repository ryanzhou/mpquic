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
	flowSent      [][]float64 // bytes sent per bucket
	flowDropped   [][]float64 // ... and of those, bytes that did not arrive
}

// lossRate returns the share of what flow i sent in [from, to) that was lost.
func (r simResult) lossRate(i int, from, to time.Duration) float64 {
	var sent, lost float64
	for b := int(from / simBucket); b < int(to/simBucket) && b < len(r.flowSent[i]); b++ {
		sent += r.flowSent[i][b]
		lost += r.flowDropped[i][b]
	}
	return lost / max(sent, 1)
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
		s := NewBbrSender(clk, size)
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
		res.flowSent = append(res.flowSent, make([]float64, nb))
		res.flowDropped = append(res.flowDropped, make([]float64, nb))
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
				res.flowSent[i][bucket] += float64(size)
				if rng.Float64() < cfg.loss || !enqueue(pkt{flow: i, pn: f.pn, sentAt: now}) {
					f.dropped = append(f.dropped, pkt{pn: f.pn, sentAt: now})
					res.flowDropped[i][bucket] += float64(size)
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

func constRate(m float64) func(time.Duration) float64 {
	return func(time.Duration) float64 { return mbit(m) }
}

// What the tests below expect is what BBRv3 is designed to do (the draft's
// sections 3 and 5), with margins: they are not fitted to what this
// implementation happens to measure.

// A bulk flow alone on a link, whatever the buffer: it uses the link, and
// keeps no standing queue (it cruises at the estimated rate, and drains
// what its probes queue).
func TestSimSteadyState(t *testing.T) {
	for _, buffer := range []time.Duration{20 * time.Millisecond, 500 * time.Millisecond} {
		r := runSim(simConfig{duration: 20 * time.Second, rtt: 50 * time.Millisecond, rate: constRate(20), buffer: buffer})
		util := r.goodput(3*time.Second, 20*time.Second) / mbit(20)
		mq := r.medianQueue(3*time.Second, 20*time.Second)
		loss := r.lossRate(0, 3*time.Second, 20*time.Second)
		t.Logf("buffer %v: utilization %.1f%%, median queue %v, loss %.2f%%", buffer, util*100, mq, loss*100)
		if util < 0.9 {
			t.Errorf("buffer %v: utilization %.1f%% < 90%%", buffer, util*100)
		}
		if mq > 10*time.Millisecond {
			t.Errorf("buffer %v: median queueing delay %v > 10ms", buffer, mq)
		}
		if loss > lossThresh {
			t.Errorf("buffer %v: loss %.2f%% above BBR.LossThresh", buffer, loss*100)
		}
	}
}

// A policer (hardly any buffer: what exceeds the rate is dropped) is what
// BBRv1 overran, because it ignored loss. BBRv3 probes above the rate only
// until the loss shows, and stays below it otherwise: the loss stays
// around BBR.LossThresh, at a rate near the policer's. With a loss
// tolerance of 100 % (loss ignored altogether) the controller has to rely
// on the delivery rate alone, and loses more.
func TestSimPolicer(t *testing.T) {
	run := func(tolerance float64) (util, loss float64) {
		r := runSim(simConfig{duration: 30 * time.Second, rtt: 100 * time.Millisecond, rate: constRate(50), buffer: 2 * time.Millisecond,
			setup: func(_ int, s *Sender) { s.LossTolerance = func() float64 { return tolerance } }})
		return r.goodput(5*time.Second, 30*time.Second) / mbit(50), r.lossRate(0, 5*time.Second, 30*time.Second)
	}
	util, loss := run(0)
	utilAll, lossAll := run(1)
	t.Logf("default: utilization %.0f%%, loss %.2f%%; loss ignored: utilization %.0f%%, loss %.2f%%", util*100, loss*100, utilAll*100, lossAll*100)
	if util < 0.7 {
		t.Errorf("utilization %.0f%% < 70%%", util*100)
	}
	if loss > 1.5*lossThresh {
		t.Errorf("loss %.2f%% at a policer: well above BBR.LossThresh", loss*100)
	}
	if lossAll <= loss {
		t.Errorf("ignoring loss lost %.2f%%, not more than the default's %.2f%%", lossAll*100, loss*100)
	}
}

// Random loss, which is not congestion. Below BBR.LossThresh the default
// keeps most of the rate; above it the default takes the path for
// congested (by design: it cannot tell), and a loss tolerance above the
// path's loss rate gives the rate back. (Up to 20 % loss: BBR finds a
// path's rate by sending 1.25 times its estimate, and with more loss than
// that even the probe delivers less than the estimate.)
func TestSimLossTolerance(t *testing.T) {
	run := func(loss, tolerance float64) float64 {
		r := runSim(simConfig{duration: 30 * time.Second, rtt: 100 * time.Millisecond, rate: constRate(50), buffer: 100 * time.Millisecond, loss: loss,
			setup: func(_ int, s *Sender) { s.LossTolerance = func() float64 { return tolerance } }})
		// of what the loss leaves of the link
		return r.goodput(5*time.Second, 30*time.Second) / mbit(50) / (1 - loss)
	}
	if u := run(0.005, 0); u < 0.8 {
		t.Errorf("0.5%% loss, default: utilization %.0f%% < 80%%", u*100)
	}
	for _, c := range []struct{ loss, tolerance float64 }{{0.05, 0.1}, {0.1, 0.2}, {0.1, 1}, {0.15, 1}} {
		with, without := run(c.loss, c.tolerance), run(c.loss, 0)
		t.Logf("%.0f%% loss: utilization %.0f%% with a tolerance of %.0f%%, %.0f%% without", c.loss*100, with*100, c.tolerance*100, without*100)
		if with < 0.9 {
			t.Errorf("%.0f%% loss, tolerance %.0f%%: utilization %.0f%% < 90%%", c.loss*100, c.tolerance*100, with*100)
		}
		if without > 0.5 {
			t.Errorf("%.0f%% loss without a tolerance: utilization %.0f%%; the default should take this for congestion", c.loss*100, without*100)
		}
	}
	// A tolerance does not stop the controller from finding a path's rate:
	// on a clean path it changes nothing.
	if u := run(0, 0.2); u < 0.9 {
		t.Errorf("no loss, tolerance 20%%: utilization %.0f%% < 90%%", u*100)
	}
}

// ACK aggregation (Wi-Fi, cellular: ACKs arrive in bursts) must not cost
// throughput: BBR.extra_acked widens the window for it.
func TestSimAckAggregation(t *testing.T) {
	for _, c := range []struct{ jitter, rtt time.Duration }{
		{20 * time.Millisecond, 30 * time.Millisecond},
		{40 * time.Millisecond, 5 * time.Millisecond},
		{20 * time.Millisecond, 50 * time.Millisecond},
	} {
		r := runSim(simConfig{duration: 15 * time.Second, rtt: c.rtt, rate: constRate(50), buffer: 200 * time.Millisecond, jitter: c.jitter})
		util := r.goodput(4*time.Second, 15*time.Second) / mbit(50)
		t.Logf("ACKs every %v, rtt %v: utilization %.0f%%", c.jitter, c.rtt, util*100)
		if util < 0.85 {
			t.Errorf("ACKs every %v, rtt %v: utilization %.0f%% < 85%%", c.jitter, c.rtt, util*100)
		}
	}
}

// The bottleneck's rate drops. Where the buffer is shallow, the loss tells
// BBRv3 at once (BBR.bw_shortterm). Where it is deep there is no loss, and
// the old estimate stands until the bandwidth filter has forgotten it: two
// ProbeBW cycles, each of up to 3 s and a few round trips (which the queue
// makes longer). The flow keeps the link busy throughout, with a queue that
// the window bounds, and that is gone after that.
func TestSimRateDrop(t *testing.T) {
	const rtt = 50 * time.Millisecond
	for _, c := range []struct {
		lo      float64
		buffer  time.Duration
		settled time.Duration // the queue is gone this long after the drop
	}{
		{20, 50 * time.Millisecond, 10 * time.Second},
		{20, 2 * time.Second, 10 * time.Second},
		{5, 50 * time.Millisecond, 20 * time.Second},
		{5, 2 * time.Second, 20 * time.Second},
	} {
		rate := func(now time.Duration) float64 {
			if now >= 6*time.Second {
				return mbit(c.lo)
			}
			return mbit(40)
		}
		const end = 60 * time.Second
		r := runSim(simConfig{duration: end, rtt: rtt, rate: rate, buffer: c.buffer})
		util := r.goodput(8*time.Second, end) / mbit(c.lo)
		late := r.medianQueue(6*time.Second+c.settled, end)
		t.Logf("40 -> %v Mbit/s, buffer %v: utilization %.0f%%, median queue from %v after the drop: %v", c.lo, c.buffer, util*100, c.settled, late)
		if util < 0.85 {
			t.Errorf("40 -> %v, buffer %v: utilization after the drop %.0f%% < 85%%", c.lo, c.buffer, util*100)
		}
		// (no standing queue: what the probes queue for a few round trips
		// in every cycle does not make the median)
		if late > rtt/2 {
			t.Errorf("40 -> %v, buffer %v: median queue %v more than %v after the drop", c.lo, c.buffer, late, c.settled)
		}
	}
}

// The rate comes back: the flow finds it with its next probes.
func TestSimRateRise(t *testing.T) {
	rate := func(now time.Duration) float64 {
		if now >= 6*time.Second {
			return mbit(40)
		}
		return mbit(10)
	}
	r := runSim(simConfig{duration: 30 * time.Second, rtt: 50 * time.Millisecond, rate: rate, buffer: 200 * time.Millisecond})
	util := r.goodput(16*time.Second, 30*time.Second) / mbit(40)
	t.Logf("10 -> 40 Mbit/s: utilization from 10 s after the rise %.0f%%", util*100)
	if util < 0.85 {
		t.Errorf("utilization after the rise %.0f%% < 85%%", util*100)
	}
}

// The ProbeRTT gate is held only while in ProbeRTT, and it is used.
func TestSimProbeGateHeldOnlyWhileProbing(t *testing.T) {
	held, probes := false, 0
	gate := func(enter bool) bool {
		if enter && !held {
			probes++
		}
		held = enter
		return true
	}
	runSim(simConfig{duration: 40 * time.Second, rtt: 50 * time.Millisecond, rate: constRate(10), buffer: 200 * time.Millisecond, gate: gate,
		onStep: func(now time.Duration, s *Sender) {
			if held != (s.st == stProbeRTT) {
				t.Fatalf("%v: gate held: %v in state %s", now, held, s.ModeName())
			}
		}})
	if probes < 2 {
		t.Errorf("ProbeRTT entered %d times in 40 s, want >= 2", probes)
	}
}

// A gate that refuses keeps the controller out of ProbeRTT until it lets
// it in.
func TestSimProbeGateRefused(t *testing.T) {
	open := false
	entered := time.Duration(0)
	var now time.Duration
	gate := func(enter bool) bool {
		if enter && open && entered == 0 {
			entered = now
		}
		return !enter || open
	}
	runSim(simConfig{duration: 20 * time.Second, rtt: 50 * time.Millisecond, rate: constRate(10), buffer: 200 * time.Millisecond, gate: gate,
		onStep: func(t0 time.Duration, s *Sender) {
			now = t0
			open = t0 >= 12*time.Second
			if !open && s.st == stProbeRTT {
				t.Fatalf("%v: in ProbeRTT although the gate refused", t0)
			}
		}})
	if entered < 12*time.Second || entered > 12*time.Second+100*time.Millisecond {
		t.Errorf("ProbeRTT entered at %v, want right after the gate opened at 12s", entered)
	}
}

// On a mostly idle (interactive) path every packet is sent with nothing in
// flight. Its round-trip samples are as good as a ProbeRTT's, so the gate
// is never taken, and never kept.
func TestProbeGateIdlePath(t *testing.T) {
	clk := &fakeClock{t: monotime.Now()}
	b := NewBbrSender(clk, 1200)
	b.SetRTTStatsProvider(fakeRTT{50 * time.Millisecond})
	held := false
	b.ProbeRTTGate = func(enter bool) bool {
		held = enter
		return true
	}
	b.AppLimitedHint = func() bool { return true }
	pn := congestion.PacketNumber(1)
	for i := 0; i < 400; i++ { // 20 s, one packet per RTT
		b.OnPacketSent(clk.t, 0, pn, 1200, true)
		clk.t = clk.t.Add(50 * time.Millisecond)
		b.OnCongestionEventEx(1200, clk.t, []congestion.AckedPacketInfo{{PacketNumber: pn, BytesAcked: 1200, ReceivedTime: clk.t}}, nil)
		pn++
		if held || b.st == stProbeRTT {
			t.Fatalf("after %v: gate held: %v, state %s", time.Duration(i+1)*50*time.Millisecond, held, b.ModeName())
		}
	}
	if b.MinRTT() != 50*time.Millisecond {
		t.Errorf("min_rtt %v, want 50ms", b.MinRTT())
	}
	// The same after a pause without any packets, when data comes: the
	// first round trip of the burst is the probe (BBRHandleRestartFromIdle),
	// not a reason to cut the burst's window.
	clk.t = clk.t.Add(8 * time.Second)
	b.AppLimitedHint = func() bool { return false }
	for i := 0; i < 10; i++ {
		b.OnPacketSent(clk.t, congestion.ByteCount(i)*1200, pn+congestion.PacketNumber(i), 1200, true)
	}
	clk.t = clk.t.Add(50 * time.Millisecond)
	for i := 0; i < 10; i++ {
		b.OnCongestionEventEx(congestion.ByteCount(10-i)*1200, clk.t, []congestion.AckedPacketInfo{{PacketNumber: pn + congestion.PacketNumber(i), BytesAcked: 1200, ReceivedTime: clk.t}}, nil)
		if held || b.st == stProbeRTT {
			t.Fatalf("burst after a pause: gate held: %v, state %s", held, b.ModeName())
		}
	}
}

// Two flows share a bottleneck (two paths of a session behind the same
// uplink), the second joining later: they use the link and share it.
func TestSimSharedBottleneck(t *testing.T) {
	for _, buffer := range []time.Duration{20 * time.Millisecond, time.Second} {
		r := runSim(simConfig{duration: 40 * time.Second, rtt: 50 * time.Millisecond, rate: constRate(20), buffer: buffer,
			flows: 2, flowStart: []time.Duration{0, 3 * time.Second}})
		a := r.flowGoodput(0, 10*time.Second, 40*time.Second) / mbit(20)
		b := r.flowGoodput(1, 10*time.Second, 40*time.Second) / mbit(20)
		mq := r.medianQueue(10*time.Second, 40*time.Second)
		t.Logf("buffer %v: shares %.0f%% + %.0f%%, median queue %v", buffer, a*100, b*100, mq)
		if a+b < 0.9 {
			t.Errorf("buffer %v: utilization %.0f%% < 90%%", buffer, (a+b)*100)
		}
		if min(a, b) < 0.2 {
			t.Errorf("buffer %v: unfair shares %.0f%% / %.0f%%", buffer, a*100, b*100)
		}
		if mq > 100*time.Millisecond {
			t.Errorf("buffer %v: median queueing delay %v > 100ms", buffer, mq)
		}
	}
}

// A rate cap bounds what a flow sends, in every state: three flows capped
// at a third of the link each (a configured rate, shared out by the
// session) use the link without ever filling it.
func TestSimRateCap(t *testing.T) {
	r := runSim(simConfig{duration: 15 * time.Second, rtt: 40 * time.Millisecond, rate: constRate(60), buffer: 100 * time.Millisecond, flows: 3,
		setup: func(_ int, s *Sender) {
			s.RateCap = func() Bandwidth { return Bandwidth(mbit(20)) * BytesPerSecond }
		}})
	var u, most float64
	for i := range 3 {
		u += r.flowGoodput(i, 3*time.Second, 15*time.Second) / mbit(60)
		for _, v := range r.flowSent[i] {
			most = max(most, v)
		}
	}
	var mq time.Duration
	for _, q := range r.queue {
		mq = max(mq, q)
	}
	t.Logf("utilization %.0f%%, longest queue %v, most sent by a flow in 100 ms: %.1f Mbit/s", u*100, mq, most*10*8/1e6)
	if u < 0.9 {
		t.Errorf("utilization %.0f%% < 90%%", u*100)
	}
	// (the pacer's burst allowance on top of the rate)
	if most > mbit(20)*0.1+16*1200 {
		t.Errorf("a flow capped at 20 Mbit/s sent %.1f Mbit/s over 100 ms", most*10*8/1e6)
	}
	if mq > 10*time.Millisecond {
		t.Errorf("queue of %v on a link that the caps never fill", mq)
	}
}

// BBR against a loss-based (Reno) flow: neither starves, whatever the
// buffer (BBR.inflight_longterm and the time-scale of its probes are meant
// for that).
func TestSimRenoCompetition(t *testing.T) {
	for _, buffer := range []time.Duration{50 * time.Millisecond, 200 * time.Millisecond, time.Second} {
		r := runSim(simConfig{duration: 60 * time.Second, rtt: 50 * time.Millisecond, rate: constRate(20), buffer: buffer,
			flows: 2, reno: []bool{false, true}})
		a := r.flowGoodput(0, 10*time.Second, 60*time.Second) / mbit(20)
		b := r.flowGoodput(1, 10*time.Second, 60*time.Second) / mbit(20)
		t.Logf("buffer %v: bbr %.0f%%, reno %.0f%%", buffer, a*100, b*100)
		if min(a, b) < 0.1 {
			t.Errorf("buffer %v: a flow starved (bbr %.0f%%, reno %.0f%%)", buffer, a*100, b*100)
		}
		if a+b < 0.9 {
			t.Errorf("buffer %v: utilization %.0f%%", buffer, (a+b)*100)
		}
	}
}
