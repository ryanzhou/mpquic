package proxy

import (
	"bytes"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	"mpquic/internal/mp"
)

func tlog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

type env struct {
	socks, tunnel string
	tcpEcho       string
	udpEcho       *net.UDPAddr
}

func setup(t *testing.T, reverse bool) *env {
	t.Helper()
	return setupMTU(t, reverse, 0)
}

func setupMTU(t *testing.T, reverse bool, mtu int) *env {
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
	udp := NewUDPLocal(nil, log)
	ropt := &mp.Options{Password: "x", Logger: log, DatagramHandler: NewUDPRemote(log).HandleDatagram,
		StreamHandler: RemoteHandler(log, 5*time.Second), MTU: mtu}
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
	t.Cleanup(func() {
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
	return &env{socks: sl.Addr().String(), tunnel: tn.Addr().String(), tcpEcho: tl.Addr().String(), udpEcho: ue.LocalAddr().(*net.UDPAddr)}
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
	for _, size := range []int{10, 1000, 4000} { // 4000 exceeds a QUIC datagram: control-stream fallback
		if !udpEcho(t, e, size, 5, time.Second) {
			t.Fatalf("no UDP echo for size %d", size)
		}
	}
}

// With a 1280-byte path MTU, a QUIC DATAGRAM carries 1280-28-37 = 1215
// bytes: the type byte, the association id (1 byte) and the IPv4 address (7)
// leave 1206 for the UDP payload. Bigger payloads up to 1472 bytes are dropped
// as a network with that MTU would; even bigger ones are sent reliably.
func TestSocksUDPOversize(t *testing.T) { both(t, testSocksUDPOversize) }

func testSocksUDPOversize(t *testing.T, reverse bool) {
	e := setupMTU(t, reverse, 1280)
	for _, c := range []struct {
		size int
		ok   bool
	}{{1206, true}, {1207, false}, {1472, false}, {4000, true}} {
		tries, wait := 5, time.Second
		if !c.ok {
			tries, wait = 2, 300*time.Millisecond
		}
		if got := udpEcho(t, e, c.size, tries, wait); got != c.ok {
			t.Errorf("payload %d: echoed %v, want %v", c.size, got, c.ok)
		}
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
