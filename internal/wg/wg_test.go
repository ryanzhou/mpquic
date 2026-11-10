package wg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"

	"mpquic/internal/mp"
	"mpquic/internal/proxy"
)

func tlog() *slog.Logger {
	lvl := slog.LevelWarn
	if os.Getenv("WG_TEST_DEBUG") != "" {
		lvl = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

// hostIP returns a non-loopback IPv4 address of this host: the peers'
// netstacks do not route loopback addresses into the tunnel.
func hostIP(t testing.TB) netip.Addr {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(n.IP.To4()); ok && n.IP.To4() != nil && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
				return ip
			}
		}
	}
	t.Skip("no non-loopback IPv4 address")
	return netip.Addr{}
}

type testEnv struct {
	tnet    *netstack.Net   // the first peer
	tnets   []*netstack.Net // all peers
	host    netip.Addr
	tcpEcho netip.AddrPort
	udpEcho []netip.AddrPort
	server  *Server
}

// fakePing answers ICMP echo requests like a host would (the test cannot
// rely on unprivileged ICMP sockets).
type fakePing struct {
	mu     sync.Mutex
	cond   *sync.Cond
	q      [][]byte
	from   []net.Addr
	closed bool
}

func newFakePing() *fakePing { f := &fakePing{}; f.cond = sync.NewCond(&f.mu); return f }

func (f *fakePing) WriteTo(b []byte, a net.Addr) (int, error) {
	m, err := icmp.ParseMessage(1, b)
	if err != nil || m.Type != ipv4.ICMPTypeEcho {
		return 0, errors.New("not an echo request")
	}
	m.Type = ipv4.ICMPTypeEchoReply
	r, _ := m.Marshal(nil)
	f.mu.Lock()
	f.q = append(f.q, r)
	f.from = append(f.from, a)
	f.cond.Signal()
	f.mu.Unlock()
	return len(b), nil
}

func (f *fakePing) ReadFrom(b []byte) (int, net.Addr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for len(f.q) == 0 && !f.closed {
		f.cond.Wait()
	}
	if f.closed {
		return 0, nil, net.ErrClosed
	}
	n := copy(b, f.q[0])
	a := f.from[0]
	f.q, f.from = f.q[1:], f.from[1:]
	return n, a, nil
}

func (f *fakePing) Close() error {
	f.mu.Lock()
	f.closed = true
	f.cond.Broadcast()
	f.mu.Unlock()
	return nil
}
func (f *fakePing) LocalAddr() net.Addr              { return &net.UDPAddr{} }
func (f *fakePing) SetDeadline(time.Time) error      { return nil }
func (f *fakePing) SetReadDeadline(time.Time) error  { return nil }
func (f *fakePing) SetWriteDeadline(time.Time) error { return nil }

// dgramPing provides unprivileged ICMP sockets; rawPing only a raw socket.
func dgramPing(network, _ string) (net.PacketConn, error) {
	if network != "udp4" {
		return nil, errors.New("unsupported")
	}
	return newFakePing(), nil
}

func rawPing(network, _ string) (net.PacketConn, error) {
	if network != "ip4:icmp" {
		return nil, errors.New("permission denied")
	}
	return newFakePing(), nil
}

func setup(t *testing.T, udpOn bool) *testEnv {
	t.Helper()
	return setupPing(t, udpOn, dgramPing)
}

func setupPing(t *testing.T, udpOn bool, listen func(string, string) (net.PacketConn, error)) *testEnv {
	t.Helper()
	return setupPeers(t, udpOn, false, listen, 1)
}

// setupPeers starts the environment with n WireGuard peers; peer i has the
// tunnel address 10.77.0.(2+i).
func setupPeers(t testing.TB, udpOn, reliable bool, listen func(string, string) (net.PacketConn, error), n int) *testEnv {
	t.Helper()
	log := tlog()
	host := hostIP(t)
	e := &testEnv{host: host}

	tl, err := net.Listen("tcp", netip.AddrPortFrom(host, 0).String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tl.Close() })
	go func() {
		for {
			c, err := tl.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	e.tcpEcho = tl.Addr().(*net.TCPAddr).AddrPort()
	for range 2 {
		ue, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(netip.AddrPortFrom(host, 0)))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ue.Close() })
		go func() {
			b := make([]byte, 65536)
			for {
				n, a, err := ue.ReadFromUDPAddrPort(b)
				if err != nil {
					return
				}
				ue.WriteToUDPAddrPort(b[:n], a)
			}
		}()
		e.udpEcho = append(e.udpEcho, ue.LocalAddr().(*net.UDPAddr).AddrPort())
	}

	// remote <-> local over two paths
	pc, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	remoteUDP := proxy.NewUDPRemote(log)
	remoteUDP.SetICMPListener(listen)
	ropt := &mp.Options{Password: "x", Logger: log, DatagramHandler: remoteUDP.HandleDatagram,
		StreamHandler: proxy.RemoteHandler(log, proxy.RemoteOptions{DialTimeout: 5 * time.Second, UDP: udpOn})}
	srv, err := mp.NewServer(ropt, nil)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(pc)
	var udp *proxy.UDPLocal
	lopt := &mp.Options{Password: "x", Logger: log}
	if udpOn {
		udp = proxy.NewUDPLocal(nil, log)
		udp.Reliable = reliable
		lopt.DatagramHandler = udp.HandleDatagram
	}
	cl, err := mp.NewClient([]string{pc.LocalAddr().String(), pc.LocalAddr().String()}, lopt)
	if err != nil {
		t.Fatal(err)
	}
	if udp != nil {
		udp.Client = cl
	}
	t.Cleanup(func() { cl.Close(); srv.Close() })

	// WireGuard server
	spriv, _ := GenerateKey()
	cprivs := make([]Key, n)
	var peers []Peer
	for i := range cprivs {
		cprivs[i], _ = GenerateKey()
		p := Peer{PublicKey: cprivs[i].PublicKey().String()}
		if n > 1 {
			p.AllowedIPs = []string{fmt.Sprintf("10.77.0.%d/32", 2+i)}
		}
		peers = append(peers, p)
	}
	s, err := Start(Options{Listen: "127.0.0.1:0", PrivateKey: spriv.String(), Peers: peers, Opener: cl, UDP: udp, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	e.server = s

	// WireGuard clients
	for i, cpriv := range cprivs {
		ctun, tnet, err := netstack.CreateNetTUN([]netip.Addr{netip.AddrFrom4([4]byte{10, 77, 0, byte(2 + i)})}, nil, DefaultMTU)
		if err != nil {
			t.Fatal(err)
		}
		disableRACK(t, tnet)
		cdev := device.NewDevice(ctun, conn.NewDefaultBind(), device.NewLogger(device.LogLevelSilent, ""))
		err = cdev.IpcSet(fmt.Sprintf("private_key=%s\npublic_key=%s\nendpoint=127.0.0.1:%d\nallowed_ip=0.0.0.0/0\n",
			cpriv.hex(), spriv.PublicKey().hex(), s.Port()))
		if err != nil {
			t.Fatal(err)
		}
		if err := cdev.Up(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(cdev.Close)
		e.tnets = append(e.tnets, tnet)
	}
	e.tnet = e.tnets[0]

	deadline := time.Now().Add(5 * time.Second)
	for (cl.Session() == nil || cl.Session().ActivePaths() < 2) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	return e
}

// disableRACK gives a client stack the server's loss recovery (see
// ipstack.New): with gVisor's default RACK, a client whose burst of data was
// lost (the server's UDP socket overflowing, likely with several peers on a
// slow machine) recovered at one or two segments per 200 ms and timed out.
// wireguard-go's netstack does not expose its gVisor stack.
func disableRACK(t testing.TB, n *netstack.Net) {
	t.Helper()
	f := reflect.ValueOf(n).Elem().FieldByName("stack")
	if !f.IsValid() {
		t.Fatal("netstack.Net has no stack field")
	}
	s := reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem().Interface().(*stack.Stack)
	opt := tcpip.TCPRecovery(0)
	if err := s.SetTransportProtocolOption(tcp.ProtocolNumber, &opt); err != nil {
		t.Fatal(err)
	}
}

func TestTCP(t *testing.T) {
	e := setup(t, false)
	tcpEcho(t, e.tnet, e.tcpEcho, 1<<20)
}

// Several peers transfer at once (their packets are processed in parallel).
func TestTCPPeers(t *testing.T) {
	e := setupPeers(t, false, false, dgramPing, 3)
	var wg sync.WaitGroup
	for _, tnet := range e.tnets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tcpEcho(t, tnet, e.tcpEcho, 1<<20)
		}()
	}
	wg.Wait()
}

func tcpEcho(t testing.TB, tnet *netstack.Net, target netip.AddrPort, size int) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := tnet.DialContextTCPAddrPort(ctx, target)
	if err != nil {
		t.Error(err)
		return
	}
	defer c.Close()
	data := bytes.Repeat([]byte("0123456789abcdef"), size/16)
	go func() {
		c.Write(data)
		c.CloseWrite()
	}()
	c.SetReadDeadline(time.Now().Add(20 * time.Second))
	got, err := io.ReadAll(c)
	if err != nil {
		t.Error(err)
		return
	}
	if !bytes.Equal(got, data) {
		t.Errorf("echo mismatch: got %d bytes, want %d", len(got), len(data))
	}
}

func TestTCPRefused(t *testing.T) {
	e := setup(t, false)
	l, _ := net.Listen("tcp", netip.AddrPortFrom(e.host, 0).String())
	closed := l.Addr().(*net.TCPAddr).AddrPort()
	l.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := e.tnet.DialContextTCPAddrPort(ctx, closed)
	if err != nil {
		return // refused outright is fine too
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err = c.Read(make([]byte, 1))
	if ne, ok := err.(net.Error); err == nil || ok && ne.Timeout() {
		t.Fatalf("connection to a closed port: read returned %v, want a reset", err)
	}
}

func udpRoundTrip(t *testing.T, c *net.UDPConn, uc interface {
	WriteTo([]byte, net.Addr) (int, error)
	ReadFrom([]byte) (int, net.Addr, error)
	SetReadDeadline(time.Time) error
}, to netip.AddrPort, size int) {
	t.Helper()
	msg := make([]byte, size)
	for i := range msg {
		msg[i] = byte(i * 7)
	}
	buf := make([]byte, 65536)
	for try := 0; try < 5; try++ {
		if _, err := uc.WriteTo(msg, net.UDPAddrFromAddrPort(to)); err != nil {
			t.Fatal(err)
		}
		uc.SetReadDeadline(time.Now().Add(time.Second))
		n, from, err := uc.ReadFrom(buf)
		if err != nil {
			continue
		}
		if from.String() != to.String() {
			t.Fatalf("reply from %v, want %v", from, to)
		}
		if !bytes.Equal(buf[:n], msg) {
			t.Fatalf("size %d: reply mismatch (%d bytes)", size, n)
		}
		return
	}
	t.Fatalf("no UDP reply for %d bytes from %v", size, to)
}

func TestUDP(t *testing.T) {
	for _, reliable := range []bool{false, true} {
		e := setupPeers(t, true, reliable, dgramPing, 1)
		uc, err := e.tnet.ListenUDPAddrPort(netip.AddrPortFrom(netip.MustParseAddr("10.77.0.2"), 0))
		if err != nil {
			t.Fatal(err)
		}
		// One socket, two targets (full-cone association). Datagrams
		// beyond a QUIC packet only pass with reliable UDP; 3000 bytes
		// need IP fragmentation in both directions.
		sizes := []int{1, 1000}
		if reliable {
			sizes = append(sizes, 3000, 9000)
		}
		for _, size := range sizes {
			for _, to := range e.udpEcho {
				udpRoundTrip(t, nil, uc, to, size)
			}
		}
		uc.Close()
	}
}

func TestUDPDisabled(t *testing.T) {
	e := setup(t, false)
	uc, err := e.tnet.DialUDPAddrPort(netip.AddrPortFrom(netip.MustParseAddr("10.77.0.2"), 0), e.udpEcho[0])
	if err != nil {
		t.Fatal(err)
	}
	defer uc.Close()
	// gVisor (the test peer) reports the ICMP error on the next operation
	// without waking a blocked Read.
	uc.Write([]byte("x"))
	var werr error
	for i := 0; i < 30 && werr == nil; i++ {
		time.Sleep(100 * time.Millisecond)
		_, werr = uc.Write([]byte("x"))
	}
	if werr == nil || !strings.Contains(werr.Error(), "refused") {
		t.Fatalf("UDP with relaying off: got %v, want connection refused (ICMP port unreachable)", werr)
	}
}

func TestPing(t *testing.T) {
	e := setup(t, true)
	ping(t, e, netip.MustParseAddr("192.0.2.33"), 4242)
}

// Without unprivileged ICMP sockets, the remote shares a raw socket among
// the flows and tells them apart by the echo identifier.
func TestPingRaw(t *testing.T) {
	e := setupPing(t, true, rawPing)
	var wg sync.WaitGroup
	for i, target := range []string{"192.0.2.1", "192.0.2.2", "192.0.2.1"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ping(t, e, netip.MustParseAddr(target), 100+i)
		}()
	}
	wg.Wait()
}

func ping(t *testing.T, e *testEnv, target netip.Addr, id int) {
	pc, err := e.tnet.DialPingAddr(netip.MustParseAddr("10.77.0.2"), target)
	if err != nil {
		t.Error(err)
		return
	}
	defer pc.Close()
	for seq := 1; seq <= 3; seq++ {
		payload := fmt.Sprintf("ping %v %d %d", target, id, seq)
		req := icmp.Message{Type: ipv4.ICMPTypeEcho, Body: &icmp.Echo{ID: id, Seq: seq, Data: []byte(payload)}}
		b, _ := req.Marshal(nil)
		got := false
		for try := 0; try < 3 && !got; try++ {
			if _, err := pc.Write(b); err != nil {
				t.Error(err)
				return
			}
			pc.SetReadDeadline(time.Now().Add(time.Second))
			buf := make([]byte, 1500)
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				continue
			}
			m, err := icmp.ParseMessage(1, buf[:n])
			if err != nil || m.Type != ipv4.ICMPTypeEchoReply {
				t.Errorf("reply %v %v", m, err)
				return
			}
			echo := m.Body.(*icmp.Echo)
			if echo.Seq != seq || string(echo.Data) != payload || from.String() != target.String() {
				t.Errorf("reply from %v seq %d data %q, want %v %d %q", from, echo.Seq, echo.Data, target, seq, payload)
				return
			}
			got = true
		}
		if !got {
			t.Errorf("no echo reply from %v for seq %d", target, seq)
			return
		}
	}
}

// BenchmarkTCP echoes data through a WireGuard peer (both directions at
// once); compare with the same benchmark of package tundev, which has no
// WireGuard in the way.
func BenchmarkTCP(b *testing.B) {
	e := setupPeers(b, false, false, dgramPing, 1)
	const size = 8 << 20
	b.SetBytes(2 * size)
	b.ResetTimer()
	for range b.N {
		tcpEcho(b, e.tnet, e.tcpEcho, size)
	}
}
