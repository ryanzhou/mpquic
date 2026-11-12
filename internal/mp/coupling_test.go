package mp

import (
	"testing"
	"time"

	"mpquic/internal/cc"
)

// cpSim is a fluid model of a session's paths behind one bottleneck: every
// path sends at its estimate (its own, or its share of the session rate
// times the session's gain), and the bottleneck serves the paths in
// proportion to what they send, queues up to its buffer and drops the rest.
// Like BBR with drain-to-target, a path sends at 0.75 of its estimate from
// one RTT in eight until the queue it sees is gone. Acknowledgements (and
// losses) are known one RTT after the send. A path's own estimate starts at
// its own bottleneck's rate and, like BBR's, comes down to 1.25 times what
// the path has been delivering lately (what its probes would find).
type cpSim struct {
	s        *Session
	now      time.Time
	el       time.Duration
	capacity float64   // common bottleneck (bytes/s), 0 = none
	buffer   float64   // its buffer in seconds (0: a policer)
	own      []float64 // each path's own bottleneck (bytes/s)
	raw      []float64 // its own estimate
	dlv      []float64 // what it has been delivering lately
	base     []time.Duration
	idle     bool          // the application has nothing to send
	extra    time.Duration // delay that is not the session's: a queue in the ACK direction
	loss     float64       // random loss on every path
	q        float64
	draining []bool
	m        []cc.Metrics
	pend     [][]cpAck // acknowledgements on their way back, per path
	sent     float64   // totals since reset
	lost     float64
}

type cpAck struct {
	at          time.Duration
	acked, lost uint64
}

func mbit(x float64) float64 { return x * 1e6 / 8 }

func newCPSim(capacity, buffer float64, own []float64, base ...time.Duration) *cpSim {
	s := &Session{opt: &Options{}, log: testLogger()}
	for i, r := range base {
		s.paths = append(s.paths, &Path{sess: s, id: i + 1, meter: &cc.Meter{}, state: stateActive, minRTT: r, latestRTT: r, srtt: r, ccMode: "probe_bw"})
	}
	return &cpSim{s: s, now: time.Now(), capacity: capacity, buffer: buffer, own: own, base: base, m: make([]cc.Metrics, len(base)),
		draining: make([]bool, len(base)), pend: make([][]cpAck, len(base)),
		raw: append([]float64(nil), own...), dlv: make([]float64, len(base))}
}

// est is what path i paces at.
func (x *cpSim) est(i int) float64 {
	p := x.s.paths[i]
	if c := float64(p.meter.RateCap()); c > 0 && c < x.raw[i] {
		if g := p.meter.RateGain(); g > 0 {
			return c * g
		}
		return c
	}
	return x.raw[i]
}

func (x *cpSim) run(d time.Duration) {
	dt := tickInterval.Seconds()
	for end := x.el + d; x.el < end; x.el += tickInterval {
		x.now = x.now.Add(tickInterval)
		send := make([]float64, len(x.own))
		var in float64
		for i, p := range x.s.paths {
			p.busy = !x.idle
			if p.state == stateActive && !x.idle {
				send[i] = x.est(i) * dt
				switch {
				case x.capacity == 0 || x.q < 1500:
					x.draining[i] = false
				case (x.el/x.base[i])%8 == 1:
					x.draining[i] = true
				}
				if x.draining[i] {
					send[i] *= 0.75
				}
			}
			in += send[i]
		}
		out, lost := in, 0.0
		if x.capacity > 0 {
			out = min(x.q+in, x.capacity*dt)
			x.q += in - out
			if limit := x.buffer * x.capacity; x.q > limit {
				lost, x.q = x.q-limit, limit
			}
		}
		x.sent += in
		x.lost += lost
		for i, p := range x.s.paths {
			m := &x.m[i]
			rtt := x.base[i] + x.extra
			if x.capacity > 0 {
				rtt += time.Duration(x.q / x.capacity * float64(time.Second))
			}
			if in > 0 {
				share := send[i] / in
				x.pend[i] = append(x.pend[i], cpAck{x.el + rtt, uint64(out * share * (1 - x.loss)), uint64((lost + out*x.loss) * share / 1200)})
				if send[i] > 0 {
					x.dlv[i] += (out*share/dt - x.dlv[i]) * dt / (10 * x.base[i].Seconds())
					x.raw[i] = min(x.own[i], max(1.25*x.dlv[i], x.raw[i]*(1-dt/(10*x.base[i].Seconds()))))
				}
			}
			for len(x.pend[i]) > 0 && x.pend[i][0].at <= x.el {
				m.AckedBytes += x.pend[i][0].acked
				m.PacketsLost += x.pend[i][0].lost
				m.AckedAt = cc.Mono(x.now)
				x.pend[i] = x.pend[i][1:]
			}
			p.lossRate = x.loss
			m.SentBytes += uint64(send[i])
			m.PacketsSent += uint64(send[i] / 1200)
			m.RawBandwidth = uint64(x.raw[i])
			p.meter.Inject(*m)
			p.latestRTT, p.srtt, p.bw = rtt, rtt, uint64(x.est(i))
		}
		x.s.couplingTickLocked(x.now)
	}
}

// total is what the paths pace at together.
func (x *cpSim) total() (t float64) {
	for i, p := range x.s.paths {
		if p.state == stateActive {
			t += x.est(i)
		}
	}
	return t
}

func (x *cpSim) queueDelay() time.Duration {
	return time.Duration(x.q / x.capacity * float64(time.Second))
}

// cpSimTol: how far below the bottleneck the session rate may settle in the
// model. Its paths drain as soon as a probe queues anything, so a probe that
// starts within 12% of the capacity is cut short and looks undelivered (real
// paths drain in one phase of eight; the benchmarks show 97-100%).
const cpSimTol = 0.15

func within(got, want, tol float64) bool { return got >= want*(1-tol) && got <= want*(1+tol) }

// Three unconstrained relays behind a 20 Mbit/s uplink, each path's own
// estimate being the whole uplink: the session rate finds the uplink, the
// shares add up to it and the queue goes away; it follows the uplink down at
// once and up within a few seconds.
func TestSessionRateCommonBottleneck(t *testing.T) {
	c := mbit(20)
	x := newCPSim(c, 0.3, []float64{c * 4, c * 4, c * 4}, 80*time.Millisecond, 200*time.Millisecond, 320*time.Millisecond)
	x.run(12 * time.Second)
	if b := x.s.cp.bw; !within(b, c, 0.07) {
		t.Fatalf("session rate %.1f Mbit/s, want 20", b*8/1e6)
	}
	x.run(100 * time.Millisecond)
	if tot := x.total(); tot > c*1.3 {
		t.Fatalf("paths pace at %.1f Mbit/s together, want about 20", tot*8/1e6)
	}
	// steady state: no standing queue, hardly any loss, the uplink is used
	x.sent, x.lost = 0, 0
	var maxQ time.Duration
	for i := 0; i < 200; i++ {
		x.run(100 * time.Millisecond)
		maxQ = max(maxQ, x.queueDelay())
	}
	// (a probe's worth is a quarter of a round, 80 ms here; when the shares
	// lapse, the model's paths all send 1.25 times what they deliver at
	// once, until the queue shows after a round or two)
	if maxQ > 250*time.Millisecond {
		t.Errorf("queue up to %v in steady state", maxQ)
	}
	if x.sent < 0.93*c*20 {
		t.Errorf("sent %.1f Mbit/s of 20", x.sent/20*8/1e6)
	}
	// the uplink halves: followed within a few rounds
	x.capacity = c / 2
	x.run(3 * time.Second)
	if b := x.s.cp.bw; !within(b, c/2, 0.2) {
		t.Fatalf("after the capacity halved: session rate %.1f Mbit/s, want 10", b*8/1e6)
	}
	// ... and comes back at four times that: found by probing
	x.capacity = 2 * c
	x.run(24 * time.Second)
	if b := x.s.cp.bw; b < 1.7*c {
		t.Fatalf("after the capacity grew to 40 Mbit/s: session rate %.1f Mbit/s", b*8/1e6)
	}
	if !x.s.Rate().Congested && x.s.Rate().Rate == 0 {
		t.Fatal("Rate() reports nothing")
	}
}

// A policer never delays, it drops: the session rate settles at its rate
// instead of losing the paths' overestimate all the time.
func TestSessionRatePolicer(t *testing.T) {
	c := mbit(50)
	x := newCPSim(c, 0, []float64{c * 2, c * 2, c * 2}, 80*time.Millisecond, 200*time.Millisecond, 320*time.Millisecond)
	x.run(8 * time.Second)
	if b := x.s.cp.bw; !within(b, c, 0.07) {
		t.Fatalf("session rate %.1f Mbit/s, want 50", b*8/1e6)
	}
	x.sent, x.lost = 0, 0
	x.run(40 * time.Second)
	// (what remains is the probes, a quarter of a round in ten as BBR's,
	// and the rounds after the shares lapse)
	if l := x.lost / x.sent; l > 0.06 {
		t.Errorf("%.1f%% lost at the policer in steady state", l*100)
	}
	if x.sent < 0.93*c*40 {
		t.Errorf("sent %.1f Mbit/s of 50", x.sent/40*8/1e6)
	}
}

// A queue that is not the session's (the peer's data delaying our ACKs,
// somebody else's traffic) raises every path's RTT like congestion does.
// Slowing down does not shrink it, so the session keeps its rate.
func TestSessionRateForeignQueue(t *testing.T) {
	c := mbit(20)
	x := newCPSim(c, 0.3, []float64{c * 4, c * 4, c * 4}, 80*time.Millisecond, 200*time.Millisecond, 320*time.Millisecond)
	x.run(12 * time.Second)
	x.extra = 60 * time.Millisecond
	x.sent = 0
	x.run(40 * time.Second)
	if b := x.s.cp.bw; !within(b, c, cpSimTol) {
		t.Errorf("session rate %.1f Mbit/s with a queue in the other direction, want 20", b*8/1e6)
	}
	if x.sent < (1-cpSimTol)*c*40 {
		t.Errorf("sent %.1f Mbit/s of 20 with a queue in the other direction", x.sent/40*8/1e6)
	}
	// It is there from the start for a session that begins later, and that
	// session still finds the uplink.
	y := newCPSim(c, 0.3, []float64{c * 4, c * 4, c * 4}, 80*time.Millisecond, 200*time.Millisecond, 320*time.Millisecond)
	y.run(4 * time.Second)
	y.extra = 60 * time.Millisecond
	y.capacity = 2 * c
	y.run(32 * time.Second)
	if b := y.s.cp.bw; b < 1.7*c {
		t.Errorf("session rate %.1f Mbit/s: did not find 40 Mbit/s with a queue in the other direction", b*8/1e6)
	}
}

// Random loss on every path is not congestion: BBR's point is to keep its
// rate through it, and so does the session.
func TestSessionRateRandomLoss(t *testing.T) {
	c := mbit(20)
	x := newCPSim(c, 0.3, []float64{c * 4, c * 4, c * 4}, 80*time.Millisecond, 200*time.Millisecond, 320*time.Millisecond)
	x.loss = 0.03
	x.run(12 * time.Second)
	x.sent = 0
	x.run(80 * time.Second)
	if x.sent < (1-cpSimTol)*c*80 {
		t.Errorf("sent %.1f Mbit/s of 20 with 3%% random loss on every path (session rate %.1f)", x.sent/80*8/1e6, x.s.cp.bw*8/1e6)
	}
	// ... and still finds capacity that appears
	x.capacity = 2 * c
	x.run(32 * time.Second)
	if b := x.s.cp.bw; b < 1.6*c {
		t.Errorf("session rate %.1f Mbit/s: did not find 40 Mbit/s with random loss", b*8/1e6)
	}
}

// Independent paths deliver the sum of their own rates: nobody is limited.
func TestSessionRateIndependentPaths(t *testing.T) {
	own := []float64{mbit(50), mbit(30), mbit(20)}
	x := newCPSim(0, 0, own, 80*time.Millisecond, 200*time.Millisecond, 320*time.Millisecond)
	x.run(12 * time.Second)
	for i := range own {
		if e := x.est(i); e < 0.93*own[i] {
			t.Errorf("path %d paces at %.1f of its %.1f Mbit/s", i, e*8/1e6, own[i]*8/1e6)
		}
	}
	// A path that fails takes its rate with it; the others keep theirs.
	x.s.paths[0].state = stateSuspect
	x.run(12 * time.Second)
	for i := 1; i < len(own); i++ {
		if e := x.est(i); e < 0.93*own[i] {
			t.Errorf("after path 0 failed: path %d paces at %.1f of its %.1f Mbit/s", i, e*8/1e6, own[i]*8/1e6)
		}
	}
}

// Rounds in which the application had nothing to send say nothing about
// capacity.
func TestSessionRateAppLimited(t *testing.T) {
	c := mbit(20)
	x := newCPSim(c, 0.3, []float64{c * 4, c * 4}, 80*time.Millisecond, 200*time.Millisecond)
	x.run(12 * time.Second)
	before := x.s.cp.bw
	x.idle = true
	x.run(20 * time.Second)
	if b := x.s.cp.bw; !within(b, before, 0.02) {
		t.Fatalf("session rate changed from %.1f to %.1f Mbit/s while idle", before*8/1e6, b*8/1e6)
	}
	x.idle = false
	x.run(4 * time.Second)
	if b := x.s.cp.bw; !within(b, c, cpSimTol) {
		t.Fatalf("session rate %.1f Mbit/s after the idle period, want 20", b*8/1e6)
	}
}

// A configured limit is a ceiling that is obeyed from the first packet and
// never probed; a single path is otherwise left to its own BBR.
func TestSessionRateLimit(t *testing.T) {
	c := mbit(20)
	x := newCPSim(c, 0.3, []float64{c * 4, c * 4, c * 4}, 80*time.Millisecond, 200*time.Millisecond, 320*time.Millisecond)
	x.s.opt.MaxSendRate = uint64(c * 0.9)
	for i := 0; i < 500; i++ {
		x.run(tickInterval)
		if tot := x.total(); tot > c*0.9*1.001 {
			t.Fatalf("paths pace at %.2f Mbit/s together with a limit of 18", tot*8/1e6)
		}
	}
	if x.lost > 0 || x.queueDelay() > time.Millisecond {
		t.Fatalf("limit below the capacity, but lost %.0f bytes, queue %v", x.lost, x.queueDelay())
	}
	if l := x.s.Rate().Limit; l != uint64(c*0.9) {
		t.Fatalf("Rate().Limit = %d", l)
	}
	// the peer's receive limit (frLimit) counts too, the lower one wins
	if err := x.s.onCtrl(x.s.paths[0], frLimit, encodeU64s(0, uint64(c/2))[2:]); err != nil {
		t.Fatal(err)
	}
	x.run(time.Second)
	if tot := x.total(); !within(tot, c/2, 0.01) {
		t.Fatalf("paths pace at %.2f Mbit/s together with a peer limit of 10", tot*8/1e6)
	}

	one := newCPSim(c, 0.3, []float64{c * 4}, 80*time.Millisecond)
	one.run(8 * time.Second)
	if cp := one.s.paths[0].meter.RateCap(); cp != 0 {
		t.Fatalf("single path capped at %d", cp)
	}
	one.s.opt.MaxSendRate = uint64(c / 2)
	one.run(100 * time.Millisecond)
	if cp := one.s.paths[0].meter.RateCap(); cp != uint64(c/2) {
		t.Fatalf("single path with a limit: cap %d", cp)
	}
}

// A path's share may be small: on a slow uplink with nearby relays (low
// RTTs) the shares must still add up to the session rate or the limit.
func TestSessionRateSmallShares(t *testing.T) {
	c := mbit(3)
	x := newCPSim(c, 0.3, []float64{c * 4, c * 4, c * 4, c * 4}, 10*time.Millisecond, 10*time.Millisecond, 20*time.Millisecond, 20*time.Millisecond)
	x.s.opt.MaxSendRate = uint64(c)
	x.run(2 * time.Second)
	if tot := x.total(); tot > c*1.001 {
		t.Fatalf("paths pace at %.2f Mbit/s together with a limit of 3", tot*8/1e6)
	}
	for i, p := range x.s.paths {
		if cp := p.meter.RateCap(); cp == 0 || float64(cp) > c/4*1.001 {
			t.Fatalf("path %d capped at %.2f Mbit/s, want 0.75", i, float64(cp)*8/1e6)
		}
	}
}

// With the session rate off (MPQUIC_COUPLE=0) only a configured limit
// applies.
func TestSessionRateOff(t *testing.T) {
	c := mbit(20)
	x := newCPSim(c, 0.3, []float64{c * 4, c * 4}, 80*time.Millisecond, 200*time.Millisecond)
	cpDisabled = true
	defer func() { cpDisabled = false }()
	x.run(8 * time.Second)
	for _, p := range x.s.paths {
		if p.meter.RateCap() != 0 {
			t.Fatal("capped without coupling")
		}
	}
	x.s.opt.MaxSendRate = uint64(c)
	x.run(100 * time.Millisecond)
	if tot := x.total(); !within(tot, c, 0.01) {
		t.Fatalf("paths pace at %.2f Mbit/s together with a limit of 20", tot*8/1e6)
	}
}

func TestBaseRTTWindow(t *testing.T) {
	var c cpPath
	t0 := time.Now()
	c.sampleRTT(t0, 30*time.Millisecond)
	c.sampleRTT(t0.Add(time.Second), 20*time.Millisecond)
	c.sampleRTT(t0.Add(2*time.Second), 25*time.Millisecond)
	if b := c.baseRTT(); b != 20*time.Millisecond {
		t.Fatalf("base %v, want 20ms", b)
	}
	// The route gets longer: the old minimum is forgotten within the window.
	for d := 3 * time.Second; d < 3*time.Second+cpBaseWindow; d += time.Second {
		c.sampleRTT(t0.Add(d), 40*time.Millisecond)
	}
	if b := c.baseRTT(); b != 40*time.Millisecond {
		t.Fatalf("base %v after the route changed, want 40ms", b)
	}
	c.rttMin = 47 * time.Millisecond
	if q := c.queue(); q != 7*time.Millisecond {
		t.Fatalf("queue %v, want 7ms", q)
	}
}

// Acknowledgements waiting for a path are replaced by newer ones, not
// queued behind them.
func TestAckCoalescing(t *testing.T) {
	p := &Path{ackSig: make(chan struct{}, 1)}
	p.queueAcks([]ackEntry{{id: 1, cum: 10}, {id: 2, cum: 5}}, []pathAck{{0, []byte{1}}, {3, []byte{7}}})
	p.queueAcks([]ackEntry{{id: 1, cum: 30, window: 99}}, []pathAck{{0, []byte{2}}})
	p.queueAcks(nil, nil)
	if len(p.ackSig) != 1 {
		t.Fatal("no signal")
	}
	entries, pathAcks := p.takeAcks()
	got := map[uint64]uint64{}
	for _, e := range entries {
		got[e.id] = e.cum
	}
	if len(entries) != 2 || got[1] != 30 || got[2] != 5 {
		t.Fatalf("entries %+v", entries)
	}
	if len(pathAcks) != 2 {
		t.Fatalf("path acks %v", pathAcks)
	}
	for _, f := range pathAcks {
		if f[0] != 2 && f[0] != 7 {
			t.Fatalf("stale path ack %v", f)
		}
	}
	if e, a := p.takeAcks(); len(e) != 0 || len(a) != 0 {
		t.Fatal("not drained")
	}
}

func TestAckInterval(t *testing.T) {
	s := &Session{opt: (&Options{}).withDefaults()}
	if d := s.ackEveryLocked(); d != maxAckEvery {
		t.Fatalf("no paths: %v", d)
	}
	add := func(srtt time.Duration, st pathState) {
		s.paths = append(s.paths, &Path{sess: s, state: st, srtt: srtt})
	}
	add(200*time.Millisecond, stateActive)
	if d := s.ackEveryLocked(); d != maxAckEvery {
		t.Fatalf("200 ms path: %v", d)
	}
	add(20*time.Millisecond, stateActive)
	if d := s.ackEveryLocked(); d != 5*time.Millisecond {
		t.Fatalf("20 ms path: %v, want 5ms", d)
	}
	add(2*time.Millisecond, stateSuspect) // does not count
	if d := s.ackEveryLocked(); d != 5*time.Millisecond {
		t.Fatalf("suspect path counted: %v", d)
	}
	add(4*time.Millisecond, stateActive)
	if d := s.ackEveryLocked(); d != s.opt.AckDelay {
		t.Fatalf("4 ms path: %v, want AckDelay", d)
	}
}

// On a slow path a chunk is no more than the path sends in chunkTime.
func TestChunkSizeFollowsRate(t *testing.T) {
	s := &Session{opt: (&Options{}).withDefaults()}
	p := &Path{sess: s, state: stateActive, ccMode: "probe_bw", minRTT: 20 * time.Millisecond, dgramChunk: dgramChunkDefault}
	if n := p.maxChunk(); n < 1<<20 {
		t.Fatalf("no estimate yet: %d", n)
	}
	p.bw = uint64(mbit(6.5))
	if n := p.maxChunk(); n < 3500 || n > 4500 {
		t.Fatalf("6.5 Mbit/s: %d bytes, want about 4 KB (5 ms)", n)
	}
	p.bw = uint64(mbit(0.5))
	if n := p.maxChunk(); n != minRateChunk {
		t.Fatalf("0.5 Mbit/s: %d, want the minimum", n)
	}
	p.bw = uint64(mbit(200))
	if n := p.maxChunk(); n < s.opt.ChunkSize {
		t.Fatalf("200 Mbit/s: %d below the configured chunk size", n)
	}
}
