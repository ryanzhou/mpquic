package mp

import (
	"fmt"
	"math"
	"math/rand"
	"testing"
	"time"

	"mpquic/internal/cc"
)

func sbTestSession(rtts ...time.Duration) *Session {
	s := &Session{opt: &Options{}, log: testLogger()}
	for i, r := range rtts {
		s.paths = append(s.paths, &Path{sess: s, id: i + 1, name: fmt.Sprint("p", i), meter: &cc.Meter{},
			state: stateActive, minRTT: r, latestRTT: r})
	}
	return s
}

// feedDelays runs the detector for d with the queueing delays given by q
// (one per path, in seconds) at each 10 ms tick, with all paths busy.
func feedDelays(s *Session, start time.Time, d time.Duration, q func(i int, t time.Duration) float64) time.Time {
	s.backlog = 1 << 20
	now := start
	var acked uint64
	for t := time.Duration(0); t < d; t += tickInterval {
		now = start.Add(t)
		acked += 100 << 10
		for i, p := range s.paths {
			p.latestRTT = p.minRTT + time.Duration(q(i, t)*float64(time.Second))
			p.sbs.push(now, acked)
		}
		s.sb.lastAt = now.Add(-tickInterval)
		var live []*Path
		live = append(live, s.paths...)
		if len(s.sb.pairs) == 0 {
			for i, a := range live {
				for _, b := range live[i+1:] {
					s.sb.pairs = append(s.sb.pairs, &sbPair{a: a, b: b})
				}
			}
		}
		for _, pr := range s.sb.pairs {
			s.sbSampleLocked(pr, 1-math.Exp(-tickInterval.Seconds()/sbTau.Seconds()), now)
		}
	}
	return now
}

// ar is a bounded random walk: a queue that BBR's probing makes go up and
// down by a few ms.
func ar(rng *rand.Rand) func() float64 {
	x := 0.005
	return func() float64 {
		x += rng.NormFloat64() * 0.0015
		x = min(max(x, 0), 0.02)
		return x
	}
}

func TestSharedBottleneckDetection(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	// The same queue (plus measurement noise) on both paths: shared.
	s := sbTestSession(20*time.Millisecond, 40*time.Millisecond)
	shared := ar(rng)
	var cur float64
	now := feedDelays(s, time.Now(), 3*time.Second, func(i int, _ time.Duration) float64 {
		if i == 0 {
			cur = shared()
		}
		return max(cur+rng.NormFloat64()*0.0003, 0)
	})
	if !s.sb.pairs[0].shared {
		t.Fatalf("common queue not detected: corr %.2f", s.sb.pairs[0].corr())
	}
	// Then each path has a queue of its own: split after sbIndepFor.
	qa, qb := ar(rng), ar(rng)
	feedDelays(s, now, time.Second, func(i int, _ time.Duration) float64 {
		if i == 0 {
			return qa()
		}
		return qb()
	})
	if !s.sb.pairs[0].shared {
		t.Fatalf("split too early")
	}
	feedDelays(s, now.Add(time.Second), 8*time.Second, func(i int, _ time.Duration) float64 {
		if i == 0 {
			return qa()
		}
		return qb()
	})
	if s.sb.pairs[0].shared {
		t.Fatalf("independent queues still coupled: corr %.2f", s.sb.pairs[0].corr())
	}

	// Independent from the start: never coupled.
	s = sbTestSession(20*time.Millisecond, 20*time.Millisecond, 30*time.Millisecond)
	q := []func() float64{ar(rng), ar(rng), ar(rng)}
	feedDelays(s, time.Now(), 10*time.Second, func(i int, _ time.Duration) float64 { return q[i]() })
	for _, pr := range s.sb.pairs {
		if pr.shared {
			t.Fatalf("independent paths %s/%s detected as shared (corr %.2f)", pr.a.name, pr.b.name, pr.corr())
		}
	}

	// No varying queue at all: no verdict.
	s = sbTestSession(20*time.Millisecond, 30*time.Millisecond)
	feedDelays(s, time.Now(), 5*time.Second, func(int, time.Duration) float64 { return 0.002 })
	if s.sb.pairs[0].shared {
		t.Fatalf("constant delay detected as shared")
	}
}

func TestSharedBottleneckCaps(t *testing.T) {
	s := sbTestSession(10*time.Millisecond, 20*time.Millisecond, 30*time.Millisecond)
	s.backlog = 1 << 20
	g := &sbGroup{members: s.paths, ids: []int{1, 2, 3}}
	start := time.Now()
	mb := func(m float64) uint64 { return uint64(m * 1e6 / 8) }
	// The group delivers 150 Mbit/s: 100 + 40 + 10.
	rates := []float64{100e6 / 8, 40e6 / 8, 10e6 / 8}
	k := 0
	now := start
	acked := make([]float64, len(rates))
	run := func(ticks int, queue time.Duration) {
		for end := k + ticks; k < end; k++ {
			now = start.Add(time.Duration(k) * tickInterval)
			for i, p := range s.paths {
				p.latestRTT = p.minRTT + queue
				acked[i] += rates[i] * tickInterval.Seconds()
				p.sbs.push(now, uint64(acked[i]))
			}
			s.groupCapsLocked(g, now)
		}
	}
	caps := func(raw ...float64) []float64 {
		for i, p := range s.paths {
			p.sbs.rawBw = mb(raw[i])
		}
		run(1, 0)
		var c []float64
		for _, p := range s.paths {
			c = append(c, math.Round(float64(p.sbs.cap)*8/1e6))
		}
		return c
	}
	for _, p := range s.paths {
		p.sbs.rawBw = mb(140)
	}
	// No busy bottleneck seen yet: no caps.
	run(30, 0)
	if g.c != 0 || fmt.Sprint(caps(140, 120, 130)) != "[0 0 0]" {
		t.Fatalf("capped before the bottleneck was seen busy: C=%.1f", g.c*8/1e6)
	}
	// A queue that every path sees for a whole window: the capacity is
	// what the group delivers, and the group drains.
	run(20, 10*time.Millisecond)
	if c := g.c * 8 / 1e6; math.Abs(c-150) > 2 || !g.draining {
		t.Fatalf("capacity %.1f Mbit/s (want 150), draining=%v", c, g.draining)
	}
	run(1, 0)
	if g.draining {
		t.Fatalf("still draining without a queue")
	}
	// Each path's own estimate covers the whole bottleneck: equal shares.
	if c := caps(140, 120, 130); fmt.Sprint(c) != "[50 50 50]" {
		t.Errorf("caps %v, want [50 50 50]", c)
	}
	// A path limited to 20 Mbit/s by its own link is not capped, the
	// others share the rest.
	if c := caps(140, 20, 130); fmt.Sprint(c) != "[65 0 65]" {
		t.Errorf("caps %v, want [65 0 65]", c)
	}
	// A path in BBR ProbeRTT (or failing) is not counted: the others take
	// its share.
	s.paths[0].benched = true
	if c := caps(140, 120, 130); fmt.Sprint(c) != "[0 75 75]" {
		t.Errorf("caps %v, want [0 75 75]", c)
	}
	s.paths[0].benched = false
	// A queue that does not go away while draining (cross traffic): the
	// group stops draining for a while.
	run(20, 10*time.Millisecond)
	if !g.draining {
		t.Fatalf("not draining a standing queue")
	}
	run(40, 10*time.Millisecond)
	if g.draining {
		t.Fatalf("still draining a queue that does not respond")
	}

	// No queue while the paths send at their caps: spare capacity, the
	// estimate is probed upwards.
	c0 := g.c
	run(50, 0)
	if g.c < 1.2*c0 {
		t.Errorf("no probing for spare capacity: C %.1f -> %.1f Mbit/s", c0*8/1e6, g.c*8/1e6)
	}
	// The bottleneck now delivers well below what the group sends: the
	// capacity dropped, and the estimate follows at once.
	for i := range rates {
		rates[i] /= 2
	}
	run(30, 20*time.Millisecond)
	if c := g.c * 8 / 1e6; c > 80 {
		t.Errorf("capacity drop to 75 Mbit/s not followed: C %.1f Mbit/s", c)
	}
}
