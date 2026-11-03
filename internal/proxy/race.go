package proxy

import (
	"context"
	"net"
	"net/netip"
	"time"
)

// raceGaps are the delays between the staggered connection attempts of
// race_connect: attempts start 0, 100, 300, 600 and 1000 ms after the
// target is resolved, until one connects. An attempt that fails outright
// (e.g. refused) starts the next one at once.
var raceGaps = []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 300 * time.Millisecond, 400 * time.Millisecond}

// raceDialer opens a TCP connection by racing staggered attempts and keeping
// the first that connects. Each attempt is a new socket (a new source port,
// so a new NAT mapping); attempts rotate over the target's addresses,
// alternating address families, and reuse them when there are fewer
// addresses than attempts. It helps when connection setup (SYN / SYN-ACK) is
// lost, e.g. by an overloaded NAT, where the OS would wait 1 s, then 3 s...
// to retransmit the SYN.
type raceDialer struct {
	gaps   []time.Duration
	lookup func(ctx context.Context, host string) ([]netip.Addr, error)
	dial   func(ctx context.Context, address string) (net.Conn, error)
}

func newRaceDialer() *raceDialer {
	d := &net.Dialer{KeepAlive: 30 * time.Second}
	return &raceDialer{
		gaps: raceGaps,
		lookup: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		},
		dial: func(ctx context.Context, address string) (net.Conn, error) {
			return d.DialContext(ctx, "tcp", address)
		},
	}
}

// raceStats describes a raced dial: the number of attempts started and which
// one connected (1-based, 0 if none).
type raceStats struct {
	attempts, winner int
}

type raceResult struct {
	n    int // attempt index
	conn net.Conn
	err  error
}

// dialContext connects to target (host:port). ctx bounds the whole dial,
// including the DNS lookup.
func (r *raceDialer) dialContext(ctx context.Context, target string) (net.Conn, raceStats, error) {
	var st raceStats
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		return nil, st, err
	}
	var addrs []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{ip}
	} else {
		if addrs, err = r.lookup(ctx, host); err != nil {
			return nil, st, err
		}
		if addrs = interleaveFamilies(addrs); len(addrs) == 0 {
			return nil, st, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel() // aborts the attempts still connecting
	maxAttempts := len(r.gaps) + 1
	results := make(chan raceResult, maxAttempts) // never blocks an attempt
	failed := make([]bool, len(addrs))            // address refused / unreachable
	addrOf := make([]int, 0, maxAttempts)         // address index of each attempt
	next, inflight := 0, 0
	start := func() bool {
		if st.attempts == maxAttempts || ctx.Err() != nil {
			return false
		}
		for range addrs {
			i := next
			next = (next + 1) % len(addrs)
			if failed[i] {
				continue
			}
			n := st.attempts
			st.attempts++
			inflight++
			addrOf = append(addrOf, i)
			a := net.JoinHostPort(addrs[i].String(), port)
			go func() {
				c, err := r.dial(ctx, a)
				results <- raceResult{n, c, err}
			}()
			return true
		}
		return false // every address failed
	}
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	// started arms the timer of the attempt after the one just started.
	started := func() {
		if st.attempts < maxAttempts {
			timer.Reset(r.gaps[st.attempts-1])
		} else {
			timer.Stop()
		}
	}
	start()
	started()
	var firstErr error
	for {
		select {
		case <-timer.C:
			if start() {
				started()
			}
		case res := <-results:
			inflight--
			if res.err == nil {
				st.winner = res.n + 1
				// Close the attempts that connect anyway (cancel can lose the
				// race with their handshake).
				go func(n int) {
					for range n {
						if r := <-results; r.conn != nil {
							r.conn.Close()
						}
					}
				}(inflight)
				return res.conn, st, nil
			}
			if firstErr == nil {
				firstErr = res.err
			}
			failed[addrOf[res.n]] = true
			if start() {
				started()
			} else if inflight == 0 {
				return nil, st, firstErr
			}
		}
	}
}

// interleaveFamilies orders addresses for racing: duplicates removed and
// families alternated, starting with the family of the resolver's first
// choice (RFC 8305).
func interleaveFamilies(addrs []netip.Addr) []netip.Addr {
	var first, other []netip.Addr
	seen := make(map[netip.Addr]bool, len(addrs))
	for _, a := range addrs {
		a = a.Unmap()
		if !a.IsValid() || seen[a] {
			continue
		}
		seen[a] = true
		if len(first) == 0 || a.Is4() == first[0].Is4() {
			first = append(first, a)
		} else {
			other = append(other, a)
		}
	}
	out := make([]netip.Addr, 0, len(first)+len(other))
	for i := range max(len(first), len(other)) {
		if i < len(first) {
			out = append(out, first[i])
		}
		if i < len(other) {
			out = append(out, other[i])
		}
	}
	return out
}
