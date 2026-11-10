package tundev

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
	"sync"
	"testing"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.zx2c4.com/wireguard/tun"
	"golang.zx2c4.com/wireguard/tun/netstack"

	"mpquic/internal/mp"
	"mpquic/internal/proxy"
)

// The tests put a userspace network stack (wireguard-go's netstack, a
// tun.Device) in the place of the operating system's interface: what a
// program on the machine would send through the interface, the test sends
// through the stack. scripts/tun-test.sh tests a real interface.

func tlog() *slog.Logger {
	lvl := slog.LevelWarn
	if os.Getenv("TUN_TEST_DEBUG") != "" {
		lvl = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

// hostIP returns a non-loopback IPv4 address of this host: the client
// stack does not route loopback addresses into the interface.
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

// fakePing answers ICMP echo requests like a host would (the test cannot
// rely on unprivileged ICMP sockets).
type fakePing struct {
	q      chan []byte
	from   chan net.Addr
	closed chan struct{}
	once   sync.Once
}

func (f *fakePing) WriteTo(b []byte, a net.Addr) (int, error) {
	m, err := icmp.ParseMessage(1, b)
	if err != nil || m.Type != ipv4.ICMPTypeEcho {
		return 0, errors.New("not an echo request")
	}
	m.Type = ipv4.ICMPTypeEchoReply
	r, _ := m.Marshal(nil)
	select {
	case f.q <- r:
		f.from <- a
	default:
	}
	return len(b), nil
}

func (f *fakePing) ReadFrom(b []byte) (int, net.Addr, error) {
	select {
	case r := <-f.q:
		return copy(b, r), <-f.from, nil
	case <-f.closed:
		return 0, nil, net.ErrClosed
	}
}

func (f *fakePing) Close() error                     { f.once.Do(func() { close(f.closed) }); return nil }
func (f *fakePing) LocalAddr() net.Addr              { return &net.UDPAddr{} }
func (f *fakePing) SetDeadline(time.Time) error      { return nil }
func (f *fakePing) SetReadDeadline(time.Time) error  { return nil }
func (f *fakePing) SetWriteDeadline(time.Time) error { return nil }

func listenPing(network, _ string) (net.PacketConn, error) {
	if network != "udp4" {
		return nil, errors.New("unsupported")
	}
	return &fakePing{q: make(chan []byte, 16), from: make(chan net.Addr, 16), closed: make(chan struct{})}, nil
}

type testEnv struct {
	host    netip.Addr
	tcpEcho netip.AddrPort
	udpEcho netip.AddrPort
	client  *mp.Client
	udp     *proxy.UDPLocal
	devs    []*Device
	kern    []tun.Device    // the "interfaces"
	tnets   []*netstack.Net // the machine's side of each interface
}

// onceDev closes a device once, as the devices of the operating systems do
// (the netstack one panics the second time).
type onceDev struct {
	tun.Device
	once sync.Once
}

func (d *onceDev) Close() (err error) {
	d.once.Do(func() { err = d.Device.Close() })
	return err
}

// clientAddr is the address of interface i.
func clientAddr(i int) netip.Addr { return netip.AddrFrom4([4]byte{10, 99, byte(i), 1}) }

// setup starts a remote, a local with n TUN devices, and echo servers.
func setup(t testing.TB, udpOn, reliable bool, n int) *testEnv {
	t.Helper()
	log := tlog()
	e := &testEnv{host: hostIP(t)}

	tl, err := net.Listen("tcp", netip.AddrPortFrom(e.host, 0).String())
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
	ue, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(netip.AddrPortFrom(e.host, 0)))
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
	e.udpEcho = ue.LocalAddr().(*net.UDPAddr).AddrPort()

	pc, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	remoteUDP := proxy.NewUDPRemote(log)
	remoteUDP.SetICMPListener(listenPing)
	ropt := &mp.Options{Password: "x", Logger: log, DatagramHandler: remoteUDP.HandleDatagram,
		StreamHandler: proxy.RemoteHandler(log, proxy.RemoteOptions{DialTimeout: 5 * time.Second, UDP: udpOn})}
	srv, err := mp.NewServer(ropt, nil)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(pc)
	lopt := &mp.Options{Password: "x", Logger: log}
	if udpOn {
		e.udp = proxy.NewUDPLocal(nil, log)
		e.udp.Reliable = reliable
		lopt.DatagramHandler = e.udp.HandleDatagram
	}
	e.client, err = mp.NewClient([]string{pc.LocalAddr().String(), pc.LocalAddr().String()}, lopt)
	if err != nil {
		t.Fatal(err)
	}
	if e.udp != nil {
		e.udp.Client = e.client
	}
	t.Cleanup(func() { e.client.Close(); srv.Close() })

	for i := range n {
		ns, tnet, err := netstack.CreateNetTUN([]netip.Addr{clientAddr(i)}, nil, DefaultMTU)
		if err != nil {
			t.Fatal(err)
		}
		kern := &onceDev{Device: ns}
		d, err := start(kern, fmt.Sprintf("test%d", i), Options{MTU: DefaultMTU, Opener: e.client, UDP: e.udp, Log: log})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(d.Close)
		e.devs, e.kern, e.tnets = append(e.devs, d), append(e.kern, kern), append(e.tnets, tnet)
	}
	deadline := time.Now().Add(5 * time.Second)
	for (e.client.Session() == nil || e.client.Session().ActivePaths() < 2) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	return e
}

func tcpEcho(t testing.TB, tnet *netstack.Net, target netip.AddrPort, size int) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
	c.SetReadDeadline(time.Now().Add(30 * time.Second))
	got, err := io.ReadAll(c)
	if err != nil {
		t.Error(err)
		return
	}
	if !bytes.Equal(got, data) {
		t.Errorf("echo mismatch: got %d bytes, want %d", len(got), len(data))
	}
}

func TestTCP(t *testing.T) {
	e := setup(t, false, false, 1)
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tcpEcho(t, e.tnets[0], e.tcpEcho, 1<<20)
		}()
	}
	wg.Wait()
}

func TestTCPRefused(t *testing.T) {
	e := setup(t, false, false, 1)
	l, _ := net.Listen("tcp", netip.AddrPortFrom(e.host, 0).String())
	closed := l.Addr().(*net.TCPAddr).AddrPort()
	l.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := e.tnets[0].DialContextTCPAddrPort(ctx, closed)
	if err == nil {
		// the connection is accepted before the remote has dialed: it is
		// reset then
		c.SetReadDeadline(time.Now().Add(10 * time.Second))
		_, err = c.Read(make([]byte, 1))
		c.Close()
	}
	if err == nil || errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("connection to a closed port: got %v, want a reset", err)
	}
}

func udpEcho(t *testing.T, tnet *netstack.Net, target netip.AddrPort, size int) {
	t.Helper()
	uc, err := tnet.DialUDPAddrPort(netip.AddrPort{}, target)
	if err != nil {
		t.Fatal(err)
	}
	defer uc.Close()
	data := bytes.Repeat([]byte("u"), size)
	buf := make([]byte, size+1)
	for range 5 {
		if _, err := uc.Write(data); err != nil {
			t.Fatal(err)
		}
		uc.SetReadDeadline(time.Now().Add(time.Second))
		n, err := uc.Read(buf)
		if err != nil {
			continue
		}
		if !bytes.Equal(buf[:n], data) {
			t.Fatalf("echo mismatch: got %d bytes, want %d", n, size)
		}
		return
	}
	t.Fatalf("no echo of a %d-byte datagram", size)
}

func TestUDP(t *testing.T) {
	e := setup(t, true, false, 1)
	udpEcho(t, e.tnets[0], e.udpEcho, 100)
	udpEcho(t, e.tnets[0], e.udpEcho, 1000)
}

// Reliable UDP carries datagrams of any size; the replies are fragmented to
// the MTU of the interface.
func TestUDPReliable(t *testing.T) {
	e := setup(t, true, true, 1)
	udpEcho(t, e.tnets[0], e.udpEcho, 100)
	udpEcho(t, e.tnets[0], e.udpEcho, 4000)
}

func ping(t *testing.T, tnet *netstack.Net, from, target netip.Addr, id int) {
	t.Helper()
	pc, err := tnet.DialPingAddr(from, target)
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	payload := fmt.Sprintf("ping %v %d", target, id)
	req := icmp.Message{Type: ipv4.ICMPTypeEcho, Body: &icmp.Echo{ID: id, Seq: 7, Data: []byte(payload)}}
	b, _ := req.Marshal(nil)
	for range 5 {
		if _, err := pc.Write(b); err != nil {
			t.Fatal(err)
		}
		pc.SetReadDeadline(time.Now().Add(time.Second))
		buf := make([]byte, 1500)
		n, src, err := pc.ReadFrom(buf)
		if err != nil {
			continue
		}
		m, err := icmp.ParseMessage(1, buf[:n])
		if err != nil || m.Type != ipv4.ICMPTypeEchoReply {
			t.Fatalf("reply %v %v", m, err)
		}
		if echo := m.Body.(*icmp.Echo); echo.Seq != 7 || string(echo.Data) != payload || src.String() != target.String() {
			t.Fatalf("reply from %v seq %d data %q, want %v 7 %q", src, echo.Seq, echo.Data, target, payload)
		}
		return
	}
	t.Fatalf("no echo reply from %v", target)
}

// Two interfaces on one session: each gets the replies to its own pings
// (they share the session's echo flow ids).
func TestPing(t *testing.T) {
	e := setup(t, true, false, 2)
	for i, tnet := range e.tnets {
		ping(t, tnet, clientAddr(i), netip.MustParseAddr("192.0.2.33"), 4242)
	}
}

// When the interface goes away under the device, Wait reports it; after
// Close it reports nothing.
func TestWait(t *testing.T) {
	e := setup(t, false, false, 2)
	done := make(chan error, 1)
	go func() { done <- e.devs[0].Wait() }()
	e.kern[0].Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Wait returned nil after the interface was closed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after the interface was closed")
	}
	e.devs[1].Close()
	if err := e.devs[1].Wait(); err != nil {
		t.Fatalf("Wait after Close: %v", err)
	}
}

func TestCheck(t *testing.T) {
	if supported() != nil {
		o := Options{}
		if o.Check() == nil {
			t.Fatal("accepted on a platform without TUN support")
		}
		return
	}
	ok := Options{MTU: 1400, Addresses: []string{"10.99.0.1/24", "fd99::1/64", "10.99.0.2"}, Routes: []string{"10.0.0.0/8", "default", "::/0", "192.0.2.1"}}
	if err := ok.Check(); err != nil {
		t.Fatal(err)
	}
	for name, o := range map[string]Options{
		"mtu":         {MTU: 500},
		"address":     {Addresses: []string{"10.0.0.300/24"}},
		"unspecified": {Addresses: []string{"0.0.0.0/0"}},
		"multicast":   {Addresses: []string{"224.0.0.1/24"}},
		"route":       {Routes: []string{"example.com"}},
		"route zone":  {Routes: []string{"fe80::1%eth0/64"}},
		"name":        {Name: "an interface name that is far too long"},
	} {
		if err := o.Check(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if p, _ := parseRoute("10.1.2.3/8"); p.String() != "10.0.0.0/8" {
		t.Errorf("route 10.1.2.3/8 parsed as %v", p)
	}
	if p, _ := parseAddress("10.1.2.3/8"); p.String() != "10.1.2.3/8" {
		t.Errorf("address 10.1.2.3/8 parsed as %v", p)
	}
}

// BenchmarkTCP echoes data through the interface (both directions at once).
func BenchmarkTCP(b *testing.B) {
	e := setup(b, false, false, 1)
	const size = 8 << 20
	b.SetBytes(2 * size)
	b.ResetTimer()
	for range b.N {
		tcpEcho(b, e.tnets[0], e.tcpEcho, size)
	}
}
