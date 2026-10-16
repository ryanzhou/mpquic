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

func (s *Session) nextChunk(p *Path) (*chunk, []uint64, error) {
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		s.mu.Lock()
		p.busy = false
		if p.state == stateDead || s.closed {
			s.mu.Unlock()
			return nil, nil, errPathClosed
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
			c.sends = append(c.sends, sendRec{p, now})
			p.busy = true
			p.busySince = now
			p.busyLen = len(c.data)
			c.st.lastActive = now
			if !s.opt.NoDup && len(c.sends) == 1 && len(c.data) <= eagerMaxChunk {
				s.wakeAllLocked() // let an idle path consider a redundant copy
			}
			s.mu.Unlock()
			return c, ids, nil
		}
		s.mu.Unlock()
		if len(ids) > 0 {
			return nil, ids, nil
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
			return nil, nil, errPathClosed
		}
	}
}

func (s *Session) pickLocked(p *Path, now time.Time) (*chunk, time.Duration, string) {
	if p.state != stateActive {
		return nil, 250 * time.Millisecond, ""
	}
	// 1. reinjection queue
	for i := 0; i < len(s.reinjectQ); {
		c := s.reinjectQ[i]
		if c.acked || c.st.err != nil {
			c.queued = false
			s.reinjectQ = append(s.reinjectQ[:i], s.reinjectQ[i+1:]...)
			continue
		}
		if c.sentOn(p) || s.betterFreePathLocked(p, c, now) {
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
			if st.err != nil || st.nextUnsent >= len(st.sendBuf) {
				st.inRR = false
				s.rr = append(s.rr[:idx], s.rr[idx+1:]...)
				continue
			}
			s.rrIdx = idx + 1
			c := st.sendBuf[st.nextUnsent]
			if len(c.data) > 0 && c.off+uint64(len(c.data)) > st.peerWindow {
				continue // flow-control blocked
			}
			if !s.ecfAllowLocked(p, s.streamBacklogLocked(st), len(c.data), now) {
				recheck = 5 * time.Millisecond
				continue
			}
			st.nextUnsent++
			sz := int64(len(c.data))
			if c.fin {
				sz++
			}
			st.unsent -= sz
			s.backlog -= sz
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
		if q == p || q.state != stateActive {
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
	return ep <= waterfill(better, k, now)
}

// waterfill returns the time t at which paths (each delivering at rate r_q
// starting after a_q = owd+penalty) have together delivered k bytes:
// sum_q r_q * max(0, t - a_q) = k.
func waterfill(paths []*Path, k float64, now time.Time) float64 {
	type pr struct{ a, r float64 }
	var arr [8]pr
	ps := arr[:0]
	for _, q := range paths {
		ps = append(ps, pr{q.owd() + q.penalty(now), q.rate()})
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

// betterFreePathLocked reports whether another idle, usable path that has not
// carried c would deliver it sooner than p.
func (s *Session) betterFreePathLocked(p *Path, c *chunk, now time.Time) bool {
	ep := p.est(len(c.data), now)
	for _, q := range s.paths {
		if q == p || q.state != stateActive || q.busy || c.sentOn(q) {
			continue
		}
		if q.est(len(c.data), now) < ep {
			return true
		}
	}
	return false
}

const farFuture = time.Duration(1<<62 - 1)

// expectedArrivalLocked returns when the earliest live transmission of c is
// expected to arrive; transmissions on unusable paths, or whose ACK is
// overdue, are disregarded (treated as lost).
func (s *Session) expectedArrivalLocked(c *chunk, now time.Time, overdueFactor float64) (time.Time, bool) {
	var best time.Time
	found := false
	known := c.off < c.st.sackLimit // do we have SACK information about c?
	for _, r := range c.sends {
		q := r.p
		if q.state != stateActive {
			continue
		}
		tx := time.Duration(float64(len(c.data)) / q.rate() * float64(time.Second))
		ackBy := r.at.Add(time.Duration(overdueFactor*float64(q.srttOrDefault())) + 4*q.rttvar + tx + s.opt.AckDelay + 15*time.Millisecond)
		if known && now.After(ackBy) {
			continue
		}
		arr := r.at.Add(q.schedRTT()/2 + tx)
		if !found || arr.Before(best) {
			best, found = arr, true
		}
	}
	return best, found
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
		for i := 0; i < st.nextUnsent && i < 1024 && cand < 32; i++ {
			c := st.sendBuf[i]
			if c.acked {
				continue
			}
			cand++
			if c.queued || len(c.sends) >= 3 || c.sentOn(p) {
				continue
			}
			arr, ok := s.expectedArrivalLocked(c, now, 1)
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
	for st := range s.unacked {
		lim := st.nextUnsent
		if lim > 128 {
			lim = 128
		}
		for i := 0; i < lim; i++ {
			c := st.sendBuf[i]
			if c.acked || c.queued || len(c.sends) == 0 {
				continue
			}
			if _, ok := s.expectedArrivalLocked(c, now, 2); ok {
				continue
			}
			c.queued = true
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
		for i := 0; i < st.nextUnsent; i++ {
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
