package proxy

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// fakeNet scripts the outcome of each dial by address.
type fakeNet struct {
	mu      sync.Mutex
	t0      time.Time
	dials   []string        // addresses in dial order
	at      []time.Duration // start time of each dial
	aborted atomic.Int32    // dials ended by cancellation
	closed  atomic.Int32    // connections closed
	// behave returns the outcome of the n-th dial (0-based) of address a.
	behave func(n int, a string) (delay time.Duration, ok bool, err error)
}

type fakeConn struct {
	net.Conn
	f    *fakeNet
	once sync.Once
}

func (c *fakeConn) Close() error {
	c.once.Do(func() { c.f.closed.Add(1) })
	return c.Conn.Close()
}

func (f *fakeNet) dialer(addrs ...string) *raceDialer {
	f.t0 = time.Now()
	var ips []netip.Addr
	for _, a := range addrs {
		ips = append(ips, netip.MustParseAddr(a))
	}
	return &raceDialer{
		gaps:   raceGaps,
		lookup: func(context.Context, string) ([]netip.Addr, error) { return ips, nil },
		dial: func(ctx context.Context, a string) (net.Conn, error) {
			f.mu.Lock()
			n := len(f.dials)
			f.dials = append(f.dials, a)
			f.at = append(f.at, time.Since(f.t0))
			f.mu.Unlock()
			delay, ok, err := f.behave(n, a)
			if !ok && err == nil { // hang until cancelled
				<-ctx.Done()
				f.aborted.Add(1)
				return nil, ctx.Err()
			}
			if delay > 0 {
				time.Sleep(delay) // ignores ctx: may connect after the race is decided
			}
			if err != nil {
				return nil, err
			}
			c, _ := net.Pipe()
			return &fakeConn{Conn: c, f: f}, nil
		},
	}
}

func (f *fakeNet) snapshot() ([]string, []time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.dials...), append([]time.Duration(nil), f.at...)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func TestRaceStaggerRotatesAddresses(t *testing.T) {
	f := &fakeNet{behave: func(n int, a string) (time.Duration, bool, error) {
		return 0, n == 3, nil // only the 4th attempt (at 600 ms) connects
	}}
	r := f.dialer("192.0.2.1", "192.0.2.2", "2001:db8::1")
	c, st, err := r.dialContext(context.Background(), "example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if st.attempts != 4 || st.winner != 4 {
		t.Fatalf("stats = %+v", st)
	}
	dials, at := f.snapshot()
	want := []string{"192.0.2.1:443", "[2001:db8::1]:443", "192.0.2.2:443", "192.0.2.1:443"}
	for i := range want {
		if dials[i] != want[i] {
			t.Fatalf("dials = %v, want %v", dials, want)
		}
	}
	for i, w := range []time.Duration{0, 100, 300, 600} {
		if d := at[i] - w*time.Millisecond; d < 0 || d > 80*time.Millisecond {
			t.Errorf("attempt %d started at %v, want %v ms", i+1, at[i], w)
		}
	}
	waitFor(t, "losing attempts to be cancelled", func() bool { return f.aborted.Load() == 3 })
}

func TestRaceFailureStartsNextAttempt(t *testing.T) {
	refused := errors.New("connection refused")
	f := &fakeNet{behave: func(n int, a string) (time.Duration, bool, error) {
		if a == "192.0.2.1:80" {
			return 0, false, refused
		}
		return 0, true, nil
	}}
	start := time.Now()
	c, st, err := f.dialer("192.0.2.1", "192.0.2.2").dialContext(context.Background(), "h:80")
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if el := time.Since(start); el > 50*time.Millisecond || st.attempts != 2 || st.winner != 2 {
		t.Fatalf("stats %+v after %v: want the second attempt at once", st, el)
	}
}

func TestRaceAllAddressesFail(t *testing.T) {
	f := &fakeNet{behave: func(int, string) (time.Duration, bool, error) {
		return 0, false, syscall.ECONNREFUSED
	}}
	start := time.Now()
	_, st, err := f.dialer("192.0.2.1", "192.0.2.2").dialContext(context.Background(), "h:80")
	if !errors.Is(err, syscall.ECONNREFUSED) || st.attempts != 2 || time.Since(start) > 50*time.Millisecond {
		t.Fatalf("err %v, stats %+v: want both addresses refused, no retries", err, st)
	}
}

func TestRaceTimeout(t *testing.T) {
	f := &fakeNet{behave: func(int, string) (time.Duration, bool, error) { return 0, false, nil }}
	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()
	_, st, err := f.dialer("192.0.2.1").dialContext(ctx, "192.0.2.1:80")
	if !errors.Is(err, context.DeadlineExceeded) || st.attempts != 5 {
		t.Fatalf("err %v, stats %+v: want 5 attempts, then the deadline", err, st)
	}
	if dials, _ := f.snapshot(); len(dials) != 5 || dials[4] != "192.0.2.1:80" {
		t.Fatalf("dials = %v", dials)
	}
}

func TestRaceLateWinnerClosed(t *testing.T) {
	f := &fakeNet{behave: func(n int, a string) (time.Duration, bool, error) {
		if n == 0 {
			return 300 * time.Millisecond, true, nil // connects after attempt 2 won
		}
		return 0, true, nil
	}}
	c, st, err := f.dialer("192.0.2.1").dialContext(context.Background(), "h:80")
	if err != nil || st.winner != 2 {
		t.Fatalf("err %v, stats %+v", err, st)
	}
	waitFor(t, "the late connection to be closed", func() bool { return f.closed.Load() == 1 })
	c.Close()
}

func TestInterleaveFamilies(t *testing.T) {
	var in []netip.Addr
	for _, s := range []string{"2001:db8::1", "2001:db8::2", "::ffff:192.0.2.1", "2001:db8::1", "192.0.2.2"} {
		in = append(in, netip.MustParseAddr(s))
	}
	got := interleaveFamilies(in)
	want := []string{"2001:db8::1", "192.0.2.1", "2001:db8::2", "192.0.2.2"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
