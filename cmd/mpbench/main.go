// Command mpbench runs an in-process testbed: a target TCP server, an mpquic
// remote, one emulated relay per --path and an mpquic local (tunnel mode),
// and measures latency, throughput, completion times and failover.
//
//	mpbench --path delay=10ms,rate=50mbit --path delay=40ms,rate=50mbit,loss=1% --tests all
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"mpquic/internal/linkem"
	"mpquic/internal/mp"
	"mpquic/internal/proxy"
)

type multi []string

func (m *multi) String() string     { return strings.Join(*m, " ") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

var (
	paths     multi
	tests     = flag.String("tests", "latency,ttfb,bulk,upload,fct,mixed,failover", "comma separated tests")
	duration  = flag.Duration("duration", 8*time.Second, "bulk test duration")
	single    = flag.Bool("single", true, "also run each path alone as baseline")
	verbose   = flag.Bool("v", false, "verbose mpquic logs")
	noDup     = flag.Bool("no-dup", false, "disable opportunistic duplication")
	window    = flag.Int64("window", 0, "stream window bytes")
	chunk     = flag.Int("chunk", 0, "chunk size")
	failAfter = flag.Duration("fail-after", 3*time.Second, "failover: take best path down after")
	failFor   = flag.Duration("fail-for", 4*time.Second, "failover: outage duration")
	streams   = flag.Int("streams", 1, "parallel streams in bulk tests")
	ccName    = flag.String("cc", "", "congestion control (bbr, bbr-aggressive, bbr-conservative)")
)

func main() {
	flag.Var(&paths, "path", "link spec of one path (both directions); 'fwd|rev' for asymmetric")
	flag.Parse()
	if len(paths) == 0 {
		paths = multi{"delay=10ms,rate=40mbit", "delay=30ms,rate=40mbit"}
	}
	lvl := slog.LevelWarn
	if *verbose {
		lvl = slog.LevelInfo
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
	slog.SetDefault(log)

	target := startTarget()
	opt := &mp.Options{Password: "bench", Logger: log, NoDup: *noDup, Window: *window, ChunkSize: *chunk, Congestion: *ccName}
	srv, err := mp.NewServer(opt, proxy.RemoteHandler(log, 5*time.Second))
	check(err)
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	check(err)
	go srv.Serve(udp)

	var relays []*linkem.Relay
	for _, p := range paths {
		f, r, _ := strings.Cut(p, "|")
		if r == "" {
			r = f
		}
		fs, err := linkem.ParseSpec(f)
		check(err)
		rs, err := linkem.ParseSpec(r)
		check(err)
		rl, err := linkem.NewRelay("127.0.0.1:0", udp.LocalAddr().String(), fs, rs)
		check(err)
		rl.Log = log
		go rl.Serve()
		relays = append(relays, rl)
		fmt.Printf("path %d: fwd[%s] rev[%s]\n", len(relays)-1, fs, rs)
	}

	type cfgRun struct {
		name string
		idx  []int
	}
	var runs []cfgRun
	all := make([]int, len(relays))
	for i := range all {
		all[i] = i
	}
	runs = append(runs, cfgRun{"multipath", all})
	if *single && len(relays) > 1 {
		for i := range relays {
			runs = append(runs, cfgRun{fmt.Sprintf("path%d-only", i), []int{i}})
		}
	}
	for _, run := range runs {
		var addrs []string
		for _, i := range run.idx {
			addrs = append(addrs, relays[i].Addr().String())
		}
		cl, err := mp.NewClient(addrs, opt)
		check(err)
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		check(err)
		loc := &proxy.Local{Client: cl, Log: log, Tunnel: target}
		go loc.Serve(ln)
		waitPaths(cl, len(addrs))
		fmt.Printf("\n===== %s (%d path(s)) =====\n", run.name, len(addrs))
		b := &bench{addr: ln.Addr().String(), cl: cl, relays: relays, idx: run.idx, srv: srv}
		for _, t := range strings.Split(*tests, ",") {
			switch strings.TrimSpace(t) {
			case "latency":
				b.latency()
			case "ttfb":
				b.ttfb()
			case "bulk":
				b.bulk('S', *duration, *streams)
			case "upload":
				b.bulk('R', *duration, *streams)
			case "fct":
				b.fct()
			case "rstats":
				fmt.Println("remote:", b.remoteStats())
			case "idle":
				time.Sleep(3 * time.Second)
			case "mixed":
				b.mixed()
			case "failover":
				if len(run.idx) > 1 {
					b.failover()
				}
			}
		}
		ln.Close()
		cl.Close()
		time.Sleep(300 * time.Millisecond)
	}
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func waitPaths(cl *mp.Client, n int) {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		up := 0
		for _, s := range cl.Session().Stats() {
			if s.State == "active" {
				up++
			}
		}
		if up >= n {
			time.Sleep(300 * time.Millisecond) // let RTT estimates settle
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	fmt.Println("warning: not all paths came up")
}

// ---- target server ----
// request: cmd(1) n(8). 'D': send n bytes. 'S': send until closed.
// 'R': read until EOF, reply total. 'E': echo.
func startTarget() string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	check(err)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go serveTarget(c)
		}
	}()
	return ln.Addr().String()
}

var junk = make([]byte, 64<<10)

// sinkBytes counts bytes received by the target in upload tests.
var sinkBytes atomic.Int64

// waitDrain waits until the upload sink stops receiving data.
func waitDrain() {
	last := sinkBytes.Load()
	for i := 0; i < 600; i++ {
		time.Sleep(250 * time.Millisecond)
		cur := sinkBytes.Load()
		if cur == last {
			return
		}
		last = cur
	}
}

func serveTarget(c net.Conn) {
	defer c.Close()
	c.(*net.TCPConn).SetNoDelay(true)
	var h [9]byte
	if _, err := io.ReadFull(c, h[:]); err != nil {
		return
	}
	n := binary.BigEndian.Uint64(h[1:])
	switch h[0] {
	case 'D':
		for n > 0 {
			k := uint64(len(junk))
			if k > n {
				k = n
			}
			if _, err := c.Write(junk[:k]); err != nil {
				return
			}
			n -= k
		}
	case 'S':
		for {
			if _, err := c.Write(junk); err != nil {
				return
			}
		}
	case 'R':
		buf := make([]byte, 64<<10)
		var m uint64
		for {
			k, err := c.Read(buf)
			m += uint64(k)
			sinkBytes.Add(int64(k))
			if err != nil {
				break
			}
		}
		binary.Write(c, binary.BigEndian, m)
	case 'E':
		io.Copy(c, c)
	}
}

// ---- benchmarks ----

type bench struct {
	addr   string
	cl     *mp.Client
	relays []*linkem.Relay
	idx    []int
	srv    *mp.Server
}

func (b *bench) remoteStats() string {
	var p []string
	for _, ss := range b.srv.Stats() {
		for _, s := range ss {
			p = append(p, fmt.Sprintf("%s rtt=%v min=%v bw=%.1fM pace=%.1fM cwnd=%dK %s", s.Name, s.SRTT.Round(time.Millisecond), s.MinRTT.Round(time.Millisecond), float64(s.Bandwidth)*8/1e6, float64(s.Pacing)*8/1e6, s.Cwnd>>10, s.CCMode))
		}
	}
	return strings.Join(p, "; ")
}

func (b *bench) dial(cmd byte, n uint64) net.Conn {
	c, err := net.Dial("tcp", b.addr)
	check(err)
	c.(*net.TCPConn).SetNoDelay(true)
	var h [9]byte
	h[0] = cmd
	binary.BigEndian.PutUint64(h[1:], n)
	c.Write(h[:])
	return c
}

func pct(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	i := int(p * float64(len(s)-1))
	return s[i]
}

func fmtLat(d []time.Duration) string {
	return fmt.Sprintf("p50=%v p90=%v p99=%v max=%v", pct(d, .5).Round(100*time.Microsecond),
		pct(d, .9).Round(100*time.Microsecond), pct(d, .99).Round(100*time.Microsecond), pct(d, 1).Round(100*time.Microsecond))
}

func echoRTTs(c net.Conn, n int, gap time.Duration, size int) []time.Duration {
	buf := make([]byte, size)
	var out []time.Duration
	for i := 0; i < n; i++ {
		t := time.Now()
		if _, err := c.Write(buf); err != nil {
			break
		}
		if _, err := io.ReadFull(c, buf); err != nil {
			break
		}
		out = append(out, time.Since(t))
		time.Sleep(gap)
	}
	return out
}

func (b *bench) latency() {
	c := b.dial('E', 0)
	defer c.Close()
	echoRTTs(c, 3, 10*time.Millisecond, 100)
	r := echoRTTs(c, 100, 20*time.Millisecond, 100)
	fmt.Printf("latency  echo 100B x%d: %s\n", len(r), fmtLat(r))
	r = echoRTTs(c, 30, 20*time.Millisecond, 32<<10)
	fmt.Printf("latency  echo 32KB x%d: %s\n", len(r), fmtLat(r))
}

func (b *bench) ttfb() {
	var r []time.Duration
	for i := 0; i < 20; i++ {
		t := time.Now()
		c := b.dial('D', 2000)
		io.Copy(io.Discard, c)
		r = append(r, time.Since(t))
		c.Close()
		time.Sleep(20 * time.Millisecond)
	}
	fmt.Printf("ttfb     new conn + 2KB x%d: %s\n", len(r), fmtLat(r))
}

func mbps(bytes int64, d time.Duration) float64 { return float64(bytes) * 8 / d.Seconds() / 1e6 }

func (b *bench) bulk(cmd byte, dur time.Duration, n int) {
	var total atomic.Int64
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := b.dial(cmd, 0)
			defer c.Close()
			go func() { <-stop; c.Close() }()
			buf := make([]byte, 64<<10)
			for {
				var k int
				var err error
				if cmd == 'S' {
					k, err = c.Read(buf)
					total.Add(int64(k))
				} else {
					k, err = c.Write(buf)
				}
				if err != nil {
					return
				}
			}
		}()
	}
	counter := &total
	if cmd == 'R' {
		counter = &sinkBytes
	}
	base := counter.Load()
	start := time.Now()
	var samples []float64
	last := base
	for time.Since(start) < dur {
		time.Sleep(500 * time.Millisecond)
		cur := counter.Load()
		samples = append(samples, mbps(cur-last, 500*time.Millisecond))
		last = cur
	}
	got := counter.Load() - base
	el := time.Since(start)
	close(stop)
	wg.Wait()
	if cmd == 'R' {
		waitDrain()
	}
	// steady state: skip first 2s
	skip := 4
	if len(samples) <= skip {
		skip = 0
	}
	var ss float64
	for _, s := range samples[skip:] {
		ss += s
	}
	ss /= float64(len(samples) - skip)
	name := "download"
	if cmd == 'R' {
		name = "upload"
	}
	fmt.Printf("bulk     %s x%d: avg=%.1f Mbit/s steady=%.1f Mbit/s  [%s]\n", name, n, mbps(got, el), ss, b.pathShare())
	if *verbose {
		fmt.Printf("         timeline: %s\n", fmtSamples(samples))
	}
}

func fmtSamples(s []float64) string {
	var p []string
	for _, x := range s {
		p = append(p, fmt.Sprintf("%.0f", x))
	}
	return strings.Join(p, " ")
}

func (b *bench) pathShare() string {
	var p []string
	for _, s := range b.cl.Session().Stats() {
		p = append(p, fmt.Sprintf("%s rtt=%v bw=%.0fM rx=%dMB", s.Name, s.SRTT.Round(time.Millisecond), float64(s.Bandwidth)*8/1e6, s.RecvBytes>>20))
	}
	return strings.Join(p, "; ")
}

func (b *bench) fct() {
	for _, size := range []uint64{16 << 10, 256 << 10, 2 << 20, 16 << 20} {
		var r []time.Duration
		reps := 6
		if size >= 16<<20 {
			reps = 3
		}
		for i := 0; i < reps; i++ {
			t := time.Now()
			c := b.dial('D', size)
			n, _ := io.Copy(io.Discard, c)
			c.Close()
			if uint64(n) != size {
				fmt.Printf("fct: short read %d/%d\n", n, size)
			}
			r = append(r, time.Since(t))
			time.Sleep(100 * time.Millisecond)
		}
		fmt.Printf("fct      %6dKB: %s (%.1f Mbit/s @p50)\n", size>>10, fmtLat(r), mbps(int64(size), pct(r, .5)))
	}
}

func (b *bench) mixed() {
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c := b.dial('S', 0)
		go func() { <-stop; c.Close() }()
		io.Copy(io.Discard, c)
	}()
	time.Sleep(2 * time.Second)
	c := b.dial('E', 0)
	r := echoRTTs(c, 60, 30*time.Millisecond, 100)
	c.Close()
	close(stop)
	wg.Wait()
	fmt.Printf("mixed    echo 100B during bulk download x%d: %s\n", len(r), fmtLat(r))
}

func (b *bench) failover() {
	// take down the path with the lowest RTT
	best := b.idx[0]
	bestRTT := time.Hour
	st := b.cl.Session().Stats()
	for i, s := range st {
		if s.SRTT < bestRTT && i < len(b.idx) {
			bestRTT = s.SRTT
			best = b.idx[i]
		}
	}
	var total atomic.Int64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		c := b.dial('S', 0)
		go func() { <-stop; c.Close() }()
		buf := make([]byte, 64<<10)
		for {
			k, err := c.Read(buf)
			total.Add(int64(k))
			if err != nil {
				return
			}
		}
	}()
	// concurrent echo probe to measure interactive stall
	var maxEcho time.Duration
	var echoMu sync.Mutex
	echoStop := make(chan struct{})
	echoDone := make(chan struct{})
	go func() {
		defer close(echoDone)
		c := b.dial('E', 0)
		defer c.Close()
		buf := make([]byte, 100)
		for {
			select {
			case <-echoStop:
				return
			default:
			}
			t := time.Now()
			c.Write(buf)
			if _, err := io.ReadFull(c, buf); err != nil {
				return
			}
			d := time.Since(t)
			echoMu.Lock()
			if d > maxEcho {
				maxEcho = d
			}
			echoMu.Unlock()
			time.Sleep(50 * time.Millisecond)
		}
	}()
	start := time.Now()
	var samples []float64
	last := int64(0)
	downAt := *failAfter
	upAt := *failAfter + *failFor
	isDown := false
	var maxGap time.Duration
	lastProgress := time.Now()
	lastBytes := int64(0)
	for time.Since(start) < upAt+3*time.Second {
		time.Sleep(50 * time.Millisecond)
		el := time.Since(start)
		if !isDown && el >= downAt && el < upAt {
			b.relays[best].SetDown(true)
			isDown = true
		}
		if isDown && el >= upAt {
			b.relays[best].SetDown(false)
			isDown = false
		}
		cur := total.Load()
		if cur > lastBytes {
			lastBytes = cur
			lastProgress = time.Now()
		} else if g := time.Since(lastProgress); g > maxGap {
			maxGap = g
		}
		if int(el/(500*time.Millisecond)) > len(samples) {
			samples = append(samples, mbps(cur-last, 500*time.Millisecond))
			last = cur
		}
	}
	close(stop)
	close(echoStop)
	<-done
	<-echoDone
	fmt.Printf("failover path%d down %v..%v: max download stall=%v, max echo RTT=%v\n", best, downAt, upAt,
		maxGap.Round(time.Millisecond), maxEcho.Round(time.Millisecond))
	fmt.Printf("         timeline (Mbit/s per 0.5s): %s\n", fmtSamples(samples))
}
