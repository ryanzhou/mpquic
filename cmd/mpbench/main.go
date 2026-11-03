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
	"runtime/pprof"
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
	cpuprof   = flag.String("cpuprofile", "", "write a CPU profile")
	subset    = flag.String("subset", "", "also run with only these path indexes (e.g. 0,1) as a baseline")
	reverse   = flag.Bool("reverse", false, "reverse mode: the local listens, the remote dials back through the relays")
)

func main() {
	flag.Var(&paths, "path", "link spec of one path (both directions); 'fwd|rev' for asymmetric")
	flag.Parse()
	if *cpuprof != "" {
		f, err := os.Create(*cpuprof)
		check(err)
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}
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
	// The QUIC listener is the remote normally and the local in reverse mode;
	// the relays always forward to the listener.
	lopt, dopt := *opt, *opt
	if *reverse {
		lopt.SingleSession = true
		dopt.StreamHandler = proxy.RemoteHandler(log, 5*time.Second)
	} else {
		lopt.StreamHandler = proxy.RemoteHandler(log, 5*time.Second)
	}
	srv, err := mp.NewServer(&lopt, nil)
	check(err)
	if *reverse {
		fmt.Println("reverse mode: local listens, remote dials back via the relays")
	}
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
	if *subset != "" {
		var idx []int
		for _, f := range strings.Split(*subset, ",") {
			var i int
			fmt.Sscan(strings.TrimSpace(f), &i)
			idx = append(idx, i)
		}
		runs = append(runs, cfgRun{"subset" + strings.ReplaceAll(*subset, ",", "+"), idx})
	}
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
		cl, err := mp.NewClient(addrs, &dopt)
		check(err)
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		check(err)
		b := &bench{relays: relays, idx: run.idx}
		var opener proxy.Opener = cl
		b.local = cl.Session
		b.remote = func() []mp.PathStat {
			var out []mp.PathStat
			for _, ss := range srv.Stats() {
				out = append(out, ss...)
			}
			return out
		}
		if *reverse {
			opener = srv
			b.local = srv.Session
			b.remote = cl.Session().Stats
		}
		loc := &proxy.Local{Client: opener, Log: log, Tunnel: target}
		go loc.Serve(ln)
		b.addr = ln.Addr().String()
		waitPaths(b.local, len(addrs))
		fmt.Printf("\n===== %s (%d path(s)) =====\n", run.name, len(addrs))
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

func waitPaths(local func() *mp.Session, n int) {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		up := 0
		sess := local()
		if sess == nil {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		for _, s := range sess.Stats() {
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
	relays []*linkem.Relay
	idx    []int
	local  func() *mp.Session   // the local (SOCKS/tunnel) side's session
	remote func() []mp.PathStat // the remote side's path stats
}

func (b *bench) localStats() []mp.PathStat {
	if s := b.local(); s != nil {
		return s.Stats()
	}
	return nil
}

func (b *bench) remoteStats() string {
	var p []string
	for _, s := range b.remote() {
		p = append(p, fmt.Sprintf("%s rtt=%v min=%v bw=%.1fM pace=%.1fM cwnd=%dK %s sent=%dMB lost=%dMB", s.Name, s.SRTT.Round(time.Millisecond), s.MinRTT.Round(time.Millisecond), float64(s.Bandwidth)*8/1e6, float64(s.Pacing)*8/1e6, s.Cwnd>>10, s.CCMode, s.SentBytes>>20, s.LostBytes>>20))
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
	// Latency under load: a small echo every 100 ms through the same session
	// while the transfer runs (after the 2 s warm-up), against the idle
	// latency measured first. This is what bufferbloat costs every other
	// stream through the tunnel.
	probe := b.dial('E', 0)
	idle := echoRTTs(probe, 5, 20*time.Millisecond, 100)
	var loaded []time.Duration
	probeDone := make(chan struct{})
	go func() {
		defer close(probeDone)
		select {
		case <-stop:
			return
		case <-time.After(2 * time.Second):
		}
		buf := make([]byte, 100)
		for {
			t := time.Now()
			if _, err := probe.Write(buf); err != nil {
				return
			}
			if _, err := io.ReadFull(probe, buf); err != nil {
				return
			}
			loaded = append(loaded, time.Since(t))
			select {
			case <-stop:
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}()
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
	var fine []float64 // 100 ms buckets after the 2 s warm-up, for jitter
	last, lastFine := base, base
	// Samples are taken every 500 ms / 100 ms of wall time and divided by the
	// time that actually elapsed (counting 10 ms sleeps overstated the rate
	// by the sleeps' overshoot, a few percent).
	lastAt, lastFineAt := start, start
	var maxStall time.Duration
	lastProgress := time.Now()
	progressAt := base
	for time.Since(start) < dur {
		time.Sleep(10 * time.Millisecond)
		cur := counter.Load()
		now := time.Now()
		if cur != progressAt {
			progressAt, lastProgress = cur, now
		} else if g := now.Sub(lastProgress); g > maxStall && now.Sub(start) > 2*time.Second {
			maxStall = g
		}
		if now.Sub(lastFineAt) >= 100*time.Millisecond {
			if now.Sub(start) > 2*time.Second {
				fine = append(fine, mbps(cur-lastFine, now.Sub(lastFineAt)))
			}
			lastFine, lastFineAt = cur, now
		}
		if now.Sub(lastAt) >= 500*time.Millisecond {
			samples = append(samples, mbps(cur-last, now.Sub(lastAt)))
			last, lastAt = cur, now
		}
	}
	got := counter.Load() - base
	el := time.Since(start)
	close(stop)
	probe.Close()
	<-probeDone
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
	if len(loaded) > 0 && len(idle) > 0 {
		fmt.Printf("loaded   echo 100B during %s x%d: %s idle_p50=%v\n", name, len(loaded), fmtLat(loaded),
			pct(idle, .5).Round(100*time.Microsecond))
	}
	if len(fine) > 0 && os.Getenv("MPBENCH_FINE") != "" {
		fmt.Printf("         100ms: %s\n", fmtSamples(fine))
	}
	if len(fine) > 0 {
		sort.Float64s(fine)
		fmt.Printf("jitter   %s per-100ms: p10=%.1f p50=%.1f min=%.1f Mbit/s, longest stall=%v\n", name,
			fine[len(fine)/10], fine[len(fine)/2], fine[0], maxStall.Round(time.Millisecond))
	}
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
	for _, s := range b.localStats() {
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
	for _, s := range b.localStats() {
		if s.SRTT < bestRTT && s.Index < len(b.idx) {
			bestRTT = s.SRTT
			best = b.idx[s.Index]
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
