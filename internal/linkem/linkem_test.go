package linkem

import (
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseSpec(t *testing.T) {
	s, err := ParseSpec("delay=40ms,jitter=5ms,loss=1%,burst=3,rate=20mbit,queue=80ms,police=10mbit,reorder=0.5,dup=0.1%,outage=30s/2s,rate2=5mbit,flap=10s,mtu=1280")
	if err != nil {
		t.Fatal(err)
	}
	if s.Delay != 40*time.Millisecond || s.Jitter != 5*time.Millisecond || s.Loss != 0.01 || s.Burst != 3 ||
		s.Rate != 20e6/8 || s.Queue != 80*time.Millisecond || s.Police != 10e6/8 || s.Reorder != 0.5 ||
		math.Abs(s.Dup-0.001) > 1e-12 || s.OutageEvery != 30*time.Second || s.OutageFor != 2*time.Second ||
		s.Rate2 != 5e6/8 || s.Flap != 10*time.Second || s.MTU != 1280 {
		t.Fatalf("parsed %+v", s)
	}
	for _, bad := range []string{"delay", "foo=1", "loss=200%", "outage=5s"} {
		if _, err := ParseSpec(bad); err == nil {
			t.Fatalf("expected error for %q", bad)
		}
	}
	if r, _ := parseRate("2MB"); r != 2e6 {
		t.Fatalf("2MB = %v", r)
	}
}

func run(t *testing.T, spec string, n, size int, gap time.Duration) (got int64, first, last time.Duration) {
	sp, err := ParseSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	l := NewLink(sp, 1)
	defer l.Close()
	var cnt atomic.Int64
	var mu sync.Mutex
	start := time.Now()
	var lastAt time.Time
	pkt := make([]byte, size)
	for i := 0; i < n; i++ {
		l.Send(pkt, func([]byte) {
			cnt.Add(1)
			mu.Lock()
			now := time.Now()
			if first == 0 {
				first = now.Sub(start)
			}
			lastAt = now
			mu.Unlock()
		})
		if gap > 0 {
			time.Sleep(gap)
		}
	}
	time.Sleep(1500 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	return cnt.Load(), first, lastAt.Sub(start)
}

func TestDelay(t *testing.T) {
	got, first, _ := run(t, "delay=50ms", 10, 100, 0)
	if got != 10 || first < 50*time.Millisecond || first > 70*time.Millisecond {
		t.Fatalf("got %d first %v", got, first)
	}
}

func TestMTU(t *testing.T) {
	// 1252 + 28 = 1280 passes, 1253 + 28 does not
	if got, _, _ := run(t, "mtu=1280", 5, 1252, 0); got != 5 {
		t.Fatalf("1252-byte payloads: delivered %d/5", got)
	}
	if got, _, _ := run(t, "mtu=1280", 5, 1253, 0); got != 0 {
		t.Fatalf("1253-byte payloads: delivered %d/5", got)
	}
}

func TestLoss(t *testing.T) {
	got, _, _ := run(t, "loss=20%", 5000, 100, 0)
	if got < 3800 || got > 4200 {
		t.Fatalf("20%% loss delivered %d/5000", got)
	}
}

func TestRateShaping(t *testing.T) {
	// 1000 x 1000B (+28B overhead) at 8 Mbit/s ~= 1.03s; generous queue so nothing drops.
	got, _, last := run(t, "rate=8mbit,queue=5s", 1000, 1000, 0)
	if got != 1000 {
		t.Fatalf("delivered %d", got)
	}
	if last < 950*time.Millisecond || last > 1200*time.Millisecond {
		t.Fatalf("shaped duration %v", last)
	}
	// with a 100ms queue, the excess is tail-dropped
	got, _, _ = run(t, "rate=8mbit,queue=100ms", 1000, 1000, 0)
	if got > 150 {
		t.Fatalf("queue limit not enforced: %d delivered", got)
	}
}

func TestOutage(t *testing.T) {
	sp, _ := ParseSpec("outage=200ms/100ms")
	l := NewLink(sp, 1)
	defer l.Close()
	time.Sleep(150 * time.Millisecond) // inside the outage window [100,200)
	var n atomic.Int64
	l.Send([]byte{1}, func([]byte) { n.Add(1) })
	time.Sleep(100 * time.Millisecond) // now in [250,...) up window
	l.Send([]byte{1}, func([]byte) { n.Add(1) })
	time.Sleep(50 * time.Millisecond)
	if n.Load() != 1 {
		t.Fatalf("outage: delivered %d, want 1", n.Load())
	}
}

// TestPassthrough: an unimpaired link delivers synchronously and in order,
// also right after an impaired phase whose packets are still queued.
func TestPassthrough(t *testing.T) {
	l := NewLink(Spec{}, 1)
	defer l.Close()
	var mu sync.Mutex
	var got []byte
	out := func(b []byte) {
		mu.Lock()
		got = append(got, b[0])
		mu.Unlock()
	}
	l.Send([]byte{1}, out)
	mu.Lock()
	if len(got) != 1 {
		mu.Unlock()
		t.Fatal("unimpaired link did not deliver synchronously")
	}
	mu.Unlock()
	l.SetSpec(Spec{Delay: 30 * time.Millisecond})
	l.Send([]byte{2}, out)
	l.SetSpec(Spec{})
	l.Send([]byte{3}, out) // must wait behind 2
	deadline := time.Now().Add(time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	l.Send([]byte{4}, out)
	mu.Lock()
	defer mu.Unlock()
	if string(got) != "\x01\x02\x03\x04" {
		t.Fatalf("delivery order %v, want [1 2 3 4]", got)
	}
	if l.Passed != 4 {
		t.Fatalf("passed %d, want 4", l.Passed)
	}
}
