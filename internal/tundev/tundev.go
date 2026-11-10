// Package tundev is the TUN front-end of the local side: a network
// interface of the operating system whose traffic is proxied transparently
// over the multipath connection (see package ipstack). Whatever the system
// routes to the interface is relayed: TCP, UDP and ping, of the machine
// itself or forwarded for others. Unlike the WireGuard listener there is
// nothing to encrypt and decrypt on the way to the tunnel.
package tundev

import (
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"

	"golang.zx2c4.com/wireguard/tun"

	"mpquic/internal/ipstack"
	"mpquic/internal/proxy"
)

// DefaultMTU is the default MTU of the interface. The tunnel adds no
// per-packet overhead to TCP (connections are terminated here); UDP payloads
// larger than a QUIC datagram need --reliable-udp.
const DefaultMTU = 1500

const (
	// headroom in front of every packet, for the device's own headers
	// (Linux: 10 bytes of virtio-net header, macOS: 4 bytes of family)
	offset    = 16
	maxPacket = 65535
)

// Options configure a Device.
type Options struct {
	// Name of the interface (default DefaultName). An existing persistent
	// TUN interface of that name is attached to.
	Name string
	MTU  int // default DefaultMTU
	// Addresses ("ADDR/BITS") are assigned to the interface.
	Addresses []string
	// Routes ("ADDR/BITS") are added to the main routing table, through the
	// interface.
	Routes []string
	Opener proxy.Opener
	// UDP relays UDP and ICMP echo; nil rejects UDP (ICMP port
	// unreachable) and drops pings.
	UDP *proxy.UDPLocal
	Log *slog.Logger
}

// Check validates the options that do not depend on the runtime.
func (o *Options) Check() error {
	if err := supported(); err != nil {
		return err
	}
	name := o.Name
	if name == "" {
		name = DefaultName
	}
	if err := checkName(name); err != nil {
		return err
	}
	if o.MTU != 0 && (o.MTU < 1280 || o.MTU > maxPacket) {
		return fmt.Errorf("TUN MTU %d: must be between 1280 and %d", o.MTU, maxPacket)
	}
	for _, a := range o.Addresses {
		if _, err := parseAddress(a); err != nil {
			return err
		}
	}
	for _, r := range o.Routes {
		if _, err := parseRoute(r); err != nil {
			return err
		}
	}
	return nil
}

// parseAddress parses an interface address: "ADDR/BITS", or "ADDR" (a
// single address).
func parseAddress(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	p, err := netip.ParsePrefix(s)
	if err != nil {
		a, aerr := netip.ParseAddr(s)
		if aerr != nil || a.Zone() != "" {
			return netip.Prefix{}, fmt.Errorf("TUN address %q: want ADDR/BITS", s)
		}
		p = netip.PrefixFrom(a, a.BitLen())
	}
	if p.Addr().Is4In6() || p.Addr().IsUnspecified() || p.Addr().IsMulticast() {
		return netip.Prefix{}, fmt.Errorf("TUN address %q: not an interface address", s)
	}
	return p, nil
}

// parseRoute parses a route destination: "ADDR/BITS", "ADDR" (a host) or
// "default" (IPv4).
func parseRoute(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if s == "default" {
		return netip.PrefixFrom(netip.IPv4Unspecified(), 0), nil
	}
	p, err := netip.ParsePrefix(s)
	if err != nil {
		a, aerr := netip.ParseAddr(s)
		if aerr != nil || a.Zone() != "" {
			return netip.Prefix{}, fmt.Errorf("TUN route %q: want ADDR/BITS", s)
		}
		p = netip.PrefixFrom(a, a.BitLen())
	}
	if p.Addr().Is4In6() {
		return netip.Prefix{}, fmt.Errorf("TUN route %q: not a route destination", s)
	}
	return p.Masked(), nil
}

// Device is a running TUN front-end.
type Device struct {
	st   *ipstack.Stack
	dev  tun.Device
	name string
	log  *slog.Logger

	wg        sync.WaitGroup
	closeOnce sync.Once
	closed    chan struct{}
	failed    chan error
}

// Start creates the interface (or attaches to a persistent one), configures
// it and starts relaying its traffic.
func Start(opt Options) (*Device, error) {
	if err := opt.Check(); err != nil {
		return nil, err
	}
	if opt.Name == "" {
		opt.Name = DefaultName
	}
	if opt.MTU == 0 {
		opt.MTU = DefaultMTU
	}
	var addrs, routes []netip.Prefix
	for _, a := range opt.Addresses {
		p, _ := parseAddress(a)
		addrs = append(addrs, p)
	}
	for _, r := range opt.Routes {
		p, _ := parseRoute(r)
		routes = append(routes, p)
	}
	dev, err := openDevice(opt.Name, opt.MTU)
	if err != nil {
		return nil, fmt.Errorf("TUN interface %s: %w%s", opt.Name, err, createHint(err))
	}
	name, err := dev.Name()
	if err == nil {
		err = configure(name, addrs, routes)
	}
	if err != nil {
		dev.Close()
		return nil, fmt.Errorf("TUN interface %s: %w", opt.Name, err)
	}
	d, err := start(dev, name, opt)
	if err != nil {
		dev.Close()
		return nil, err
	}
	return d, nil
}

// start relays the traffic of an open device.
func start(dev tun.Device, name string, opt Options) (*Device, error) {
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	st, err := ipstack.New(ipstack.Options{Name: "tun", MTU: opt.MTU, Opener: opt.Opener, UDP: opt.UDP, Log: opt.Log})
	if err != nil {
		return nil, err
	}
	d := &Device{st: st, dev: dev, name: name, log: opt.Log, closed: make(chan struct{}), failed: make(chan error, 1)}
	d.wg.Add(3)
	go d.readLoop()
	go d.writeLoop()
	go d.eventLoop()
	return d, nil
}

// Name returns the name of the interface (on macOS the system chooses the
// number of "utun").
func (d *Device) Name() string { return d.name }

// MTU returns the MTU the interface was given.
func (d *Device) MTU() int { return d.st.MTU() }

// Wait blocks until the device fails (e.g. the interface was deleted) and
// returns the error, or until Close and returns nil.
func (d *Device) Wait() error {
	select {
	case err := <-d.failed:
		return err
	case <-d.closed:
		return nil
	}
}

// Close stops relaying and closes the device; an interface this process
// created disappears with its addresses and routes.
func (d *Device) Close() {
	d.closeOnce.Do(func() {
		close(d.closed)
		d.st.Close()
		d.dev.Close()
		d.wg.Wait()
	})
}

func (d *Device) isClosed() bool {
	select {
	case <-d.closed:
		return true
	default:
		return false
	}
}

// readLoop hands the packets the system routes to the interface to the
// stack.
func (d *Device) readLoop() {
	defer d.wg.Done()
	batch := d.dev.BatchSize()
	bufs := make([][]byte, batch)
	sizes := make([]int, batch)
	for i := range bufs {
		bufs[i] = make([]byte, offset+maxPacket)
	}
	for {
		n, err := d.dev.Read(bufs, sizes, offset)
		for i := range n {
			d.st.Inject(bufs[i][offset : offset+sizes[i]])
		}
		if err == nil || errors.Is(err, tun.ErrTooManySegments) {
			continue
		}
		if !d.isClosed() {
			select {
			case d.failed <- fmt.Errorf("TUN interface %s: read: %w", d.name, err):
			default:
			}
		}
		return
	}
}

// writeLoop sends the packets of the stack (and the relayed UDP and ICMP)
// to the system, in batches: the device coalesces the segments of a batch
// that belong together (GRO), where the system supports it.
func (d *Device) writeLoop() {
	defer d.wg.Done()
	batch := d.dev.BatchSize()
	backing := make([][]byte, batch)
	for i := range backing {
		backing[i] = make([]byte, offset+maxPacket)
	}
	bufs := make([][]byte, 0, batch)
	var lastWarn time.Time
	for {
		var p []byte
		select {
		case p = <-d.st.Out():
		case <-d.st.Done():
			return
		}
		bufs = bufs[:0]
	fill:
		for {
			b := backing[len(bufs)][:offset+len(p)]
			copy(b[offset:], p)
			bufs = append(bufs, b)
			if len(bufs) == batch {
				break
			}
			select {
			case p = <-d.st.Out():
			default:
				break fill
			}
		}
		if _, err := d.dev.Write(bufs, offset); err != nil {
			if errors.Is(err, os.ErrClosed) || d.isClosed() {
				return
			}
			// e.g. the interface is down: the packets are lost, like on any
			// link that is down
			if time.Since(lastWarn) > 10*time.Second {
				lastWarn = time.Now()
				d.log.Debug("tun: write", "interface", d.name, "err", err)
			}
		}
	}
}

func (d *Device) eventLoop() {
	defer d.wg.Done()
	events := d.dev.Events()
	for {
		select {
		case <-d.closed:
			return
		case e, ok := <-events:
			if !ok {
				return
			}
			if e&tun.EventUp != 0 {
				d.log.Debug("tun: interface is up", "interface", d.name)
			}
			if e&tun.EventDown != 0 {
				d.log.Debug("tun: interface is down", "interface", d.name)
			}
			if e&tun.EventMTUUpdate != 0 {
				if mtu, err := d.dev.MTU(); err == nil && mtu != d.st.MTU() && !d.isClosed() {
					d.log.Warn("the MTU of the TUN interface was changed; set it with --tun-mtu instead, so that connections use it", "interface", d.name, "mtu", mtu, "configured", d.st.MTU())
				}
			}
		}
	}
}
