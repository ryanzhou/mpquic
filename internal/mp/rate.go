package mp

import (
	"fmt"
	"math"
	"os"
	"strings"
	"time"
)

// Session rate: one congestion controller for all paths of a session.
//
// The paths of a session are not independent. Every one leaves through the
// sending endpoint's uplink and arrives through the receiving endpoint's
// downlink, and those are often slower than the relays. A controller per
// path then has each path probe for the whole line: their estimates add up
// to more than it carries, each starts up on its own, and none of them
// knows what the others lose.
//
// So the session has one rate, and the paths have none of their own: a
// path's controller paces at the share it is given and bounds what it has in
// flight (cc.follower). The session works in rounds of the longest round
// trip. Per round it knows what was sent, and of that what was delivered and
// lost (every acknowledgement and loss is counted for the round its packet
// was sent in), what the pipe put out meanwhile, and whether the paths saw
// queues. From that it decides the session rate B:
//
//   - Start-up: the rate is twice the rate at which acknowledgements
//     arrive, until that stops growing, or loss or a queue shows.
//   - After that the session sends at B. Every few rounds one round probes
//     at 1.25 B, and the next one sends as much less, so that what the probe
//     queued is gone again. B goes up if at least half of what the probe
//     added was delivered; otherwise the next probe is smaller.
//   - Loss counts when it comes with the rate: beyond the loss a path also
//     has when less is sent (its baseline; the round after a probe shows
//     it), and on every path. Two such rounds at B: B is what the pipe puts
//     out, a little less, and probes become rare. A policer or a shallow
//     buffer is followed this way; random loss is not mistaken for either.
//   - A queue that every path sees: B follows what the pipe puts out if that
//     is less, and a queue that stays is drained.
//
// B is shared out equally, except that a path gets no more than it has
// shown to carry on its own (a slow relay): when a path alone loses or
// queues, that is its limit, tried again for more now and then. The
// scheduler decides what each path carries of the streams.
//
// docs/design/congestion.md has the reasons. With Options.NoCoupling every
// path runs a BBR of its own instead.

const (
	cpMinRound = 50 * time.Millisecond
	cpMaxRound = time.Second
	cpRounds   = 10 // rounds of the delivery-rate maximum

	cpStartGain   = 2.0
	cpStartGrowth = 1.25 // delivery grows by this much per round, or start-up is over ...
	cpStartStall  = 2    // ... after this many rounds without
	cpStartKept   = 0.9  // ... or at once, if less than this much of what was sent came out

	cpProbeGain    = 1.25
	cpMaxProbeGain = 2.0
	cpMinProbeGain = 1.06
	cpSmallProbes  = 3   // probes at the least gain before a full one again
	cpProbeShow    = 0.5 // share of a probe's gain that must show in the delivery rate
	cpDrainGain    = 0.75
	cpMaxBackoff   = 3 // probes at least every 2^3 rounds

	cpLossExcess  = 0.02 // loss beyond the baseline that counts
	cpBurst       = 4    // random loss comes in bursts of about this many packets
	cpStartLoss   = 0.05 // loss beyond the baselines that ends a start-up ...
	cpStartPkts   = 40   // ... if it is this many packets (one burst of random loss is not)
	cpMargin      = 0.98 // B after such loss: this much of what was delivered
	cpProbeMargin = 0.96 // ... or of what a start-up's lossy last round delivered
	cpLossRounds  = 2    // rounds in a row with it at B before B comes down ...
	cpProbation   = 4    // rounds without loss after which a higher B is taken to hold
	cpBaseKeep    = 0.75 // a baseline: what an older calm round counts for after each newer one
	cpBasePkts    = 200  // ... and the packets it takes to know one
	cpCommon      = 0.75 // "every path": the paths that lost carried this much of the round
	cpHold        = 16   // rounds without a probe after that, doubled each time it happens at about the same B ...
	cpMaxHold     = 128  // ... up to this
	cpSameCeil    = 0.15 // "about the same": within this much

	cpQueueTarget = 3 * time.Millisecond // a queue: every path's lowest RTT of the round is this much above its base
	cpQueueTicks  = 2                    // ... or, within a round, its latest RTT is clearly above it at this many ticks in a row
	cpHoldQ       = 0.9                  // with a queue, B follows a delivery rate below this much of it
	cpTrim        = 0.98                 // ... and over cpTrimRounds queued rounds, below this much
	cpTrimRounds  = 3
	cpDrainEvery  = 8 // rounds between drains of a queue that stays
	cpMaxDrain    = 8 // ... and the rounds one drain takes at most
	cpSatRounds   = 3

	cpLoadedBytes = 10 * 1200 // a path carried data in a round: at least this much
	cpBusyFrac    = 0.85      // the session was busy: it sent this much of what it could
	cpThinFrac    = 0.5       // ... and it has little to send: less than this much
	cpKept        = 0.95      // a round came out: this much of it is acknowledged a round later
	cpBaseWindow  = 30 * time.Second

	cpMinShare    = 64 << 10              // the least a path's share is (bytes/s), where there is that much to share
	cpStartWindow = 32 * 1200             // start-up begins with this much per path and round trip
	cpStartRTT    = 50 * time.Millisecond // ... taken to be this long until it is measured

	cpPathRounds  = 2 // rounds with a queue of its own after which a path has a limit
	cpPathProbe   = 8 // rounds between a limited path's tries for more (doubling when they fail)
	cpPathBackoff = 1 // ... up to twice
)

// MPQUIC_COUPLE=0 gives every path a BBR of its own (like
// Options.NoCoupling), for comparisons; MPQUIC_RATE_DEBUG=1 logs every round.
var cpDisabled = os.Getenv("MPQUIC_COUPLE") == "0"
var cpDebug = os.Getenv("MPQUIC_RATE_DEBUG") != ""

type cpPhase int

const (
	cpStartup cpPhase = iota
	cpCruise
	cpProbe // sending above B for a round
	cpDrain // sending below B for a round
)

func (p cpPhase) String() string {
	return [...]string{"startup", "cruise", "probe", "drain"}[p]
}

type cpAck struct {
	at time.Time
	n  uint64
}

type cpSample struct {
	round int64
	v     float64
}

// cpRound is what the session remembers of a round.
type cpRound struct {
	phase cpPhase
	gain  float64
	dt    time.Duration
	busy  bool    // the session was not waiting for data: it sent what it could, or the paths took no more
	full  bool    // ... and it did send what it could: what the round delivered and lost says what the pipe takes
	queue bool    // a path saw a queue
	allq  bool    // ... every path that carried data did
	out   float64 // the rate at which acknowledgements came in during it (bytes/s)
	thin  bool    // the session sent less than half of what it could: it has little to send
}

// coupling is the session's part (sess.mu held).
type coupling struct {
	roundAt time.Time
	roundNo int64 // the round under way
	hist    [8]cpRound

	bw     float64    // B (bytes/s)
	filter []cpSample // monotone deque: windowed maximum of the delivery rate
	phase  cpPhase
	gain   float64

	best      float64 // start-up: the delivery rate that the next round must exceed
	stall     int     // ... and the rounds that did not
	startOver bool    // ... and it is over: loss or a queue showed within the round
	startLoss bool    // ... (loss)
	acks      []cpAck // the bytes acknowledged as of the last ticks (a round's worth)
	qTicks    int     // ticks in a row at which every path saw a queue
	prevOut   float64 // the rate at which acknowledgements came in during the last round

	raised    int64   // a probe found more at the end of this round (-1: not lately) ...
	clean     int     // ... and rounds at the new B without loss since
	lossN     int     // rounds in a row at B with loss beyond the baseline on every path
	wantDrain bool    // a round below B is due, for the baseline
	cutRound  int64   // B came down for loss at the end of this round
	hold      int64   // rounds without a probe after that (0: not held)
	ceil      float64 // the B at which loss last stopped it ...
	ceilHold  int64   // ... and the hold that followed

	probeGain float64
	probeBw   float64
	probeLeft int  // rounds of the probe still to come
	small     int  // probes at the least gain in a row that found nothing
	restart   bool // a start-up is due: two probes in a row found clear room
	backoff   int
	streak    int
	nextProbe int64

	congested bool // last round: every loaded path saw a queue
	queued    int  // rounds in a row
	sat       [cpSatRounds]float64
	satN      int
	trimN     int
	trimSum   float64
	drainQ    time.Duration // a drain for a queue is under way: the queue before its last round
	drainN    int           // ... and the rounds it has taken
	dropped   bool          // B came down because the capacity dropped: what queued meanwhile must go
	lastDrain int64

	peerLimit uint64 // the peer's maximum receive rate (bytes/s), 0 = none
}

// cpPath is the per-path part (sess.mu held).
type cpPath struct {
	rttMin time.Duration // lowest RTT sample of the round
	// base RTT: the lowest sample of the last cpBaseWindow/2..cpBaseWindow
	// (two buckets). The connection's lifetime minimum would make a path
	// whose route got longer look queued forever.
	base   [2]time.Duration
	baseAt time.Time

	acked0  uint64  // bytes acknowledged when the round began
	rate    float64 // the rate at which acknowledgements came in during the last round (bytes/s)
	rate1   float64 // the rate at which what it sent in the last round was delivered, so far
	rate2   float64 // the rate at which what it sent in the round before last was delivered
	sent    float64 // how fast it sent in the round before that (bytes/s)
	lossy   bool    // the round before that lost more than the path's baseline
	sent2   uint64  // ... of this much sent
	lost2   uint64
	full    bool    // it sent its share in the last round, or as much as its window let it
	blocked bool    // ... its window held it back in the last round
	prev    float64 // the rate at which acknowledgements came in during the round before

	// lossBase is the share the path loses whatever the rate (random
	// loss), as the rounds that send less than B show it.
	lossBase     float64
	lossOK       bool
	calmN, calmX float64 // packets sent and lost in the calm rounds (older ones fading)

	// limit is what the path carries on its own account (bytes/s, 0: no
	// limit known): it alone queued or lost at more.
	limit     float64
	short     int     // rounds in a row in which it did
	drainAt   int64   // the round in which its queue is drained (after the limit was found)
	tryAt     int64   // the round in which it is tried for more (0: no try under way)
	tryGain   float64 // ... and for how much more (times its limit)
	tryFull   bool    // ... and whether it sent what it could in it
	limitNext int64   // round of the next try
	limitBack int

	cap   uint64 // the share it paces at (bytes/s)
	rawBw uint64 // its own limit, for the stats (bytes/s, 0: none)
}

// cpPkt is the packet size the loss statistics reckon with.
const cpPkt = 1200

// cpExcess reports whether lost of sent bytes is more than the share base
// by cpLossExcess/div, and by more than chance explains: three standard
// deviations, of losses that come in bursts of cpBurst packets (random loss
// on real links does; counting every packet as its own draw would take
// every burst for the pipe's doing).
func cpExcess(sent, lost uint64, base, div float64) bool {
	n, x := float64(sent)/cpPkt, float64(lost)/cpPkt
	d := x - n*base
	return d >= cpLossExcess/div*n && d >= 3*math.Sqrt(cpBurst*(n*base+1))
}

// learn updates the path's baseline with a round's loss. A calm round (one
// that sent clearly less than the pipe takes) shows what the path loses
// anyway; another round can only show that it is less. The baseline is the
// loss of the calm rounds together, the later ones counting more: one round
// of a few dozen packets says little (two lost of a hundred is as likely as
// none), and a baseline of nothing from such a round would make every later
// loss look like the pipe's.
func (c *cpPath) learn(sent, lost uint64, calm bool) {
	n, x := float64(sent)/cpPkt, float64(lost)/cpPkt
	if !calm && !(c.lossOK && n*c.lossBase-x >= 2*math.Sqrt(n*c.lossBase+1)) {
		return
	}
	c.calmN, c.calmX = c.calmN*cpBaseKeep+n, c.calmX*cpBaseKeep+x
	c.lossBase, c.lossOK = c.calmX/c.calmN, c.calmN >= cpBasePkts
}

// limitAt is the path's own limit in a round (0: none): the limit, or more
// and then less of it while the path is tried for more.
func (c *cpPath) limitAt(round int64) float64 {
	switch {
	case c.drainAt > 0 && round == c.drainAt:
		return c.limit / 2
	case c.tryAt > 0 && round == c.tryAt:
		return c.limit * c.tryGain
	case c.tryAt > 0 && round == c.tryAt+1:
		return c.limit * max(2-c.tryGain, 0.5)
	}
	return c.limit
}

func (c *cpPath) sampleRTT(now time.Time, rtt time.Duration) {
	if rtt <= 0 {
		return
	}
	switch {
	case now.Sub(c.baseAt) >= cpBaseWindow/2:
		c.base[1], c.base[0], c.baseAt = c.base[0], rtt, now
	case rtt < c.base[0]:
		c.base[0] = rtt
	}
	if c.rttMin == 0 || rtt < c.rttMin {
		c.rttMin = rtt
	}
}

func (c *cpPath) baseRTT() time.Duration {
	if c.base[1] > 0 && c.base[1] < c.base[0] {
		return c.base[1]
	}
	return c.base[0]
}

// queue is the queueing delay the path saw throughout the round.
func (c *cpPath) queue() time.Duration {
	if base := c.baseRTT(); base > 0 && c.rttMin > base {
		return c.rttMin - base
	}
	return 0
}

// rateLimitLocked is the configured ceiling of the session's send rate
// (bytes/s, 0 = none): our own limit and what the peer wants to receive.
func (s *Session) rateLimitLocked() uint64 {
	l := s.opt.MaxSendRate
	if p := s.cp.peerLimit; p > 0 && (l == 0 || p < l) {
		l = p
	}
	return l
}

func (s *Session) coupled() bool { return !cpDisabled && !s.opt.NoCoupling }

// couplingTickLocked updates the session rate and the paths' shares of it.
func (s *Session) couplingTickLocked(now time.Time) {
	limit := s.rateLimitLocked()
	if !s.coupled() {
		// Every path has a BBR of its own. A configured limit is split
		// equally among them.
		n := 0
		for _, p := range s.paths {
			if p.meter != nil && p.state == stateActive {
				n++
			}
		}
		for _, p := range s.paths {
			if p.meter == nil {
				continue
			}
			c := uint64(0)
			if limit > 0 && n > 0 {
				c = limit / uint64(n)
			}
			if p.cps.cap != c {
				p.cps.cap = c
				p.meter.SetRateCap(c)
			}
		}
		return
	}
	cp := &s.cp
	round := cpMinRound
	for _, p := range s.paths {
		if p.meter == nil || p.state == stateDead {
			continue
		}
		p.cps.sampleRTT(now, p.latestRTT)
		if p.state == stateActive {
			// (not longer than twice the base RTT: when a queue builds, the
			// session must not react more slowly for it)
			round = max(round, min(p.srtt, 2*p.cps.baseRTT()))
		}
	}
	if dt := now.Sub(cp.roundAt); cp.roundAt.IsZero() {
		cp.gain, cp.raised, cp.cutRound = cpStartGain, -1, -1
		s.cpStartRoundLocked(now)
	} else if dt >= min(round, cpMaxRound) {
		s.cpRoundLocked(now, dt, limit)
		s.cpStartRoundLocked(now)
	} else {
		s.cpTickLocked(now, min(round, cpMaxRound))
	}
	s.cpShareLocked(limit)
}

func (s *Session) cpStartRoundLocked(now time.Time) {
	cp := &s.cp
	cp.roundAt = now
	for _, p := range s.paths {
		if p.meter == nil {
			continue
		}
		p.cps.rttMin = 0
		p.cps.acked0 = p.meter.Acked()
		// Packets sent from now on are counted for this round, and what
		// becomes of them too (cc.Meter.RoundStats).
		p.meter.StartRound(cp.roundNo)
	}
}

// cpTickLocked is what cannot wait for the end of a round.
//
// In the start-up the rate is twice what is being acknowledged, so it
// follows that at once and doubles every round trip (TCP's slow start does
// the same with its window). And the start-up is over, without waiting for
// the round to end, as soon as it has gone too far: the last two rounds
// lose more than the paths' baselines on every path, or every path sees a
// queue. From then on it sends no more than is delivered.
//
// And when the capacity drops under a session that sends at B (every path
// sees a queue, and clearly less than B comes out), B follows at once: a
// round at the old rate may be more than the bottleneck's buffer holds.
func (s *Session) cpTickLocked(now time.Time, w time.Duration) {
	cp := &s.cp
	k := cp.roundNo
	var total, known, lost, lossy uint64
	var expected, allowed, used float64
	loaded, queued := 0, 0
	el := now.Sub(cp.roundAt).Seconds()
	for _, p := range s.paths {
		if p.meter == nil || p.state == stateDead {
			continue
		}
		c := &p.cps
		total += p.meter.Acked()
		t0, t1 := p.meter.RoundStats(k), p.meter.RoundStats(k-1)
		if p.usable() {
			share := float64(c.cap) * el
			allowed += share
			if p.meter.Blocked() {
				used += share
			} else {
				used += min(float64(t0.Sent), share)
			}
		}
		pk, pl := t0.Acked+t0.Lost+t1.Acked+t1.Lost, t0.Lost+t1.Lost
		known, lost, expected = known+pk, lost+pl, expected+float64(pk)*c.lossBase
		if c.lossOK && cpExcess(pk, pl, c.lossBase, 8) {
			lossy += pk
		}
		if base := c.baseRTT(); t0.Sent+t1.Sent >= cpLoadedBytes && base > 0 {
			loaded++
			if p.latestRTT >= base+max(cpQueueTarget, base/4) {
				queued++
			}
		}
	}
	if loaded > 0 && queued == loaded {
		cp.qTicks++
	} else {
		cp.qTicks = 0
	}
	if n := len(cp.acks); n > 0 && total < cp.acks[n-1].n {
		cp.acks = cp.acks[:0] // a path went away
	}
	cp.acks = append(cp.acks, cpAck{now, total})
	i := 0
	for i+1 < len(cp.acks) && now.Sub(cp.acks[i+1].at) >= w {
		i++
	}
	cp.acks = append(cp.acks[:0], cp.acks[i:]...)
	d := now.Sub(cp.acks[0].at)
	if d < w {
		return
	}
	r := float64(total-cp.acks[0].n) / d.Seconds()
	// A queue or loss only says something about B while the session sends
	// at B. A queue may be anybody's: the other direction's, for one, which
	// delays this direction's acknowledgements just the same. A session
	// with little to send (the acknowledgements of a download) must not
	// take it for its own, or end its start-up for a few lost packets.
	r1 := &cp.hist[((k-1)%8+8)%8]
	busy := r1.busy && el >= w.Seconds()/4 && used >= cpBusyFrac*allowed
	switch {
	case cp.phase == cpStartup:
		if r > cp.bw {
			cp.bw = r
			cp.pushLocked(r)
		}
		if cp.gain <= 1 {
			break
		}
		if !r1.thin && known > 0 && float64(lossy) >= cpCommon*float64(known) &&
			float64(lost)-expected >= max(cpStartLoss*float64(known), cpStartPkts*cpPkt) && cpExcess(known, lost, expected/float64(known), 1) {
			cp.gain, cp.startOver, cp.startLoss = 1, true, true
		} else if busy && cp.qTicks >= cpQueueTicks {
			cp.gain, cp.startOver = 1, true
		}
	case cp.gain >= 1 && r1.gain >= 1 && r1.phase != cpStartup && busy && el >= w.Seconds()/2:
		if cp.qTicks >= cpQueueTicks && r < cpHoldQ*cp.bw {
			cp.setLocked(r)
			cp.satN, cp.trimN, cp.trimSum, cp.dropped = 0, 0, 0, true
		}
	}
}

func (cp *coupling) pushLocked(v float64) {
	for len(cp.filter) > 0 && cp.filter[len(cp.filter)-1].v <= v {
		cp.filter = cp.filter[:len(cp.filter)-1]
	}
	cp.filter = append(cp.filter, cpSample{cp.roundNo, v})
}

func (cp *coupling) setLocked(v float64) {
	cp.filter = append(cp.filter[:0], cpSample{cp.roundNo, v})
	cp.bw = v
}

// cpRoundLocked ends round k of length dt. What became of the packets of
// round k-1 is known by now (their acknowledgements took a round trip), and
// of round k-2 for good, the losses included: the session rate follows, and
// the next round's phase is chosen.
func (s *Session) cpRoundLocked(now time.Time, dt time.Duration, limit uint64) {
	cp := &s.cp
	k := cp.roundNo
	h := func(r int64) *cpRound { return &cp.hist[((r%8)+8)%8] }
	// A queue that all paths pass delays them all by the same amount, so
	// one threshold serves: a tenth of the shortest base RTT.
	target := time.Duration(0)
	for _, p := range s.paths {
		if b := p.cps.baseRTT(); p.state == stateActive && b > 0 && (target == 0 || b/10 < target) {
			target = b / 10
		}
	}
	target = max(target, cpQueueTarget)
	r1, r2 := h(k-1), h(k-2)
	// All paths: what rounds k-1 and k-2 sent, of that what was
	// acknowledged and lost, and what the baselines expected to be lost.
	var s0, s1, a1, l1, s2, a2, l2 uint64
	var e1, e2 float64
	var allowed, used, usedRaw float64 // round k: what the paths could send, and did
	var rate, rate2 float64            // the rate at which rounds k-1 (so far) and k-2 were delivered
	var out float64                    // the rate at which acknowledgements came in during round k
	loaded, queued := 0, 0
	known1 := true               // the baselines of the paths that sent in round k-1 are known
	var lossy1, lossy2 uint64    // ... and what the paths sent that lost more than their baseline
	commonQ := time.Duration(-1) // the queue all loaded paths saw
	for _, p := range s.paths {
		c := &p.cps
		if p.meter == nil || p.state == stateDead {
			continue
		}
		t1, t2, t3 := p.meter.RoundStats(k-1), p.meter.RoundStats(k-2), p.meter.RoundStats(k-3)
		ps0 := p.meter.RoundStats(k).Sent
		s0 += ps0
		ps1, pa1, pl1 := t1.Sent, t1.Acked, t1.Lost
		ps2, pa2, pl2 := t2.Sent, t2.Acked, t2.Lost
		blocked := p.meter.TakeBlocked()
		c.full, c.blocked = false, blocked
		if p.usable() {
			share := float64(c.cap) * dt.Seconds()
			allowed += share
			usedRaw += min(float64(ps0), share)
			if blocked { // it has all in flight that it may: not waiting for data
				used += share
			} else {
				used += min(float64(ps0), share)
			}
			c.full = blocked || float64(ps0) >= cpBusyFrac*share
		}
		c.sent, c.lossy = 0, false
		if r1.dt > 0 {
			c.sent = float64(ps1) / r1.dt.Seconds()
		}
		c.rate1, c.rate2 = t1.Rate(t2, r1.dt), t2.Rate(t3, r2.dt)
		rate += c.rate1
		rate2 += c.rate2
		// What the path put out during the round, whatever round it was
		// sent in.
		c.prev = c.rate
		if acked := p.meter.Acked(); acked >= c.acked0 {
			c.rate = float64(acked-c.acked0) / dt.Seconds()
			out += c.rate
		}
		if ps1 >= cpLoadedBytes {
			known1 = known1 && c.lossOK
			s1, a1, l1, e1 = s1+ps1, a1+pa1, l1+pl1, e1+float64(ps1)*c.lossBase
			if cpExcess(ps1, pl1, c.lossBase, 8) {
				lossy1 += ps1
			}
		}
		if ps2 >= cpLoadedBytes {
			if c.lossy = cpExcess(ps2, pl2, c.lossBase, 8); c.lossy {
				lossy2 += ps2
			}
			s2, a2, l2, e2 = s2+ps2, a2+pa2, l2+pl2, e2+float64(ps2)*c.lossBase
		}
		c.sent2, c.lost2 = ps2, pl2
		if ps0 >= cpLoadedBytes {
			loaded++
			q := c.queue()
			if q >= target {
				queued++
			}
			if commonQ < 0 || q < commonQ {
				commonQ = q
			}
		}
	}
	commonQ = max(commonQ, 0)
	// The baselines: a round shows what a path loses anyway if it sent
	// clearly less than comes out now, two rounds later, of the pipe and
	// of the path itself (the round after a probe; a start-up's rounds
	// while the rate still grows). A round below B is not enough, nor one
	// below what the pipe put out before: after the capacity dropped,
	// three quarters of B may still be twice what the pipe takes, and what
	// is lost then is not lost anyway. Nor is what a path loses while its
	// own relay takes less than its share.
	below := func(r *cpRound, sent uint64, than float64) bool {
		return r.dt > 0 && float64(sent)/r.dt.Seconds() <= cpHoldQ*than
	}
	calm := below(r2, s2, out)
	for _, p := range s.paths {
		if c := &p.cps; p.meter != nil && p.state != stateDead && c.sent2 >= cpLoadedBytes {
			c.learn(c.sent2, c.lost2, calm && below(r2, c.sent2, c.rate))
		}
	}
	busy := allowed > 0 && used >= cpBusyFrac*allowed
	*h(k) = cpRound{cp.phase, cp.gain, dt, busy, allowed > 0 && usedRaw >= cpBusyFrac*allowed, queued > 0, loaded > 0 && queued == loaded, out, used < cpThinFrac*allowed}

	// Round k-1: what it delivered (nearly all of it is acknowledged by
	// now, unless a queue is growing) and lost so far. Round k-2: its
	// losses are final. A round is lossy if the paths lost more of it than
	// their baselines, all of them: that is not a path's own trouble. (All
	// but a small one, or one whose packets passed before the loss began.)
	var sentPrev, kept float64
	ok1, ok2 := s1 >= 4*cpLoadedBytes, s2 >= 4*cpLoadedBytes
	if r1.dt > 0 {
		sentPrev = float64(s1) / r1.dt.Seconds()
	}
	if ok1 {
		kept = rate / sentPrev
	}
	common1 := ok1 && float64(lossy1) >= cpCommon*float64(s1) && cpExcess(s1, l1, e1/float64(max(s1, 1)), 1)
	common2 := ok2 && float64(lossy2) >= cpCommon*float64(s2) && cpExcess(s2, l2, e2/float64(max(s2, 1)), 1)
	// What the pipe took, as a send rate: with random loss more is sent
	// than is delivered, and B is what is sent.
	sample, now1 := rate, out
	if ok1 {
		sample = rate / (1 - min(e1/float64(s1), 0.5))
		now1 = out / (1 - min(e1/float64(s1), 0.5))
	}
	sample2 := rate2
	if ok2 {
		sample2 = rate2 / (1 - min(e2/float64(s2), 0.5))
	}
	// What the pipe puts out now says what it takes if the session has
	// been sending at B for two rounds: the acknowledgements of a round
	// are for what was sent in it and in the one before.
	steady := busy && r1.busy && ok1 && cp.phase == cpCruise && r1.phase == cpCruise

	// The paths, each on its own account: a path that alone has a queue or
	// loses (a relay slower than its share) carries what it puts out then.
	// That is only known while the session sends steadily and the path
	// gets its share: after a start-up, or while the rate changes, a
	// path's output says little about what it can carry. Now and then the
	// path is tried for more the way the session probes: one round at 1.25
	// times its limit, the next at 0.75, and the limit goes up if at least
	// half of what the round added was delivered.
	raised := 0.0
	for _, p := range s.paths {
		c := &p.cps
		if p.meter == nil || p.state == stateDead {
			continue
		}
		// (its own trouble: a queue, or loss beyond its baseline, while
		// there are other paths, and they put out their shares)
		others := allowed/dt.Seconds() - float64(c.cap)
		own := (c.queue() >= max(target, c.baseRTT()/4) || c.lossy && c.lossOK) &&
			others > 0 && out-c.rate >= cpKept*others
		switch {
		case c.tryAt > 0:
			if k == c.tryAt {
				c.tryFull = c.full
			}
			if k < c.tryAt+2 {
				break // under way
			}
			if c.tryFull && c.rate2 >= c.limit*(1+cpProbeShow*(c.tryGain-1)) {
				// It carries more: again at once, and for twice as much
				// (its capacity came back). The session's rate goes up
				// with it: the path's share is all it was held to.
				was := c.limit
				c.limit = min(c.rate2, c.limit*c.tryGain)
				raised += c.limit - was
				c.limitBack, c.limitNext, c.tryGain = 0, k+1, cpMaxProbeGain
			} else {
				c.limitBack = min(c.limitBack+1, cpPathBackoff)
				c.limitNext, c.tryGain = k+cpPathProbe<<c.limitBack, cpProbeGain
			}
			c.tryAt = 0
		case cp.phase == cpStartup && c.blocked && own && c.prev > 0 && c.rate < 1.1*c.prev &&
			out-c.rate >= cpStartGrowth*(cp.prevOut-c.prev):
			// In the start-up every path gets twice as much every round. One
			// whose window holds it back, with a queue, and that puts out no
			// more than a round ago, while the others put out more and
			// more, is full: what it does not take of its share is the
			// others' to use.
			c.limit = max(cpMargin*c.rate, cpMinShare)
			c.short, c.limitNext, c.drainAt = 0, k+cpPathProbe<<c.limitBack, k+1
		case cp.phase != cpCruise || !busy || !c.full:
			// nothing to learn from this round
		case c.blocked && own && c.rate <= cpHoldQ*float64(c.cap),
			steady && others > 0 && (c.queue() >= commonQ+max(target, c.baseRTT()/4) || c.lossy && c.lossOK && !common2) &&
				c.rate <= cpTrim*float64(c.cap):
			// Its window holds it back, it has a queue or loses, and it
			// puts out clearly less than its share, while the others put
			// out theirs (a window alone is just a path that is waiting
			// for its acknowledgements); or it alone has
			// a queue or loses, and puts out less than its share. (A path
			// that puts out what it is given is not in trouble, whatever
			// its queue or loss looks like. And when all paths fall short,
			// it is the session's rate that is wrong, not a path.)
			need := cpPathRounds
			if c.blocked && own && c.rate <= 0.75*float64(c.cap) {
				need = 1 // far less: its share is wasted on it
			}
			if c.short++; c.short >= need {
				// A little less than it puts out, and half of that for a
				// round: its queue is still as long as its share made it.
				c.limit = max(cpMargin*c.rate, cpMinShare)
				c.short, c.limitNext, c.drainAt = 0, k+cpPathProbe<<c.limitBack, k+1
			}
		default:
			c.short = 0
			if c.limit > 0 && k >= c.limitNext && c.sent >= 0.9*c.limit {
				c.tryAt = k + 1
				c.tryGain = max(c.tryGain, cpProbeGain)
			}
		}
		c.rawBw = uint64(c.limit)
	}

	if raised > 0 && cp.phase != cpStartup {
		cp.bw += raised
		cp.pushLocked(cp.bw)
	}
	cp.congested = loaded > 0 && queued == loaded
	if cp.congested && cp.phase == cpCruise {
		cp.queued++
	} else if !cp.congested {
		cp.queued = 0
	}
	// A path with a limit of its own (a slow relay, found full just now or
	// before) does not say whether the pipe is full: what comes out of
	// what went in is judged on the others.
	if cp.phase == cpStartup {
		var in, got float64
		for _, p := range s.paths {
			if c := &p.cps; p.meter != nil && p.state != stateDead && c.limit == 0 {
				in, got = in+c.sent, got+c.rate1
			}
		}
		if in > 0 && in < sentPrev {
			kept = got / in
		}
	}
	was := cp.phase
	next := cp.phase
	// Loss stopped B here: there is a ceiling (a policer, a buffer), and
	// probing it costs. Probes become rare, and rarer each time the
	// ceiling turns out to be where it was.
	held := func() {
		if cp.ceil > 0 && math.Abs(cp.bw-cp.ceil) <= cpSameCeil*cp.ceil {
			cp.ceilHold = min(2*cp.ceilHold, cpMaxHold)
		} else {
			cp.ceilHold = cpHold
		}
		cp.ceil, cp.hold = cp.bw, cp.ceilHold
		cp.streak, cp.probeGain, cp.raised = 0, cpProbeGain, -1
		cp.nextProbe = k + cp.hold
	}
	if cp.phase == cpStartup {
		if sample > cp.bw {
			cp.pushLocked(sample)
			cp.bw = sample
		}
		early := cp.startLoss
		over := cp.startOver
		cp.startOver, cp.startLoss = false, false
		switch {
		case over:
		case r1.thin || !ok1:
			// A session with little to send (the acknowledgements of a
			// download) learns nothing about the pipe, whatever it loses.
		case common1 && known1 && float64(l1)-e1 >= max(cpStartLoss*float64(s1), cpStartPkts*cpPkt):
			// Loss on every path, clearly beyond what the paths lose anyway
			// (which the first rounds have shown): more than the pipe
			// takes.
			over = true
		case !r1.busy:
		case cp.bw >= cpStartGrowth*cp.best:
			cp.best, cp.stall = cp.bw, 0
		case kept < cpStartKept:
			// clearly less came out than went in (queued or lost)
			over = true
		case loaded > 0 && queued == loaded || r1.allq:
			// It did not grow, and every path has a queue: over after two
			// such rounds. (Otherwise it is not the pipe that holds the
			// rate back. The paths' windows open with what they deliver,
			// a round trip behind; and one path's queue is that path's: a
			// slow relay is full long before the others are.)
			cp.stall++
			over = cp.stall >= cpStartStall
		}
		if over {
			// B is what the pipe delivers now, not the most it ever did.
			cp.setLocked(min(cp.bw, max(now1, cp.best/cpStartGrowth)))
		}
		if over || limit > 0 && cp.bw >= float64(limit) {
			next = cpDrain
			cp.nextProbe = k + 3
			if common1 || early {
				// A start-up that ends in loss leaves B roughly right.
				// The first probe measures it.
				held()
				cp.nextProbe = k + cpProbation + 2
			}
		}
	} else {
		// The last round of the start-up lost too, and not to a queue
		// that was full: a policer. What that round delivered is what the
		// policer lets pass (the round before had its burst allowance on
		// top). Roughly a round later, better after two.
		switch {
		case was == cpDrain && r1.phase == cpStartup && common1 && r1.full && !r1.queue:
			cp.setLocked(min(cp.bw, sample))
			cp.ceil = cp.bw
		case r2.phase == cpStartup && r1.phase == cpDrain && common2 && r2.full && !r2.queue:
			cp.setLocked(min(cp.bw, cpProbeMargin*sample2))
			cp.ceil = cp.bw
		}
		// A probe, once all of it is accounted for (its last round is
		// k-2). More that it delivered counts if nothing was lost for it.
		if r2.phase == cpProbe && r1.phase != cpProbe {
			lost := ok2 && float64(lossy2) >= cpCommon*float64(s2) && cpExcess(s2, l2, e2/float64(s2), 4)
			switch {
			case !ok2 || !r2.busy:
				cp.nextProbe = k + 2 // there was not the data for it
			case lost && !r2.queue && !r1.queue:
				// The ceiling: B stays, probes become rarer, and the next
				// one asks for half as much more. (What the lossy probe
				// delivered does not say where the ceiling is: its packets
				// met the next round's at the policer, which were fewer,
				// so more of them passed than the policer's rate. If B is
				// below the ceiling, a smaller probe finds out without
				// loss.)
				g := max(1+(r2.gain-1)/2, cpMinProbeGain)
				held()
				cp.probeGain = g
			case !lost && sample2 >= cp.probeBw*(1+cpProbeShow*(r2.gain-1)):
				// There was more: again, and for more. Right away, unless
				// loss stopped B before: then the rounds at the new B must
				// show first that it holds.
				if v := min(sample2, cp.probeBw*r2.gain); v > cp.bw {
					cp.pushLocked(v)
				}
				cp.streak++
				cp.backoff, cp.nextProbe = 0, k
				cp.raised, cp.clean = k, 0
				cp.probeGain = cpProbeGain
				switch {
				case cp.hold > 0:
					cp.nextProbe = k + cpProbation + 2
				case r2.allq || r1.allq:
					// It was delivered, but it queued: the pipe is nearly
					// full, and a larger probe would mostly add to the
					// queue (which is latency for every stream, and for
					// the other direction's acknowledgements).
				case cp.streak >= 2:
					// Twice in a row, and no queue: B is far below what
					// the pipe takes (the capacity came back, or a slow
					// start ended early). A start-up finds it in a few
					// rounds, where probes take three rounds a step.
					cp.restart = true
				default:
					cp.probeGain = 1.5
				}
			default:
				// Less than half of what it added (or it overflowed a
				// queue): no more, or only a little. A probe's delivery is
				// not measured exactly (the
				// paths' shares of it reach a common queue at different
				// times), so B stays, and the next probe asks for half as
				// much more: a little more shows in a small probe.
				// (And sooner: a small probe queues little.) Every few
				// probes a full one again: where delivery is uneven
				// (other traffic in the same queue), a small probe proves
				// nothing either way, and B would stay where a dip put it.
				cp.streak = 0
				cp.probeGain = max(1+(r2.gain-1)/2, cpMinProbeGain)
				if r2.gain <= cpMinProbeGain {
					if cp.small++; cp.small >= cpSmallProbes {
						cp.probeGain, cp.small = cpProbeGain, 0
					}
				}
				cp.backoff = min(cp.backoff+1, cpMaxBackoff)
				wait := float64(int64(1)<<cp.backoff) * (cp.probeGain - 1) / (cpProbeGain - 1)
				cp.nextProbe = k + max(int64(wait), 1, cp.hold)
			}
		}
		// A round at B delivers B or less: B if what was sent came out (a
		// sample is not exact, and the highest of ten would creep up), and
		// less if it queued. (With loss, it may hold a policer's burst
		// allowance.)
		if steady && !common1 {
			if now1 >= cpKept*cp.bw {
				cp.pushLocked(cp.bw)
			} else {
				cp.pushLocked(now1)
			}
		}
		// Loss beyond the baseline at B, on every path, two rounds in a
		// row: B is too high (a policer, a buffer that is full), and what
		// the second round delivered is what the pipe takes. A round
		// below B follows, to see what loss is there anyway. (A round
		// below B that loses behind a queue on every path says the same:
		// the drain is not draining. Not right after a probe, whose
		// packets it met on the way, and not without the queue: a burst
		// of random loss in a drain's round would bring B down to what
		// the drain sent.)
		atB := r2.phase == cpCruise || r2.phase == cpDrain && r2.allq && h(k-3).gain <= 1
		switch {
		case !ok2 || !r2.busy || !atB || k-2 <= cp.cutRound:
		case common2:
			if cp.lossN++; cp.lossN >= cpLossRounds {
				v := sample2
				if steady {
					v = now1
				}
				cp.setLocked(min(cpMargin*v, cp.bw))
				held()
				cp.lossN, cp.cutRound, cp.wantDrain = 0, k, true
			}
		default:
			cp.lossN = 0
			if cp.raised >= 0 && k-2 > cp.raised {
				if cp.clean++; cp.clean >= cpProbation {
					cp.raised, cp.hold = -1, 0 // a probe found more, and it holds
				}
			}
		}
		for len(cp.filter) > 1 && cp.filter[0].round <= k-cpRounds {
			cp.filter = cp.filter[1:]
		}
		if len(cp.filter) > 0 {
			cp.bw = cp.filter[0].v
		}
		// A queue on every path. If clearly less than B was delivered (the
		// average of up to three such rounds), the capacity dropped and B
		// follows at once; a queue alone may not be ours. And if a little
		// less is put out than B for three such rounds, B is that.
		if cp.congested && steady {
			sample := now1
			cp.satN = min(cp.satN+1, cpSatRounds)
			cp.sat[int(k)%cpSatRounds] = sample
			var mean float64
			for i := 0; i < cp.satN; i++ {
				mean += cp.sat[(int(k)-i)%cpSatRounds] / float64(cp.satN)
			}
			cp.trimN++
			cp.trimSum += sample
			switch {
			case mean < cpHoldQ*cp.bw:
				cp.setLocked(mean)
				cp.trimN, cp.trimSum, cp.dropped = 0, 0, true
			case cp.trimN >= cpTrimRounds:
				if m := cp.trimSum / float64(cp.trimN); m < cpTrim*cp.bw {
					cp.setLocked(m)
				}
				cp.trimN, cp.trimSum = 0, 0
			}
		} else if !cp.congested {
			cp.satN, cp.trimN, cp.trimSum = 0, 0, 0
		}
	}
	if limit > 0 && cp.bw > float64(limit) {
		cp.setLocked(float64(limit))
	}
	if cp.hold == 0 && cp.bw > (1+cpSameCeil)*cp.ceil {
		// B is far above where loss stopped it once, and no loss holds
		// it back: that ceiling is gone, and one found there later is new.
		cp.ceil = 0
	}
	if cp.hold > 0 && cp.bw < (1-cpSameCeil)*cp.ceil {
		// B is far below where loss stopped it (the capacity dropped):
		// what was learned there does not hold probes back here.
		cp.hold, cp.nextProbe = 0, min(cp.nextProbe, k+2)
	}
	// The next round.
	atLimit := limit > 0 && cp.bw >= float64(limit)
	switch cp.phase {
	case cpStartup:
		// set above
	case cpProbe:
		// While the probe's ACKs come in, send as much less as the probe
		// sent more: what it queued is gone again by the time we know.
		if cp.probeLeft--; cp.probeLeft <= 0 {
			next = cpDrain
		}
	case cpDrain:
		next = cpCruise
		switch {
		case cp.drainQ == 0 || !cp.congested:
			// not a drain for a queue, or the queue is gone
		case cp.drainQ-commonQ < max(target, dt/16) && h(k).gain < 1 && below(h(k), s0, r1.out):
			// The queue is still there and the drain did not shrink it,
			// though it sent less than the pipe puts out: the queue is
			// not ours (the other direction's, or somebody else's
			// traffic). It becomes part of the base RTT.
			for _, p := range s.paths {
				if c := &p.cps; c.rttMin > 0 {
					c.base[0], c.base[1], c.baseAt = c.rttMin, c.rttMin, now
				}
			}
			cp.queued, cp.congested = 0, false
		case cp.drainN < cpMaxDrain:
			// It is ours, and still there: on with it. A queue that was a
			// buffer's worth (the capacity dropped under the session) is
			// not gone after one round at three quarters, and until it
			// is, it is latency for every stream, and for the other
			// direction's acknowledgements too.
			next, cp.drainN = cpDrain, cp.drainN+1
		}
		if next != cpDrain {
			if cp.dropped && cp.hold == 0 {
				// The capacity dropped. It may come back: a full probe,
				// soon.
				cp.probeGain, cp.backoff, cp.small = cpProbeGain, 0, 0
				cp.nextProbe = min(cp.nextProbe, k+3)
			}
			cp.drainQ, cp.dropped = 0, false
		} else {
			cp.drainQ = commonQ
		}
	case cpCruise:
		if !cp.congested {
			cp.dropped = false // nothing queued
		}
		switch {
		case !busy:
		case cp.wantDrain:
			next, cp.wantDrain = cpDrain, false
		case cp.congested && (cp.dropped || cp.queued >= 2 && k-cp.lastDrain >= cpDrainEvery):
			next, cp.drainQ, cp.lastDrain, cp.drainN = cpDrain, max(commonQ, 1), k, 1
		case !atLimit && cp.restart:
			next, cp.restart = cpStartup, false
			cp.best, cp.stall, cp.streak, cp.probeGain = cp.bw, 0, 0, cpProbeGain
		case !atLimit && k >= cp.nextProbe && cp.lossN == 0:
			next = cpProbe
			if cp.probeGain == 0 {
				cp.probeGain = cpProbeGain
			}
			cp.probeBw = cp.bw
			// Where loss stopped B before, a probe is two rounds: one does
			// not tell a policer's burst allowance from capacity.
			cp.probeLeft = 1
			if cp.hold > 0 {
				cp.probeLeft = 2
			}
		}
	}
	cp.prevOut = out
	cp.phase = next
	switch next {
	case cpStartup:
		cp.gain = cpStartGain
	case cpProbe:
		cp.gain = cp.probeGain
	case cpDrain:
		cp.gain = cpDrainGain
		if cp.drainQ > dt/2 {
			cp.gain = 0.5 // a long queue: half the rate, or it takes too long
		}
		if was == cpProbe {
			cp.gain = max(2-h(k).gain, 0.5)
			if h(k-1).phase == cpProbe {
				cp.gain = max(3-2*h(k).gain, 0.5)
			}
		} else if was == cpStartup {
			cp.gain = 1 / cpStartGain
		}
	default:
		cp.gain = 1
	}
	cp.roundNo++
	if cpDebug {
		var b strings.Builder
		for _, p := range s.paths {
			if p.meter != nil && p.state != stateDead {
				fmt.Fprintf(&b, " %d:%.1f/%.1f", p.id, p.cps.rate*8/1e6, p.cps.sent*8/1e6)
				if p.cps.limit > 0 {
					fmt.Fprintf(&b, "<%.1f", p.cps.limit*8/1e6)
				}
			}
		}
		pct := func(l, s uint64) float64 { return 100 * float64(l) / float64(max(s, 1)) }
		fmt.Fprintf(os.Stderr, "%s RATE dialer=%v round=%d %v %s: k-1 %s delivered %.1fM of %.1fM, lost %.1f%% (common=%v); k-2 lost %.1f%% (base %.1f%% common=%v); B=%.1fM busy=%v (%.0f%%) q=%d/%d hold=%d next=%s gain=%.2f%s\n",
			now.Format("15:04:05.000"), s.dialer, k, dt.Round(time.Millisecond), was, r1.phase, rate*8/1e6, sentPrev*8/1e6, pct(l1, s1), common1, pct(l2, s2), 100*e2/float64(max(s2, 1)), common2,
			cp.bw*8/1e6, busy, 100*used/max(allowed, 1), queued, loaded, cp.hold, cp.phase, cp.gain, b.String())
	}
}

// cpShareLocked gives every path its share of the session rate.
func (s *Session) cpShareLocked(limit uint64) {
	cp := &s.cp
	open := s.tmpPaths[:0]
	floor := 0.0 // what a start-up begins with
	for _, p := range s.paths {
		if p.meter == nil || p.state == stateDead {
			continue
		}
		if !p.usable() {
			s.setShareLocked(p, cpMinShare) // enough for its pings
			continue
		}
		open = append(open, p)
		// The path's shortest round trip: the smoothed one begins with
		// the handshake's, which may be many times the path's. (The
		// window bounds what is in flight either way.)
		rtt := p.minRTT
		if rtt <= 0 {
			rtt = p.srtt
		}
		if rtt <= 0 {
			rtt = cpStartRTT // not measured yet
		}
		floor += cpStartWindow / max(rtt, 10*time.Millisecond).Seconds()
	}
	n := len(open)
	total := cp.bw
	if cp.phase == cpStartup {
		// (twice the initial window per round trip: the paths' windows
		// bound what is in flight, and a first flight that is paced over
		// a whole round trip arrives a round trip late)
		total = max(total, floor)
	}
	total *= cp.gain
	// A path that is tried for more gets it on top (and gives it back the
	// round after): the session's rate is the sum of what the paths were
	// held to, and has nothing to try with.
	for _, p := range open {
		if c := &p.cps; c.limit > 0 {
			total += c.limitAt(cp.roundNo) - c.limit
		}
	}
	if limit > 0 {
		total = min(total, float64(limit))
	}
	// The least a share is, unless the paths then add up to more than
	// there is to share (a slow line).
	minShare := min(cpMinShare, total/float64(max(n, 1)))
	// Max-min fair shares: a path whose own limit is below an equal share
	// gets that, and the others share the rest equally. Equal shares keep
	// every path loaded: a failing path leaves only its share to be taken
	// over, and loss recovery and rescues have capacity on every path.
	// (The scheduler still puts small transfers on the fastest path.)
	left := total
	for changed := true; changed && len(open) > 0; {
		changed = false
		fair := left / float64(len(open))
		k := 0
		for _, p := range open {
			if l := p.cps.limitAt(cp.roundNo); l > 0 && l <= fair {
				s.setShareLocked(p, uint64(max(l, minShare)))
				left -= l
				changed = true
			} else {
				open[k], k = p, k+1
			}
		}
		open = open[:k]
	}
	for _, p := range open {
		s.setShareLocked(p, uint64(max(left/float64(len(open)), minShare)))
	}
	clear(s.tmpPaths[:cap(s.tmpPaths)])
	s.tmpPaths = open[:0]
}

func (s *Session) setShareLocked(p *Path, c uint64) {
	p.meter.SetBaseRTT(p.cps.baseRTT())
	if p.cps.cap != c {
		p.cps.cap = c
		p.meter.SetRate(c)
	}
}

// RateStat is a snapshot of the session's rate control, for monitoring.
type RateStat struct {
	Rate      uint64 // session rate: what all paths deliver together (bytes/s), 0 = unknown
	Limit     uint64 // configured ceiling (bytes/s), 0 = none
	Congested bool   // every loaded path saw a queue in the last round
}

// Rate returns the session's rate control state.
func (s *Session) Rate() RateStat {
	s.mu.Lock()
	defer s.mu.Unlock()
	return RateStat{Rate: uint64(s.cp.bw), Limit: s.rateLimitLocked(), Congested: s.cp.congested}
}
