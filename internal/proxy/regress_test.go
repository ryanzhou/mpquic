package proxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"mpquic/internal/mp"
)

// Regression tests for bugs found in review: each failed before its fix.

// a 255-byte domain name overflows readAddrRest's buffer.
func TestRegressDomain255(t *testing.T) {
	raw, err := EncodeAddr(strings.Repeat("a", 255) + ":80")
	if err != nil {
		t.Fatal(err)
	}
	if al := addrLen(raw); al != len(raw) {
		t.Fatalf("addrLen %d != %d", al, len(raw))
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("PANIC: %v", r)
		}
	}()
	_, _, err = ReadAddr(bytes.NewReader(raw))
	t.Log("no panic, err:", err)
}

// Serve gives up on EMFILE.
type emfileListener struct {
	net.Listener
	n int
}

func (l *emfileListener) Accept() (net.Conn, error) {
	l.n++
	if l.n == 1 {
		return nil, &net.OpError{Op: "accept", Net: "tcp", Err: os.NewSyscallError("accept4", syscall.EMFILE)}
	}
	return l.Listener.Accept()
}

func TestRegressServeEMFILE(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	l := &Local{Log: tlog()}
	errc := make(chan error, 1)
	go func() { errc <- l.Serve(&emfileListener{Listener: ln}) }()
	select {
	case err := <-errc:
		var ne net.Error
		t.Fatalf("Serve returned on a transient error: %v (timeout=%v)", err, errors.As(err, &ne) && ne.Timeout())
	case <-time.After(500 * time.Millisecond):
		t.Log("Serve kept running")
	}
}

// one transient listen error disables ICMP relaying for good.
func TestRegressICMPLatch(t *testing.T) {
	u := newICMPRemote(tlog())
	calls := 0
	u.listen = func(network, addr string) (net.PacketConn, error) {
		calls++
		if calls <= 1 { // the first flow's socket
			return nil, os.NewSyscallError("socket", syscall.EMFILE)
		}
		return net.ListenPacket("udp4", "127.0.0.1:0") // stands in for a working ICMP socket
	}
	u.mu.Lock()
	f1 := u.newFlowLocked(echoKey{nil, 1, false})
	f2 := u.newFlowLocked(echoKey{nil, 2, false})
	f3 := u.newFlowLocked(echoKey{nil, 3, false})
	u.mu.Unlock()
	t.Logf("listen calls=%d f1=%v f2=%v f3=%v noDgram=%v noRaw=%v", calls, f1 != nil, f2 != nil, f3 != nil, u.noDgram, u.noRaw)
	if f1 != nil || f2 == nil || f3 == nil || f2.conn == nil {
		t.Fatalf("flows after the transient error still fail: listen was called only %d times", calls)
	}
}

// half-closed Pipe on the remote survives the stream's reset.
func testHalfClose(t *testing.T, idle time.Duration, reset bool) {
	log := tlog()
	tl, _ := net.Listen("tcp", "127.0.0.1:0")
	defer tl.Close()
	hold := make(chan struct{})
	defer close(hold)
	go func() {
		c, err := tl.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		io.Copy(io.Discard, c) // until the client's FIN
		<-hold                 // stay silent and open
	}()
	pc, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	ropt := &mp.Options{Password: "x", Logger: log, StreamHandler: RemoteHandler(log, defaultRO), IdleTimeout: idle}
	lopt := &mp.Options{Password: "x", Logger: log, IdleTimeout: idle}
	srv, err := mp.NewServer(ropt, nil)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(pc)
	cl, err := mp.NewClient([]string{pc.LocalAddr().String()}, lopt)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { cl.Close(); srv.Close() }()
	for d := time.Now().Add(5 * time.Second); cl.Session().ActivePaths() < 1 && time.Now().Before(d); {
		time.Sleep(20 * time.Millisecond)
	}
	st, _ := cl.OpenStream()
	raw, _ := EncodeAddr(tl.Addr().String())
	st.Write(append(raw, "hello"...))
	st.CloseWrite() // the client's FIN
	time.Sleep(500 * time.Millisecond)
	if !pipeRunning() {
		t.Fatal("Pipe not running at all")
	}
	if reset {
		st.Reset(mp.ResetGeneric) // the local side gives up (client closed / reset)
		st.Close()
	}
	for d := time.Now().Add(5 * time.Second); pipeRunning() && time.Now().Before(d); {
		time.Sleep(50 * time.Millisecond)
	}
	if pipeRunning() {
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		for _, g := range strings.Split(string(buf[:n]), "\n\n") {
			if strings.Contains(g, "proxy.Pipe") {
				t.Log(g)
			}
		}
		t.Fatalf("remote Pipe (target connection, goroutines) still alive 5 s after the stream ended (reset=%v idle=%v)", reset, idle)
	}
}

func pipeRunning() bool {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	return strings.Contains(string(buf[:n]), "internal/proxy.Pipe")
}

func TestRegressHalfCloseReset(t *testing.T) { testHalfClose(t, 0, true) }
func TestRegressHalfCloseIdle(t *testing.T)  { testHalfClose(t, 1*time.Second, false) }

// a UDP datagram to a name whose lookup hangs blocks the remote's datagram receive loop.
func TestRegressRemoteDNSBlocks(t *testing.T) {
	e := setup(t, false)
	if !udpEcho(t, e, 10, 5, time.Second) {
		t.Fatal("no UDP echo")
	}
	start := time.Now()
	udpEcho(t, e, 10, 1, 5*time.Second)
	t.Logf("baseline UDP echo (new association): %v", time.Since(start))

	// a resolver that never answers
	hang := func(ctx context.Context, network, host string) ([]netip.Addr, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	testLookup.Store(&hang)
	defer testLookup.Store(nil)

	ctl, bound := socksRequest(t, e.socks, 3, "0.0.0.0:0")
	defer ctl.Close()
	port := binary.BigEndian.Uint16(bound[len(bound)-2:])
	relay := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(port)}
	uc, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	defer uc.Close()
	target, _ := EncodeAddr("slow-name.test:53")
	pkt := append(append([]byte{0, 0, 0}, target...), "query"...)
	for i := 0; i < 4; i++ { // a client retrying, as DNS/QUIC clients do
		uc.WriteToUDP(pkt, relay)
	}
	time.Sleep(100 * time.Millisecond)

	start = time.Now()
	ok := udpEcho(t, e, 10, 1, 60*time.Second)
	d := time.Since(start)
	t.Logf("UDP echo on another association while the lookup hangs: ok=%v after %v", ok, d)

	// TCP through the tunnel at the same time
	c, _ := socksRequest(t, e.socks, 1, e.tcpEcho)
	defer c.Close()
	start = time.Now()
	c.Write([]byte("hello"))
	c.SetReadDeadline(time.Now().Add(30 * time.Second))
	_, err := io.ReadFull(c, make([]byte, 5))
	t.Logf("TCP echo: err=%v after %v", err, time.Since(start))
	if d > time.Second {
		t.Fatalf("unrelated UDP association stalled %v by one client's DNS-name datagrams", d)
	}
}

// race.go: the deadline passes while the name is being resolved (the lookup still succeeds).
func TestRegressRaceExpiredAfterLookup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &raceDialer{
		gaps: raceGaps,
		lookup: func(ctx context.Context, host string) ([]netip.Addr, error) {
			cancel() // the context ends just as the answer arrives
			return []netip.Addr{netip.MustParseAddr("192.0.2.1")}, nil
		},
		dial: func(ctx context.Context, address string) (net.Conn, error) { return nil, ctx.Err() },
	}
	done := make(chan any, 1)
	go func() {
		defer func() { done <- recover() }()
		_, _, err := r.dialContext(ctx, "example.test:80")
		t.Log("returned:", err)
	}()
	select {
	case p := <-done:
		if p != nil {
			t.Fatalf("PANIC: %v", p)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("dialContext hangs")
	}
}

// one SOCKS5 UDP datagram addressed to a 255-byte name.
func TestRegressDomain255E2E(t *testing.T) {
	e := setup(t, false)
	ctl, bound := socksRequest(t, e.socks, 3, "0.0.0.0:0")
	defer ctl.Close()
	port := binary.BigEndian.Uint16(bound[len(bound)-2:])
	uc, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	defer uc.Close()
	target, _ := EncodeAddr(strings.Repeat("a", 255) + ":53")
	uc.WriteToUDP(append(append([]byte{0, 0, 0}, target...), "x"...), &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(port)})
	time.Sleep(time.Second)
	t.Log("remote survived")
}
