package linkem

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func echoServer(t *testing.T, addr string) *net.UDPConn {
	ua, _ := net.ResolveUDPAddr("udp", addr)
	c, err := net.ListenUDP("udp", ua)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		b := make([]byte, 2048)
		for {
			n, a, err := c.ReadFromUDP(b)
			if err != nil {
				return
			}
			c.WriteToUDP(b[:n], a)
		}
	}()
	return c
}

func ping(c *net.UDPConn, to *net.UDPAddr, wait time.Duration) (time.Duration, bool) {
	start := time.Now()
	c.WriteToUDP([]byte("ping"), to)
	c.SetReadDeadline(time.Now().Add(wait))
	b := make([]byte, 16)
	_, _, err := c.ReadFromUDP(b)
	return time.Since(start), err == nil
}

// The flow's reader must survive ECONNREFUSED (the target briefly down).
func TestRegressRelayTargetRestart(t *testing.T) {
	// reserve a port for the target, then free it: the target is "down"
	tmp, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	target := tmp.LocalAddr().String()
	tmp.Close()
	r, err := NewRelay("127.0.0.1:0", target, Spec{}, Spec{})
	if err != nil {
		t.Fatal(err)
	}
	go r.Serve()
	defer r.Close()
	cl, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	defer cl.Close()
	if _, ok := ping(cl, r.Addr(), 300*time.Millisecond); ok {
		t.Fatal("unexpected reply while the target is down")
	}
	// the target comes (back) up
	es := echoServer(t, target)
	defer es.Close()
	for i := 0; i < 10; i++ {
		if d, ok := ping(cl, r.Addr(), 300*time.Millisecond); ok {
			t.Logf("reply after %d retries (%v)", i, d)
			return
		}
	}
	// a fresh client address works: the relay itself is fine
	cl2, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	defer cl2.Close()
	_, ok2 := ping(cl2, r.Addr(), 300*time.Millisecond)
	t.Fatalf("client flow stays deaf after the target came back (10 retries over 3 s); new client address works: %v", ok2)
}

// A resolver that stops answering must not hold up the relay: established
// flows keep going, and new ones use the last answer.
func TestRegressRelayDNSStall(t *testing.T) {
	es := echoServer(t, "127.0.0.1:0")
	defer es.Close()
	var dnsUp atomic.Bool
	dnsUp.Store(true)
	var queries atomic.Int64
	resolve := func(ctx context.Context, hostport string) (*net.UDPAddr, error) {
		queries.Add(1)
		if !dnsUp.Load() {
			<-ctx.Done() // resolver outage: no answer
			return nil, ctx.Err()
		}
		return es.LocalAddr().(*net.UDPAddr), nil
	}
	testResolve.Store(&resolve)
	defer testResolve.Store(nil)
	defer func(d time.Duration) { targetTTL = d }(targetTTL)
	targetTTL = 50 * time.Millisecond

	r, err := NewRelay("127.0.0.1:0", "relay-target.test:9", Spec{}, Spec{})
	if err != nil {
		t.Fatal(err)
	}
	go r.Serve()
	defer r.Close()
	a, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	defer a.Close()
	ok := false
	for i := 0; i < 20 && !ok; i++ { // the first packets may precede the first answer
		_, ok = ping(a, r.Addr(), 100*time.Millisecond)
	}
	if !ok {
		t.Fatal("no echo through the relay")
	}

	dnsUp.Store(false)        // the resolver stops answering
	time.Sleep(2 * targetTTL) // the answer is stale now
	b, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	defer b.Close()
	if d, ok := ping(b, r.Addr(), 2*time.Second); !ok || d > 500*time.Millisecond {
		t.Fatalf("new client while the lookup hangs: echo ok=%v after %v", ok, d)
	}
	if d, ok := ping(a, r.Addr(), 2*time.Second); !ok || d > 500*time.Millisecond {
		t.Fatalf("established flow while the lookup hangs: echo ok=%v after %v", ok, d)
	}
	for d := time.Now().Add(2 * time.Second); queries.Load() < 2 && time.Now().Before(d); {
		time.Sleep(5 * time.Millisecond) // the lookup runs in the background
	}
	if queries.Load() < 2 {
		t.Fatalf("the stale answer was not looked up again (%d lookups)", queries.Load())
	}
}
