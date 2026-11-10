// Package wg is the WireGuard front-end of the local side: a userspace
// WireGuard server whose peers' traffic is proxied transparently over the
// multipath connection (see package ipstack).
package wg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"strings"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"

	"mpquic/internal/ipstack"
	"mpquic/internal/proxy"
)

// DefaultMTU is the default MTU of the WireGuard interface (as wg-quick).
const DefaultMTU = 1420

// Peer is a WireGuard peer (client).
type Peer struct {
	PublicKey    string
	PresharedKey string   // optional
	AllowedIPs   []string // source addresses of the peer; default: all
}

// Options configure a Server.
type Options struct {
	// Listen is the UDP address of the WireGuard endpoint: "host:port" or
	// ":port" (all interfaces).
	Listen     string
	PrivateKey string
	Peers      []Peer
	MTU        int // default DefaultMTU
	Opener     proxy.Opener
	// UDP relays UDP and ICMP echo; nil rejects UDP (ICMP port
	// unreachable) and drops pings.
	UDP *proxy.UDPLocal
	Log *slog.Logger
}

// Server is a running WireGuard front-end.
type Server struct {
	st   *ipstack.Stack
	dev  *device.Device
	pub  Key
	port int
}

// ParseListen normalizes a WireGuard listen address: "PORT", ":PORT" or
// "HOST:PORT".
func ParseListen(s string) (host string, port int, err error) {
	s = strings.TrimSpace(s)
	if p, e := strconv.Atoi(s); e == nil {
		host, port = "", p
	} else {
		h, ps, e := net.SplitHostPort(s)
		if e != nil {
			return "", 0, fmt.Errorf("WireGuard listen address %q: want [HOST:]PORT", s)
		}
		p, e := strconv.Atoi(ps)
		if e != nil {
			return "", 0, fmt.Errorf("WireGuard listen address %q: bad port", s)
		}
		host, port = h, p
	}
	if port < 0 || port > 65535 {
		return "", 0, fmt.Errorf("WireGuard listen port %d out of range", port)
	}
	if host != "" {
		if _, e := netip.ParseAddr(host); e != nil {
			return "", 0, fmt.Errorf("WireGuard listen address %q: host must be an IP address", s)
		}
	}
	return host, port, nil
}

// Check validates the options that do not depend on the runtime.
func (o *Options) Check() error {
	if _, _, err := ParseListen(o.Listen); err != nil {
		return err
	}
	if o.PrivateKey == "" {
		return errors.New("WireGuard private key is required")
	}
	if _, err := ParseKey(o.PrivateKey); err != nil {
		return fmt.Errorf("WireGuard private key: %v", err)
	}
	if len(o.Peers) == 0 {
		return errors.New("WireGuard needs at least one peer public key")
	}
	for i, p := range o.Peers {
		if _, err := ParseKey(p.PublicKey); err != nil {
			return fmt.Errorf("WireGuard peer %d public key: %v", i+1, err)
		}
		if p.PresharedKey != "" {
			if _, err := ParseKey(p.PresharedKey); err != nil {
				return fmt.Errorf("WireGuard peer %d preshared key: %v", i+1, err)
			}
		}
		if len(p.AllowedIPs) == 0 && len(o.Peers) > 1 {
			return fmt.Errorf("WireGuard peer %d: with several peers, every peer needs its allowed IPs (its tunnel addresses)", i+1)
		}
		for _, a := range p.AllowedIPs {
			if _, err := parsePrefix(a); err != nil {
				return fmt.Errorf("WireGuard peer %d: %v", i+1, err)
			}
		}
	}
	if o.MTU != 0 && (o.MTU < 1280 || o.MTU > 9000) {
		return fmt.Errorf("WireGuard MTU %d: must be between 1280 and 9000", o.MTU)
	}
	return nil
}

func parsePrefix(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if p, err := netip.ParsePrefix(s); err == nil {
		return p.Masked(), nil
	}
	if a, err := netip.ParseAddr(s); err == nil {
		return netip.PrefixFrom(a, a.BitLen()), nil
	}
	return netip.Prefix{}, fmt.Errorf("allowed IP %q: want an address or CIDR prefix", s)
}

// Start starts the WireGuard server.
func Start(opt Options) (*Server, error) {
	if err := opt.Check(); err != nil {
		return nil, err
	}
	if opt.MTU == 0 {
		opt.MTU = DefaultMTU
	}
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	priv, _ := ParseKey(opt.PrivateKey)
	host, port, _ := ParseListen(opt.Listen)
	st, err := ipstack.New(ipstack.Options{Name: "wireguard", MTU: opt.MTU, Opener: opt.Opener, UDP: opt.UDP, Log: opt.Log})
	if err != nil {
		return nil, err
	}
	s := &Server{st: st, pub: priv.PublicKey()}
	var bind conn.Bind
	if a, err := netip.ParseAddr(host); err == nil && !a.IsUnspecified() {
		bind = &addrBind{addr: a}
	} else {
		bind = conn.NewDefaultBind()
	}
	logf := func(lvl slog.Level) func(string, ...any) {
		return func(f string, args ...any) {
			opt.Log.Log(context.Background(), lvl, "wireguard: "+fmt.Sprintf(f, args...))
		}
	}
	s.dev = device.NewDevice(newNetTun(st), bind, &device.Logger{Verbosef: logf(slog.LevelDebug), Errorf: logf(slog.LevelDebug)})
	var uapi strings.Builder
	fmt.Fprintf(&uapi, "private_key=%s\nlisten_port=%d\n", priv.hex(), port)
	for _, p := range opt.Peers {
		pk, _ := ParseKey(p.PublicKey)
		fmt.Fprintf(&uapi, "public_key=%s\n", pk.hex())
		if p.PresharedKey != "" {
			psk, _ := ParseKey(p.PresharedKey)
			fmt.Fprintf(&uapi, "preshared_key=%s\n", psk.hex())
		}
		allowed := p.AllowedIPs
		if len(allowed) == 0 {
			allowed = []string{"0.0.0.0/0", "::/0"}
		}
		for _, a := range allowed {
			pfx, _ := parsePrefix(a)
			fmt.Fprintf(&uapi, "allowed_ip=%s\n", pfx)
		}
	}
	if err := s.dev.IpcSet(uapi.String()); err != nil {
		s.dev.Close()
		return nil, fmt.Errorf("WireGuard: %w", err)
	}
	if err := s.dev.Up(); err != nil {
		s.dev.Close()
		return nil, fmt.Errorf("WireGuard listener %s: %w", opt.Listen, err)
	}
	s.port = port
	if cfg, err := s.dev.IpcGet(); err == nil {
		for _, l := range strings.Split(cfg, "\n") {
			if v, ok := strings.CutPrefix(l, "listen_port="); ok {
				s.port, _ = strconv.Atoi(v)
			}
		}
	}
	return s, nil
}

// PublicKey returns the server's public key (for the peers' configuration).
func (s *Server) PublicKey() string { return s.pub.String() }

// Port returns the UDP port the server listens on.
func (s *Server) Port() int { return s.port }

// Close stops the server.
func (s *Server) Close() {
	s.dev.Close() // closes the tun, and so the stack, too
}
