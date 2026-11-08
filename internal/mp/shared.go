package mp

import (
	"fmt"
	"math"
	"os"
	"slices"
	"strings"
	"time"
)

// Shared bottlenecks
//
// Each path runs its own BBR, which is right when the paths are independent
// routes. When several paths share a bottleneck, typically the uplink of the
// sending endpoint (three 100 Mbit/s relays behind a 150 Mbit/s uplink), each
// BBR probes for the whole bottleneck: the per-path estimates add up to more
// than its capacity, the paths together keep its buffer full (latency for
// every stream of the session), with a shallow buffer they lose a few
// percent of their packets (each costs a QUIC retransmission, often a
// session-level rescue too, and a head-of-line stall), and a path whose
// share shrinks looks like a rate drop to BBR's stale-estimate reset.
//
// Detection: all paths through one FIFO see the same queueing delay at the
// same time. Every tick, while the session has a backlog (the paths are
// busy, so their queues reflect the bottlenecks), the queueing delay of each
// path (latest RTT minus min RTT) is sampled, and exponentially weighted
// means, variances and covariances are kept per pair of paths. A pair whose
// delays are strongly correlated and of similar magnitude shares a
// bottleneck; one whose delays then vary independently for a while no
// longer does. Without a varying delay a pair keeps its verdict. Pairs that
// share a bottleneck link paths into a group.
//
// Coupling: the group's capacity C is measured as its aggregate delivery
// (ACK) rate while every member sees a queue, i.e. while the bottleneck is
// busy. C is shared out max-min fairly, and each path's BBR estimate is
// capped at its share (bbr.Sender.RateCap), so the paths together pace at
// C; their gain cycles still probe, a share at a time. A standing queue
// makes the group drain; delivery well below what the group sends means the
// capacity dropped; no queue while the paths send at their caps means spare
// capacity, which the group probes for. A path that is no longer coupled,
// or whose group lost a member, restarts BBR STARTUP to find its new rate
// quickly. Until a busy bottleneck has been seen, nothing is capped.

const (
	sbTau         = 2 * time.Second // averaging time constant of the statistics
	sbMinWeight   = 0.6             // evidence needed (1-exp(-t/tau) of loaded time)
	sbMinStd      = 0.7e-3          // s: delay variation needed for a verdict
	sbCorrShared  = 0.5
	sbCorrIndep   = 0.25
	sbJoinFor     = time.Second     // sharing must persist this long to join a pair
	sbIndepFor    = 2 * time.Second // independence must persist this long to split a pair
	sbHist        = 64              // ticks of acknowledged-bytes history per path
	sbCapWindow   = 2 * time.Second
	sbQueueTarget = 3 * time.Millisecond // standing queue that makes the group drain
	sbDrainGain   = 0.85
	sbDropFrac    = 0.75                  // delivery below this fraction of what is sent, ...
	sbDropQueue   = 10 * time.Millisecond // ... with at least this queue: capacity dropped
	sbProbeGain   = 1.25                  // estimate growth per probe step
)

// MPQUIC_COUPLE=0 turns coupling off (like Options.NoCoupling), for
// comparisons; MPQUIC_SBD_DEBUG=1 logs the detector's state every second.
var sbDisabled = os.Getenv("MPQUIC_COUPLE") == "0"
var sbDebug = os.Getenv("MPQUIC_SBD_DEBUG") != ""

type sbPair struct {
	a, b       *Path
	w          float64 // accumulated evidence weight
	ma, mb     float64 // mean queueing delay (s)
	va, vb, cv float64 // variances, covariance
	shared     bool
	lastAt     time.Time // last sample
	joinAt     time.Time // since when an unshared pair looks shared
	indepAt    time.Time // since when a shared pair looks independent
}

func (q *sbPair) corr() float64 {
	if q.va <= 0 || q.vb <= 0 {
		return 0
	}
	return q.cv / math.Sqrt(q.va*q.vb)
}

type sbSample struct {
	at time.Time
	v  float64
}

type sbGroup struct {
	ids       []int
	members   []*Path
	maxq      []sbSample // monotone deque: windowed max of the delivery rate
	standAt   time.Time  // since when every member sees a queue
	draining  bool       // pacing below the estimate to drain a standing queue
	drainAt   time.Time  // drain start
	drainHold time.Time  // no draining until then
	meas      float64    // measured capacity (bytes/s), 0 = unknown
	boost     float64    // probed capacity, while there is spare capacity
	freeAt    time.Time  // since when the bottleneck has had no queue
	c         float64    // capacity estimate: max(meas, boost)
	alloc     float64    // what the paths may send: caps or own estimates
}

type sbState struct {
	pairs  []*sbPair
	groups []*sbGroup
	dirty  bool    // pairs or paths changed: regroup
	up     []*Path // active group members at the last tick
	upPrev []*Path
	lastAt time.Time
	dbgAt  time.Time
}

type sbAck struct {
	at    time.Time
	acked uint64
}

// sbPathState is the per-path part (sess.mu held).
type sbPathState struct {
	hist    [sbHist]sbAck
	n, head int
	rawBw   uint64
	ackAt   time.Time // last time acknowledged bytes grew
	cap     uint64    // rate cap currently set (bytes/s)
	group   *sbGroup
}

func (h *sbPathState) push(at time.Time, acked uint64) {
	if h.n > 0 {
		if last := h.hist[(h.head+sbHist-1)%sbHist]; acked > last.acked {
			h.ackAt = at
		}
	}
	h.hist[h.head] = sbAck{at, acked}
	h.head = (h.head + 1) % sbHist
	h.n = min(h.n+1, sbHist)
}

// rate is the acknowledgement rate over about the last d.
func (h *sbPathState) rate(now time.Time, d time.Duration) float64 {
	if h.n < 2 {
		return 0
	}
	cur := h.hist[(h.head+sbHist-1)%sbHist]
	old := cur
	for i := 2; i <= h.n; i++ {
		old = h.hist[(h.head+sbHist-i)%sbHist]
		if now.Sub(old.at) >= d {
			break
		}
	}
	dt := cur.at.Sub(old.at).Seconds()
	if dt <= 0 || cur.acked < old.acked {
		return 0
	}
	return float64(cur.acked-old.acked) / dt
}

// queueDelay is the path's current queueing delay in seconds.
func (p *Path) queueDelay() float64 {
	if p.minRTT <= 0 || p.latestRTT <= p.minRTT {
		return 0
	}
	return (p.latestRTT - p.minRTT).Seconds()
}

// sbActive: the path is carrying data (its delay samples are current).
func (p *Path) sbActive(now time.Time) bool {
	return p.state == stateActive && now.Sub(p.sbs.ackAt) < 50*time.Millisecond
}

// sbSettled: the path's BBR is past STARTUP and its drain. A STARTUP
// overshoot can queue at the path's own link as much as at a shared one, so
// these samples are left out of the statistics.
func (p *Path) sbSettled() bool { return p.ccMode != "startup" && p.ccMode != "drain" }

// sharedTickLocked updates shared-bottleneck detection and the rate caps.
func (s *Session) sharedTickLocked(now time.Time) {
	if sbDisabled || s.opt.NoCoupling {
		return
	}
	sb := &s.sb
	dt := now.Sub(sb.lastAt)
	sb.lastAt = now
	if dt <= 0 || dt > 200*time.Millisecond {
		dt = tickInterval
	}
	var live []*Path
	for _, p := range s.paths {
		if p.meter == nil || p.state == stateDead {
			continue
		}
		m := p.meter.Snapshot()
		p.sbs.rawBw = m.RawBandwidth
		p.sbs.push(now, m.AckedBytes)
		live = append(live, p)
	}
	// pairs: drop those of removed paths, add new ones
	n := len(sb.pairs)
	sb.pairs = slices.DeleteFunc(sb.pairs, func(q *sbPair) bool {
		return !slices.Contains(live, q.a) || !slices.Contains(live, q.b)
	})
	sb.dirty = sb.dirty || len(sb.pairs) != n
	if len(sb.pairs) != len(live)*(len(live)-1)/2 {
		for i, a := range live {
			for _, b := range live[i+1:] {
				if !slices.ContainsFunc(sb.pairs, func(q *sbPair) bool { return q.a == a && q.b == b || q.a == b && q.b == a }) {
					sb.pairs = append(sb.pairs, &sbPair{a: a, b: b})
				}
			}
		}
	}
	// statistics, while the paths are loaded
	if s.backlog > 0 {
		alpha := 1 - math.Exp(-dt.Seconds()/sbTau.Seconds())
		for _, q := range sb.pairs {
			if q.a.sbActive(now) && q.b.sbActive(now) && q.a.sbSettled() && q.b.sbSettled() {
				s.sbSampleLocked(q, alpha, now)
			}
		}
	}
	if sb.dirty {
		sb.dirty = false
		s.regroupLocked(live)
	}
	for _, g := range sb.groups {
		s.groupCapsLocked(g, now)
	}
	// A member of a group that failed (suspect or gone) leaves capacity to
	// the others, which are only limited by their own estimates, i.e. about
	// their old shares: they restart STARTUP to take it over quickly.
	failed := false
	for _, p := range sb.up {
		if p.state != stateActive {
			failed = true
		}
	}
	up := sb.upPrev[:0] // double-buffered: sb.up is read meanwhile
	for _, p := range s.paths {
		if p.sbs.group != nil && p.state == stateActive {
			if failed && slices.Contains(sb.up, p) {
				p.meter.Restart()
			}
			up = append(up, p)
		}
		if p.sbs.group == nil && p.sbs.cap != 0 && p.meter != nil {
			s.setCapLocked(p, 0)
		}
	}
	clear(sb.up)
	sb.up, sb.upPrev = up, sb.up
	if sbDebug && now.Sub(sb.dbgAt) >= time.Second {
		sb.dbgAt = now
		var b strings.Builder
		for _, q := range sb.pairs {
			fmt.Fprintf(&b, " [%d-%d w=%.2f q=%.1f/%.1fms sd=%.1f/%.1fms corr=%.2f shared=%v]", q.a.id, q.b.id, q.w, q.ma*1e3, q.mb*1e3,
				math.Sqrt(q.va)*1e3, math.Sqrt(q.vb)*1e3, q.corr(), q.shared)
		}
		for _, g := range sb.groups {
			fmt.Fprintf(&b, " group%v C=%.1fM caps=", g.ids, g.c*8/1e6)
			for _, p := range g.members {
				fmt.Fprintf(&b, "%d:%.1f/%.1fM ", p.id, float64(p.sbs.cap)*8/1e6, float64(p.sbs.rawBw)*8/1e6)
			}
		}
		fmt.Fprintf(os.Stderr, "%s SBD dialer=%v backlog=%d%s\n", now.Format("15:04:05.000"), s.dialer, s.backlog, b.String())
	}
}

// sbSampleLocked adds the pair's current queueing delays to its statistics
// and updates its verdict. Joining needs a strongly correlated delay of
// similar magnitude; splitting needs an uncorrelated one for sbIndepFor (a
// path that is also limited by its own link adds its own queue, so the
// magnitudes may differ and the correlation weakens).
func (s *Session) sbSampleLocked(q *sbPair, alpha float64, now time.Time) {
	da, db := q.a.queueDelay(), q.b.queueDelay()
	if now.Sub(q.lastAt) > 100*time.Millisecond {
		q.joinAt, q.indepAt = time.Time{}, time.Time{} // evidence must be continuous
	}
	q.lastAt = now
	if q.w == 0 {
		q.ma, q.mb = da, db
	}
	// Deviations from a faster-moving mean: a slow trend that both paths
	// share (each draining its own queue after STARTUP, say) is not
	// evidence of a common queue; the fluctuations of a common queue are.
	xa, xb := da-q.ma, db-q.mb
	am := min(2*alpha, 1)
	q.ma += am * xa
	q.mb += am * xb
	q.va = (1 - alpha) * (q.va + alpha*xa*xa)
	q.vb = (1 - alpha) * (q.vb + alpha*xb*xb)
	q.cv = (1 - alpha) * (q.cv + alpha*xa*xb)
	q.w = min(q.w+alpha, 1)
	if q.w < sbMinWeight || math.Sqrt(q.va) < sbMinStd || math.Sqrt(q.vb) < sbMinStd {
		return
	}
	c := q.corr()
	if !q.shared {
		switch {
		case c < sbCorrShared || math.Abs(q.ma-q.mb) > 0.35*max(q.ma, q.mb)+2e-3:
			q.joinAt = time.Time{}
		case q.joinAt.IsZero():
			q.joinAt = now
		case now.Sub(q.joinAt) >= sbJoinFor:
			q.shared, q.indepAt, s.sb.dirty = true, time.Time{}, true
			s.log.Info("paths share a bottleneck", "paths", q.a.name+" "+q.b.name, "corr", fmt.Sprintf("%.2f", c),
				"queue_ms", fmt.Sprintf("%.1f/%.1f", q.ma*1e3, q.mb*1e3))
		}
		return
	}
	switch {
	case c > sbCorrIndep:
		q.indepAt = time.Time{}
	case q.indepAt.IsZero():
		q.indepAt = now
	case now.Sub(q.indepAt) >= sbIndepFor:
		q.shared, s.sb.dirty = false, true
		s.log.Info("paths no longer share a bottleneck", "paths", q.a.name+" "+q.b.name, "corr", fmt.Sprintf("%.2f", c),
			"queue_ms", fmt.Sprintf("%.1f/%.1f", q.ma*1e3, q.mb*1e3))
	}
}

// regroupLocked forms groups of paths connected by shared pairs. A group
// that overlaps an old one keeps its capacity estimate: the bottleneck is
// still the same when a path joins or leaves.
func (s *Session) regroupLocked(live []*Path) {
	sb := &s.sb
	parent := make(map[*Path]*Path, len(live))
	var find func(p *Path) *Path
	find = func(p *Path) *Path {
		if parent[p] == nil || parent[p] == p {
			return p
		}
		r := find(parent[p])
		parent[p] = r
		return r
	}
	for _, q := range sb.pairs {
		if q.shared {
			ra, rb := find(q.a), find(q.b)
			parent[ra], parent[rb] = rb, rb
		}
	}
	sets := map[*Path][]*Path{}
	for _, p := range live {
		if parent[p] != nil {
			r := find(p)
			sets[r] = append(sets[r], p)
		}
	}
	var groups []*sbGroup
	for _, m := range sets {
		if len(m) < 2 {
			continue
		}
		ids := make([]int, len(m))
		for i, p := range m {
			ids[i] = p.id
		}
		slices.Sort(ids)
		g := &sbGroup{ids: ids, members: m}
		for _, old := range sb.groups {
			if overlap(old.ids, ids) >= 2 || overlap(old.ids, ids) >= 1 && len(old.ids) < len(ids) {
				g.meas = max(g.meas, old.meas)
			}
		}
		if g.meas > 0 {
			g.maxq = []sbSample{{time.Now(), g.meas}}
			g.c = g.meas
		}
		groups = append(groups, g)
	}
	for _, p := range live {
		if p.sbs.group != nil && !slices.ContainsFunc(groups, func(g *sbGroup) bool { return slices.Contains(g.members, p) }) && p.sbs.cap > 0 {
			// No longer coupled: its own rate may be higher than its share.
			p.meter.Restart()
		}
		p.sbs.group = nil
	}
	for _, g := range groups {
		for _, p := range g.members {
			p.sbs.group = g
		}
	}
	sb.groups = groups
}

// pushLocked adds a capacity sample (bytes/s) to the windowed maximum.
func (g *sbGroup) pushLocked(now time.Time, v float64) {
	for len(g.maxq) > 0 && g.maxq[len(g.maxq)-1].v <= v {
		g.maxq = g.maxq[:len(g.maxq)-1]
	}
	g.maxq = append(g.maxq, sbSample{now, v})
	for len(g.maxq) > 1 && now.Sub(g.maxq[0].at) > sbCapWindow {
		g.maxq = g.maxq[1:]
	}
	g.meas = g.maxq[0].v
}

func overlap(a, b []int) int {
	n := 0
	for _, x := range a {
		if slices.Contains(b, x) {
			n++
		}
	}
	return n
}

func (s *Session) setCapLocked(p *Path, c uint64) {
	if p.sbs.cap == c {
		return
	}
	p.sbs.cap = c
	p.meter.SetRateCap(c)
}

// groupCapsLocked updates the group's capacity estimate and shares it out.
func (s *Session) groupCapsLocked(g *sbGroup, now time.Time) {
	var usable []*Path
	window := 50 * time.Millisecond
	minRTT := time.Duration(math.MaxInt64)
	queue := math.Inf(1) // the group's common queueing delay
	active := 0
	for _, p := range g.members {
		if p.usable() {
			usable = append(usable, p)
		}
		window = max(window, p.minRTT)
		if p.minRTT > 0 {
			minRTT = min(minRTT, p.minRTT)
		}
		if p.sbActive(now) {
			queue = min(queue, p.queueDelay())
			active++
		}
	}
	window = min(max(2*window, 100*time.Millisecond), 500*time.Millisecond)
	var agg float64
	for _, p := range g.members {
		agg += p.sbs.rate(now, window)
	}
	// Capacity. A queue that every member sees for a whole window means
	// that the bottleneck was busy throughout, so the group's delivery rate
	// over the window is its capacity (or, with cross traffic, the group's
	// share of it). The estimate is the windowed maximum of such samples,
	// and of samples above it once it is known (lower bounds: a probe
	// found more). The window counts from the newest sample, so the
	// estimate survives idle periods. Until a busy bottleneck has been
	// seen, the paths are not capped: what they delivered so far says
	// nothing about the capacity.
	target := max(sbQueueTarget, time.Duration(float64(minRTT)*0.1)).Seconds()
	busy := active >= 2 && queue >= target
	if !busy {
		g.standAt = time.Time{}
	} else if g.standAt.IsZero() {
		g.standAt = now
	}
	standing := !g.standAt.IsZero() && now.Sub(g.standAt) >= window && s.backlog > 0
	if agg > 0 && (standing || g.meas > 0 && agg > g.meas) {
		g.pushLocked(now, agg)
	}
	// Draining. As a maximum of noisy rates, the estimate tends to be a
	// little high, and the paths' own drain phases are too short to drain
	// what the group then queues. A standing queue makes the group pace
	// below the estimate until it is gone. If it does not go away, it is
	// not the group's (cross traffic filling the buffer): the group stops
	// draining for a while rather than give up its share. A bottleneck
	// that delivers far below what the group sends, with a deep queue,
	// means that the capacity dropped: the estimate is replaced at once
	// (draining itself does not trigger this, as the group then sends
	// less). The thresholds are those of BBR's stale-estimate reset: a
	// competing flow's probes must not make the group give up its share.
	deep := max(sbDropQueue, minRTT/2).Seconds()
	if standing && queue >= deep && agg < sbDropFrac*min(g.meas, g.alloc) {
		if sbDebug {
			fmt.Fprintf(os.Stderr, "%s SBD group%v capacity drop: C %.1fM, sending %.1fM, delivering %.1fM\n", now.Format("15:04:05.000"),
				g.ids, g.c*8/1e6, g.alloc*8/1e6, agg*8/1e6)
		}
		g.maxq = g.maxq[:0]
		g.pushLocked(now, agg)
		g.boost = 0
	}
	switch {
	case g.draining && queue < target:
		g.draining = false
	case g.draining && now.Sub(g.drainAt) >= 3*window:
		g.draining, g.drainHold = false, now.Add(sbCapWindow)
		if sbDebug {
			fmt.Fprintf(os.Stderr, "%s SBD group%v queue %.1fms does not drain: hold\n", now.Format("15:04:05.000"), g.ids, queue*1e3)
		}
	case !g.draining && standing && g.meas > 0 && now.After(g.drainHold):
		g.draining, g.drainAt = true, now
		if sbDebug {
			fmt.Fprintf(os.Stderr, "%s SBD group%v standing queue %.1fms, C %.1fM (delivering %.1fM): drain\n", now.Format("15:04:05.000"),
				g.ids, queue*1e3, g.c*8/1e6, agg*8/1e6)
		}
	}
	// Probing. The paths' own gain cycles probe for capacity a share at a
	// time, and only find more if it shows in the group's delivery rate.
	// When the bottleneck has had no queue for two windows while the
	// paths send at their caps, there is spare capacity (the bottleneck got
	// faster, or cross traffic left): the estimate grows by 25% per two
	// windows until a queue builds, which ends the probe.
	capped := false
	for _, p := range usable {
		capped = capped || p.sbs.cap > 0
	}
	switch {
	case standing || g.meas == 0:
		g.boost, g.freeAt = 0, time.Time{}
	case busy || s.backlog == 0 || !capped || agg < 0.85*g.c:
		g.freeAt = time.Time{}
	case g.freeAt.IsZero():
		g.freeAt = now
	case now.Sub(g.freeAt) >= 2*window:
		g.boost, g.freeAt = sbProbeGain*g.c, now
	}
	g.c = max(g.meas, g.boost)
	if g.c <= 0 || len(usable) == 0 {
		g.alloc = 0
		for _, p := range g.members {
			s.setCapLocked(p, 0)
			g.alloc += float64(p.sbs.rawBw)
		}
		return
	}
	// Share C out max-min fairly: a path whose own estimate is below an
	// equal share is not capped (it may grow, and then takes from the
	// others); the others are capped at equal shares of the rest. Equal
	// shares keep every path loaded: a path in ProbeRTT or failing leaves
	// only its share to be taken over, and loss recovery and rescues have
	// capacity on every path. (The scheduler still puts small transfers on
	// the fastest path.)
	left := g.c
	if g.draining {
		left *= sbDrainGain
	}
	open := slices.Clone(usable)
	for changed := true; changed && len(open) > 0; {
		changed = false
		fair := left / float64(len(open))
		open = slices.DeleteFunc(open, func(p *Path) bool {
			raw := float64(p.sbs.rawBw)
			if raw == 0 || raw <= fair {
				s.setCapLocked(p, 0)
				left -= raw
				changed = true
				return true
			}
			return false
		})
	}
	for _, p := range open {
		fair := max(left/float64(len(open)), 4*1200/max(p.minRTT, time.Millisecond).Seconds())
		s.setCapLocked(p, uint64(fair))
	}
	g.alloc = 0
	for _, p := range usable {
		if p.sbs.cap > 0 {
			g.alloc += float64(p.sbs.cap)
		} else {
			g.alloc += float64(p.sbs.rawBw)
		}
	}
	for _, p := range g.members {
		if !p.usable() {
			s.setCapLocked(p, 0)
		}
	}
}
