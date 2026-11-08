package procs

import (
	"runtime"
	"slices"
	"testing"
	"time"
)

// fake drives a governor with a synthetic CPU clock: load is the CPU usage
// (in cores) from now on.
type fake struct {
	now  time.Time
	cpu  time.Duration
	load float64
	set  []int // GOMAXPROCS values set
	// scheduling latency: lat(n) is the mean with n Ps (nil: no samples)
	lat    func(n int) time.Duration
	latN   uint64
	latSum float64
	g      *governor
}

func (f *fake) governor(max int) *governor {
	g := &governor{
		cpu:      func() (time.Duration, bool) { return f.cpu, true },
		setProcs: func(n, _ int) { f.set = append(f.set, n) },
		max:      max,
	}
	g.latency = func() (uint64, float64) { return f.latN, f.latSum }
	f.g = g
	g.start(f.now)
	return g
}

func (f *fake) run(g *governor, d time.Duration) {
	for end := f.now.Add(d); f.now.Before(end); {
		f.now = f.now.Add(tick)
		f.cpu += time.Duration(f.load * float64(tick))
		if f.lat != nil {
			f.latN += 1000
			f.latSum += 1000 * f.lat(g.n).Seconds()
		}
		g.step(f.now)
	}
}

func TestGovernor(t *testing.T) {
	f := &fake{now: time.Unix(1000, 0)}
	g := f.governor(8)
	if g.n != 1 || !slices.Equal(f.set, []int{1}) {
		t.Fatalf("does not start on one P: %v", f.set)
	}
	// Moderate load stays on one P.
	f.load = 0.6
	f.run(g, 10*time.Second)
	if g.n != 1 {
		t.Fatalf("%d Ps at moderate load", g.n)
	}
	// Saturation doubles the Ps within about upWindow, step by step.
	f.load = 0.97
	f.run(g, upWindow+tick)
	if g.n != 2 {
		t.Fatalf("%d Ps after saturating one, want 2", g.n)
	}
	// 2 Ps with headroom: stay (and do not drop back: 1.3 > 0.4).
	f.load = 1.3
	f.run(g, 20*time.Second)
	if g.n != 2 {
		t.Fatalf("%d Ps at 1.3 cores, want 2", g.n)
	}
	// Heavier load climbs to the maximum.
	f.load = 7
	f.run(g, 3*time.Second)
	if g.n != 8 {
		t.Fatalf("%d Ps at full load, want 8", g.n)
	}
	// The same work at a lower load steps down, one halving at a time.
	f.load = 1.4 // fits 4 Ps (< 0.4*8/2) but not 2 (>= 0.4*4/2)
	f.run(g, 20*time.Second)
	if g.n != 4 {
		t.Fatalf("%d Ps at 1.4 cores, want 4", g.n)
	}
	// A short dip does not step down: (2 s at 0.2 + 3 s at 1.4) / 5 s >= 0.8.
	f.load = 0.2
	f.run(g, 2*time.Second)
	if g.n != 4 {
		t.Fatalf("stepped down on a short dip (%d Ps)", g.n)
	}
	f.run(g, 3*downWindow)
	if g.n != 1 {
		t.Fatalf("%d Ps at low load, want 1", g.n)
	}
	if want := []int{1, 2, 4, 8, 4, 2, 1}; !slices.Equal(f.set, want) {
		t.Fatalf("GOMAXPROCS steps %v, want %v", f.set, want)
	}
}

func TestGovernorOddMax(t *testing.T) {
	f := &fake{now: time.Unix(1000, 0)}
	g := f.governor(3)
	f.load = 3
	f.run(g, 3*time.Second)
	f.load = 0
	f.run(g, 30*time.Second)
	if want := []int{1, 2, 3, 1}; !slices.Equal(f.set, want) {
		t.Fatalf("GOMAXPROCS steps %v, want %v", f.set, want)
	}
}

func TestGovernorLatency(t *testing.T) {
	f := &fake{now: time.Unix(1000, 0)}
	g := f.governor(8)
	// Goroutines queue with up to 2 Ps, at a moderate CPU load.
	f.load = 0.9
	f.lat = func(n int) time.Duration {
		if n <= 2 {
			return 300 * time.Microsecond / time.Duration(n)
		}
		return 40 * time.Microsecond
	}
	f.run(g, 3*time.Second)
	if g.n != 4 {
		t.Fatalf("%d Ps with queueing below 4, want 4", g.n)
	}
	// 40 µs is not low enough to try fewer Ps, even at low CPU.
	f.load = 0.3
	f.run(g, 30*time.Second)
	if g.n != 4 {
		t.Fatalf("%d Ps, want 4 (latency not low)", g.n)
	}
	// Latency drops: it tries 2, which queues again; back to 4 and held.
	f.lat = func(n int) time.Duration {
		if n <= 2 {
			return 300 * time.Microsecond
		}
		return 10 * time.Microsecond
	}
	for i := 0; i < 40 && g.hold == 0; i++ {
		f.run(g, tick)
	}
	if g.n != 4 || g.hold != minHold {
		t.Fatalf("%d Ps, hold %v after rebound, want 4, %v", g.n, g.hold, minHold)
	}
	before := len(f.set)
	f.run(g, minHold-time.Second)
	if len(f.set) != before {
		t.Fatalf("changed within the hold: %v", f.set[before:])
	}
	// After the hold it tries again, rebounds, and holds twice as long.
	f.run(g, 8*time.Second)
	if g.n != 4 || g.hold != 2*minHold {
		t.Fatalf("%d Ps, hold %v after second rebound, want 4, %v", g.n, g.hold, 2*minHold)
	}
	// Once fewer Ps no longer queue, it steps down for good.
	f.lat = func(int) time.Duration { return 5 * time.Microsecond }
	f.run(g, 2*minHold+3*downWindow)
	if g.n != 1 {
		t.Fatalf("%d Ps at low load and latency, want 1", g.n)
	}
}

func TestStartStop(t *testing.T) {
	t.Setenv("GOMAXPROCS", "")
	def := runtime.GOMAXPROCS(0)
	_, ok := processCPU()
	active := ok && def > 1
	stop1 := Start(nil)
	stop2 := Start(nil)
	if active && runtime.GOMAXPROCS(0) != 1 {
		t.Fatalf("GOMAXPROCS %d after Start, want 1", runtime.GOMAXPROCS(0))
	}
	stop1()
	stop1() // idempotent
	if active && runtime.GOMAXPROCS(0) != 1 {
		t.Fatal("first stop ended the shared governor")
	}
	stop2()
	if got := runtime.GOMAXPROCS(0); got != def {
		t.Fatalf("GOMAXPROCS %d after stop, want %d", got, def)
	}
}

func TestProcessCPU(t *testing.T) {
	c0, ok := processCPU()
	if !ok {
		t.Skip("process CPU time not available")
	}
	for end := time.Now().Add(50 * time.Millisecond); time.Now().Before(end); {
	}
	c1, _ := processCPU()
	if c1 <= c0 {
		t.Fatalf("CPU time did not advance: %v -> %v", c0, c1)
	}
}
