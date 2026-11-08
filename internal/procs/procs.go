// Package procs adapts GOMAXPROCS to the load of the process.
//
// Tunnel traffic is a chain of short goroutine hand-offs per packet: the UDP
// socket reader, the QUIC connection loop, its send queue, the stream reader
// and the proxied connection. With idle CPUs around, every hand-off wakes
// another OS thread (a futex wake; the woken thread spins looking for work
// and goes back to sleep), and the goroutines bounce between CPUs. At
// moderate rates this costs as much as the packet processing itself: at
// 100 Mbit/s the local and remote used ~1.8x the CPU with GOMAXPROCS=8 than
// with GOMAXPROCS=1, at the same throughput and latency. Peak throughput
// suffers too: a single path peaked at ~1.4 Gbit/s with 2 Ps and ~0.9 with 8.
//
// The governor therefore starts with one P and doubles the number of Ps,
// up to the runtime default (all CPUs, or the container's limit), whenever
// they are short: busy (CPU usage near the number of Ps), or with goroutines
// queueing for them (a mean scheduling latency of 100 µs; a single busy
// path stays at a few µs even on a saturated P, several paths with parallel
// streams queue). It halves the number again once the load fits in half.
// The same load costs less CPU with fewer Ps and the CPU thresholds are a
// factor of two apart; scheduling latency cannot be predicted for fewer Ps,
// so stepping down is held off (with backoff) where that turned out to be
// short. Setting GOMAXPROCS in the environment disables the governor.
package procs

import (
	"fmt"
	"log/slog"
	"math"
	"os"
	"runtime"
	"runtime/metrics"
	"sync"
	"time"
)

const (
	tick = 250 * time.Millisecond
	// Doubling: over upWindow, a process CPU usage (in cores) of at least
	// upLoad*n with n Ps, or a mean scheduling latency of at least upLatency.
	upLoad    = 0.8
	upLatency = 100 * time.Microsecond
	upWindow  = 500 * time.Millisecond
	// Halving: over downWindow, a usage below downLoad*n/2 (half the
	// threshold to double again) and a mean latency below downLatency.
	downLoad    = 0.4
	downLatency = 30 * time.Microsecond
	downWindow  = 5 * time.Second
	// latMinSamples: fewer latency samples in a window are not conclusive.
	latMinSamples = 50
	// A doubling within reboundWindow of a halving holds off halving for
	// hold, which doubles on every such rebound (up to maxHold).
	reboundWindow = 10 * time.Second
	minHold       = 30 * time.Second
	maxHold       = 10 * time.Minute
)

var (
	mu    sync.Mutex
	users int
	stopC chan struct{}
	doneC chan struct{}
)

// Start runs the governor until the returned function is called. Nested
// calls share one governor, which stops (restoring the default GOMAXPROCS)
// when the last caller stops. It does nothing if GOMAXPROCS is set in the
// environment, if only one CPU is available, or if the platform does not
// report the process CPU time.
func Start(log *slog.Logger) (stop func()) {
	mu.Lock()
	defer mu.Unlock()
	users++
	if users == 1 && enabled() {
		stopC, doneC = make(chan struct{}), make(chan struct{})
		g := &governor{cpu: processCPU, latency: newLatencyReader(), setProcs: setProcs, max: runtime.GOMAXPROCS(0), log: log}
		g.start(time.Now())
		go g.run(stopC, doneC)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			mu.Lock()
			defer mu.Unlock()
			users--
			if users == 0 && stopC != nil {
				close(stopC)
				<-doneC
				stopC, doneC = nil, nil
			}
		})
	}
}

func enabled() bool {
	if os.Getenv("GOMAXPROCS") != "" || runtime.GOMAXPROCS(0) <= 1 {
		return false
	}
	_, ok := processCPU()
	return ok
}

func (g *governor) run(stop, done chan struct{}) {
	defer close(done)
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			g.step(time.Now())
		case <-stop:
			if g.n < g.max {
				g.setProcs(g.max, g.max)
			}
			return
		}
	}
}

// setProcs sets GOMAXPROCS to n; n = max restores the runtime default
// (which also resumes the runtime's tracking of the CPU limit).
func setProcs(n, max int) {
	if n >= max {
		runtime.SetDefaultGOMAXPROCS()
		return
	}
	runtime.GOMAXPROCS(n)
}

// newLatencyReader returns a reader of the cumulative scheduling latency
// (the time goroutines wait to run once runnable, as sampled by the
// runtime): sample count and sum in seconds.
func newLatencyReader() func() (uint64, float64) {
	s := []metrics.Sample{{Name: "/sched/latencies:seconds"}}
	return func() (uint64, float64) {
		metrics.Read(s)
		if s[0].Value.Kind() != metrics.KindFloat64Histogram {
			return 0, 0
		}
		h := s[0].Value.Float64Histogram()
		var n uint64
		var sum float64
		for i, c := range h.Counts {
			if c == 0 {
				continue
			}
			lo, hi := h.Buckets[i], h.Buckets[i+1]
			v := (lo + hi) / 2
			if math.IsInf(lo, -1) {
				v = hi
			} else if math.IsInf(hi, 1) {
				v = lo
			}
			n += c
			sum += float64(c) * v
		}
		return n, sum
	}
}

type sample struct {
	t      time.Time
	cpu    time.Duration
	latN   uint64
	latSum float64
}

type governor struct {
	cpu      func() (time.Duration, bool)
	latency  func() (uint64, float64) // nil: CPU only
	setProcs func(n, max int)
	max      int // the runtime default
	log      *slog.Logger

	n        int       // current GOMAXPROCS
	since    time.Time // last change
	downAt   time.Time // last halving
	hold     time.Duration
	holdTill time.Time
	samples  []sample // in time order
}

func (g *governor) start(now time.Time) {
	g.set(1, now, "start", 0)
	g.record(now)
}

func (g *governor) record(now time.Time) bool {
	c, ok := g.cpu()
	if !ok {
		return false
	}
	s := sample{t: now, cpu: c}
	if g.latency != nil {
		s.latN, s.latSum = g.latency()
	}
	g.samples = append(g.samples, s)
	// Keep the newest sample at or before now-downWindow, and all after it.
	k := 0
	for k+1 < len(g.samples) && !g.samples[k+1].t.After(now.Add(-downWindow)) {
		k++
	}
	g.samples = g.samples[k:]
	return true
}

// window returns the newest sample at least w old, taken since the last
// change, and the latest sample.
func (g *governor) window(now time.Time, w time.Duration) (from, to sample, ok bool) {
	n := len(g.samples)
	if n < 2 {
		return
	}
	to = g.samples[n-1]
	for i := n - 2; i >= 0; i-- {
		s := g.samples[i]
		if s.t.Before(g.since) {
			break
		}
		if now.Sub(s.t) >= w {
			return s, to, true
		}
	}
	return
}

// usage is the CPU usage in cores over (at least) the last w.
func usage(from, to sample) float64 {
	return float64(to.cpu-from.cpu) / float64(to.t.Sub(from.t))
}

// latency is the mean scheduling latency between two samples (false if
// there are too few latency samples).
func latency(from, to sample) (time.Duration, bool) {
	n := to.latN - from.latN
	if to.latN < from.latN || n < latMinSamples {
		return 0, false
	}
	return time.Duration((to.latSum - from.latSum) / float64(n) * 1e9), true
}

func (g *governor) step(now time.Time) {
	if !g.record(now) {
		return
	}
	if g.n < g.max {
		if from, to, ok := g.window(now, upWindow); ok {
			u := usage(from, to)
			l, lok := latency(from, to)
			if u >= upLoad*float64(g.n) || lok && l >= upLatency {
				if !g.downAt.IsZero() && now.Sub(g.downAt) < reboundWindow {
					g.hold = min(max(2*g.hold, minHold), maxHold)
					g.holdTill = now.Add(g.hold)
				}
				g.set(min(2*g.n, g.max), now, "busy", u, l)
				return
			}
		}
	}
	if g.n > 1 && !now.Before(g.holdTill) {
		half := max(g.n/2, 1)
		if from, to, ok := g.window(now, downWindow); ok {
			u := usage(from, to)
			l, lok := latency(from, to)
			if u < downLoad*float64(half) && (!lok || l < downLatency) {
				g.downAt = now
				g.set(half, now, "idle", u, l)
			}
		}
	}
}

func (g *governor) set(n int, now time.Time, why string, load float64, lat ...time.Duration) {
	g.setProcs(n, g.max)
	g.n, g.since = n, now
	if g.log != nil {
		args := []any{"gomaxprocs", n, "reason", why, "cpu_load", fmt.Sprintf("%.2f", load)}
		if len(lat) > 0 {
			args = append(args, "sched_latency", lat[0].Round(time.Microsecond))
		}
		g.log.Debug("GOMAXPROCS adjusted", args...)
	}
}
