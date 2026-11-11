package mp

import (
	"testing"
	"time"
)

// With a configured limit below the capacity: does the session ever pace below the limit?
func TestRegressLimitObserveDip(t *testing.T) {
	c := mbit(20)
	for _, n := range []int{1, 3} {
		own := []float64{c * 4, c * 4, c * 4}[:n]
		base := []time.Duration{80 * time.Millisecond, 200 * time.Millisecond, 320 * time.Millisecond}[:n]
		x := newCPSim(c, 0.3, own, base...)
		limit := c * 0.9
		x.s.opt.MaxSendRate = uint64(limit)
		x.run(5 * time.Second)
		x.sent = 0
		ticks, low := 0, 0
		minTot := limit
		gains := map[float64]int{}
		phases := map[cpPhase]int{}
		for i := 0; i < 4000; i++ {
			x.run(tickInterval)
			ticks++
			tot := x.total()
			if tot < 0.99*limit {
				low++
			}
			minTot = min(minTot, tot)
			gains[x.s.paths[0].meter.RateGain()]++
			phases[x.s.cp.phase]++
		}
		t.Logf("paths=%d: sent %.2f%% of limit over 40s; ticks below 99%% of limit: %d/%d; min total %.1f%% of limit; gains=%v phases=%v B=%.1f Mbit",
			n, 100*x.sent/(limit*40), low, ticks, 100*minTot/limit, gains, phases, x.s.cp.bw*8/1e6)
		// drain rounds (0.75, at most one in eight) remain; probes do not
		if low > ticks/20 || x.sent < 0.985*limit*40 {
			t.Errorf("paths=%d: a configured limit is probed: %d of %d ticks below it, %.1f%% of it sent", n, low, ticks, 100*x.sent/(limit*40))
		}
	}
}

// A limit below what the paths' minimum shares add up to is still obeyed.
func TestRegressLowLimit(t *testing.T) {
	c := mbit(1)
	x := newCPSim(mbit(100), 0.3, []float64{mbit(50), mbit(50), mbit(50), mbit(50)}, 10*time.Millisecond, 10*time.Millisecond, 20*time.Millisecond, 20*time.Millisecond)
	x.s.opt.MaxSendRate = uint64(c)
	x.run(5 * time.Second)
	x.sent = 0
	x.run(10 * time.Second)
	if got := x.sent / 10; got > 1.05*c {
		t.Errorf("limit 1 Mbit/s, 4 paths: sent %.2f Mbit/s", got*8/1e6)
	}
}
