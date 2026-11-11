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
// flight (cc.follower). The session measures what all paths deliver
// together, in rounds of the longest round trip:
//
//   - Start-up: the rate is twice what has been delivered, until that stops
//     growing or a fifth of a round is lost.
//   - After that the rate is B, the highest delivery rate of the last ten
//     rounds. Every few rounds one round probes at 1.25 B, and the next one
//     sends as much less, so that what the probe queued is gone again.
//   - Loss counts when it comes with the rate: beyond the loss that is also
//     there when less is sent (the round after a probe shows that), on every
//     path. B is then what was delivered, a little less, and probes become
//     rare. More that a probe delivers only counts if it was not lost for.
//     A policer or a shallow buffer is followed this way; random loss is
//     not mistaken for either.
//   - A queue that every path sees for rounds on end is drained, and B
//     follows what is delivered if that is clearly less.
//
// B is shared out equally, except that a path gets no more than it has
// shown to carry on its own (a slow relay): when a path alone loses or
// queues, that is its limit, tried again for more now and then. The
// scheduler decides what each path carries of the stream.
//
// With Options.NoCoupling every path runs a BBR of its own instead.

const (
	cpMinRound = 50 * time.Millisecond
	cpMaxRound = time.Second
	cpRounds   = 10 // rounds of the delivery-rate maximum

	cpStartGain   = 2.0
	cpStartGrowth = 1.25 // delivery grows by this much per round, or start-up is over ...
	cpStartStall  = 2    // ... after this many rounds without
	cpStartLoss   = 0.2  // ... or with this share of a round lost beyond the baseline
	cpStartKept   = 0.9  // ... or at once, if less than this much of what was sent came out

	cpProbeGain    = 1.25
	cpMaxProbeGain = 2.0
	cpProbeShow    = 0.5 // share of a probe's gain that must show in the delivery rate
	cpDrainGain    = 0.75
	cpMaxBackoff   = 3 // probes at least every 2^3 rounds

	cpLossExcess = 0.02 // loss beyond the baseline that counts
	cpMargin     = 0.98 // B after such loss: this much of what was delivered
	cpLossRounds = 2    // rounds in a row with it at B before B comes down ...
	cpProbation  = 4    // rounds without loss after which a higher B is taken to hold
	cpCommon     = 0.75 // "every path": the paths that lost carried this much of the round
	cpHold       = 16   // rounds without a probe after that, doubled each time it happens at about the same B ...
	cpMaxHold    = 128  // ... up to this
	cpSameCeil   = 0.15 // "about the same": within this much

	cpQueueTarget = 3 * time.Millisecond // a queue: every path's lowest RTT of the round is this much above its base
	cpHoldQ       = 0.9                  // with a queue, B follows a delivery rate below this much of it
	cpTrim        = 0.98                 // ... and over ten queued rounds, below this much
	cpDrainEvery  = 8                    // rounds between drains of a queue that stays
	cpSatRounds   = 3

	cpLoadedBytes = 10 * 1200 // a path carried data in a round: at least this much
	cpBusyFrac    = 0.85      // the session was busy: it sent this much of what it could
	cpKept        = 0.95      // a round came out: this much of it is acknowledged a round later
	cpBaseWindow  = 30 * time.Second

	cpMinShare    = 64 << 10  // the least a path's share is (bytes/s), where there is that much to share
	cpStartWindow = 32 * 1200 // start-up begins with this much per path and round trip

	cpPathRounds = 2 // rounds with a queue of its own after which a path has a limit
	cpPathProbe  = 8 // rounds between a limited path's tries for more (doubling when they fail)
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
	busy  bool // the session was not waiting for data: it sent what it could, or the paths took no more
	full  bool // ... and it did send what it could: what the round delivered and lost says what the pipe takes
	queue bool // a path saw a queue
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
	acks      []cpAck // ... the bytes acknowledged as of the last ticks
	startOver bool    // ... and it is over: loss showed within the round

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
	probeLeft int // rounds of the probe still to come
	backoff   int
	streak    int
	nextProbe int64

	congested bool // last round: every loaded path saw a queue
	queued    int  // rounds in a row
	sat       [cpSatRounds]float64
	satN      int
	trimN     int
	trimSum   float64
	drainQ    time.Duration
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

	acked0 uint64  // bytes acknowledged when the round began
	rate   float64 // the rate at which acknowledgements came in during the last round (bytes/s)
	sent   float64 // how fast it sent in the round before that (bytes/s)
	lossy  bool    // the round before that lost more than the path's baseline

	// lossBase is the share the path loses whatever the rate (random
	// loss), as the rounds that send less than B show it.
	lossBase float64
	lossOK   bool

	// limit is what the path carries on its own account (bytes/s, 0: no
	// limit known): it alone queued or lost at more.
	limit     float64
	short     int   // rounds in a row in which it did
	trying    int64 // round until which a try for more is under way
	limitNext int64 // round of the next try
	limitBack int

	cap   uint64 // the share it paces at (bytes/s)
	rawBw uint64 // its own limit, for the stats (bytes/s, 0: none)
}

// cpPkt is the packet size the loss statistics reckon with.
const cpPkt = 1200

// cpExcess reports whether lost of sent bytes is more than the share base
// by cpLossExcess/div, and by more than chance explains.
func cpExcess(sent, lost uint64, base, div float64) bool {
	n, x := float64(sent)/cpPkt, float64(lost)/cpPkt
	d := x - n*base
	return d >= cpLossExcess/div*n && d >= 3*math.Sqrt(n*base+1)
}

// learn updates the path's baseline with a round's loss. A round below B
// (drain) shows it; another round can only show that it is less.
func (c *cpPath) learn(sent, lost uint64, drain bool) {
	loss := float64(lost) / float64(sent)
	n := float64(sent) / cpPkt
	switch {
	case drain && !c.lossOK:
		c.lossBase, c.lossOK = loss, true
	case drain:
		c.lossBase += (loss - c.lossBase) / 4
	case n*(c.lossBase-loss) >= 2*math.Sqrt(n*c.lossBase+1):
		c.lossBase += (loss - c.lossBase) / 4
	}
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
	} else if cp.phase == cpStartup {
		s.cpStartupTickLocked(now, min(round, cpMaxRound))
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

// cpStartupTickLocked is the start-up between the ends of rounds. The rate
// is twice what is being acknowledged, so it follows that at once and
// doubles every round trip (TCP's slow start does the same with its
// window). And the start-up must not wait for the end of a round to see
// that it went too far: a tenth of what the last two rounds are known to
// have delivered or lost being lost, it sends no more than was delivered.
func (s *Session) cpStartupTickLocked(now time.Time, w time.Duration) {
	cp := &s.cp
	k := cp.roundNo
	var total, known, lost, lossy uint64
	var expected float64
	for _, p := range s.paths {
		if p.meter == nil || p.state == stateDead {
			continue
		}
		total += p.meter.Acked()
		t0, t1 := p.meter.RoundStats(k), p.meter.RoundStats(k-1)
		pk, pl := t0.Acked+t0.Lost+t1.Acked+t1.Lost, t0.Lost+t1.Lost
		known, lost, expected = known+pk, lost+pl, expected+float64(pk)*p.cps.lossBase
		if cpExcess(pk, pl, p.cps.lossBase, 8) {
			lossy += pk
		}
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
	if d := now.Sub(cp.acks[0].at); d >= w {
		if r := float64(total-cp.acks[0].n) / d.Seconds(); r > cp.bw {
			cp.bw = r
			cp.pushLocked(r)
		}
	}
	if cp.gain > 1 && known > 0 && float64(lossy) >= cpCommon*float64(known) &&
		float64(lost)-expected >= cpStartLoss/2*float64(known) && cpExcess(known, lost, expected/float64(known), 1) {
		cp.gain, cp.startOver = 1, true
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
	var s1, a1, l1, s2, a2, l2 uint64
	var e1, e2 float64
	var allowed, used, usedRaw float64 // round k: what the paths could send, and did
	var rate, rate2 float64            // the rate at which rounds k-1 (so far) and k-2 were delivered
	var out float64                    // the rate at which acknowledgements came in during round k
	loaded, queued := 0, 0
	var lossy1, lossy2 uint64    // ... and what the paths sent that lost more than their baseline
	commonQ := time.Duration(-1) // the queue all loaded paths saw
	for _, p := range s.paths {
		c := &p.cps
		if p.meter == nil || p.state == stateDead {
			continue
		}
		t1, t2, t3 := p.meter.RoundStats(k-1), p.meter.RoundStats(k-2), p.meter.RoundStats(k-3)
		ps0 := p.meter.RoundStats(k).Sent
		ps1, pa1, pl1 := t1.Sent, t1.Acked, t1.Lost
		ps2, pa2, pl2 := t2.Sent, t2.Acked, t2.Lost
		blocked := p.meter.TakeBlocked()
		if p.usable() {
			share := float64(c.cap) * dt.Seconds()
			allowed += share
			usedRaw += min(float64(ps0), share)
			if blocked { // it has all in flight that it may: not waiting for data
				used += share
			} else {
				used += min(float64(ps0), share)
			}
		}
		c.sent, c.lossy = 0, false
		if r1.dt > 0 {
			c.sent = float64(ps1) / r1.dt.Seconds()
		}
		rate += t1.Rate(t2, r1.dt)
		rate2 += t2.Rate(t3, r2.dt)
		// What the path put out during the round, whatever round it was
		// sent in.
		if acked := p.meter.Acked(); acked >= c.acked0 {
			c.rate = float64(acked-c.acked0) / dt.Seconds()
			out += c.rate
		}
		if ps1 >= cpLoadedBytes {
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
			c.learn(ps2, pl2, r2.gain < 1)
		}
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
	busy := allowed > 0 && used >= cpBusyFrac*allowed
	*h(k) = cpRound{cp.phase, cp.gain, dt, busy, allowed > 0 && usedRaw >= cpBusyFrac*allowed, queued > 0}

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
	// loses (a relay slower than its share) carries what it delivers then.
	for _, p := range s.paths {
		c := &p.cps
		if p.meter == nil || p.state == stateDead || c.sent*r1.dt.Seconds() < cpLoadedBytes {
			continue
		}
		alone := c.queue() >= commonQ+max(target, c.baseRTT()/4) || c.lossy && !common2
		switch {
		case alone && cp.phase != cpStartup:
			if c.short++; c.short >= cpPathRounds {
				c.limit = max(c.rate, cpMinShare)
				if c.trying > 0 { // a try for more failed
					c.limitBack = min(c.limitBack+1, cpMaxBackoff)
				}
				c.trying, c.limitNext = 0, k+cpPathProbe<<c.limitBack
				c.short = 0
			}
		default:
			c.short = 0
			if c.trying > 0 && k >= c.trying {
				// the try for more went through: again soon
				c.trying, c.limitBack, c.limitNext = 0, 0, k+2
			}
		}
		if c.limit > 0 && c.trying == 0 && k >= c.limitNext && c.sent >= 0.9*c.limit {
			c.limit *= cpProbeGain
			c.trying = k + 3 // its result is known two rounds on
		}
		c.rawBw = uint64(c.limit)
	}

	cp.congested = loaded > 0 && queued == loaded
	if cp.congested && cp.phase == cpCruise {
		cp.queued++
	} else if !cp.congested {
		cp.queued = 0
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
		early := cp.startOver
		over := early
		cp.startOver = false
		switch {
		case over:
		case ok1 && common1 && float64(l1)-e1 >= cpStartLoss*float64(s1):
			// A fifth of what was sent is lost: this is beyond what the
			// pipe takes, whether or not there was data all the time (a
			// stream that waits for lost data has none).
			over = true
		case !r1.busy || !ok1:
		case cp.bw >= cpStartGrowth*cp.best:
			cp.best, cp.stall = cp.bw, 0
		default:
			// It did not grow: over after two such rounds, or at once if
			// clearly less came out than went in (queued or lost).
			cp.stall++
			over = cp.stall >= cpStartStall || kept < cpStartKept
		}
		if over {
			// B is what the pipe delivers now, not the most it ever did.
			cp.setLocked(min(cp.bw, max(now1, cp.best/cpStartGrowth)))
		}
		if over || limit > 0 && cp.bw >= float64(limit) {
			next = cpDrain
			cp.nextProbe = k + 3
			cp.acks = cp.acks[:0]
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
			cp.setLocked(min(cp.bw, cpMargin*sample2))
			cp.ceil = cp.bw
		}
		// A probe, once all of it is accounted for (its last round is
		// k-2). More that it delivered counts if nothing was lost for it.
		if r2.phase == cpProbe && r1.phase != cpProbe {
			lost := ok2 && float64(lossy2) >= cpCommon*float64(s2) && cpExcess(s2, l2, e2/float64(s2), 4)
			switch {
			case !ok2 || !r2.full:
				cp.nextProbe = k + 2 // there was not the data for it
			case lost:
				// The ceiling. The second round of a probe of two shows
				// where it is: the first has used up what a policer lets
				// pass in a burst.
				if h(k-3).phase == cpProbe && cpMargin*sample2 > cp.bw {
					cp.setLocked(min(cpMargin*sample2, cp.probeBw*r2.gain))
				}
				held()
			case sample2 >= cp.probeBw*(1+cpProbeShow*(r2.gain-1)):
				// There was more: again, and for more. Right away, unless
				// loss stopped B before: then the rounds at the new B must
				// show first that it holds.
				if sample2 > cp.bw {
					cp.pushLocked(sample2)
				}
				cp.streak++
				cp.backoff, cp.nextProbe = 0, k
				cp.raised, cp.clean = k, 0
				if cp.hold > 0 {
					cp.nextProbe = k + cpProbation + 2
				} else if cp.streak >= 2 {
					cp.probeGain = min(cp.probeGain*1.2, cpMaxProbeGain)
				}
			default:
				// Little or nothing more: B is what it delivered, if that
				// is more, and the next probe waits.
				if sample2 > cp.bw {
					cp.pushLocked(sample2)
				}
				cp.streak, cp.probeGain = 0, cpProbeGain
				cp.backoff = min(cp.backoff+1, cpMaxBackoff)
				cp.nextProbe = k + max(int64(1)<<cp.backoff, cp.hold)
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
		// below B follows, to see what loss is there anyway.
		switch {
		case !ok2 || !r2.full || r2.phase != cpCruise || k-2 <= cp.cutRound:
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
		// follows at once; a queue alone may not be ours. And a maximum of
		// noisy samples is a few percent high: over ten queued rounds B is
		// what was delivered if that is less.
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
				cp.trimN, cp.trimSum = 0, 0
			case cp.trimN >= cpRounds:
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
		if cp.queued > 0 && cp.drainQ > 0 && cp.drainQ-commonQ < max(target, dt/16) {
			// The queue is still there and the drain did not shrink it: it
			// is not ours (the other direction's, or somebody else's
			// traffic). It becomes part of the base RTT.
			for _, p := range s.paths {
				if c := &p.cps; c.rttMin > 0 {
					c.base[0], c.base[1], c.baseAt = c.rttMin, c.rttMin, now
				}
			}
			cp.queued, cp.congested = 0, false
		}
		cp.drainQ = 0
	case cpCruise:
		switch {
		case !busy:
		case cp.wantDrain:
			next, cp.wantDrain = cpDrain, false
		case cp.queued >= 2 && k-cp.lastDrain >= cpDrainEvery:
			next, cp.drainQ, cp.lastDrain = cpDrain, commonQ, k
		case !atLimit && k >= cp.nextProbe && cp.lossN == 0:
			next = cpProbe
			cp.probeGain = max(cp.probeGain, cpProbeGain)
			cp.probeBw = cp.bw
			// Where loss stopped B before, a probe is two rounds: one does
			// not tell a policer's burst allowance from capacity.
			cp.probeLeft = 1
			if cp.hold > 0 {
				cp.probeLeft = 2
			}
		}
	}
	cp.phase = next
	switch next {
	case cpStartup:
		cp.gain = cpStartGain
	case cpProbe:
		cp.gain = cp.probeGain
	case cpDrain:
		cp.gain = cpDrainGain
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
		floor += cpStartWindow / max(p.srttOrDefault(), 10*time.Millisecond).Seconds()
	}
	n := len(open)
	total := cp.bw
	if cp.phase == cpStartup {
		total = max(total, floor/cpStartGain)
	}
	total *= cp.gain
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
			if l := p.cps.limit; l > 0 && l <= fair {
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
