package mp

import (
	"bytes"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

func TestPathQUICConfig(t *testing.T) {
	v4 := &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 1}
	v4m := &net.UDPAddr{IP: net.ParseIP("::ffff:192.0.2.1"), Port: 1}
	v6 := &net.UDPAddr{IP: net.ParseIP("2001:db8::1"), Port: 1}
	base := quicConfig(&Options{})
	if base.InitialPacketSize != 1200 || base.DisablePathMTUDiscovery {
		t.Fatalf("default: %d %v", base.InitialPacketSize, base.DisablePathMTUDiscovery)
	}
	if c, _ := pathQUICConfig(base, &Options{}, v4); c != base {
		t.Fatal("no MTU: config changed")
	}
	for _, c := range []struct {
		mtu      int
		addr     net.Addr
		size     uint16
		tooSmall bool
	}{
		{1280, v4, 1252, false},
		{1280, v4m, 1252, false},
		{1280, v6, 1232, false},
		{1500, v4, 1452, false}, // quic-go's maximum
		{1500, v6, 1452, false},
		{9000, v4, 1452, false},
		{1240, v6, 1200, true},
	} {
		qc, small := pathQUICConfig(base, &Options{MTU: c.mtu}, c.addr)
		if qc.InitialPacketSize != c.size || !qc.DisablePathMTUDiscovery || small != c.tooSmall {
			t.Errorf("mtu %d %v: size %d discovery off %v tooSmall %v", c.mtu, c.addr, qc.InitialPacketSize, qc.DisablePathMTUDiscovery, small)
		}
	}
	if base.DisablePathMTUDiscovery || base.InitialPacketSize != 1200 {
		t.Fatal("base config modified")
	}
}

// Paths whose MTU is below 1308 bytes (e.g. 1280-byte tunnels) never
// connected while the first packet was padded to 1280 bytes.
func TestSmallMTUPaths(t *testing.T) {
	for _, c := range []struct {
		mtu   int
		paths []string
	}{
		{0, []string{"delay=5ms,mtu=1280", "delay=10ms,mtu=1240"}},
		{1280, []string{"delay=5ms,mtu=1280", "delay=10ms,mtu=1300"}},
	} {
		mtu := c.mtu
		tb := newTestbed(t, c.paths, echoHandler, func(o *Options) { o.MTU = mtu })
		echoRoundTrip(t, tb.cl, randBytes(1<<20))
		tb.close()
	}
}

type dgramSink struct {
	mu  sync.Mutex
	got [][]byte
}

func (d *dgramSink) handle(_ *Session, b []byte) {
	d.mu.Lock()
	d.got = append(d.got, append([]byte(nil), b...))
	d.mu.Unlock()
}

func (d *dgramSink) has(b []byte, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for {
		d.mu.Lock()
		for _, g := range d.got {
			if bytes.Equal(g, b) {
				d.mu.Unlock()
				return true
			}
		}
		d.mu.Unlock()
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestOversizeDatagrams(t *testing.T) {
	old := pmtuSettle
	pmtuSettle = 300 * time.Millisecond
	defer func() { pmtuSettle = old }()
	const hdr = 9 // e.g. association id + SOCKS IPv4 address
	dgram := func(payload int) []byte {
		b := randBytes(hdr + payload)
		return b
	}
	for _, mtu := range []int{0, 1280} {
		sink := &dgramSink{}
		tb := newTestbed(t, []string{"delay=5ms,mtu=1280"}, echoHandler, func(o *Options) {
			o.MTU = mtu
			o.DatagramHandler = sink.handle
		})
		sess := tb.cl.Session()
		if mtu == 0 {
			// before the path MTU is known, a datagram that does not fit
			// is sent reliably (the limit may still grow)
			b := dgram(1300)
			if err := sess.SendDatagram(b, hdr); err != nil {
				t.Fatalf("mtu %d, unsettled: %v", mtu, err)
			}
			if !sink.has(b, 2*time.Second) {
				t.Fatalf("mtu %d, unsettled: 1300-byte datagram not delivered", mtu)
			}
			time.Sleep(time.Second) // > pmtuSettle and 40 RTTs
		}
		// 1280-byte path: 1252-byte UDP payload, 1215 for the DATAGRAM,
		// minus the type byte and the header (discovery may end up to 20
		// bytes lower, and start at 1200)
		maxLim := 1280 - 28 - 37 - 1 - hdr
		minLim := maxLim
		if mtu == 0 {
			minLim = 1200 - 37 - 1 - hdr
		}
		if lim := sess.DatagramLimit(hdr); lim < minLim || lim > maxLim {
			t.Errorf("mtu %d: limit %d", mtu, lim)
		}
		small := dgram(1000)
		if err := sess.SendDatagram(small, hdr); err != nil {
			t.Fatalf("mtu %d: %v", mtu, err)
		}
		mid := dgram(1300)
		err := sess.SendDatagram(mid, hdr)
		var tl *DatagramTooLargeError
		if !errors.As(err, &tl) || tl.Size != 1300 || tl.Max < minLim || tl.Max > maxLim {
			t.Fatalf("mtu %d: 1300-byte payload: err = %v", mtu, err)
		}
		big := dgram(3000)
		if err := sess.SendDatagram(big, hdr); err != nil {
			t.Fatalf("mtu %d: %v", mtu, err)
		}
		if !sink.has(small, 2*time.Second) || !sink.has(big, 2*time.Second) {
			t.Fatalf("mtu %d: datagrams not delivered", mtu)
		}
		if sink.has(mid, 200*time.Millisecond) {
			t.Fatalf("mtu %d: oversize datagram delivered", mtu)
		}
		tb.close()
	}
}
