package mp

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"mpquic/internal/linkem"
)

type testbed struct {
	srv    *Server
	relays []*linkem.Relay
	cl     *Client
	udp    net.PacketConn
}

func testLogger() *slog.Logger {
	lvl := slog.LevelWarn
	if os.Getenv("MPQUIC_TEST_LOG") != "" {
		lvl = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

// echoHandler echoes every stream back.
func echoHandler(st *Stream) {
	io.Copy(st, st)
	st.CloseWrite()
	st.Close()
}

func newTestbed(t *testing.T, specs []string, handler func(*Stream), mod func(*Options)) *testbed {
	t.Helper()
	opt := &Options{Password: "pw", Logger: testLogger()}
	if mod != nil {
		mod(opt)
	}
	srv, err := NewServer(opt, handler)
	if err != nil {
		t.Fatal(err)
	}
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(udp)
	tb := &testbed{srv: srv, udp: udp}
	var addrs []string
	for _, s := range specs {
		sp, err := linkem.ParseSpec(s)
		if err != nil {
			t.Fatal(err)
		}
		r, err := linkem.NewRelay("127.0.0.1:0", udp.LocalAddr().String(), sp, sp)
		if err != nil {
			t.Fatal(err)
		}
		go r.Serve()
		tb.relays = append(tb.relays, r)
		addrs = append(addrs, r.Addr().String())
	}
	tb.cl, err = NewClient(addrs, opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tb.close)
	tb.waitPaths(t, len(addrs))
	return tb
}

func (tb *testbed) waitPaths(t *testing.T, n int) {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		up := 0
		for _, s := range tb.cl.Session().Stats() {
			if s.State == "active" {
				up++
			}
		}
		if up >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("paths did not come up")
}

func (tb *testbed) close() {
	tb.cl.Close()
	tb.srv.Close()
	for _, r := range tb.relays {
		r.Close()
	}
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

type streamOpener interface{ OpenStream() (*Stream, error) }

func echoRoundTrip(t *testing.T, cl streamOpener, data []byte) {
	t.Helper()
	st, err := cl.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() {
		_, err := st.Write(data)
		if err == nil {
			err = st.CloseWrite()
		}
		errc <- err
	}()
	got, err := io.ReadAll(st)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("write: %v", err)
	}
	st.Close()
	if !bytes.Equal(got, data) {
		t.Fatalf("data mismatch: got %d bytes, want %d", len(got), len(data))
	}
}

func TestEchoTwoPaths(t *testing.T) {
	tb := newTestbed(t, []string{"delay=5ms,rate=50mbit", "delay=20ms,rate=50mbit"}, echoHandler, nil)
	for _, n := range []int{0, 1, 1000, 16 << 10, 100 << 10, 3 << 20} {
		echoRoundTrip(t, tb.cl, randBytes(n))
	}
}

func TestEchoImpairedConcurrent(t *testing.T) {
	tb := newTestbed(t, []string{
		"delay=5ms,jitter=4ms,loss=3%,rate=30mbit",
		"delay=15ms,reorder=5%,loss=1%,burst=3,rate=20mbit",
		"delay=40ms,dup=2%,rate=10mbit,queue=50ms",
	}, echoHandler, func(o *Options) { o.Window = 1 << 20; o.ChunkSize = 4096 })
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			echoRoundTrip(t, tb.cl, randBytes(1000+i*97000))
		}(i)
	}
	wg.Wait()
}

func TestPathFailureMidTransfer(t *testing.T) {
	tb := newTestbed(t, []string{"delay=5ms,rate=40mbit", "delay=10ms,rate=40mbit"}, echoHandler,
		func(o *Options) { o.SuspectTimeout = 300 * time.Millisecond })
	go func() {
		time.Sleep(300 * time.Millisecond)
		tb.relays[0].SetDown(true)
		time.Sleep(1500 * time.Millisecond)
		tb.relays[0].SetDown(false)
	}()
	echoRoundTrip(t, tb.cl, randBytes(12<<20))
	// both paths must be usable again afterwards
	tb.waitPaths(t, 2)
	echoRoundTrip(t, tb.cl, randBytes(1<<20))
}

func TestAllPathsFlap(t *testing.T) {
	tb := newTestbed(t, []string{"delay=5ms,rate=40mbit,outage=1500ms/400ms", "delay=10ms,rate=40mbit,outage=1100ms/300ms"}, echoHandler, nil)
	echoRoundTrip(t, tb.cl, randBytes(8<<20))
}

func TestResetPropagates(t *testing.T) {
	handler := func(st *Stream) {
		b := make([]byte, 5)
		io.ReadFull(st, b)
		st.Reset(ResetDialFailed)
		st.Close()
	}
	tb := newTestbed(t, []string{"delay=2ms"}, handler, nil)
	st, _ := tb.cl.OpenStream()
	st.Write([]byte("hello"))
	_, err := io.ReadAll(st)
	var re *ResetError
	if !errors.As(err, &re) || re.Code != ResetDialFailed || !re.Remote {
		t.Fatalf("expected remote reset with code %d, got %v", ResetDialFailed, err)
	}
	st.Close()
}

func TestWrongPassword(t *testing.T) {
	opt := &Options{Password: "right", Logger: testLogger()}
	srv, _ := NewServer(opt, echoHandler)
	udp, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	go srv.Serve(udp)
	defer srv.Close()
	cl, _ := NewClient([]string{udp.LocalAddr().String()}, &Options{Password: "wrong", Logger: testLogger()})
	defer cl.Close()
	time.Sleep(1500 * time.Millisecond)
	if n := len(cl.Session().Stats()); n != 0 {
		t.Fatalf("path with wrong password came up (%d)", n)
	}
}

func TestServerRestartResetsStreams(t *testing.T) {
	tb := newTestbed(t, []string{"delay=2ms"}, echoHandler, func(o *Options) { o.PathIdleTimeout = 2 * time.Second })
	st, _ := tb.cl.OpenStream()
	st.Write([]byte("x"))
	b := make([]byte, 1)
	if _, err := io.ReadFull(st, b); err != nil {
		t.Fatal(err)
	}
	// Restart the server on the same socket address.
	addr := tb.udp.LocalAddr().String()
	tb.srv.Close()
	tb.udp.Close()
	opt := &Options{Password: "pw", Logger: testLogger()}
	srv2, _ := NewServer(opt, echoHandler)
	tb.srv = srv2
	ua, _ := net.ResolveUDPAddr("udp", addr)
	udp2, err := net.ListenUDP("udp", ua)
	if err != nil {
		t.Fatal(err)
	}
	go srv2.Serve(udp2)
	done := make(chan error, 1)
	go func() { _, err := io.ReadAll(st); done <- err }()
	select {
	case err := <-done:
		var re *ResetError
		if !errors.As(err, &re) {
			t.Fatalf("expected reset, got %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("old stream was not reset after server restart")
	}
	st.Close()
	tb.waitPaths(t, 1)
	echoRoundTrip(t, tb.cl, randBytes(100<<10))
}

func TestWaterfill(t *testing.T) {
	mk := func(srtt time.Duration, bw uint64) *Path {
		return &Path{srtt: srtt, minRTT: srtt, bw: bw}
	}
	now := time.Now()
	// one path: owd 10ms, 1MB/s -> 1MB drains at 1.01s
	got := waterfill([]*Path{mk(20*time.Millisecond, 1<<20)}, 1<<20, now)
	if got < 1.009 || got > 1.011 {
		t.Fatalf("waterfill single = %v", got)
	}
	// two equal paths share the backlog
	got = waterfill([]*Path{mk(20*time.Millisecond, 1<<20), mk(20*time.Millisecond, 1<<20)}, 1<<20, now)
	if got < 0.509 || got > 0.511 {
		t.Fatalf("waterfill double = %v", got)
	}
	// a path that starts after the backlog is drained does not contribute
	got = waterfill([]*Path{mk(20*time.Millisecond, 1<<20), mk(10*time.Second, 1<<20)}, 1<<20, now)
	if got < 1.009 || got > 1.011 {
		t.Fatalf("waterfill late path = %v", got)
	}
}

func TestAckCodec(t *testing.T) {
	in := []ackEntry{{id: 1, cum: 100, window: 5000}, {id: 1 << 40, cum: ackAll, window: 0, ranges: []ackRange{{200, 300}, {400, 1 << 33}}}}
	typ, body, err := readFrame(bufio.NewReader(bytes.NewReader(encodeAck(in))))
	if err != nil || typ != frAck {
		t.Fatal(err)
	}
	out, err := decodeAck(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[1].id != in[1].id || out[1].cum != ackAll || len(out[1].ranges) != 2 || out[1].ranges[1].end != 1<<33 {
		t.Fatalf("roundtrip mismatch: %+v", out)
	}
}

// A relay that restarts loses its flow table, so the remote sees the path's
// packets arrive from a new source port (QUIC connection migration).
func TestRelayRestart(t *testing.T) {
	tb := newTestbed(t, []string{"delay=3ms,rate=40mbit", "delay=8ms,rate=40mbit"}, echoHandler,
		func(o *Options) { o.SuspectTimeout = 300 * time.Millisecond })
	addr := tb.relays[0].Addr().String()
	go func() {
		time.Sleep(200 * time.Millisecond)
		tb.relays[0].Close()
		time.Sleep(time.Second)
		sp, _ := linkem.ParseSpec("delay=3ms,rate=40mbit")
		r, err := linkem.NewRelay(addr, tb.udp.LocalAddr().String(), sp, sp)
		if err != nil {
			t.Error(err)
			return
		}
		tb.relays[0] = r
		go r.Serve()
	}()
	echoRoundTrip(t, tb.cl, randBytes(10<<20))
	time.Sleep(2 * time.Second)
	tb.waitPaths(t, 2)
	echoRoundTrip(t, tb.cl, randBytes(2<<20))
	st := tb.cl.Session().Stats()
	if len(st) != 2 {
		t.Fatalf("expected 2 paths, got %d", len(st))
	}
}

func TestECFDecision(t *testing.T) {
	now := time.Now()
	fast := &Path{srtt: 20 * time.Millisecond, minRTT: 20 * time.Millisecond, bw: 5 << 20}   // 10ms owd, 5MB/s
	slow := &Path{srtt: 100 * time.Millisecond, minRTT: 100 * time.Millisecond, bw: 5 << 20} // 50ms owd, 5MB/s
	s := &Session{paths: []*Path{fast, slow}}
	// The fast path may always send.
	if !s.ecfAllowLocked(fast, 1<<30, 16<<10, now) {
		t.Fatal("fast path denied")
	}
	// A small backlog stays on the fast path: it drains 64KB in ~22ms < 53ms.
	if s.ecfAllowLocked(slow, 64<<10, 16<<10, now) {
		t.Fatal("slow path allowed for a small backlog")
	}
	// A bulk backlog spills over: 4MB takes ~0.8s on the fast path alone.
	if !s.ecfAllowLocked(slow, 4<<20, 16<<10, now) {
		t.Fatal("slow path denied for bulk backlog")
	}
	// A low-bandwidth "fast" path loses to a high-bandwidth one for big chunks.
	thin := &Path{srtt: 10 * time.Millisecond, minRTT: 10 * time.Millisecond, bw: 100 << 10} // 100KB/s
	s.paths = []*Path{thin, slow}
	if !s.ecfAllowLocked(slow, 256<<10, 16<<10, now) {
		t.Fatal("high-bandwidth path denied although the thin path would take seconds")
	}
}

// Reverse roles: the QUIC listener opens streams, the dialer accepts them.
func TestReverseRoles(t *testing.T) {
	log := testLogger()
	srv, err := NewServer(&Options{Password: "pw", Logger: log, SingleSession: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	udp, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	go srv.Serve(udp)
	defer srv.Close()
	if _, err := srv.OpenStream(); err != ErrNoSession {
		t.Fatalf("expected ErrNoSession, got %v", err)
	}
	var relays []*linkem.Relay
	var addrs []string
	for _, spec := range []string{"delay=5ms,rate=40mbit", "delay=15ms,rate=40mbit,loss=1%"} {
		sp, _ := linkem.ParseSpec(spec)
		r, _ := linkem.NewRelay("127.0.0.1:0", udp.LocalAddr().String(), sp, sp)
		go r.Serve()
		defer r.Close()
		relays = append(relays, r)
		addrs = append(addrs, r.Addr().String())
	}
	dial := func() *Client {
		cl, err := NewClient(addrs, &Options{Password: "pw", Logger: log, StreamHandler: echoHandler})
		if err != nil {
			t.Fatal(err)
		}
		return cl
	}
	cl := dial()
	waitActive := func(n int) *Session {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if s := srv.Session(); s != nil && s.ActivePaths() >= n {
				return s
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("reverse paths did not come up")
		return nil
	}
	first := waitActive(2)
	opener := &serverOpener{srv}
	echoRoundTrip(t, opener, randBytes(4<<20))
	// The dialer (remote) restarts: its new session supersedes the old one.
	cl.Close()
	cl = dial()
	defer cl.Close()
	deadline := time.Now().Add(10 * time.Second)
	for srv.Session() == first && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	waitActive(2)
	if srv.Session() == first {
		t.Fatal("old session not superseded")
	}
	echoRoundTrip(t, opener, randBytes(1<<20))
}

type serverOpener struct{ s *Server }

func (o *serverOpener) OpenStream() (*Stream, error) { return o.s.OpenStream() }
