package mp

import (
	"fmt"
	"os"
	"strings"
	"time"

	"mpquic/internal/cc"
)

// Session rate: the paths of a session are congestion-controlled as one flow
//
// Each path runs its own BBR, which measures that path: its bottleneck rate
// and its round-trip time. That is all there is to it when the paths are
// independent routes. Usually they are not: every path leaves through the
// sending endpoint's uplink and arrives through the receiving endpoint's
// downlink, and those are often slower than the relays. Each path's BBR
// then takes the highest rate it has seen for itself as its bandwidth, and
// the paths saw their best rates at different moments: the estimates add up
// to more than the bottleneck carries. The paths together keep its buffer
// full (latency for every stream), and where it drops instead (a shallow
// buffer, a policer) they lose that excess all the time, which BBR ignores:
// every lost packet is retransmitted, often rescued on another path as
// well, and stalls the stream that waits for it. A bottleneck behind the
// relays makes them carry all of that for nothing.
//
// So the session measures what it delivers over all paths together, and the
// paths' estimates have to add up to that (the idea of MPTCP's coupled
// congestion control: at a common bottleneck the subflows together behave
// like one flow). Nothing needs to be detected: independent paths deliver
// the sum of their own rates, and then the session rate limits nobody.
// Like BBR, the session rate is a matter of delivery rates. Delay is only
// supporting evidence, because a round-trip time also grows with queues
// that are not ours (the other direction's, other people's), and loss is
// not judged at all, only what it leaves of the delivery rate.
//
//   - Rounds. The session works in rounds of the largest smoothed RTT of its
//     paths (50 ms to 1 s, at most twice a path's base RTT). The delivery
//     rate of a round is how fast the bytes sent in it came out at the
//     other end: per path, what was acknowledged in the round shifted by
//     that path's RTT (so it is known a round later), and never more than
//     was sent.
//   - Session rate B: the highest delivery rate seen. It stands while what
//     is sent is delivered (that only shows a lower bound, as do rounds in
//     which the session had no data for its paths). Rounds in which the
//     network kept or dropped part of what *every* path sent show what a
//     bottleneck that all of them pass carries: B is the maximum of the
//     last ten of those. That is all there is to loss, as in BBR: a
//     policer drops the same share from every path, and its rate is found
//     this way; one path's own loss, however heavy, is that path's
//     business.
//   - Shares. B is shared out max-min fairly: a path whose own estimate is
//     below an equal share keeps it (its own bottleneck limits it), the
//     others are capped at equal shares of the rest (bbr.Sender.RateCap).
//     Only for 32 rounds after the pipe was last seen full, though:
//     delivery has stopped growing although the network keeps part of what
//     every path sends, or every path sees a queue. A bottleneck that all
//     paths pass keeps showing that; independent paths do not (BBR's
//     estimate for a path with random loss is what it sends, not what it
//     delivers, so the estimates of lossy paths never add up to B), and
//     before the first time B only shows what was sent so far. A path in
//     STARTUP gets a share when the pipe is full; otherwise it is left to
//     find its rate.
//   - Probing. Capped paths cannot find more capacity alone (and do not
//     try: their own 1.25 phases are off), so the session probes on all of
//     them at once (bbr.Sender.RateGain): one round at 1.25 B, then one at
//     0.75 B while the ACKs come in, so that whatever the probe queued is
//     gone again. If the probe was delivered (B grew by half of what it
//     added), the session probes again right away, harder from the second
//     success in a row (1.5, then 2; one success may be a policer's burst
//     allowance). If not, it waits twice as long before the next probe: 2,
//     4, then every 8 rounds, as often as BBR itself probes. No path is
//     driven harder than its own probes would drive it: its share may be
//     all that its own bottleneck carries.
//   - Queues. When every path that carried data saw a queue for a whole
//     round (its lowest RTT sample above its base RTT), the bottleneck
//     they all pass is congested. One path's own queue does not count.
//     If such rounds also delivered clearly less than B (their average, of
//     up to 3, is below 0.9 B), the capacity dropped and B follows at
//     once. A queue with all of B delivered changes nothing: it may not be
//     ours, and following the delivery rate down for it would never end.
//     A maximum of noisy samples is a few percent high, though: while B
//     limits every path and all of them see a queue, B is trimmed to the
//     average of 10 rounds when that is clearly lower.
//     A queue that stays is drained by a round at 0.75 B, at most every 8
//     rounds. If that does not shrink it, it is not ours (the other
//     direction's, or other people's traffic), and becomes part of the
//     paths' base RTT.
//
// A configured limit (Options.MaxSendRate, or the peer's MaxRecvRate) is a
// ceiling for B that is never probed: with a known uplink or downlink
// rate, the session does not need to find it by filling it.

const (
	cpMinRound     = 50 * time.Millisecond
	cpMaxRound     = time.Second
	cpRounds       = 10                   // rounds the session's delivery rate is remembered for
	cpSatRounds    = 3                    // congested rounds whose delivery rate is averaged
	cpHold         = 0.9                  // ... and replaces B if below this share of it
	cpTrim         = 0.98                 // "clearly less": delivered below this share of what was sent, or of B
	cpQueueTarget  = 3 * time.Millisecond // a path sees a queue: this much (or a tenth of the shortest base RTT) above its base RTT
	cpLoadedBytes  = 10 * 1200            // a path carried data in a round: this much acknowledged
	cpIdleFrac     = 0.2                  // the session was busy: its paths waited for data less than this
	cpProbeGain    = 1.25
	cpMaxProbeGain = 2.0
	cpProbeShow    = 0.5 // share of a probe's gain that must show in the delivery rate
	cpDrainGain    = 0.75
	cpDrainEvery   = 8    // rounds between drains of a queue that stays
	cpShareRounds  = 32   // rounds for which B is shared out after the pipe was seen full
	cpGrowth       = 1.25 // delivery rate growth per round (session, STARTUP paths) below which a congested pipe is full
	cpMaxBackoff   = 3    // probes at least every 2^3 rounds, the cadence of BBR's own gain cycle
	cpBaseWindow   = 30 * time.Second
)

// MPQUIC_COUPLE=0 turns the session rate off, for comparisons (every
// path's controller is then on its own, and only a configured limit is
// shared out); MPQUIC_RATE_DEBUG=1 logs every round.
var cpDisabled = os.Getenv("MPQUIC_COUPLE") == "0"
var cpDebug = os.Getenv("MPQUIC_RATE_DEBUG") != ""

type cpPhase int

const (
	cpCruise  cpPhase = iota
	cpProbe           // sending above the session rate for a round
	cpObserve         // ... and as much below it for a round, while its ACKs come in
	cpDrain           // sending below the session rate for a round
	cpSettle          // ... and a round for its effect to show
)

func (p cpPhase) String() string {
	return [...]string{"cruise", "probe", "observe", "drain", "settle"}[p]
}

type cpSample struct {
	round int64
	v     float64
}

// coupling is the session's part (sess.mu held).
type coupling struct {
	roundAt   time.Time
	roundNo   int64                // rounds so far
	busyNo    int64                // rounds in which the session had data for its paths
	shortNo   int64                // ... and every path delivered clearly less than it sent
	ticks     int                  // (path, tick) samples of this round ...
	idleTicks int                  // ... in which the path was waiting for data
	benched   bool                 // ... a path sat out (ProbeRTT)
	filter    []cpSample           // monotone deque: windowed maximum of the delivery rate, by short round
	sat       [cpSatRounds]float64 // delivery rates of the last congested rounds (ring, by busy round)
	satN      int
	// busy rounds with every loaded path capped and seeing a queue, since B
	// last changed: their number, delivery rates and loss rates added up
	capN            int
	capSum, capLost float64
	bw              float64 // session rate B (bytes/s), 0 = unknown
	// the previous round: its length, what it sent (bytes/s), whether the
	// session was busy in it, and the delivery rate of the round before it
	prevDt    time.Duration
	prevSent  float64
	prevBusy  bool
	prevRate  float64
	congested bool // last round: every loaded path saw a queue or loss
	full      bool // ... and the delivery rate has stopped growing, also of every path in STARTUP
	// B is shared out until this round: the pipe was full not long ago
	sharedUntil int64
	queued      int // consecutive rounds in which every loaded path saw a queue
	phase       cpPhase
	gain        float64       // pacing gain of the capped paths in this round
	probeGain   float64       // gain of the next probe
	probeBw     float64       // B when the probe started
	drainQ      time.Duration // the common queue when the drain started
	lastDrain   int64         // round
	backoff     int
	streak      int    // successful probes in a row
	nextProbe   int64  // round
	peerLimit   uint64 // the peer's maximum receive rate (bytes/s), 0 = none
	active      int    // active paths at the last tick
}

// cpPath is the per-path part (sess.mu held).
type cpPath struct {
	started bool
	// counters at the start of the round
	acked, sent, lost uint64
	// acknowledged bytes as of the start of the round plus the path's RTT
	// (and when the last of them were acknowledged): what was delivered of
	// everything sent before the round
	markAcked uint64
	markAt    time.Duration
	marks     int
	rttMin    time.Duration // lowest RTT sample of the round
	// base RTT: the lowest sample of the last cpBaseWindow/2..cpBaseWindow
	// (two buckets). The connection's lifetime minimum would make a path
	// whose route got longer look queued forever.
	base   [2]time.Duration
	baseAt time.Time
	rate   float64 // delivery rate of the path's sends in the previous round (bytes/s)
	sent1  float64 // ... and how fast it sent them
	rawBw  uint64  // the path's own estimate (bytes/s)
	cap    uint64  // rate cap currently set (bytes/s), 0 = none
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

func (s *Session) coupled() bool { return !cpDisabled }

// couplingTickLocked updates the session rate and the paths' shares of it.
func (s *Session) couplingTickLocked(now time.Time) {
	limit := s.rateLimitLocked()
	if !s.coupled() && limit == 0 {
		for _, p := range s.paths { // a limit the peer has withdrawn
			if p.cps.cap != 0 && p.meter != nil {
				s.setCapLocked(p, 0)
			}
		}
		return
	}
	cp := &s.cp
	round := cpMinRound
	active := 0
	for _, p := range s.paths {
		if p.meter == nil || p.state == stateDead {
			continue
		}
		p.cps.sampleRTT(now, p.latestRTT)
		if p.state == stateActive {
			active++
			// (not longer than twice the base RTT: when a queue builds, the
			// session must not react more slowly for it)
			round = max(round, min(p.srtt, 2*p.cps.baseRTT()))
		}
		if p.usable() {
			cp.ticks++
			if !p.busy {
				cp.idleTicks++
			}
		} else if p.benched {
			cp.benched = true
		}
	}
	if s.coupled() {
		if dt := now.Sub(cp.roundAt); cp.roundAt.IsZero() {
			s.cpStartRoundLocked(now)
		} else if dt >= min(round, cpMaxRound) {
			s.cpRoundLocked(now, dt)
			s.cpStartRoundLocked(now)
		}
	}
	// A path that failed leaves its share to the others. Their own estimates
	// are about their old shares, so they restart STARTUP to take it over
	// within a few round trips.
	if active < cp.active {
		for _, p := range s.paths {
			if p.state == stateActive && p.cps.cap > 0 {
				p.meter.Restart()
			}
		}
	}
	cp.active = active
	s.cpShareLocked(limit)
}

func (s *Session) cpStartRoundLocked(now time.Time) {
	cp := &s.cp
	cp.roundAt, cp.ticks, cp.idleTicks, cp.benched = now, 0, 0, false
	for _, p := range s.paths {
		if p.meter == nil {
			continue
		}
		m := p.meter.Snapshot()
		c := &p.cps
		c.started, c.rttMin = true, 0
		c.acked, c.sent, c.lost = m.AckedBytes, m.SentBytes, m.LostBytes
		// What is sent before now has been acknowledged one RTT from now.
		p.meter.Mark(cc.Mono(now) + p.srtt)
	}
}

// cpRoundLocked ends a round of length dt: the delivery rate of the round
// before it, the congestion verdict, the session rate and the next round's
// gain.
func (s *Session) cpRoundLocked(now time.Time, dt time.Duration) {
	cp := &s.cp
	// A queue that all paths pass delays them all by the same amount, so
	// one threshold serves: a tenth of the shortest base RTT.
	target := time.Duration(0)
	for _, p := range s.paths {
		if b := p.cps.baseRTT(); p.state == stateActive && b > 0 && (target == 0 || b/10 < target) {
			target = b / 10
		}
	}
	target = max(target, cpQueueTarget)
	var delivered, sent, lost float64
	loaded, queued, capped := 0, 0, 0
	measured, kept := 0, 0       // paths that sent data in the previous round / delivered clearly less of it
	limited := false             // some path is capped: the session rate is what limits it
	growth := 0.0                // by how much a path in STARTUP delivers more than a round ago
	commonQ := time.Duration(-1) // the queue all loaded paths saw
	for _, p := range s.paths {
		c := &p.cps
		if p.meter == nil || p.state == stateDead || !c.started {
			continue
		}
		m := p.meter.Snapshot()
		// What the path delivered of its sends in the previous round: the
		// bytes acknowledged between that round's start and end, each
		// shifted by the path's RTT. Timed from ACK to ACK: a bunch of
		// ACKs just before or after a tick must not count for the wrong
		// round.
		ma, mt := p.meter.Marked()
		was := c.rate
		c.rate = 0
		if c.marks > 0 && cp.prevDt > 0 {
			span := mt - c.markAt
			if span < cp.prevDt/2 || span > 2*cp.prevDt {
				span = cp.prevDt
			}
			c.rate = float64(ma-c.markAcked) / span.Seconds()
		}
		c.markAcked, c.markAt, c.marks = ma, mt, c.marks+1
		delivered += c.rate
		// Did the path deliver what it sent in that round?
		if c.sent1*cp.prevDt.Seconds() >= cpLoadedBytes {
			measured++
			if c.rate < cpTrim*c.sent1 {
				kept++
			}
		}
		c.sent1 = float64(m.SentBytes-c.sent) / dt.Seconds()
		// A path in STARTUP whose own delivery rate still grows has not
		// filled its pipe, whatever the others see (see below).
		if p.ccMode == "startup" && p.state == stateActive && c.rate > 0 && c.rate >= cpGrowth*was {
			growth = max(growth, c.rate-was)
		}
		sent += float64(m.SentBytes - c.sent)
		lost += float64(m.LostBytes - c.lost)
		isCapped := p.usable() && c.cap > 0 && c.cap < c.rawBw
		limited = limited || isCapped
		if a := m.AckedBytes - c.acked; p.state != stateActive || a < cpLoadedBytes {
			continue
		}
		loaded++
		q := c.queue()
		if commonQ < 0 || q < commonQ {
			commonQ = q
		}
		if isCapped {
			capped++
		}
		if q >= target {
			queued++
		}
	}
	commonQ = max(commonQ, 0)
	// The delivery rate of the previous round, and whether the session was
	// busy in it: its paths had data whenever they could send. Only then
	// does a lower delivery rate mean less capacity. (A drain round sends
	// or the one after a probe sends less on purpose; with a path in
	// ProbeRTT its share is missing.)
	sentPrev := cp.prevSent
	rate, wasBusy := min(delivered, sentPrev), cp.prevBusy
	lostRate := lost / dt.Seconds()
	busy := sent > 0 && cp.ticks > 0 && float64(cp.idleTicks) <= cpIdleFrac*float64(cp.ticks)
	cp.prevDt, cp.prevSent, cp.prevBusy = dt, sent/dt.Seconds(), busy && !(cp.gain > 0 && cp.gain < 1) && !cp.benched
	// Congestion that all paths see: a queue throughout the round on every
	// path that carried data.
	cp.congested = loaded > 0 && queued == loaded
	// The network kept or dropped part of what every path sent in the
	// previous round: a bottleneck that all of them pass is full. (One path
	// that loses a fifth of its packets does not make it so.)
	short := wasBusy && measured > 0 && kept == measured
	if short {
		cp.shortNo++
	}
	// A path in STARTUP is still filling its own pipe if the session's
	// delivery rate grows with it. Behind a common bottleneck the paths
	// only take from each other, and the total stays where it is.
	growing := growth > 0 && rate-cp.prevRate >= growth/2
	cp.full = (cp.congested || short) && !growing && rate < cpGrowth*cp.prevRate
	// B is shared out for a while after the pipe was seen full (not by a
	// probe: that fills every path's own bottleneck as well). A bottleneck
	// that all paths pass keeps showing it; independent paths do not, and
	// are left alone again. Before that, what was delivered only shows
	// what was sent so far (and lags a path that ramps up within a round).
	if wasBusy && cp.full && cp.phase == cpCruise {
		cp.sharedUntil = cp.roundNo + cpShareRounds
	}
	cp.prevRate = rate
	if cp.congested {
		cp.queued++
	} else {
		cp.queued = 0
	}
	if wasBusy {
		cp.busyNo++
	}
	bw := cp.bw
	switch {
	case rate > cp.bw:
		cp.pushLocked(rate)
		cp.satN = 0
	case wasBusy && cp.congested:
		// A queue on every path and clearly less delivered than B (the
		// average of up to three such rounds): the capacity dropped, and B
		// follows at once. A queue alone, with all of B delivered, may not
		// be ours: following the delivery rate down then never ends.
		cp.satN = min(cp.satN+1, cpSatRounds)
		cp.sat[int(cp.busyNo)%cpSatRounds] = rate
		var mean float64
		for i := 0; i < cp.satN; i++ {
			mean += cp.sat[(int(cp.busyNo)-i)%cpSatRounds] / float64(cp.satN)
		}
		if mean < cpHold*cp.bw {
			cp.filter = append(cp.filter[:0], cpSample{cp.shortNo, mean})
		}
	case short:
		// A sample of what the bottleneck carries. B is the maximum of
		// the last ten of them, as in BBR: a policer's rate is found this
		// way, and loss that all paths have costs nothing as long as the
		// probes (1.25) more than make up for it.
		cp.pushLocked(rate)
		cp.satN = 0
	default:
		// Everything sent was delivered on some path (or the session had
		// no data for its paths): that shows a lower bound, not the
		// capacity, and B stands.
		cp.satN = 0
	}
	for len(cp.filter) > 1 && cp.filter[0].round <= cp.shortNo-cpRounds {
		cp.filter = cp.filter[1:]
	}
	if len(cp.filter) > 0 {
		cp.bw = cp.filter[0].v
	}
	// A maximum of noisy samples is a few percent too high, and the paths'
	// own drain phases then keep taking back what queues. While B is what
	// limits every path and all of them see a queue, the session should
	// deliver B on average (loss aside); if over 10 such rounds it
	// delivered clearly less, B is what was delivered.
	if cp.bw != bw {
		cp.capN, cp.capSum, cp.capLost = 0, 0, 0
	} else if wasBusy && cp.congested && capped == loaded {
		cp.capN++
		cp.capSum += rate
		cp.capLost += lostRate
		if cp.capN >= cpRounds {
			if mean := cp.capSum / float64(cp.capN); mean+cp.capLost/float64(cp.capN) < cpTrim*cp.bw {
				cp.filter = append(cp.filter[:0], cpSample{cp.shortNo, mean})
				cp.bw = mean
			}
			cp.capN, cp.capSum, cp.capLost = 0, 0, 0
		}
	}
	was := cp.phase
	switch cp.phase {
	case cpProbe:
		// While the probe's ACKs come in, send as much less as the probe
		// sent more: if the bottleneck did not carry it, what it queued is
		// gone again by the time we know.
		cp.phase, cp.gain = cpObserve, max(2-cp.probeGain, 0.5)
	case cpObserve:
		// rate is what was delivered of the probe round's sends.
		cp.phase, cp.gain = cpCruise, 1
		if cp.bw >= cp.probeBw*(1+cpProbeShow*(cp.probeGain-1)) {
			// There was more: probe again right away. One success may be a
			// policer's burst allowance, refilled since the last probe;
			// from the second in a row the probes get harder.
			cp.nextProbe = cp.roundNo
			if cp.streak++; cp.streak >= 2 {
				cp.backoff = 0
				cp.probeGain = min(1+2*(cp.probeGain-1), cpMaxProbeGain)
			}
			break
		}
		cp.streak = 0
		cp.backoff = min(cp.backoff+1, cpMaxBackoff)
		cp.nextProbe = cp.roundNo + 1<<cp.backoff
		cp.probeGain = cpProbeGain
	case cpDrain:
		cp.phase, cp.gain, cp.lastDrain = cpSettle, 1, cp.roundNo
	case cpSettle:
		cp.phase = cpCruise
		if cp.queued > 0 && cp.drainQ-commonQ < max(target, dt/16) {
			// The queue is still there and the drain did not shrink it: it
			// is not ours (the other direction's, or somebody else's
			// traffic). It becomes part of the base RTT.
			for _, p := range s.paths {
				if c := &p.cps; c.rttMin > 0 {
					c.base[0], c.base[1], c.baseAt = c.rttMin, c.rttMin, now
				}
			}
			cp.queued, cp.congested, cp.full = 0, false, false
		}
	}
	if cp.phase == cpCruise && busy && limited {
		switch {
		case cp.roundNo >= cp.nextProbe:
			cp.probeGain = max(cp.probeGain, cpProbeGain)
			cp.phase, cp.gain, cp.probeBw = cpProbe, cp.probeGain, cp.bw
		case cp.queued >= 2 && cp.roundNo-cp.lastDrain >= cpDrainEvery:
			cp.phase, cp.gain, cp.drainQ = cpDrain, cpDrainGain, commonQ
		}
	}
	cp.roundNo++
	if cpDebug {
		var b strings.Builder
		for _, p := range s.paths {
			c := &p.cps
			fmt.Fprintf(&b, " %d:%s raw=%.1f cap=%.1f q=%.1fms", p.id, p.ccMode, float64(c.rawBw)*8/1e6, float64(c.cap)*8/1e6,
				float64(c.queue())/1e6)
		}
		fmt.Fprintf(os.Stderr, "%s RATE dialer=%v round=%d %v rate=%.1fM sent=%.1fM B=%.1fM busy=%v congested=%v(%d/%d) %s->%s gain=%.2f%s\n",
			now.Format("15:04:05.000"), s.dialer, cp.roundNo, dt.Round(time.Millisecond), rate*8/1e6, cp.prevSent*8/1e6, cp.bw*8/1e6,
			wasBusy, cp.congested, queued, loaded, was, cp.phase, cp.gain, b.String())
	}
}

// pushLocked adds a delivery rate sample to the windowed maximum.
func (cp *coupling) pushLocked(v float64) {
	for len(cp.filter) > 0 && cp.filter[len(cp.filter)-1].v <= v {
		cp.filter = cp.filter[:len(cp.filter)-1]
	}
	cp.filter = append(cp.filter, cpSample{cp.shortNo, v})
}

// cpShareLocked shares the session rate (or the configured limit, if that
// is lower) out to the paths as caps on their estimates.
func (s *Session) cpShareLocked(limit uint64) {
	cp := &s.cp
	n := 0
	for _, p := range s.paths {
		if p.meter != nil && p.state != stateDead {
			n++
		}
	}
	// What there is to share: the session rate while the pipe has been full
	// not long ago (a single path is its own BBR's business), or the
	// configured limit if lower.
	total := 0.0
	if s.coupled() && n >= 2 && cp.roundNo < cp.sharedUntil {
		total = cp.bw
	}
	ceiling := limit > 0 && (total == 0 || total >= float64(limit))
	if ceiling {
		total = float64(limit)
	}
	// A configured limit is not probed: a probe goes as far as the limit,
	// and the round after it takes back only what the probe added (nothing,
	// if the limit is what is shared out).
	gain := cp.gain
	if limit > 0 && total > 0 {
		room := float64(limit) / total
		switch cp.phase {
		case cpProbe:
			gain = min(gain, room)
		case cpObserve:
			gain = max(2-min(cp.probeGain, room), 0.5)
		default:
			gain = min(gain, room)
		}
	}
	// The least a share is: BBR's minimum pacing rate, unless the paths
	// then add up to more than there is to share (a slow line).
	minShare := min(cpMinShare, total/float64(max(n, 1)))
	open := s.tmpPaths[:0]
	var starting []*Path
	reserved := 0.0
	for _, p := range s.paths {
		if p.meter == nil || p.state == stateDead {
			continue
		}
		p.cps.rawBw = p.meter.Snapshot().RawBandwidth
		p.meter.SetRateGain(gain)
		switch {
		case total == 0 || !p.usable():
			s.setCapLocked(p, 0)
		case p.ccMode == "startup" && !cp.full && !ceiling:
			// A path in STARTUP is still finding its rate: it gets no share
			// until the pipe is evidently full. What it delivered is part
			// of the session rate, and is set aside for it.
			starting = append(starting, p)
			reserved += p.cps.rate
		default:
			open = append(open, p)
		}
	}
	left := max(total-reserved, total/float64(n+1))
	for _, p := range starting {
		c := uint64(0)
		if limit > 0 { // ... but it stays below a configured limit
			c = uint64(max((float64(limit)-left)/float64(len(starting)), minShare))
		}
		s.setCapLocked(p, c)
	}
	// Max-min fair shares: a path whose own estimate is below an equal share
	// is not capped (it may grow, and then takes from the others); the
	// others get equal shares of the rest. Equal shares keep every path
	// loaded: a path in ProbeRTT or failing leaves only its share to be
	// taken over, and loss recovery and rescues have capacity on every
	// path. (The scheduler still puts small transfers on the fastest path.)
	for changed := true; changed && len(open) > 0; {
		changed = false
		fair := left / float64(len(open))
		k := 0
		for _, p := range open {
			if raw := float64(p.cps.rawBw); raw <= fair {
				c := uint64(0)
				if ceiling {
					// A limit holds for what the paths send, whatever
					// their estimates: this one may grow up to an equal
					// share (and at the start, when no path has an
					// estimate, that is what every path gets).
					c = uint64(max(fair, minShare))
				}
				s.setCapLocked(p, c)
				left -= raw
				changed = true
			} else {
				open[k], k = p, k+1
			}
		}
		open = open[:k]
	}
	for _, p := range open {
		s.setCapLocked(p, uint64(max(left/float64(len(open)), minShare)))
	}
	clear(s.tmpPaths[:cap(s.tmpPaths)])
	s.tmpPaths = open[:0]
}

// cpMinShare is the least a path is capped at (bytes/s), where there is
// that much to share: BBR's own minimum pacing rate. (Four packets per RTT,
// as it was, is 4 Mbit/s at 10 ms: more than a path's share of a slow
// uplink, which the paths then overran together however well it was
// measured.)
const cpMinShare = 64 << 10

func (s *Session) setCapLocked(p *Path, c uint64) {
	if p.cps.cap != c {
		p.cps.cap = c
		p.meter.SetRateCap(c)
	}
}

// RateStat is a snapshot of the session's rate control, for monitoring.
type RateStat struct {
	Rate      uint64 // session rate: what all paths deliver together (bytes/s), 0 = unknown
	Limit     uint64 // configured ceiling (bytes/s), 0 = none
	Congested bool   // every loaded path saw a queue or loss in the last round
}

// Rate returns the session's rate control state.
func (s *Session) Rate() RateStat {
	s.mu.Lock()
	defer s.mu.Unlock()
	return RateStat{Rate: uint64(s.cp.bw), Limit: s.rateLimitLocked(), Congested: s.cp.congested}
}
