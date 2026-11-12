package mp

import (
	"math"
	"testing"
	"time"

	"mpquic/internal/cc"
)

func mbit(x float64) uint64 { return uint64(x * 1e6 / 8) }

// limitSession is a session with n active paths that are still starting up.
func limitSession(n int) *Session {
	s := &Session{opt: &Options{}, log: testLogger()}
	for i := range n {
		s.paths = append(s.paths, &Path{sess: s, id: i + 1, meter: &cc.Meter{}, state: stateActive, ccMode: "startup"})
	}
	return s
}

func caps(s *Session) (each []uint64, sum uint64) {
	s.shareLimitLocked()
	for _, p := range s.paths {
		each = append(each, p.meter.RateCap())
		sum += p.meter.RateCap()
	}
	return
}

func near(a, b uint64) bool { return math.Abs(float64(a)-float64(b)) <= 2 }

func TestRateLimitShares(t *testing.T) {
	s := limitSession(3)
	if each, _ := caps(s); each[0] != 0 || each[1] != 0 || each[2] != 0 {
		t.Fatalf("no limit configured, but bounds %v", each)
	}

	// Paths that are finding their rates get equal shares.
	limit := mbit(30)
	s.opt.MaxSendRate = limit
	each, sum := caps(s)
	for i, c := range each {
		if !near(c, limit/3) {
			t.Fatalf("path %d: bound %d, want a third of %d", i, c, limit)
		}
	}
	if sum > limit+3 {
		t.Fatalf("bounds add up to %d, limit %d", sum, limit)
	}

	// A path that carries less than its share is bound to what it can use
	// (BBR's probing gain times its estimate), and the others share the
	// rest.
	for _, p := range s.paths {
		p.ccMode = "probe_bw"
	}
	s.paths[0].bw, s.paths[1].bw, s.paths[2].bw = mbit(2), mbit(9.9), mbit(9.9)
	each, sum = caps(s)
	if !near(each[0], mbit(2.5)) || !near(each[1], mbit(12.375)) || !near(each[2], mbit(12.375)) {
		t.Fatalf("bounds %v, want 2.5 + 12.375 + 12.375 Mbit/s (1.25 times the estimates)", each)
	}
	if sum > limit+3 {
		t.Fatalf("bounds add up to %d, limit %d", sum, limit)
	}

	// As the others' estimates grow, so do their bounds, up to equal shares
	// of what the slow path leaves; when the slow path turns out to carry
	// more, it grows up to an equal share of everything.
	s.paths[1].bw, s.paths[2].bw = mbit(13.6), mbit(13.6)
	if each, _ = caps(s); !near(each[1], mbit(13.75)) || !near(each[2], mbit(13.75)) {
		t.Fatalf("bounds %v, want 2.5 + 13.75 + 13.75 Mbit/s", each)
	}
	s.paths[0].bw = mbit(20)
	each, sum = caps(s)
	for i, c := range each {
		if !near(c, limit/3) {
			t.Fatalf("path %d: bound %d, want a third of %d (bounds %v)", i, c, limit, each)
		}
	}

	// A path that is not carrying data has no share; a dead one is left
	// alone.
	s.paths[2].state = stateSuspect
	each, sum = caps(s)
	if each[2] != 0 || !near(each[0], limit/2) || !near(each[1], limit/2) {
		t.Fatalf("bounds %v with the third path suspect, want two halves of %d", each, limit)
	}
	s.paths[2].state = stateDead
	if each, _ = caps(s); each[2] != 0 {
		t.Fatalf("dead path bound to %d", each[2])
	}

	// The peer's receive limit counts too: the lower one wins.
	if err := s.onCtrl(s.paths[0], frLimit, encodeU64s(0, limit/3)[2:]); err != nil {
		t.Fatal(err)
	}
	if _, sum = caps(s); sum > limit/3+3 || sum < limit/3-3 {
		t.Fatalf("bounds add up to %d with a peer limit of %d", sum, limit/3)
	}
	// ... and it can be withdrawn, as can ours.
	if err := s.onCtrl(s.paths[0], frLimit, encodeU64s(0, 0)[2:]); err != nil {
		t.Fatal(err)
	}
	s.opt.MaxSendRate = 0
	if each, _ = caps(s); each[0] != 0 || each[1] != 0 {
		t.Fatalf("limit withdrawn, but bounds %v", each)
	}
}

// Whatever the paths' estimates are, the bounds never add up to more than
// the limit.
func TestRateLimitNeverExceeded(t *testing.T) {
	limit := mbit(10)
	for n := 1; n <= 6; n++ {
		s := limitSession(n)
		s.opt.MaxSendRate = limit
		for step := range 200 {
			for i, p := range s.paths {
				// estimates all over the place, some paths still starting
				p.bw = uint64((step*7919+i*104729)%1000) * limit / 400
				p.ccMode = "probe_bw"
				if (step+i)%7 == 0 {
					p.ccMode = "startup"
				}
			}
			each, sum := caps(s)
			if sum > limit+uint64(n) {
				t.Fatalf("%d paths, step %d: bounds %v add up to %d, limit %d", n, step, each, sum, limit)
			}
			for i, c := range each {
				if c == 0 {
					t.Fatalf("%d paths, step %d: path %d has no bound", n, step, i)
				}
			}
		}
	}
}

// A path's loss tolerance is the larger of what the two ends have
// configured for it: the dialer's per-server value reaches the listener.
func TestLossToleranceAnnounced(t *testing.T) {
	tb := newTestbed(t, []string{"delay=5ms,rate=50mbit", "delay=10ms,rate=50mbit", "delay=15ms,rate=50mbit"}, echoHandler,
		func(o *Options) {
			o.LossTolerance = 0.05
			o.PathLossTolerance = []float64{0, 0.2}
		})
	want := map[int]float64{0: 0.05, 1: 0.2, 2: 0.05}
	check := func(stats []PathStat) bool {
		if len(stats) != len(want) {
			return false
		}
		for _, st := range stats {
			if math.Abs(st.LossTolerance-want[st.Index]) > 1e-6 {
				return false
			}
		}
		return true
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var remote []PathStat
		for _, st := range tb.srv.Stats() {
			remote = st
		}
		local := tb.cl.Session().Stats()
		if check(local) && check(remote) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("loss tolerances: dialer %+v, listener %+v, want %v by path index", tolerances(local), tolerances(remote), want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func tolerances(stats []PathStat) map[int]float64 {
	m := map[int]float64{}
	for _, st := range stats {
		m[st.Index] = st.LossTolerance
	}
	return m
}
