package mp

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// Scheduling overview
//
// Every path has a sender goroutine that pulls the next chunk from the
// session when QUIC is able to take more data (quic-go's Write blocks while
// the congestion window / pacer is exhausted, so a pull means "this path has
// capacity right now"). The session decides what — if anything — the path
// should send:
//
//  1. Reinjection queue: chunks whose every transmission is on a failed /
//     suspect path, or that are long overdue, are resent first.
//  2. New data, gated by an Earliest-Completion-First rule generalised to N
//     paths: a path p may take a chunk of stream S only if p would deliver
//     it no later than the set of strictly-better paths could drain S's
//     remaining backlog (water-filling over their one-way delay and rate).
//     Small/interactive transfers therefore stay on the lowest-latency path,
//     while bulk transfers spill onto slower paths and aggregate bandwidth.
//  3. Opportunistic duplication: an otherwise idle path re-sends in-flight
//     chunks (at the head of a stream's unacked data) if it would deliver
//     them earlier than their current transmission is expected to arrive,
//     e.g. tail data on a slow path, or data whose ACK is overdue (likely
//     lost). This uses only spare capacity and cuts head-of-line blocking.

var errPathClosed = errSessionClosed

var ()

// traceSched (env MPQUIC_TRACE=1) logs every scheduling decision.
var traceSched = os.Getenv("MPQUIC_TRACE") != ""

func (s *Session) trace(p *Path, c *chunk, why string, now time.Time) {
	var b strings.Builder
	for _, q := range s.paths {
		fmt.Fprintf(&b, " [%s %s srtt=%v est=%.1fms rate=%.1fM busy=%v]", q.name, q.state, q.srtt.Round(time.Millisecond),
			q.est(len(c.data), now)*1000, q.rate()*8/1e6, q.busy)
	}
	fmt.Fprintf(os.Stderr, "%s sched %s on %s: st=%d off=%d len=%d fin=%v backlog=%d%s\n", now.Format("15:04:05.000"),
		why, p.name, c.st.id, c.off, len(c.data), c.fin, s.backlog, b.String())
}

// nextChunk blocks until p should send something. dseq >= 0 means: send the
// chunk as a datagram with that sequence number (decided here, under the
// lock, consistently with the recorded transmission).
func (s *Session) nextChunk(p *Path) (*chunk, int64, int, []uint64, error) {
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		s.mu.Lock()
		p.busy = false
		p.busyChunk = nil
		if p.state == stateDead || s.closed {
			s.mu.Unlock()
			return nil, -1, 0, nil, errPathClosed
		}
		ids := p.closeIDs
		p.closeIDs = nil
		now := time.Now()
		c, recheck, why := s.pickLocked(p, now)
		if p.meter != nil {
			p.meter.SetAppLimited(c == nil)
		}
		if c != nil {
			if traceSched {
				s.trace(p, c, why, now)
			}
			dgram := p.dgramMode() && len(c.data) <= p.dgramChunk
			due := now.Add(time.Duration(p.deadlineBound(len(c.data), dgram, now) * float64(time.Second)))
			c.sends = append(c.sends, sendRec{p: p, at: now, dgram: dgram, dueBy: due})
			dseq := int64(-1)
			if dgram {
				dseq = int64(p.dgNext)
				p.dgSent = append(p.dgSent, dgSent{seq: p.dgNext, c: c, at: now})
				p.dgNext++
				if len(p.dgSent) > 1<<15 { // bound memory; oldest presumed lost
					s.dgLostLocked(p, &p.dgSent[0], now)
					p.dgSent = p.dgSent[1:]
				}
			}
			p.busy = true
			p.busySince = now
			p.busyLen = len(c.data)
			p.busyChunk = c
			if len(c.data) >= 8<<10 && p.burstAt.IsZero() {
				p.burstAt = now
			}
			c.st.lastActive = now
			if !s.opt.NoDup && len(c.sends) == 1 && len(c.data) <= eagerMaxChunk {
				s.wakeAllLocked() // let an idle path consider a redundant copy
			}
			perStream := p.chunksPerStream()
			s.mu.Unlock()
			return c, dseq, perStream, ids, nil
		}
		s.mu.Unlock()
		if len(ids) > 0 {
			return nil, -1, 0, ids, nil
		}
		if recheck <= 0 {
			recheck = 250 * time.Millisecond
		}
		if timer == nil {
			timer = time.NewTimer(recheck)
		} else {
			timer.Reset(recheck)
		}
		select {
		case <-p.wake:
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
		case <-p.conn.Context().Done():
			return nil, -1, 0, nil, errPathClosed
		}
	}
}

// onPathAckLocked processes a PATHACK for path p (sender side): datagrams
// reported received update RACK state; datagrams that are >= 3 sequence
// numbers and a reorder window older than a received one are lost, and their
// chunks are reinjected at once (on the best path, see pickLocked).
func (s *Session) onPathAckLocked(p *Path, largest uint64, bm []byte, now time.Time) {
	var largestAt time.Time
	for i := range p.dgSent {
		if p.dgSent[i].seq == largest {
			largestAt = p.dgSent[i].at
			break
		}
	}
	received := func(seq uint64) bool {
		if seq == largest {
			return true
		}
		if seq > largest {
			return false
		}
		d := largest - 1 - seq
		return d < pathAckBits && bm[d/8]&(1<<(d%8)) != 0
	}
	lost := 0
	for i := range p.dgSent {
		e := &p.dgSent[i]
		if e.resolved || e.seq > largest {
			continue
		}
		if received(e.seq) {
			e.resolved = true
			if e.at.After(p.ackedSentMax) {
				p.ackedSentMax = e.at
			}
			continue
		}
		if largest-e.seq >= 3 && (largestAt.IsZero() || e.at.Add(p.reorderWindow()).Before(largestAt)) || largest-e.seq > pathAckBits {
			s.dgLostLocked(p, e, now)
			lost++
		}
	}
	i := 0
	for i < len(p.dgSent) && p.dgSent[i].resolved {
		i++
	}
	p.dgSent = p.dgSent[i:]
	if lost > 0 {
		s.wakeAllLocked()
	}
}

// dgLostLocked marks a datagram transmission lost and queues its chunk for
// reinjection if no other transmission of it may still arrive.
func (s *Session) dgLostLocked(p *Path, e *dgSent, now time.Time) {
	e.resolved = true
	c := e.c
	for i := range c.sends {
		if c.sends[i].p == p && c.sends[i].at.Equal(e.at) {
			c.sends[i].failed = true
		}
	}
	if c.acked || c.queued || c.st.err != nil {
		return
	}
	if _, alive := s.expectedArrivalLocked(c, now, 2, false); alive {
		return
	}
	c.queued, c.queuedAt = true, now
	s.reinjectQ = append(s.reinjectQ, c)
}

// sendFailed marks p's latest transmission of c as not having happened.
func (s *Session) sendFailed(c *chunk, p *Path) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(c.sends) - 1; i >= 0; i-- {
		if c.sends[i].p == p {
			c.sends[i].failed = true
			break
		}
	}
	s.wakeAllLocked()
}

// datagramTooLarge shrinks p's datagram chunk size to what QUIC accepts.
func (s *Session) datagramTooLarge(p *Path, maxPayload int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p.dgramChunk = max(min(p.dgramChunk, maxPayload-dgDataOverhead), 256)
}

func (s *Session) pickLocked(p *Path, now time.Time) (*chunk, time.Duration, string) {
	if !p.usable() {
		return nil, 20 * time.Millisecond, ""
	}
	// 1. reinjection queue
	for i := 0; i < len(s.reinjectQ); {
		c := s.reinjectQ[i]
		if c.acked || c.st.err != nil {
			c.queued = false
			s.reinjectQ = append(s.reinjectQ[:i], s.reinjectQ[i+1:]...)
			continue
		}
		if st := c.st; c.off > st.sq.frontOff() && st.pendingData() > 0 && st.peerWindow > st.sq.frontOff() {
			// Earliest deadline first: the stream needs its unsent front
			// before this chunk (lost far ahead of the front, e.g. on a slow
			// path). The front catches up with it soon enough.
			c.queuedAt = now // target-choice timeouts start when it is due
			i++
			continue
		}
		if s.carriedLocked(c, p, now) || !p.canCarry(c) || s.betterFreePathLocked(p, c, now) || !s.goodReinjectTargetLocked(p, c, now) {
			i++
			continue
		}
		c.queued = false
		s.reinjectQ = append(s.reinjectQ[:i], s.reinjectQ[i+1:]...)
		return c, 0, "reinject"
	}
	var recheck time.Duration
	// 2. new data
	if s.backlog > 0 && len(s.rr) > 0 {
		n := len(s.rr)
		for tries := 0; tries < n && len(s.rr) > 0; tries++ {
			if s.rrIdx >= len(s.rr) {
				s.rrIdx = 0
			}
			idx := s.rrIdx
			st := s.rr[idx]
			if st.err != nil || st.unsent <= 0 {
				st.inRR = false
				s.rr = append(s.rr[:idx], s.rr[idx+1:]...)
				continue
			}
			s.rrIdx = idx + 1
			size := 0
			front := st.sq.frontOff()
			if avail := st.pendingData(); avail > 0 {
				if st.peerWindow <= front {
					continue // flow-control blocked
				}
				size = int(min(int64(min(s.opt.ChunkSize, p.maxChunk())), avail, int64(st.peerWindow-front)))
			}
			if !s.ecfAllowLocked(p, s.streamBacklogLocked(st), size, now) {
				recheck = 5 * time.Millisecond
				continue
			}
			var c *chunk
			if size > 0 {
				// A slower path takes bytes from ahead of the front: the
				// amount the faster paths deliver while this chunk is in
				// flight (pessimistically) plus the time a rescue would take.
				// In-order delivery then never waits for this path; if the
				// chunk is late anyway it is resent on the best path before
				// the stream needs it (see sendAliveLocked).
				if k := s.lookaheadLocked(p, size, now); k >= uint64(size) {
					if off, ok := st.sq.aheadSlot(front+k, size, st.peerWindow); ok {
						c = st.carveAheadLocked(off, size)
					}
				}
			}
			if c == nil {
				c = st.carveLocked(size)
			}
			s.insureLocked(p, c, now)
			if !st.inUnacked {
				st.inUnacked = true
				s.unacked[st] = struct{}{}
			}
			return c, 0, "new"
		}
	}
	// 3. opportunistic duplication
	if len(s.unacked) > 0 {
		if !s.opt.NoDup {
			if c := s.dupCandidateLocked(p, now); c != nil {
				return c, 0, "dup"
			}
		}
		if recheck == 0 {
			recheck = 10 * time.Millisecond
		}
	}
	return nil, recheck, ""
}

// insureLocked: a small chunk of a short/interactive transfer (a request, a
// reply, the tail of a message) that was just placed on a lossy path is also
// queued for a copy on the best other path. Waiting for loss detection and a
// resend would add one or more RTTs to the transfer in, say, one case in five
// at 20% loss; the copy bounds its latency by what the other path alone
// would deliver, so the lossy path can only help. Budgeted like eager
// duplication.
func (s *Session) insureLocked(p *Path, c *chunk, now time.Time) {
	if s.opt.NoDup || c.queued || len(c.data) > eagerMaxChunk || !p.lossy() {
		return
	}
	if c.st.unsent > 0 && s.streamBacklogLocked(c.st) > 64<<10 {
		return // bulk: look-ahead placement and rescue handle it
	}
	others := false
	for _, q := range s.paths {
		if q != p && q.usable() && q.canCarry(c) {
			others = true
			break
		}
	}
	s.refillEagerLocked(now)
	cost := float64(len(c.data) + 64)
	if !others || s.eagerTokens < cost {
		return
	}
	s.eagerTokens -= cost
	c.queued, c.queuedAt = true, now
	s.reinjectQ = append(s.reinjectQ, c)
	s.wakeAllLocked()
}

// lookaheadLocked returns how far ahead of the front a size-byte chunk sent
// on p now should be placed: the in-order frontier advances at the aggregate
// rate of all usable paths (the front also skips regions carved ahead by
// slower paths), and the chunk must arrive, pessimistically, before the
// frontier reaches it, with time to spare for a rescue on the best path.
// Zero if p is the fastest path.
func (s *Session) lookaheadLocked(p *Path, size int, now time.Time) uint64 {
	// Smoothed, stable inputs: the placement distance must not jump around
	// with every RTT sample, or the slow path's chunks land in clusters and
	// in-order delivery becomes bursty.
	stable := func(q *Path) float64 {
		return q.fwdDelay(q.srttOrDefault()) + q.rttvar.Seconds() + float64(size)/q.rate() + q.lossDelay(size) + q.penalty(now) + q.late
	}
	// safe: when a chunk sent now on q has arrived even in a bad case: a
	// jitter allowance, or on lossy paths enough time to notice the loss (a
	// later packet on q acknowledged) and resend it on the best path.
	lag := s.rescueLagLocked()
	safe := func(q *Path, e float64) float64 {
		margin := 2*q.rttvar.Seconds() + 0.005
		if q.lossy() {
			margin = max(margin, q.srttOrDefault().Seconds()+q.reorderWindow().Seconds()+lag)
		}
		if q.ccMode == "startup" && q.burstSeen {
			// STARTUP overshoots: the queue (and the delay of what is sent
			// now) may grow by up to about one more RTT before BBR backs off.
			margin += q.srttOrDefault().Seconds()
		}
		return e + margin
	}
	// The front is carried by the path that delivers soonest *reliably*: a
	// lossy path with a short RTT would leave holes right at the front, so
	// it takes data from further ahead instead, like a slow path.
	ep := stable(p)
	sp := safe(p, ep)
	front, frontE := sp, ep
	R := 0.0
	for _, q := range s.paths {
		if !q.usable() {
			continue
		}
		R += q.rate()
		if q == p {
			continue
		}
		if e := stable(q); safe(q, e) < front {
			front, frontE = safe(q, e), e
		}
	}
	if front >= sp {
		return 0 // p carries the front
	}
	// The frontier passes offset front+x at about frontE + x/R; p's chunk
	// has arrived (or been rescued) by sp.
	return uint64(R * max(sp-frontE, 0))
}

// rescueLagLocked is how long it takes to notice a late chunk and resend it
// on the best path: the ACK delay and trip, plus the resend's trip.
func (s *Session) rescueLagLocked() float64 {
	best := 1.0
	for _, q := range s.paths {
		if q.usable() {
			best = min(best, q.owdTail())
		}
	}
	return s.opt.AckDelay.Seconds() + 2*best + 0.005
}

// streamBacklogLocked estimates how many bytes will be sent (by round-robin)
// until st's backlog is drained.
func (s *Session) streamBacklogLocked(st *Stream) float64 {
	if len(s.rr) > 64 {
		return float64(s.backlog)
	}
	var k int64
	for _, o := range s.rr {
		if o.unsent < st.unsent {
			k += o.unsent
		} else {
			k += st.unsent
		}
	}
	return float64(k)
}

// ecfAllowLocked decides whether p may send a new chunk of size bytes given
// a backlog of k bytes.
func (s *Session) ecfAllowLocked(p *Path, k float64, size int, now time.Time) bool {
	ep := p.est(size, now)
	better := s.tmpPaths[:0]
	for _, q := range s.paths {
		if q == p || !q.usable() {
			continue
		}
		if q.est(size, now) < ep {
			better = append(better, q)
		}
	}
	s.tmpPaths = better
	if len(better) == 0 {
		return true
	}
	return ep <= waterfill(better, k, size, now)
}

// waterfill returns the time t at which paths (each delivering at rate r_q
// starting after a_q = owd+penalty) have together delivered k bytes:
// sum_q r_q * max(0, t - a_q) = k.
func waterfill(paths []*Path, k float64, size int, now time.Time) float64 {
	type pr struct{ a, r float64 }
	var arr [8]pr
	ps := arr[:0]
	for _, q := range paths {
		ps = append(ps, pr{q.owdTail() + q.penalty(now) + q.lossDelay(size), q.rate()})
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].a < ps[j].a })
	var R, RA float64
	for i, x := range ps {
		R += x.r
		RA += x.r * x.a
		t := (k + RA) / R
		if i == len(ps)-1 || t <= ps[i+1].a {
			return t
		}
	}
	return 0
}

// goodReinjectTargetLocked: a reinjected chunk is already late, so it should
// go to (nearly) the best path that has not carried it, even if that path is
// busy for a moment, rather than to whichever path frees up first (which may
// be another lossy one). After waiting briefly, any path is acceptable.
func (s *Session) goodReinjectTargetLocked(p *Path, c *chunk, now time.Time) bool {
	tried := func(q *Path) bool {
		for _, r := range c.sends {
			if r.p == q {
				return true
			}
		}
		return false
	}
	// Do not send it back over a path that already lost it if another
	// usable path has not tried yet.
	if tried(p) {
		for _, q := range s.paths {
			if q != p && q.usable() && !tried(q) && q.canCarry(c) {
				if now.Sub(c.queuedAt) < 100*time.Millisecond {
					return false
				}
				break
			}
		}
	}
	if now.Sub(c.queuedAt) > 30*time.Millisecond {
		return true
	}
	ep := p.rescueCost(len(c.data), now)
	for _, q := range s.paths {
		if q == p || !q.usable() || s.carriedLocked(c, q, now) || !q.canCarry(c) {
			continue
		}
		wait := 0.0
		if q.busy {
			wait = float64(q.busyLen) / q.rate()
		}
		if q.rescueCost(len(c.data), now)+wait+0.005 < ep*0.8 {
			return false
		}
	}
	return true
}

// betterFreePathLocked reports whether another idle, usable path that has not
// carried c would deliver it sooner than p.
func (s *Session) betterFreePathLocked(p *Path, c *chunk, now time.Time) bool {
	ep := p.est(len(c.data), now)
	for _, q := range s.paths {
		if q == p || !q.usable() || q.busy || s.carriedLocked(c, q, now) || !q.canCarry(c) {
			continue
		}
		if q.est(len(c.data), now) < ep {
			return true
		}
	}
	return false
}

const farFuture = time.Duration(1<<62 - 1)

// sendAliveLocked reports whether transmission r of c may still arrive, and
// when. Transmissions on unusable paths, whose ACK is overdue, or that are
// overtaken by later acknowledged sends on the same path (RACK) are presumed
// lost.
// aggressive applies the short lossy-path timeout and RACK to stream sends as
// well; it is used when a copy would only consume idle capacity. Datagram
// sends always use them (nothing else will recover them), while stream sends
// are otherwise left to QUIC's own loss recovery unless long overdue.
func (s *Session) sendAliveLocked(c *chunk, r sendRec, now time.Time, overdueFactor float64, aggressive bool) (time.Time, bool) {
	q := r.p
	if r.failed || q.state != stateActive {
		return time.Time{}, false
	}
	known := c.off < c.st.sackLimit // do we have SACK information about c?
	tx := time.Duration(float64(len(c.data)) / q.rate() * float64(time.Second))
	f, v, slack := overdueFactor, 4*q.rttvar, 15*time.Millisecond
	fast := r.dgram || aggressive
	if fast && q.lossy() || r.dgram {
		// On lossy paths a late ACK most likely means a lost packet whose
		// QUIC retransmission (on the same lossy path) takes another RTT or
		// more; a datagram is never retransmitted at all. Give up sooner.
		f, v, slack = min(f, 1.25), 2*q.rttvar, 10*time.Millisecond
	}
	ackBy := r.at.Add(time.Duration(f*float64(q.srttOrDefault())) + v + tx + s.opt.AckDelay + slack)
	if known && now.After(ackBy) {
		return time.Time{}, false
	}
	// Deadline: the chunk is needed (it is a hole: data after it has
	// arrived; or the stream's front has been sent past it, so the data
	// after it is on its way) and is not acknowledged although even a
	// generous arrival time plus the ACK's trip has passed. Rescue it on the
	// best path instead of waiting for this one. (A chunk placed far ahead on
	// a slow path is not needed, and not rescued, until the front catches
	// up with it.)
	if known && !r.dueBy.IsZero() && c.neededLocked() {
		ackTrip := s.opt.AckDelay + time.Duration(s.bestOwdTailLocked()*float64(time.Second)) + 5*time.Millisecond
		// The bound is re-evaluated with the path's current RTT: if a queue
		// has built up since the send, everything sent is merely delayed
		// (and rescuing all of it would only duplicate it).
		due := r.dueBy
		if cur := r.at.Add(time.Duration(q.deadlineBound(len(c.data), r.dgram, now) * float64(time.Second))); cur.After(due) {
			due = cur
		}
		if now.After(due.Add(ackTrip)) {
			return time.Time{}, false
		}
	}
	// RACK: a chunk sent on q sufficiently later has been acknowledged while
	// this one has not, so this transmission was (most likely) lost.
	if known && q.ackedSentMax.After(r.at.Add(q.reorderWindow())) {
		// For stream sends QUIC retransmits on q itself (arriving after
		// another trip over q); only race it if another path is faster.
		if fast || s.altFasterLocked(c, q, now) {
			return time.Time{}, false
		}
	}
	return r.at.Add(time.Duration((q.owd()+q.lossDelay(len(c.data)))*float64(time.Second)) + tx), true
}

func (s *Session) bestOwdTailLocked() float64 {
	best := 1.0
	for _, q := range s.paths {
		if q.usable() {
			best = min(best, q.owdTail())
		}
	}
	return best
}

// altFasterLocked: would a path that has not carried c deliver it sooner than
// QUIC's own retransmission on q (one more trip over q, repeated on loss)?
func (s *Session) altFasterLocked(c *chunk, q *Path, now time.Time) bool {
	recovery := q.owd()/(1-q.loss()) + float64(len(c.data))/q.rate()
	for _, p := range s.paths {
		if p == q || !p.usable() || !p.canCarry(c) {
			continue
		}
		used := false
		for _, r := range c.sends {
			if r.p == p && !r.failed {
				used = true
				break
			}
		}
		if !used && p.est(len(c.data), now)+0.005 < recovery {
			return true
		}
	}
	return false
}

// expectedArrivalLocked returns when the earliest live transmission of c is
// expected to arrive (false if all transmissions are presumed lost).
func (s *Session) expectedArrivalLocked(c *chunk, now time.Time, overdueFactor float64, aggressive bool) (time.Time, bool) {
	var best time.Time
	found := false
	for _, r := range c.sends {
		if arr, ok := s.sendAliveLocked(c, r, now, overdueFactor, aggressive); ok && (!found || arr.Before(best)) {
			best, found = arr, true
		}
	}
	return best, found
}

// carriedLocked reports whether p already carries c in a way that makes
// another copy on p pointless: a stream send (QUIC retransmits it), or a
// datagram send that may still arrive.
func (s *Session) carriedLocked(c *chunk, p *Path, now time.Time) bool {
	for _, r := range c.sends {
		if r.p != p || r.failed {
			continue
		}
		if !r.dgram {
			return true
		}
		if _, ok := s.sendAliveLocked(c, r, now, 2, false); ok {
			return true
		}
	}
	return false
}

// Eager redundancy: small chunks (requests, handshakes, keystrokes) are
// duplicated onto an idle second path if the copy would arrive before a
// retransmission on the original path could. Budgeted by a token bucket.
const (
	eagerMaxChunk = 8 << 10
	eagerRate     = 512 << 10 // bytes/s
	eagerBurst    = 128 << 10
)

func (s *Session) refillEagerLocked(now time.Time) {
	if s.eagerLast.IsZero() {
		s.eagerLast = now
		s.eagerTokens = eagerBurst
		return
	}
	s.eagerTokens += now.Sub(s.eagerLast).Seconds() * eagerRate
	if s.eagerTokens > eagerBurst {
		s.eagerTokens = eagerBurst
	}
	s.eagerLast = now
}

func (s *Session) dupCandidateLocked(p *Path, now time.Time) *chunk {
	s.refillEagerLocked(now)
	for st := range s.unacked {
		// Consider the first few not-yet-acked chunks (SACKed ones are
		// skipped without counting), bounded to keep this cheap.
		cand := 0
		for i := 0; i < len(st.sendBuf) && i < 1024 && cand < 32; i++ {
			c := st.sendBuf[i]
			if c.acked {
				continue
			}
			cand++
			if c.queued || len(c.sends) >= 3 || s.carriedLocked(c, p, now) || !p.canCarry(c) {
				continue
			}
			arr, ok := s.expectedArrivalLocked(c, now, 1, true)
			mine := now.Add(time.Duration(p.est(len(c.data), now)*float64(time.Second)) + 2*time.Millisecond)
			eager := false
			if ok && !mine.Before(arr) {
				// Not faster. Eagerly duplicate small chunks if this copy
				// beats a loss recovery on the original path.
				if len(c.data) > eagerMaxChunk || len(c.sends) != 1 || s.eagerTokens < float64(len(c.data)+64) {
					continue
				}
				q := c.sends[0].p
				if !mine.Before(arr.Add(q.srttOrDefault())) {
					continue
				}
				eager = true
			}
			if s.betterFreePathLocked(p, c, now) {
				continue
			}
			if eager {
				s.eagerTokens -= float64(len(c.data) + 64)
			}
			return c
		}
	}
	return nil
}

// reinjectOverdueLocked queues head-of-line chunks whose ACK is long overdue
// on every path they were sent on (bulk mode, where no path is idle).
func (s *Session) reinjectOverdueLocked(now time.Time) bool {
	any := false
	// A chunk stuck in the send queue of a path that entered BBR ProbeRTT
	// (window cut to 4 packets for ~200 ms) is sent elsewhere right away.
	for _, p := range s.paths {
		if c := p.busyChunk; c != nil && p.benched && p.probingRTT() && !c.acked && !c.queued && c.st.err == nil {
			c.queued, c.queuedAt = true, now
			s.reinjectQ = append(s.reinjectQ, c)
			any = true
		}
	}
	for st := range s.unacked {
		lim := min(len(st.sendBuf), 4096)
		cand := 0
		for i := 0; i < lim && cand < 512; i++ {
			c := st.sendBuf[i]
			if c.acked || c.queued || len(c.sends) == 0 {
				continue
			}
			cand++
			if _, ok := s.expectedArrivalLocked(c, now, 2, false); ok {
				continue
			}
			c.queued = true
			c.queuedAt = now
			s.reinjectQ = append(s.reinjectQ, c)
			any = true
		}
	}
	return any
}

// requeueLocked queues every unacked chunk none of whose transmissions is on
// an active path (after a path failed or became suspect).
func (s *Session) requeueLocked(now time.Time) {
	for st := range s.unacked {
		for i := 0; i < len(st.sendBuf); i++ {
			c := st.sendBuf[i]
			if c.acked || c.queued {
				continue
			}
			live := false
			for _, r := range c.sends {
				if r.p.state == stateActive {
					live = true
					break
				}
			}
			if !live {
				c.queued = true
				c.queuedAt = now
				s.reinjectQ = append(s.reinjectQ, c)
			}
		}
	}
	if len(s.reinjectQ) > 1 {
		sort.SliceStable(s.reinjectQ, func(i, j int) bool {
			a, b := s.reinjectQ[i], s.reinjectQ[j]
			if a.st.id != b.st.id {
				return a.st.id < b.st.id
			}
			return a.off < b.off
		})
	}
}
