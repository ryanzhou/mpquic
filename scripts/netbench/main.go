// Command netbench measures throughput and latency of TCP and UDP, directly
// or through a SOCKS5 proxy (CONNECT and UDP ASSOCIATE). It is used by
// scripts/bench-wireguard.sh to compare the SOCKS5 and WireGuard entrypoints.
//
//	netbench server [-tcp :5201] [-udp :5202]
//	netbench client -target HOST [-socks HOST:PORT] -mode down|up|rr|connect|udprr [...]
package main

import (
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: netbench server|client ...")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "server":
		err = server(os.Args[2:])
	case "client":
		err = client(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "netbench:", err)
		os.Exit(1)
	}
}

// TCP sessions start with one byte: 'D' server sends until the client
// closes, 'U' server discards, 'E' server echoes.
func server(args []string) error {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	tcpAddr := fs.String("tcp", ":5201", "TCP listen address")
	udpAddr := fs.String("udp", ":5202", "UDP echo listen address")
	fs.Parse(args)
	ln, err := net.Listen("tcp", *tcpAddr)
	if err != nil {
		return err
	}
	pc, err := net.ListenPacket("udp", *udpAddr)
	if err != nil {
		return err
	}
	go func() {
		b := make([]byte, 65536)
		for {
			n, a, err := pc.ReadFrom(b)
			if err != nil {
				return
			}
			pc.WriteTo(b[:n], a)
		}
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go serve(c)
	}
}

func serve(c net.Conn) {
	defer c.Close()
	c.(*net.TCPConn).SetNoDelay(true)
	var m [1]byte
	if _, err := io.ReadFull(c, m[:]); err != nil {
		return
	}
	buf := make([]byte, 128<<10)
	switch m[0] {
	case 'D':
		for {
			if _, err := c.Write(buf); err != nil {
				return
			}
		}
	case 'U':
		io.CopyBuffer(io.Discard, c, buf)
	case 'E':
		io.CopyBuffer(c, c, buf)
	}
}

type opts struct {
	target, socks string
	tcpPort       int
	udpPort       int
}

func client(args []string) error {
	fs := flag.NewFlagSet("client", flag.ExitOnError)
	var o opts
	fs.StringVar(&o.target, "target", "", "server host")
	fs.StringVar(&o.socks, "socks", "", "SOCKS5 proxy host:port (default: direct)")
	fs.IntVar(&o.tcpPort, "tcp-port", 5201, "server TCP port")
	fs.IntVar(&o.udpPort, "udp-port", 5202, "server UDP port")
	mode := fs.String("mode", "down", "down|up|rr|connect|udprr")
	dur := fs.Duration("t", 10*time.Second, "measured duration (down/up)")
	warm := fs.Duration("warmup", 2*time.Second, "unmeasured warm-up (down/up)")
	par := fs.Int("P", 1, "parallel streams (down/up)")
	size := fs.Int("size", 64, "message size (rr/udprr)")
	n := fs.Int("n", 1000, "iterations (rr/connect/udprr)")
	fs.Parse(args)
	switch *mode {
	case "down", "up":
		bps, err := bulk(o, *mode == "down", *par, *warm, *dur)
		if err != nil {
			return err
		}
		fmt.Printf("mode=%s P=%d mbps=%.1f\n", *mode, *par, bps/1e6)
	case "rr", "connect", "udprr":
		var lat []time.Duration
		var lost int
		var err error
		switch *mode {
		case "rr":
			lat, err = rr(o, *size, *n)
		case "connect":
			lat, err = connects(o, *n)
		default:
			lat, lost, err = udprr(o, *size, *n)
		}
		if err != nil {
			return err
		}
		slices.Sort(lat)
		q := func(p float64) float64 {
			if len(lat) == 0 {
				return 0
			}
			return float64(lat[min(len(lat)-1, int(p*float64(len(lat))))].Microseconds()) / 1000
		}
		fmt.Printf("mode=%s n=%d lost=%d p50_ms=%.3f p90_ms=%.3f p99_ms=%.3f\n", *mode, len(lat), lost, q(0.5), q(0.9), q(0.99))
	default:
		return fmt.Errorf("unknown mode %q", *mode)
	}
	return nil
}

func socksAddr(host string, port int) []byte {
	ip := net.ParseIP(host)
	var b []byte
	if v4 := ip.To4(); v4 != nil {
		b = append([]byte{1}, v4...)
	} else if ip != nil {
		b = append([]byte{4}, ip.To16()...)
	} else {
		b = append([]byte{3, byte(len(host))}, host...)
	}
	return binary.BigEndian.AppendUint16(b, uint16(port))
}

// socksRequest performs a SOCKS5 request and returns the control
// connection and the bound address.
func socksRequest(proxy string, cmd byte, addr []byte) (net.Conn, []byte, error) {
	c, err := net.Dial("tcp", proxy)
	if err != nil {
		return nil, nil, err
	}
	c.(*net.TCPConn).SetNoDelay(true)
	req := append([]byte{5, 1, 0, 5, cmd, 0}, addr...)
	if _, err := c.Write(req); err != nil {
		c.Close()
		return nil, nil, err
	}
	var r [2 + 4]byte
	if _, err := io.ReadFull(c, r[:]); err != nil || r[1] != 0 || r[3] != 0 {
		c.Close()
		return nil, nil, fmt.Errorf("socks handshake failed: %v %v", r, err)
	}
	var bnd []byte
	switch r[5] {
	case 1:
		bnd = make([]byte, 4+2)
	case 4:
		bnd = make([]byte, 16+2)
	default:
		c.Close()
		return nil, nil, errors.New("unexpected socks bound address")
	}
	if _, err := io.ReadFull(c, bnd); err != nil {
		c.Close()
		return nil, nil, err
	}
	return c, append([]byte{r[5]}, bnd...), nil
}

func dialTCP(o opts) (net.Conn, error) {
	if o.socks == "" {
		c, err := net.Dial("tcp", net.JoinHostPort(o.target, strconv.Itoa(o.tcpPort)))
		if err == nil {
			c.(*net.TCPConn).SetNoDelay(true)
		}
		return c, err
	}
	c, _, err := socksRequest(o.socks, 1, socksAddr(o.target, o.tcpPort))
	return c, err
}

func bulk(o opts, down bool, par int, warm, dur time.Duration) (float64, error) {
	var total atomic.Int64
	var measuring atomic.Bool
	stop := make(chan struct{})
	errc := make(chan error, par)
	var wg sync.WaitGroup
	for range par {
		c, err := dialTCP(o)
		if err != nil {
			return 0, err
		}
		defer c.Close()
		m := byte('U')
		if down {
			m = 'D'
		}
		if _, err := c.Write([]byte{m}); err != nil {
			return 0, err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 128<<10)
			for {
				select {
				case <-stop:
					return
				default:
				}
				var n int
				var err error
				if down {
					n, err = c.Read(buf)
				} else {
					n, err = c.Write(buf)
				}
				if measuring.Load() {
					total.Add(int64(n))
				}
				if err != nil {
					errc <- err
					return
				}
			}
		}()
	}
	select {
	case <-time.After(warm):
	case err := <-errc:
		return 0, err
	}
	measuring.Store(true)
	start := time.Now()
	select {
	case <-time.After(dur):
	case err := <-errc:
		return 0, err
	}
	measuring.Store(false)
	elapsed := time.Since(start)
	close(stop)
	return float64(total.Load()) * 8 / elapsed.Seconds(), nil
}

func rr(o opts, size, n int) ([]time.Duration, error) {
	c, err := dialTCP(o)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if _, err := c.Write([]byte{'E'}); err != nil {
		return nil, err
	}
	msg := make([]byte, size)
	got := make([]byte, size)
	lat := make([]time.Duration, 0, n)
	for i := range n + 10 { // 10 warm-up round trips
		t0 := time.Now()
		if _, err := c.Write(msg); err != nil {
			return nil, err
		}
		if _, err := io.ReadFull(c, got); err != nil {
			return nil, err
		}
		if i >= 10 {
			lat = append(lat, time.Since(t0))
		}
	}
	return lat, nil
}

// connects measures new-connection latency: from dialing to the first echo,
// so that a proxy that accepts immediately is still timed end to end.
func connects(o opts, n int) ([]time.Duration, error) {
	lat := make([]time.Duration, 0, n)
	for range n {
		t0 := time.Now()
		c, err := dialTCP(o)
		if err != nil {
			return nil, err
		}
		_, err = c.Write([]byte{'E', 1})
		var b [1]byte
		if err == nil {
			_, err = io.ReadFull(c, b[:])
		}
		c.Close()
		if err != nil {
			return nil, err
		}
		lat = append(lat, time.Since(t0))
	}
	return lat, nil
}

func udprr(o opts, size, n int) ([]time.Duration, int, error) {
	var pc *net.UDPConn
	var dst *net.UDPAddr
	var hdr []byte
	var err error
	target := net.JoinHostPort(o.target, strconv.Itoa(o.udpPort))
	if o.socks == "" {
		dst, err = net.ResolveUDPAddr("udp", target)
		if err != nil {
			return nil, 0, err
		}
		pc, err = net.ListenUDP("udp", nil)
	} else {
		ctl, bnd, err := socksRequest(o.socks, 3, socksAddr("0.0.0.0", 0))
		if err != nil {
			return nil, 0, err
		}
		defer ctl.Close()
		port := int(binary.BigEndian.Uint16(bnd[len(bnd)-2:]))
		host, _, _ := net.SplitHostPort(o.socks)
		dst = &net.UDPAddr{IP: net.ParseIP(host), Port: port}
		if dst.IP == nil {
			a, err := net.ResolveIPAddr("ip", host)
			if err != nil {
				return nil, 0, err
			}
			dst.IP = a.IP
		}
		hdr = append([]byte{0, 0, 0}, socksAddr(o.target, o.udpPort)...)
		pc, err = net.ListenUDP("udp", nil)
	}
	if err != nil {
		return nil, 0, err
	}
	defer pc.Close()
	msg := make([]byte, len(hdr)+size+8)
	copy(msg, hdr)
	buf := make([]byte, 65536)
	lat := make([]time.Duration, 0, n)
	lost := 0
	for i := range n + 10 {
		seq := uint64(i)
		binary.BigEndian.PutUint64(msg[len(hdr):], seq)
		t0 := time.Now()
		if _, err := pc.WriteToUDP(msg, dst); err != nil {
			return nil, 0, err
		}
		pc.SetReadDeadline(t0.Add(time.Second))
		for {
			m, _, err := pc.ReadFromUDP(buf)
			if err != nil {
				if i >= 10 {
					lost++
				}
				break
			}
			if m == len(msg) && binary.BigEndian.Uint64(buf[len(hdr):]) == seq {
				if i >= 10 {
					lat = append(lat, time.Since(t0))
				}
				break
			}
		}
	}
	return lat, lost, nil
}
