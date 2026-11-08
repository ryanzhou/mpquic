// Package linkem is a userspace UDP relay with link emulation (delay, jitter,
// reordering, random/bursty loss, rate shaping, policing, duplication and
// periodic outages). Without impairments it is a plain UDP relay, equivalent
// to `socat UDP-LISTEN:port,fork UDP:target`.
package linkem

import (
	"container/heap"
	"fmt"
	"math"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Spec describes the impairments applied to one direction of a link.
type Spec struct {
	Delay        time.Duration // fixed one-way delay
	Jitter       time.Duration // extra uniform random delay [0, Jitter)
	Loss         float64       // packet loss probability [0,1]
	Burst        float64       // mean loss burst length (>1 enables Gilbert-Elliott)
	Rate         float64       // shaping rate in bytes/s (0 = unlimited)
	Rate2        float64       // alternative rate; toggled every Flap
	Flap         time.Duration // rate toggle period
	Queue        time.Duration // max shaper queueing delay (tail drop), default 100ms
	Police       float64       // policer rate in bytes/s (token bucket, drop on excess)
	PoliceBurst  float64       // policer bucket size in bytes
	Reorder      float64       // probability a packet is held back
	ReorderDelay time.Duration // extra delay of reordered packets
	Dup          float64       // duplication probability
	OutageEvery  time.Duration // periodic outage period
	OutageFor    time.Duration // outage duration
	MTU          int           // path MTU: drop packets whose IPv4 size (UDP payload + 28) exceeds it
}

func (s Spec) String() string {
	var p []string
	add := func(k, v string) { p = append(p, k+"="+v) }
	if s.Delay > 0 {
		add("delay", s.Delay.String())
	}
	if s.Jitter > 0 {
		add("jitter", s.Jitter.String())
	}
	if s.Loss > 0 {
		add("loss", fmt.Sprintf("%g%%", s.Loss*100))
	}
	if s.Burst > 1 {
		add("burst", fmt.Sprint(s.Burst))
	}
	if s.Rate > 0 {
		add("rate", fmtRate(s.Rate))
	}
	if s.Rate2 > 0 {
		add("rate2", fmtRate(s.Rate2))
		add("flap", s.Flap.String())
	}
	if s.Police > 0 {
		add("police", fmtRate(s.Police))
	}
	if s.Reorder > 0 {
		add("reorder", fmt.Sprintf("%g%%", s.Reorder*100))
	}
	if s.Dup > 0 {
		add("dup", fmt.Sprintf("%g%%", s.Dup*100))
	}
	if s.OutageEvery > 0 {
		add("outage", s.OutageEvery.String()+"/"+s.OutageFor.String())
	}
	if s.MTU > 0 {
		add("mtu", strconv.Itoa(s.MTU))
	}
	if len(p) == 0 {
		return "none"
	}
	return strings.Join(p, ",")
}

func fmtRate(r float64) string { return fmt.Sprintf("%gmbit", r*8/1e6) }

// ParseSpec parses "delay=50ms,jitter=5ms,loss=1%,burst=3,rate=20mbit,
// queue=50ms,police=10mbit,reorder=1%,reorder_delay=10ms,dup=0.1%,
// outage=30s/2s,rate2=5mbit,flap=10s,mtu=1280". Empty string means no impairment.
func ParseSpec(str string) (Spec, error) {
	var s Spec
	str = strings.TrimSpace(str)
	if str == "" || str == "none" {
		return s, nil
	}
	for _, kv := range strings.Split(str, ",") {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return s, fmt.Errorf("bad link spec item %q", kv)
		}
		var err error
		switch strings.ToLower(k) {
		case "delay":
			s.Delay, err = time.ParseDuration(v)
		case "jitter":
			s.Jitter, err = time.ParseDuration(v)
		case "loss":
			s.Loss, err = parseProb(v)
		case "burst":
			s.Burst, err = strconv.ParseFloat(v, 64)
		case "rate", "bw":
			s.Rate, err = parseRate(v)
		case "rate2":
			s.Rate2, err = parseRate(v)
		case "flap":
			s.Flap, err = time.ParseDuration(v)
		case "mtu":
			s.MTU, err = strconv.Atoi(v)
		case "queue":
			s.Queue, err = time.ParseDuration(v)
		case "police":
			s.Police, err = parseRate(v)
		case "police_burst":
			var b float64
			b, err = strconv.ParseFloat(strings.TrimSuffix(strings.ToLower(v), "b"), 64)
			s.PoliceBurst = b
		case "reorder":
			s.Reorder, err = parseProb(v)
		case "reorder_delay":
			s.ReorderDelay, err = time.ParseDuration(v)
		case "dup":
			s.Dup, err = parseProb(v)
		case "outage":
			a, b, ok := strings.Cut(v, "/")
			if !ok {
				return s, fmt.Errorf("outage wants every/duration, got %q", v)
			}
			if s.OutageEvery, err = time.ParseDuration(a); err == nil {
				s.OutageFor, err = time.ParseDuration(b)
			}
		default:
			return s, fmt.Errorf("unknown link spec key %q", k)
		}
		if err != nil {
			return s, fmt.Errorf("link spec %q: %v", kv, err)
		}
	}
	return s, nil
}

func parseProb(v string) (float64, error) {
	pct := strings.HasSuffix(v, "%")
	f, err := strconv.ParseFloat(strings.TrimSuffix(v, "%"), 64)
	if err != nil {
		return 0, err
	}
	if pct || f > 1 {
		f /= 100
	}
	if f < 0 || f > 1 {
		return 0, fmt.Errorf("probability out of range")
	}
	return f, nil
}

// parseRate parses e.g. "20mbit", "500kbit", "1gbit", "2MB" (bytes), "1000" (bit/s).
func parseRate(v string) (float64, error) {
	l := strings.ToLower(strings.TrimSpace(v))
	l = strings.TrimSuffix(l, "/s")
	l = strings.TrimSuffix(l, "ps")
	mult := 1.0 / 8 // bits by default
	units := []struct {
		suf string
		m   float64
	}{
		{"gbit", 1e9 / 8}, {"mbit", 1e6 / 8}, {"kbit", 1e3 / 8}, {"bit", 1.0 / 8},
		{"gb", 1e9}, {"mb", 1e6}, {"kb", 1e3}, {"b", 1},
		{"g", 1e9 / 8}, {"m", 1e6 / 8}, {"k", 1e3 / 8},
	}
	for _, u := range units {
		if strings.HasSuffix(l, u.suf) {
			l = strings.TrimSuffix(l, u.suf)
			mult = u.m
			break
		}
	}
	f, err := strconv.ParseFloat(l, 64)
	if err != nil {
		return 0, err
	}
	return f * mult, nil
}

type pkt struct {
	at   time.Time
	seq  uint64
	data []byte
	out  func([]byte)
}

type pktHeap []*pkt

func (h pktHeap) Len() int { return len(h) }
func (h pktHeap) Less(i, j int) bool {
	if h[i].at.Equal(h[j].at) {
		return h[i].seq < h[j].seq
	}
	return h[i].at.Before(h[j].at)
}
func (h pktHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *pktHeap) Push(x any)   { *h = append(*h, x.(*pkt)) }
func (h *pktHeap) Pop() any     { o := *h; x := o[len(o)-1]; *h = o[:len(o)-1]; return x }

// Link applies a Spec to packets flowing in one direction.
type Link struct {
	mu          sync.Mutex
	spec        Spec
	rng         *rand.Rand
	start       time.Time
	shapeFree   time.Time
	tokens      float64
	lastTok     time.Time
	geBad       bool
	lastDeliver time.Time
	seq         uint64
	q           pktHeap
	sig         chan struct{}
	down        bool
	closed      bool

	// counters
	Passed, Dropped uint64
}

// NewLink creates a link and starts its delivery goroutine.
func NewLink(spec Spec, seed int64) *Link {
	l := &Link{spec: spec, rng: rand.New(rand.NewSource(seed)), start: time.Now(), sig: make(chan struct{}, 1)}
	l.tokens = l.bucket()
	go l.run()
	return l
}

func (l *Link) bucket() float64 {
	if l.spec.PoliceBurst > 0 {
		return l.spec.PoliceBurst
	}
	return math.Max(l.spec.Police*0.05, 16*1500)
}

// SetSpec changes the impairments at runtime.
func (l *Link) SetSpec(s Spec) {
	l.mu.Lock()
	l.spec = s
	l.mu.Unlock()
}

// Spec returns the current impairments.
func (l *Link) Spec() Spec {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.spec
}

// SetDown makes the link drop everything (true) or restores it.
func (l *Link) SetDown(down bool) {
	l.mu.Lock()
	l.down = down
	l.mu.Unlock()
}

// Counters returns the number of packets passed and dropped so far.
func (l *Link) Counters() (passed, dropped uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.Passed, l.Dropped
}

// Close stops the delivery goroutine.
func (l *Link) Close() {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	l.signal()
}

func (l *Link) signal() {
	select {
	case l.sig <- struct{}{}:
	default:
	}
}

func (l *Link) currentRate(now time.Time) float64 {
	s := &l.spec
	if s.Rate2 > 0 && s.Flap > 0 {
		if (now.Sub(l.start)/s.Flap)%2 == 1 {
			return s.Rate2
		}
	}
	return s.Rate
}

// passthrough reports whether the spec leaves packets unchanged.
func (s *Spec) passthrough() bool {
	return s.Delay == 0 && s.Jitter == 0 && s.Loss == 0 && s.Rate == 0 && (s.Rate2 == 0 || s.Flap == 0) &&
		s.Police == 0 && s.Reorder == 0 && s.Dup == 0 && (s.OutageEvery == 0 || s.OutageFor == 0) && s.MTU == 0
}

// Send submits a packet; out is called when it is delivered: from the link
// goroutine with a copy of b, or, on an unimpaired link (a plain relay),
// directly with b before Send returns (no copy and no goroutine hand-off
// per packet).
func (l *Link) Send(b []byte, out func([]byte)) {
	now := time.Now()
	l.mu.Lock()
	s := &l.spec
	if l.down || l.closed {
		l.Dropped++
		l.mu.Unlock()
		return
	}
	if len(l.q) == 0 && s.passthrough() { // nothing queued: order is kept
		l.Passed++
		l.mu.Unlock()
		out(b)
		return
	}
	if s.OutageEvery > 0 && s.OutageFor > 0 {
		phase := now.Sub(l.start) % s.OutageEvery
		if phase >= s.OutageEvery-s.OutageFor {
			l.Dropped++
			l.mu.Unlock()
			return
		}
	}
	if s.Loss > 0 {
		lost := false
		if s.Burst > 1 {
			pbg := 1 / s.Burst
			pgb := s.Loss * pbg / (1 - math.Min(s.Loss, 0.99))
			if l.geBad {
				if l.rng.Float64() < pbg {
					l.geBad = false
				}
			} else if l.rng.Float64() < pgb {
				l.geBad = true
			}
			lost = l.geBad
		} else {
			lost = l.rng.Float64() < s.Loss
		}
		if lost {
			l.Dropped++
			l.mu.Unlock()
			return
		}
	}
	if s.MTU > 0 && len(b)+28 > s.MTU {
		// too big for the path, and the sender set DF (as QUIC does)
		l.Dropped++
		l.mu.Unlock()
		return
	}
	size := float64(len(b) + 28) // IP+UDP header
	if s.Police > 0 {
		l.tokens += now.Sub(l.lastTok).Seconds() * s.Police
		if bk := l.bucket(); l.tokens > bk {
			l.tokens = bk
		}
		l.lastTok = now
		if l.tokens < size {
			l.Dropped++
			l.mu.Unlock()
			return
		}
		l.tokens -= size
	}
	depart := now
	if r := l.currentRate(now); r > 0 {
		start := l.shapeFree
		if start.Before(now) {
			start = now
		}
		done := start.Add(time.Duration(size / r * float64(time.Second)))
		q := s.Queue
		if q <= 0 {
			q = 100 * time.Millisecond
		}
		if done.Sub(now) > q {
			l.Dropped++
			l.mu.Unlock()
			return
		}
		l.shapeFree = done
		depart = done
	}
	at := depart.Add(s.Delay)
	if s.Jitter > 0 {
		at = at.Add(time.Duration(l.rng.Int63n(int64(s.Jitter))))
	}
	copies := 1
	if s.Dup > 0 && l.rng.Float64() < s.Dup {
		copies = 2
	}
	for i := 0; i < copies; i++ {
		t := at
		if s.Reorder > 0 && l.rng.Float64() < s.Reorder {
			rd := s.ReorderDelay
			if rd <= 0 {
				rd = 10*time.Millisecond + s.Delay/4
			}
			t = t.Add(rd)
		} else {
			if t.Before(l.lastDeliver) {
				t = l.lastDeliver
			}
			l.lastDeliver = t
		}
		l.seq++
		heap.Push(&l.q, &pkt{at: t, seq: l.seq, data: append([]byte(nil), b...), out: out})
	}
	l.Passed++
	l.mu.Unlock()
	l.signal()
}

func (l *Link) run() {
	timer := time.NewTimer(time.Hour)
	var due []*pkt
	for {
		l.mu.Lock()
		if l.closed {
			l.mu.Unlock()
			return
		}
		now := time.Now()
		due = due[:0]
		for len(l.q) > 0 && !l.q[0].at.After(now) {
			due = append(due, heap.Pop(&l.q).(*pkt))
		}
		var wait time.Duration = time.Hour
		if len(l.q) > 0 {
			wait = l.q[0].at.Sub(now)
		}
		l.mu.Unlock()
		for _, p := range due {
			p.out(p.data)
		}
		if len(due) > 0 {
			continue
		}
		timer.Reset(wait)
		select {
		case <-timer.C:
		case <-l.sig:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
	}
}
