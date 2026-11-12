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
// So the session has one rate, B, and the paths have none of their own: a
// path's controller paces at the share it is given and bounds what it has in
// flight (cc.follower). The session works in rounds of the longest round
// trip, and every acknowledgement and loss is counted for the round its
// packet was sent in (cc.Meter), so what a round delivered and lost is
// exact. The rules, and where each comes from:
//
//   - B is the highest rate at which a round of the last ten was delivered.
//     (BBR's bandwidth estimate: a path delivers no faster than its
//     bottleneck, so the highest recent delivery rate is the bottleneck's;
//     ten rounds outlast a probe cycle.) Rounds in which the application
//     had less to send than B say nothing and are skipped.
//   - Start-up sends twice what is being acknowledged (slow start). It ends
//     when B stops growing (BBR: less than a quarter in three rounds), when
//     every path's round trip rises (HyStart), or on loss (BBRv2).
//   - After that the session sends at B. Every eight rounds one round sends
//     a quarter more, and the next one as much less (BBR's probe cycle). If
//     the probe was delivered, B has risen with it, and the next probe
//     follows at once and asks for twice as much more.
//   - A drain (after the start-up, a probe, a cut) lasts until no more is
//     in flight than the paths' shares need (BBR's drain).
//   - Loss counts when it comes with the rate: beyond what a path loses
//     also when the pipe is not full (its baseline), and on all paths. Two
//     such rounds at B: B is what they delivered (BBRv2 bounds its rate by
//     loss in the same way; the baseline is what keeps random loss out of
//     it). A probe answered by loss doubles the wait for the next (a
//     policer makes every probe cost loss).
//
// B is shared out equally, except that a path gets no more than it has
// shown to carry: a path that its own window holds back while others are
// not held back has a limit of its own (a slow relay).
//
// docs/design/congestion.md has the reasons at length.

const (
	// A round is the longest round trip of the session's paths: what was
	// sent in a round is acknowledged in the next. It is at least
	// cpMinRound, so that a round holds enough packets to measure a rate
	// and a loss share, and at most cpMaxRound.
	cpMinRound = 50 * time.Millisecond
	cpMaxRound = time.Second
	cpRounds   = 10 // B is the highest delivery rate of this many rounds (BBR)
	cpOwnKnown = 3  // a path's own rate is known once this many rounds have shown it

	cpStartGain   = 2.0  // slow start: twice what is acknowledged
	cpStartGrowth = 1.25 // start-up is over when B grew by less than this ...
	cpStartRounds = 3    // ... in this many rounds (BBR's "full pipe")

	cpProbeGain    = 1.25 // a probe: a quarter more for a round (BBR)
	cpMaxProbeGain = 2.0  // ... up to twice as much, while probes keep finding more
	cpProbeEvery   = 8    // rounds from one probe to the next (BBR's cycle) ...
	cpMaxHold      = 128  // ... doubled each time loss answers a probe, up to this
	cpSameCeil     = 0.15 // loss stopped B "at the same rate": within this much
	cpMaxDrain     = 2    // rounds a drain lasts at most

	cpLossExcess = 0.02 // loss that counts: this much beyond the baseline (BBRv2's threshold)
	cpBurst      = 4    // random loss comes in bursts of about this many packets
	cpLossEvents = 8    // loss ends a start-up only if it is this many bursts (BBRv2)
	cpCut        = 0.5  // B comes down by what was lost, but not below this much of it at once
	cpCommon     = 0.75 // "all paths": the paths that lost carried this much of the round
	cpBaseKeep   = 0.75 // a baseline: what an older round counts for after each newer one
	cpBasePkts   = 200  // ... and the packets it takes to know one

	cpClearly     = 0.9              // "clearly less": below this much
	cpBusyFrac    = 0.85             // the session had data for the round: it sent this much of what it could
	cpStartBusy   = 0.5              // ... in the start-up, where it may send twice what was delivered: as much as that
	cpLoadedBytes = 10 * 1200        // a path carried data in a round: at least this much
	cpBaseWindow  = 10 * time.Second // a path's base RTT is the lowest of this long (BBR's min_rtt window)

	cpMinShare    = 64 << 10              // the least a path's share is (bytes/s): its pings and acknowledgements
	cpStartWindow = 32 * 1200             // start-up begins with this much per path and round trip (quic-go's initial window)
	cpStartRTT    = 50 * time.Millisecond // ... taken to be this long until it is measured
)

// MPQUIC_RATE_DEBUG=1 logs every round.
var cpDebug = os.Getenv("MPQUIC_RATE_DEBUG") != ""

type cpPhase int

const (
	cpStartup cpPhase = iota
	cpCruise          // sending at B
	cpProbe           // one round above B
	cpDrain           // below B, until what is in flight is what B needs
)

func (p cpPhase) String() string {
	return [...]string{"startup", "cruise", "probe", "drain"}[p]
}

type cpAck struct {
	at time.Time
	n  uint64
}

type cpSample struct {
	n int64 // the how-many-th round that counted
	v float64
}

// cpRate is a rate and how it is found: the highest delivery rate of the
// last rounds, and the probes for more. The session has one for all paths
// together (B), and every path has one of its own.
type cpRate struct {
	bw      float64    // bytes/s (0: not known)
	filter  []cpSample // the delivery rates of the last cpRounds rounds that counted (a monotone deque: its head is the highest)
	counted int64      // ... and how many have counted so far
	cut     int64      // bw was set at the end of this round: what earlier rounds delivered does not count

	hold      int64   // rounds from one probe to the next
	next      int64   // the round of the next one
	probeGain float64 // ... and its gain
	at        int64   // the round of the probe that is not judged yet (-1: none)
	bw0, g0   float64 // bw when it began, and its gain
	ceil      float64 // the bw at which loss last answered
}

func (r *cpRate) init() {
	r.cut, r.at, r.hold, r.probeGain = -1, -1, cpProbeEvery, cpProbeGain
}

// set makes the rate v, forgetting what was delivered before.
func (r *cpRate) set(v float64) {
	r.counted++
	r.filter = append(r.filter[:0], cpSample{r.counted, v})
	r.bw = v
}

// push adds the delivery rate of a round: the rate is the highest of the
// last cpRounds. (Rounds that do not count, because there was little to
// send, do not age the others: the rate is not forgotten while idle.)
func (r *cpRate) push(v float64) {
	r.counted++
	for len(r.filter) > 0 && r.filter[len(r.filter)-1].v <= v {
		r.filter = r.filter[:len(r.filter)-1]
	}
	r.filter = append(r.filter, cpSample{r.counted, v})
	for r.filter[0].n <= r.counted-cpRounds {
		r.filter = r.filter[1:]
	}
	r.bw = r.filter[0].v
}

// lossAnswered: loss stopped the rate at v. Probing costs loss here (a
// policer drops what a probe adds), so probes become rarer: the wait
// doubles each time loss answers at about the same rate, and starts over at
// another.
func (r *cpRate) lossAnswered(v float64) {
	if r.ceil > 0 && math.Abs(v-r.ceil) <= cpSameCeil*r.ceil {
		r.hold = min(2*r.hold, cpMaxHold)
	} else {
		r.hold = cpProbeEvery
	}
	r.ceil = v
}

// judge decides, at the end of round k, what the probe of round r.at
// showed: its packets were acknowledged in that round and the next, and
// both have been counted by k = r.at+3 (a round's delivery counts two
// rounds later, when its losses are known).
//
// Paths differ in delay, so a probe's packets reach the receiver interleaved
// with those of the rounds around it, and only part of what a delivered
// probe added shows in any one round: a third at least. A quarter counts.
func (r *cpRate) judge(k int64, lossy bool) {
	if r.at < 0 || k < r.at+3 {
		return
	}
	found := !lossy && r.bw >= r.bw0*(1+(r.g0-1)/4)
	switch {
	case found:
		// There is more. The next probe at once, and for twice as much
		// more: a rate that is far away is found in a few steps, as in
		// slow start.
		r.probeGain, r.next = min(1+2*(r.g0-1), cpMaxProbeGain), k+1
	case lossy:
		r.lossAnswered(r.bw0)
		fallthrough
	default:
		r.probeGain, r.next = cpProbeGain, r.at+r.hold
	}
	if !found && r.bw > r.bw0 {
		// A little more than before is not capacity: a round's delivery
		// rate is only so exact, and a rate that takes every such sample
		// creeps above what the pipe carries, and keeps a queue there.
		r.set(r.bw0)
	}
	r.at = -1
}

// forget: the rate is not known any more.
func (r *cpRate) forget(k int64) {
	r.bw, r.filter, r.counted, r.cut, r.at = 0, r.filter[:0], 0, k, -1
}

// relax: the rate is far below where loss answered (the capacity dropped):
// what was learned there does not hold probes back here.
func (r *cpRate) relax(k int64) {
	if r.hold > cpProbeEvery && r.bw < (1-cpSameCeil)*r.ceil {
		r.hold, r.next = cpProbeEvery, min(r.next, k+1)
	}
}

// cpRound is what the session remembers of a round, and a path of its part
// in it.
type cpRound struct {
	phase cpPhase
	gain  float64
	dt    time.Duration
	busy  bool    // there was the data to send what could be sent
	plain bool    // ... at the rate itself: no probe, no drain
	held  bool    // a path: its window held it back, or it was being probed
	sent  float64 // the rate at which it was sent (bytes/s)
	out   float64 // the rate at which acknowledgements came in during it (bytes/s)
	lossy bool    // it lost beyond the baseline (the session: on all paths); known two rounds later
}

// sample is the delivery rate of round r2, as a send rate: the rate at
// which acknowledgements came in during it, but no more than was sent (in
// it or the round before, r3, whose packets they acknowledge):
// acknowledgements that come in a bunch are not capacity. (BBR takes the
// longer of the send and the acknowledgement interval for the same reason.)
// A path with random loss delivers less than is sent on it, by its baseline
// base, and a rate is what is sent.
//
// A round counts if there was the data for it, and not if it lost beyond
// the baseline: what such a round delivered may include a policer's burst
// allowance, or what a buffer took before it overflowed.
func cpDelivery(r2, r3 *cpRound, base float64) (v float64, ok bool) {
	if !r2.busy || !r3.busy || r2.lossy || r3.lossy {
		return 0, false
	}
	return min(r2.out/(1-min(base, 0.5)), max(r2.sent, r3.sent)), true
}

// coupling is the session's part (sess.mu held).
type coupling struct {
	roundAt time.Time
	roundNo int64 // the round under way
	hist    [4]cpRound

	cpRate // B
	phase  cpPhase
	gain   float64 // what the round sends: B times this

	// start-up
	best   float64   // the B that the coming rounds must exceed by cpStartGrowth
	stall  int       // ... and the rounds that did not
	over   bool      // loss or a queue showed within the round: it is over
	acks   []cpAck   // the bytes acknowledged as of the last ticks (a round's worth)
	qSince time.Time // every path has seen a queue since then (zero: not)

	drainN int // rounds the drain under way has taken

	congested bool   // last round: every path that carried data saw a queue
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

	acked0 uint64 // bytes acknowledged when the round began
	hist   [4]cpRound

	// lossBase is the share the path loses whatever the rate (random
	// loss), as rounds show it in which the pipe was clearly not full.
	lossBase     float64
	lossOK       bool
	calmN, calmX float64 // packets sent and lost in those rounds (older ones fading)

	// own is what the path carries on its own account: a relay's rate.
	own    cpRate
	free   int     // rounds in a row in which that was no less than an equal share of B
	bound  bool    // ... and that is what the path gets: an equal share of B would be more
	fair   float64 // ... namely this (bytes/s)
	tryAt  int64   // the round in which it is probed for more (-1: not)
	down   float64 // it gets this much of its rate until what queued at its relay is gone (0: not draining)
	drainN int     // ... and the rounds that has taken

	cap   uint64 // the rate it paces at (bytes/s): its share times the round's gain
	share uint64 // its share of B (bytes/s)
	rawBw uint64 // its own rate if it is held to it, for the stats (bytes/s, 0: not)
}

// cpPkt is the packet size the loss statistics reckon with.
const cpPkt = 1200

// cpExcess reports whether lost of sent bytes is more than the share base
// by cpLossExcess, and by more than chance explains: three standard
// deviations, of losses that come in bursts of cpBurst packets (random loss
// on real links does; counting every packet as its own draw would take
// every burst for the pipe's doing).
func cpExcess(sent, lost uint64, base float64) bool {
	n, x := float64(sent)/cpPkt, float64(lost)/cpPkt
	d := x - n*base
	return d >= cpLossExcess*n && d >= 3*math.Sqrt(cpBurst*(n*base+1))
}

// learn updates the path's baseline with the loss of a round in which the
// pipe was clearly not full. The baseline is the loss of such rounds
// together, the later ones counting more: one round of a few dozen packets
// says little (two lost of a hundred is as likely as none).
func (c *cpPath) learn(sent, lost uint64) {
	n, x := float64(sent)/cpPkt, float64(lost)/cpPkt
	c.calmN, c.calmX = c.calmN*cpBaseKeep+n, c.calmX*cpBaseKeep+x
	c.lossBase, c.lossOK = c.calmX/c.calmN, c.calmN >= cpBasePkts
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

// noQueue is the round trip of a path that has no queue, generously: its
// base RTT and what cpQueueDelay allows for jitter.
func (c *cpPath) noQueue() float64 {
	return (c.baseRTT() + cpQueueDelay(c.baseRTT())).Seconds()
}

// cpQueueDelay is how much longer than base a round trip must be to show a
// queue: an eighth, at least 4 ms and at most 16 ms (HyStart++, RFC 9406:
// less is jitter).
func cpQueueDelay(base time.Duration) time.Duration {
	return min(max(base/8, 4*time.Millisecond), 16*time.Millisecond)
}

func cpQueued(rtt, base time.Duration) bool {
	return base > 0 && rtt >= base+cpQueueDelay(base)
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

// couplingTickLocked updates the session rate and the paths' shares of it.
func (s *Session) couplingTickLocked(now time.Time) {
	limit := s.rateLimitLocked()
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
	round = min(round, cpMaxRound)
	if dt := now.Sub(cp.roundAt); cp.roundAt.IsZero() {
		cp.init()
		cp.gain = cpStartGain
		s.cpStartRoundLocked(now)
	} else if dt >= round {
		s.cpRoundLocked(now, dt, limit)
		s.cpStartRoundLocked(now)
	} else if cp.phase == cpStartup {
		s.cpStartupTickLocked(now, round)
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
// doubles every round trip, as slow start does with its window. And it is
// over as soon as it has gone too far, without waiting for the round to end
// (a round at twice the pipe's rate queues or loses a round's worth):
//
//   - every path's round trip has been clearly above its base for a
//     round's length (HyStart: the common bottleneck is filling its
//     buffer; and it was busy all that time, so what came out in it is
//     what it carries), or
//   - the last two rounds lost more than the paths' baselines, on all paths,
//     and it is not one burst (BBRv2 ends its start-up on loss in the same
//     way).
//
// From then on the session sends no more than is delivered.
func (s *Session) cpStartupTickLocked(now time.Time, w time.Duration) {
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
			if p.meter.Blocked() { // it has in flight all it may: not waiting for data
				used += share
			} else {
				used += min(float64(t0.Sent), share)
			}
		}
		pk, pl := t0.Acked+t0.Lost+t1.Acked+t1.Lost, t0.Lost+t1.Lost
		known, lost, expected = known+pk, lost+pl, expected+float64(pk)*c.lossBase
		if cpExcess(pk, pl, c.lossBase) {
			lossy += pk
		}
		if t0.Sent+t1.Sent >= cpLoadedBytes {
			loaded++
			if cpQueued(p.latestRTT, c.baseRTT()) {
				queued++
			}
		}
	}
	if loaded == 0 || queued < loaded {
		cp.qSince = time.Time{}
	} else if cp.qSince.IsZero() {
		cp.qSince = now
	}
	// The rate at which acknowledgements came in over the last round's
	// length.
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
	if d < w || cp.over {
		return
	}
	r := float64(total-cp.acks[0].n) / d.Seconds()
	if known > 0 {
		r /= 1 - min(expected/float64(known), 0.5) // as a send rate (see cpDelivery)
	}
	if r > cp.bw {
		cp.set(r)
	}
	// A queue or loss says something about B only while the session sends
	// what it may: one with little to send (the acknowledgements of a
	// download) sees the other direction's queue and loss, not its own.
	if allowed == 0 || used < cpStartBusy*allowed || !cp.hist[(k+3)%4].busy {
		return
	}
	lossOver := known > 0 && float64(lossy) >= cpCommon*float64(known) &&
		cpExcess(known, lost, expected/float64(known)) && float64(lost)-expected >= cpLossEvents*cpBurst*cpPkt
	if !cp.qSince.IsZero() && now.Sub(cp.qSince) >= w || lossOver {
		// B is what the pipe puts out now, not the most it ever did.
		cp.set(min(cp.bw, r))
		cp.over, cp.gain = true, 1
	}
}

// cpRoundLocked ends round k of length dt. What became of the packets of
// round k-2 is known for good by now (their acknowledgements took a round
// trip, and their losses another to be noticed), and of round k-1 so far.
func (s *Session) cpRoundLocked(now time.Time, dt time.Duration, limit uint64) {
	cp := &s.cp
	k := cp.roundNo
	h := func(r int64) *cpRound { return &cp.hist[((r%4)+4)%4] }
	r1, r2, r3 := h(k-1), h(k-2), h(k-3)

	// All paths: what rounds k-1 and k-2 sent and lost, what the baselines
	// expected to be lost of that, and what the paths that lost more than
	// their baseline sent.
	var s1, l1, lossy1, s2, l2, lossy2 uint64
	var e1, e2 float64
	var allowed, used float64    // round k: what the paths could send, and did
	var sent, out float64        // the rates at which round k sent, and acknowledgements came in during it
	var inflight, needed float64 // what is in flight, and what B needs in flight without a queue
	loaded, queued := 0, 0
	for _, p := range s.paths {
		c := &p.cps
		if p.meter == nil || p.state == stateDead {
			continue
		}
		ph := func(r int64) *cpRound { return &c.hist[((r%4)+4)%4] }
		t0, t1, t2 := p.meter.RoundStats(k), p.meter.RoundStats(k-1), p.meter.RoundStats(k-2)
		blocked := p.meter.TakeBlocked()
		// The path's part in round k.
		pr := cpRound{dt: dt, plain: cp.gain == 1 && c.down == 0 && c.tryAt != k, sent: float64(t0.Sent) / dt.Seconds()}
		if acked := p.meter.Acked(); acked >= c.acked0 {
			pr.out = float64(acked-c.acked0) / dt.Seconds()
		}
		sent, out = sent+pr.sent, out+pr.out
		if p.usable() {
			share := float64(c.cap) * dt.Seconds()
			allowed += share
			if blocked { // it has in flight all it may: not waiting for data
				used += share
			} else {
				used += min(float64(t0.Sent), share)
			}
			pr.busy = blocked || float64(t0.Sent) >= cpBusyFrac*share
			pr.held = blocked || c.tryAt == k || c.tryAt == k-1
			inflight += float64(p.meter.Snapshot().InFlight)
			needed += float64(c.share) * c.noQueue()
		}
		*ph(k) = pr
		x1 := t1.Sent >= cpLoadedBytes && cpExcess(t1.Sent, t1.Lost, c.lossBase)
		x2 := t2.Sent >= cpLoadedBytes && cpExcess(t2.Sent, t2.Lost, c.lossBase)
		ph(k - 2).lossy = x2
		if t1.Sent >= cpLoadedBytes {
			s1, l1, e1 = s1+t1.Sent, l1+t1.Lost, e1+float64(t1.Sent)*c.lossBase
			if x1 {
				lossy1 += t1.Sent
			}
		}
		if t2.Sent >= cpLoadedBytes {
			s2, l2, e2 = s2+t2.Sent, l2+t2.Lost, e2+float64(t2.Sent)*c.lossBase
			if x2 {
				lossy2 += t2.Sent
			}
		}
		if t0.Sent >= cpLoadedBytes {
			loaded++
			if cpQueued(c.rttMin, c.baseRTT()) {
				queued++
			}
		}

		// The path's own rate: what a relay carries that is slower than the
		// path's share of B, or that polices. It is found like B, but only
		// from rounds in which the path's window held it back: the highest
		// rate at which it delivered in the last ten of those (a path that
		// delivers what it is given has shown no limit). It is known after
		// three such rounds (acknowledgements that stall for a moment hold
		// a path back, too), and from then on the path gets no more than
		// that (cpShareLocked). And like B:
		//
		//   - When the path loses beyond its baseline for two rounds at its
		//     plain share (not in a probe), its rate comes down by what it
		//     lost.
		//   - It is probed, one round at a quarter more and then as much
		//     less until what that queued is gone, but with the other paths
		//     getting that much less and more: that costs a common
		//     bottleneck nothing, and shows what the path carries even
		//     when the common bottleneck is full.
		own := &c.own
		if own.hold == 0 {
			own.init()
			c.tryAt = -1
		}
		base := min(c.lossBase, 0.5)
		// (A round counts for the path's own rate if the path could not
		// carry what it was given, or was being probed; and one in which
		// it delivered more than that rate always does.)
		if v, ok := cpDelivery(ph(k-2), ph(k-3), base); ok && k-3 > own.cut && (ph(k-2).held || own.bw > 0 && v > own.bw) {
			own.push(v)
		}
		if c.bound || own.bw == 0 {
			c.free = 0
		} else if c.free++; c.free > cpRounds {
			// An equal share of B has been less than its own rate for ten
			// rounds: that says nothing about the path any more.
			own.forget(k)
		}
		own.judge(k, ph(own.at).lossy)
		if c.down > 0 {
			if c.drainN++; float64(p.meter.Snapshot().InFlight) <= own.bw*c.noQueue() || c.drainN >= cpMaxDrain {
				c.down = 0
			}
		}
		switch p1, p2 := ph(k-1), ph(k-2); {
		case c.tryAt == k:
			c.down, c.drainN = max(2-own.probeGain, 0.5), 0
		case x2 && x1 && p1.plain && p2.plain && c.lossOK && k-2 > own.cut:
			// (as for B: what was not lost beyond the baseline is what the
			// path takes, of the rate it was paced at)
			v := own.bw
			if v == 0 || float64(c.cap) < v {
				v = float64(c.cap)
			}
			excess := float64(t1.Lost+t2.Lost)/float64(t1.Sent+t2.Sent) - c.lossBase
			own.set(v * max(1-excess, cpCut))
			own.counted = cpOwnKnown
			own.lossAnswered(own.bw)
			own.cut, own.next = k, k+own.hold
		}
		own.relax(k)
	}
	busy := allowed > 0 && used >= cpBusyFrac*allowed
	if cp.phase == cpStartup {
		// (The start-up may send twice what was delivered. A session that
		// sends as much as was delivered is not short of data: at high
		// rates it is the sender's CPU that does not reach twice.)
		busy = allowed > 0 && used >= cpStartBusy*allowed
	}
	*h(k) = cpRound{phase: cp.phase, gain: cp.gain, dt: dt, busy: busy, sent: sent, out: out}
	cp.congested = loaded > 0 && queued == loaded

	// A round is lossy for the session if the paths lost clearly more of
	// it than their baselines, all of them (all but a small one): that is
	// not a path's own trouble, and not chance.
	ok1, ok2 := s1 >= 4*cpLoadedBytes, s2 >= 4*cpLoadedBytes
	lossyRound1 := ok1 && float64(lossy1) >= cpCommon*float64(s1) && cpExcess(s1, l1, e1/float64(s1))
	r2.lossy = ok2 && float64(lossy2) >= cpCommon*float64(s2) && cpExcess(s2, l2, e2/float64(s2))

	// The baselines: a round shows what a path loses anyway if the pipe was
	// clearly not full, that is, if the session and the path sent clearly
	// less in it than they put out now, two rounds later (the round after a
	// probe; a start-up's rounds while the rate still doubles).
	if r2.sent < cpClearly*out {
		for _, p := range s.paths {
			if p.meter == nil || p.state == stateDead {
				continue
			}
			c := &p.cps
			if t2 := p.meter.RoundStats(k - 2); t2.Sent >= cpLoadedBytes && c.hist[((k-2)%4+4)%4].sent < cpClearly*c.hist[(k%4+4)%4].out {
				c.learn(t2.Sent, t2.Lost)
			}
		}
	}

	was := cp.phase
	if cp.phase == cpStartup {
		// (B has followed the acknowledgements, tick by tick.)
		switch {
		case cp.over:
		case !busy:
			// nothing to learn while the application sends less than it may
		case cp.bw >= cpStartGrowth*cp.best:
			cp.best, cp.stall = cp.bw, 0
		default:
			cp.stall++
		}
		if cp.over || cp.stall >= cpStartRounds || limit > 0 && cp.bw >= float64(limit) {
			cp.phase, cp.over = cpDrain, false
			cp.cut, cp.next = k, k+cp.hold
			// The paths' own rates are found from here on: what a path
			// delivered while the rates doubled says little.
			for _, p := range s.paths {
				p.cps.own.forget(k)
			}
		}
	} else {
		// B: the highest delivery rate of the last rounds (cpDelivery).
		base2 := 0.0
		if ok2 {
			base2 = e2 / float64(s2)
		}
		if v, ok := cpDelivery(r2, r3, base2); ok && k-3 > cp.cut {
			cp.push(v)
		}
		cp.judge(k, h(cp.at).lossy)
		// Loss beyond the baselines on all paths, in two rounds in a row
		// that were paced at B: B is more than the pipe takes (a policer, a
		// buffer that is full). What was not lost is what it takes: B
		// comes down by the share that was lost beyond the baselines. (A
		// policer passes its rate of what arrives at more, and so does a
		// full buffer. This holds whether or not the session had the data
		// to send at B all the time: what it sent, it paced at B. Not
		// below half of B at once.)
		if r2.lossy && lossyRound1 && r2.phase == cpCruise && r1.phase == cpCruise && k-2 > cp.cut {
			excess := (float64(l1+l2) - e1 - e2) / float64(s1+s2)
			cp.set(cp.bw * max(1-excess, cpCut))
			cp.lossAnswered(cp.bw)
			cp.cut, cp.next = k, k+cp.hold
			cp.phase = cpDrain
		}
		cp.relax(k)
	}
	if limit > 0 && cp.bw > float64(limit) {
		cp.set(float64(limit))
	}

	// The next round.
	switch {
	case cp.phase == cpDrain && was != cpDrain:
		cp.drainN = 0 // begun above: after the start-up, or a cut
	case cp.phase == cpProbe:
		// While the probe's acknowledgements come in, send as much less as
		// the probe sent more: what it queued is gone by the time we know.
		cp.phase, cp.drainN = cpDrain, 0
	case cp.phase == cpDrain:
		// Until no more is in flight than B needs over a round trip
		// without a queue (BBR's drain target). (Not for longer than
		// cpMaxDrain rounds: round trips also grow with queues that are
		// not ours, and sending less does not shorten those.)
		if cp.drainN++; inflight <= needed || cp.drainN >= cpMaxDrain {
			cp.phase = cpCruise
		}
	case cp.phase == cpCruise:
		if busy && r1.phase == cpCruise && cp.at < 0 && k >= cp.next && !(limit > 0 && cp.bw >= float64(limit)) {
			cp.phase, cp.at, cp.bw0, cp.g0 = cpProbe, k+1, cp.bw, cp.probeGain
		}
	}
	switch {
	case cp.phase == cpStartup:
		cp.gain = cpStartGain
	case cp.phase == cpProbe:
		cp.gain = cp.probeGain
	case cp.phase == cpDrain && was == cpProbe:
		cp.gain = max(2-h(k).gain, 0.5) // as much less as the probe sent more
	case cp.phase == cpDrain && was != cpDrain:
		cp.gain = 0.5 // after a start-up or a cut, a round's worth may be queued
	case cp.phase == cpCruise:
		cp.gain = 1
	}
	cp.roundNo++
	// The paths' probes: a path that is held to its own rate is probed in a
	// round that the session sends at B, so that what the path delivers
	// more shows. (Only if what the probe asks for is still no more than an
	// equal share of B: a path that gets about that is probed with the
	// session.)
	for _, p := range s.paths {
		c := &p.cps
		if p.meter == nil || p.state == stateDead {
			continue
		}
		c.tryAt = -1
		if own := &c.own; cp.phase == cpCruise && was == cpCruise && busy && c.bound && c.down == 0 && own.at < 0 && k >= own.next &&
			own.bw*own.probeGain <= c.fair {
			c.tryAt, own.at, own.bw0, own.g0 = k+1, k+1, own.bw, own.probeGain
		}
	}
	if cpDebug {
		var b strings.Builder
		for _, p := range s.paths {
			if c := &p.cps; p.meter != nil && p.state != stateDead {
				fmt.Fprintf(&b, " %d:%.1f", p.id, c.hist[(k%4+4)%4].out*8/1e6)
				if c.bound {
					fmt.Fprintf(&b, "<%.1f", c.own.bw*8/1e6)
				}
			}
		}
		pct := func(l, s uint64) float64 { return 100 * float64(l) / float64(max(s, 1)) }
		fmt.Fprintf(os.Stderr, "%s RATE dialer=%v round=%d %v %s: sent %.1fM out %.1fM; k-2 %s lost %.1f%% (base %.1f%% lossy=%v); k-1 lost %.1f%% (lossy=%v); B=%.1fM busy=%v q=%d/%d hold=%d next=%s gain=%.2f%s\n",
			now.Format("15:04:05.000"), s.dialer, k, dt.Round(time.Millisecond), was, sent*8/1e6, out*8/1e6,
			r2.phase, pct(l2, s2), 100*e2/float64(max(s2, 1)), r2.lossy, pct(l1, s1), lossyRound1,
			cp.bw*8/1e6, busy, queued, loaded, cp.hold, cp.phase, cp.gain, b.String())
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
			s.setShareLocked(p, cpMinShare, cpMinShare) // enough for its pings
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
	gain := cp.gain
	if gain == 0 {
		gain = cpStartGain // before the first tick
	}
	total := cp.bw
	if cp.phase == cpStartup {
		total = max(total, floor)
	}
	// A path's window is for its share of B as it was measured, and for
	// more only while the round sends more: until there is a B, it is the
	// initial window, whatever the first rounds are paced at.
	window := cp.bw * max(gain, 1) / (total * gain)
	total *= gain
	if limit > 0 {
		total = min(total, float64(limit))
	}
	// Max-min fair shares: a path whose own rate is below an equal share
	// gets that, and the others share the rest equally. Equal shares keep
	// every path loaded: a failing path leaves only its share to be taken
	// over, and loss recovery and rescues have capacity on every path.
	// (The scheduler still puts small transfers on the fastest path.)
	//
	// A path's pace is its share times the round's gain; a path that is
	// held to its own rate has a gain of its own while it is probed or
	// drained.
	own := func(c *cpPath) float64 {
		switch {
		case cp.phase == cpStartup || c.own.counted < cpOwnKnown:
			// Not in the start-up: a path's own rate is two rounds old,
			// and the rates double every round. (Its window bounds a path
			// that cannot follow.) And not before it is known.
			return 0
		case c.tryAt == cp.roundNo:
			return c.own.bw * c.own.probeGain
		case c.down > 0:
			return c.own.bw * c.down
		}
		return c.own.bw * gain
	}
	// The least a share is, unless the paths then add up to more than
	// there is to share (a slow line).
	minShare := min(cpMinShare, total/float64(max(n, 1)))
	left := total
	for _, p := range open {
		p.cps.bound, p.cps.rawBw = false, 0
	}
	for changed := true; changed && len(open) > 0; {
		changed = false
		fair := left / float64(len(open))
		j := 0
		for _, p := range open {
			if l := own(&p.cps); l > 0 && l <= fair {
				p.cps.bound, p.cps.rawBw, p.cps.fair = true, uint64(p.cps.own.bw), fair/gain
				s.setShareLocked(p, max(l, minShare), max(p.cps.own.bw*max(l/p.cps.own.bw, 1), minShare))
				left -= l
				changed = true
			} else {
				open[j], j = p, j+1
			}
		}
		open = open[:j]
	}
	for _, p := range open {
		pace := max(left/float64(len(open)), minShare)
		s.setShareLocked(p, pace, pace*window)
	}
	clear(s.tmpPaths[:cap(s.tmpPaths)])
	s.tmpPaths = open[:0]
}

// setShareLocked: the path paces at pace; its window is for share (bytes/s).
func (s *Session) setShareLocked(p *Path, pace, share float64) {
	p.meter.SetBaseRTT(p.cps.baseRTT())
	if c := &p.cps; c.cap != uint64(pace) || c.share != uint64(share) {
		c.cap, c.share = uint64(pace), uint64(share)
		p.meter.SetRate(c.cap, c.share)
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
