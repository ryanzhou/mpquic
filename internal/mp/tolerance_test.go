package mp

import (
	"math"
	"testing"
	"time"
)

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
