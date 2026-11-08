package proxy

import (
	"bytes"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"

	"mpquic/internal/linkem"
	"mpquic/internal/mp"
)

func tlog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

type env struct {
	socks, tunnel string
	udpTunnel     *net.UDPAddr // UDP tunnel to udpEcho
	tcpEcho       string
	udpEcho       *net.UDPAddr
	remoteUDP     *UDPRemote
}

var defaultRO = RemoteOptions{DialTimeout: 5 * time.Second, UDP: true}

func setup(t *testing.T, reverse bool) *env {
	t.Helper()
	return setupOpts(t, reverse, 0, false, defaultRO)
}

func setupMTU(t *testing.T, reverse bool, mtu int, reliable bool) *env {
	t.Helper()
	return setupOpts(t, reverse, mtu, reliable, defaultRO)
}

func setupOpts(t *testing.T, reverse bool, mtu int, reliable bool, ro RemoteOptions) *env {
	t.Helper()
	return setupLink(t, reverse, mtu, reliable, ro, "")
}

// setupLink runs both paths through emulated links with the given spec (none
// if empty).
func setupLink(t *testing.T, reverse bool, mtu int, reliable bool, ro RemoteOptions, link string) *env {
	t.Helper()
	log := tlog()
	// TCP echo target
	tl, _ := net.Listen("tcp", "127.0.0.1:0")
	go func() {
		for {
			c, err := tl.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	// UDP echo target
	ue, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	go func() {
		b := make([]byte, 65536)
		for {
			n, a, err := ue.ReadFromUDP(b)
			if err != nil {
				return
			}
			ue.WriteToUDP(b[:n], a)
		}
	}()
	pc, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	paths := []string{pc.LocalAddr().String(), pc.LocalAddr().String()}
	if link != "" {
		sp, err := linkem.ParseSpec(link)
		if err != nil {
			t.Fatal(err)
		}
		for i := range paths {
			r, err := linkem.NewRelay("127.0.0.1:0", pc.LocalAddr().String(), sp, sp)
			if err != nil {
				t.Fatal(err)
			}
			go r.Serve()
			t.Cleanup(func() { r.Close() })
			paths[i] = r.Addr().String()
		}
	}
	udp := NewUDPLocal(nil, log)
	udp.Reliable = reliable
	remoteUDP := NewUDPRemote(log)
	ropt := &mp.Options{Password: "x", Logger: log, DatagramHandler: remoteUDP.HandleDatagram,
		StreamHandler: RemoteHandler(log, ro), MTU: mtu}
	lopt := &mp.Options{Password: "x", Logger: log, DatagramHandler: udp.HandleDatagram, MTU: mtu}
	var opener Opener
	var stop func()
	if !reverse {
		// remote listens, local dials
		srv, err := mp.NewServer(ropt, nil)
		if err != nil {
			t.Fatal(err)
		}
		go srv.Serve(pc)
		cl, err := mp.NewClient(paths, lopt)
		if err != nil {
			t.Fatal(err)
		}
		opener, stop = cl, func() { cl.Close(); srv.Close() }
	} else {
		// local listens, remote dials back
		lopt.SingleSession = true
		srv, err := mp.NewServer(lopt, nil)
		if err != nil {
			t.Fatal(err)
		}
		go srv.Serve(pc)
		cl, err := mp.NewClient(paths, ropt)
		if err != nil {
			t.Fatal(err)
		}
		opener, stop = srv, func() { cl.Close(); srv.Close() }
	}
	udp.Client = opener
	sl, _ := net.Listen("tcp", "127.0.0.1:0")
	go (&Local{Client: opener, Log: log, UDP: udp}).Serve(sl)
	tn, _ := net.Listen("tcp", "127.0.0.1:0")
	go (&Local{Client: opener, Log: log, Tunnel: tl.Addr().String()}).Serve(tn)
	ut, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	go udp.ServeTunnel(ut, ue.LocalAddr().String())
	t.Cleanup(func() {
		ut.Close()
		stop()
		tl.Close()
		ue.Close()
		sl.Close()
		tn.Close()
	})
	deadline := time.Now().Add(5 * time.Second)
	for (opener.Session() == nil || opener.Session().ActivePaths() < 2) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	return &env{socks: sl.Addr().String(), tunnel: tn.Addr().String(), udpTunnel: ut.LocalAddr().(*net.UDPAddr), tcpEcho: tl.Addr().String(), udpEcho: ue.LocalAddr().(*net.UDPAddr), remoteUDP: remoteUDP}
}

func socksRequest(t *testing.T, addr string, cmd byte, target string) (net.Conn, []byte) {
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	c.Write([]byte{5, 1, 0})
	r := make([]byte, 2)
	io.ReadFull(c, r)
	if r[1] != 0 {
		t.Fatal("auth negotiation failed")
	}
	a, _ := EncodeAddr(target)
	c.Write(append([]byte{5, cmd, 0}, a...))
	h := make([]byte, 3)
	if _, err := io.ReadFull(c, h); err != nil || h[1] != 0 {
		t.Fatalf("socks reply %v %v", h, err)
	}
	_, bound, err := ReadAddr(c)
	if err != nil {
		t.Fatal(err)
	}
	return c, bound
}

func TestSocksConnect(t *testing.T) { both(t, testSocksConnect) }

func testSocksConnect(t *testing.T, reverse bool) {
	e := setup(t, reverse)
	c, _ := socksRequest(t, e.socks, 1, e.tcpEcho)
	defer c.Close()
	msg := bytes.Repeat([]byte("hello mpquic "), 10000)
	go c.Write(msg)
	got := make([]byte, len(msg))
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(c, got); err != nil || !bytes.Equal(got, msg) {
		t.Fatalf("echo mismatch: %v", err)
	}
}

// With race_connect, the remote's outbound connections work the same.
func TestSocksConnectRace(t *testing.T) {
	both(t, func(t *testing.T, reverse bool) {
		e := setupOpts(t, reverse, 0, false, RemoteOptions{DialTimeout: 5 * time.Second, UDP: true, RaceConnect: true})
		_, port, _ := net.SplitHostPort(e.tcpEcho)
		for _, target := range []string{e.tcpEcho, net.JoinHostPort("localhost", port)} {
			c, _ := socksRequest(t, e.socks, 1, target)
			c.Write([]byte("ping"))
			b := make([]byte, 4)
			c.SetReadDeadline(time.Now().Add(5 * time.Second))
			if _, err := io.ReadFull(c, b); err != nil || string(b) != "ping" {
				t.Fatalf("%s: echo %q %v", target, b, err)
			}
			c.Close()
		}
	})
}

func TestSocksConnectRefused(t *testing.T) { both(t, testSocksConnectRefused) }

func testSocksConnectRefused(t *testing.T, reverse bool) {
	e := setup(t, reverse)
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := l.Addr().String()
	l.Close()
	c, _ := socksRequest(t, e.socks, 1, dead)
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("expected connection to be closed")
	}
}

func TestTunnel(t *testing.T) { both(t, testTunnel) }

func testTunnel(t *testing.T, reverse bool) {
	e := setup(t, reverse)
	c, err := net.Dial("tcp", e.tunnel)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("ping"))
	b := make([]byte, 4)
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(c, b); err != nil || string(b) != "ping" {
		t.Fatalf("tunnel echo: %q %v", b, err)
	}
}

func TestSocksUDP(t *testing.T) { both(t, testSocksUDP) }

func testSocksUDP(t *testing.T, reverse bool) {
	e := setup(t, reverse)
	for _, size := range []int{10, 1000} {
		if !udpEcho(t, e, size, 5, time.Second) {
			t.Fatalf("no UDP echo for size %d", size)
		}
	}
}

// Closing the SOCKS control connection releases the remote's socket at once.
func TestSocksUDPRelease(t *testing.T) {
	e := setup(t, false)
	if !udpEcho(t, e, 10, 5, time.Second) {
		t.Fatal("no UDP echo")
	}
	n := -1
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		e.remoteUDP.mu.Lock()
		n = len(e.remoteUDP.assocs)
		e.remoteUDP.mu.Unlock()
		if n == 0 {
			return
		}
	}
	t.Fatalf("%d remote associations left after the SOCKS association ended", n)
}

// With a 1280-byte path MTU, a QUIC DATAGRAM carries 1280-28-37 = 1215
// bytes: the type byte, the association id (1 byte) and the IPv4 address (7)
// leave 1206 for the UDP payload. Bigger payloads are dropped, as a network
// with that MTU would drop them; reliable UDP has no size limit.
func TestSocksUDPOversize(t *testing.T) { both(t, testSocksUDPOversize) }

func testSocksUDPOversize(t *testing.T, reverse bool) {
	for _, reliable := range []bool{false, true} {
		e := setupMTU(t, reverse, 1280, reliable)
		for _, c := range []struct {
			size int
			ok   bool
		}{{1206, true}, {1207, reliable}, {4000, reliable}, {65000, reliable}} {
			tries, wait := 5, time.Second
			if !c.ok {
				tries, wait = 2, 300*time.Millisecond
			}
			if got := udpEcho(t, e, c.size, tries, wait); got != c.ok {
				t.Errorf("reliable %v: payload %d: echoed %v, want %v", reliable, c.size, got, c.ok)
			}
		}
	}
}

// Reliable UDP through SOCKS5 and the UDP tunnel: no datagram is lost on
// lossy paths, and order is kept.
func TestReliableUDP(t *testing.T) { both(t, testReliableUDP) }

func testReliableUDP(t *testing.T, reverse bool) {
	e := setupLink(t, reverse, 0, true, defaultRO, "delay=5ms,loss=5%")
	if !udpEcho(t, e, 3000, 5, 2*time.Second) {
		t.Fatal("no SOCKS UDP echo")
	}
	uc, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	defer uc.Close()
	const n = 500
	go func() {
		for i := range n {
			b := make([]byte, 1000+i)
			binary.BigEndian.PutUint32(b, uint32(i))
			uc.WriteToUDP(b, e.udpTunnel)
			if i%5 == 4 {
				// ~3 MB/s: a burst far beyond the paths' rate would
				// (rightly) be dropped at the tunnel entrance
				time.Sleep(2 * time.Millisecond)
			}
		}
	}()
	b := make([]byte, 65536)
	for i := range n {
		uc.SetReadDeadline(time.Now().Add(5 * time.Second))
		k, from, err := uc.ReadFromUDP(b)
		if err != nil {
			t.Fatalf("datagram %d: %v", i, err)
		}
		if from.Port != e.udpTunnel.Port || k != 1000+i || binary.BigEndian.Uint32(b) != uint32(i) {
			t.Fatalf("datagram %d: got %d bytes #%d from %v", i, k, binary.BigEndian.Uint32(b), from)
		}
	}
}

// A UDP sender faster than the paths gets datagrams dropped at the tunnel
// entrance instead of seconds of queueing; what arrives is in order.
func TestReliableUDPBacklog(t *testing.T) {
	e := setupLink(t, false, 0, true, defaultRO, "delay=5ms,rate=8mbit")
	uc, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	defer uc.Close()
	uc.SetReadBuffer(8 << 20)
	const n, size = 4000, 1200 // 4.8 MB: 2.4 s at the paths' 2 MB/s
	var sentAll time.Time
	go func() {
		b := make([]byte, size)
		for i := range n {
			binary.BigEndian.PutUint32(b, uint32(i))
			uc.WriteToUDP(b, e.udpTunnel)
			if i%20 == 19 {
				time.Sleep(time.Millisecond) // ~20 MB/s
			}
		}
		sentAll = time.Now()
	}()
	b := make([]byte, 65536)
	got, last := 0, -1
	for {
		uc.SetReadDeadline(time.Now().Add(time.Second))
		k, _, err := uc.ReadFromUDP(b)
		if err != nil {
			break
		}
		i := int(binary.BigEndian.Uint32(b))
		if k != size || i <= last {
			t.Fatalf("datagram #%d (%d bytes) after #%d", i, k, last)
		}
		got, last = got+1, i
	}
	lag := time.Since(sentAll) - time.Second // minus the read timeout
	if got == n || got < n/10 {
		t.Fatalf("%d of %d datagrams arrived", got, n)
	}
	if lag > time.Second {
		t.Fatalf("last datagram arrived %v after the sender finished", lag)
	}
	t.Logf("%d of %d datagrams arrived, the last %v after the sender finished", got, n, lag.Round(time.Millisecond))
}

// The UDP tunnel in datagram mode.
func TestUDPTunnel(t *testing.T) { both(t, testUDPTunnel) }

func testUDPTunnel(t *testing.T, reverse bool) {
	e := setup(t, reverse)
	uc, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	defer uc.Close()
	b := make([]byte, 65536)
	for try := 0; try < 5; try++ {
		uc.WriteToUDP([]byte("ping"), e.udpTunnel)
		uc.SetReadDeadline(time.Now().Add(time.Second))
		k, _, err := uc.ReadFromUDP(b)
		if err == nil && string(b[:k]) == "ping" {
			return
		}
	}
	t.Fatal("no echo through the UDP tunnel")
}

// A remote without UDP relaying refuses reliable UDP streams.
func TestReliableUDPRefused(t *testing.T) {
	e := setupOpts(t, false, 0, true, RemoteOptions{DialTimeout: 5 * time.Second})
	if udpEcho(t, e, 10, 2, 300*time.Millisecond) {
		t.Fatal("UDP relayed by a remote with UDP off")
	}
}

// udpEcho sends a size-byte payload to the UDP echo target through SOCKS and
// reports whether it came back.
func udpEcho(t *testing.T, e *env, size, tries int, wait time.Duration) bool {
	ctl, bound := socksRequest(t, e.socks, 3, "0.0.0.0:0")
	defer ctl.Close()
	port := binary.BigEndian.Uint16(bound[len(bound)-2:])
	relay := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(port)}
	uc, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	defer uc.Close()
	target, _ := EncodeAddr(e.udpEcho.String())
	{
		payload := bytes.Repeat([]byte{byte(size)}, size)
		pkt := append(append([]byte{0, 0, 0}, target...), payload...)
		ok := false
		for try := 0; try < tries && !ok; try++ {
			uc.WriteToUDP(pkt, relay)
			uc.SetReadDeadline(time.Now().Add(wait))
			b := make([]byte, 65536)
			n, _, err := uc.ReadFromUDP(b)
			if err != nil {
				continue
			}
			if n < 3 || b[2] != 0 {
				t.Fatal("bad UDP reply header")
			}
			r := bytes.NewReader(b[3:n])
			from, _, err := ReadAddr(r)
			if err != nil || from != e.udpEcho.String() {
				t.Fatalf("reply from %q (%v), want %s", from, err, e.udpEcho)
			}
			got, _ := io.ReadAll(r)
			ok = bytes.Equal(got, payload)
		}
		return ok
	}
}

func TestEchoCodec(t *testing.T) {
	for _, ip := range []string{"192.0.2.1", "2001:db8::1"} {
		a := netip.MustParseAddr(ip)
		msg, hdr := AppendEcho([]byte{0xff}, a, 513, 65535, []byte("data"))
		if msg[0] != 0xff || msg[1] != icmpAssoc || string(msg[hdr:]) != "data" {
			t.Fatalf("bad echo datagram %x", msg)
		}
		got, flow, seq, data, ok := ParseEcho(msg[2:])
		if !ok || got != a || flow != 513 || seq != 65535 || string(data) != "data" {
			t.Fatalf("ParseEcho: %v %d %d %q %v", got, flow, seq, data, ok)
		}
		if _, _, _, _, ok := ParseEcho(msg[2 : hdr-1]); ok {
			t.Fatal("truncated echo datagram accepted")
		}
	}
	if _, _, ok := ParseAddrPort([]byte{atypDomain, 1, 'a', 0, 80}); ok {
		t.Fatal("domain accepted as an IP address")
	}
}

func TestAddrCodec(t *testing.T) {
	for _, a := range []string{"1.2.3.4:80", "[2001:db8::1]:443", "example.com:8080"} {
		b, err := EncodeAddr(a)
		if err != nil {
			t.Fatal(err)
		}
		got, raw, err := ReadAddr(bytes.NewReader(b))
		if err != nil || got != a || !bytes.Equal(raw, b) {
			t.Fatalf("%s -> %s (%v)", a, got, err)
		}
		if n := addrLen(append(b, 1, 2, 3)); n != len(b) {
			t.Fatalf("addrLen(%s) = %d, want %d", a, n, len(b))
		}
		if n := addrLen(b[:len(b)-1]); n != -1 {
			t.Fatalf("addrLen(truncated %s) = %d", a, n)
		}
	}
}

func both(t *testing.T, f func(*testing.T, bool)) {
	t.Run("normal", func(t *testing.T) { f(t, false) })
	t.Run("reverse", func(t *testing.T) { f(t, true) })
}
