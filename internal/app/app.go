// Package app runs the mpquic roles (local, tunnel, remote, relay) from a
// config. It is shared by the command line, the Windows service and the GUI
// (which uses Validate).
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	"mpquic/internal/cc"
	"mpquic/internal/config"
	"mpquic/internal/linkem"
	"mpquic/internal/mp"
	"mpquic/internal/proxy"
	"mpquic/internal/wg"
)

// Version is reported in logs; the main package sets it.
var Version = "dev"

// Roles lists the roles a config can run, in display order.
var Roles = []string{"local", "tunnel", "remote", "relay"}

// NormalizeRole maps aliases ("server", "ss-local", ...) to a role and
// reports whether the role is known.
func NormalizeRole(role string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "local", "ss-local", "sslocal", "client":
		return "local", true
	case "tunnel", "ss-tunnel":
		return "tunnel", true
	case "remote", "server", "ss-server", "ssserver":
		return "remote", true
	case "relay":
		return "relay", true
	}
	return "", false
}

// NewLogger returns a logger at the level selected by the config and makes
// it the default logger.
func NewLogger(cfg *config.Config, w io.Writer) *slog.Logger {
	lvl := slog.LevelInfo
	if cfg.Verbose {
		lvl = slog.LevelDebug
	}
	switch strings.ToLower(cfg.LogLevel) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	l := slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: lvl}))
	slog.SetDefault(l)
	return l
}

// Validate checks that cfg is complete for role, without binding anything.
func Validate(role string, cfg *config.Config) error {
	r, ok := NormalizeRole(role)
	if !ok {
		return fmt.Errorf("unknown role %q (want local, tunnel, remote or relay)", role)
	}
	if r == "relay" {
		if len(cfg.Relays) == 0 {
			return errors.New("no relays configured (--listen/--target or \"relays\" in config)")
		}
		for i, rc := range cfg.Relays {
			if strings.TrimSpace(rc.Listen) == "" {
				return fmt.Errorf("relay %d: listen address is required", i+1)
			}
			if _, _, err := net.SplitHostPort(rc.Listen); err != nil {
				return fmt.Errorf("relay %d: listen address %q: %v", i+1, rc.Listen, err)
			}
			if _, _, err := net.SplitHostPort(rc.Target); err != nil {
				return fmt.Errorf("relay %d: target %q: %v", i+1, rc.Target, err)
			}
			for _, spec := range []string{rc.Link, rc.Fwd, rc.Rev} {
				if _, err := linkem.ParseSpec(spec); err != nil {
					return fmt.Errorf("relay %d: %v", i+1, err)
				}
			}
		}
		return nil
	}
	if cfg.Plugin != "" {
		return errors.New("SIP003 plugins are not supported")
	}
	if cfg.EffectivePassword() == "" {
		return errors.New("password is required (-k or \"password\")")
	}
	if _, err := cc.ParseProfile(cfg.Congestion); err != nil {
		return err
	}
	if err := checkPort("server port", cfg.ServerPort); err != nil {
		return err
	}
	if err := checkPort("local port", cfg.LocalPort); err != nil {
		return err
	}
	if (cfg.Cert == "") != (cfg.Key == "") {
		return errors.New("cert and key must be set together")
	}
	if cfg.Window < 0 || cfg.ChunkSize < 0 || cfg.Timeout < 0 || cfg.StatsInterval < 0 {
		return errors.New("window, chunk size, timeout and stats interval must not be negative")
	}
	if cfg.MTU != 0 && (cfg.MTU < 1228 || cfg.MTU > 65535) {
		// QUIC needs 1200-byte UDP payloads: 1228-byte IPv4 / 1248-byte IPv6 packets
		return fmt.Errorf("mtu %d: must be between 1228 and 65535 (QUIC needs paths that carry 1200-byte UDP payloads), or 0 to discover it", cfg.MTU)
	}
	if r != "local" && cfg.WireGuard != nil {
		return errors.New("the WireGuard listener (--wg-*) is only available in local mode")
	}
	if r != "local" && cfg.NoSOCKS {
		return errors.New("--no-socks applies to local mode only")
	}
	if r == "local" {
		if cfg.NoSOCKS && cfg.WireGuard == nil {
			return errors.New("--no-socks leaves no listener: enable WireGuard (--wg-listen) or drop --no-socks")
		}
		if cfg.WireGuard != nil {
			o := wgOptions(cfg)
			if err := o.Check(); err != nil {
				return err
			}
		}
	}
	switch r {
	case "local", "tunnel":
		if !cfg.Reverse && len(cfg.ServerAddrs()) == 0 {
			return errors.New("at least one server address is required (-s)")
		}
		if r == "tunnel" {
			if cfg.TunnelAddress == "" {
				return errors.New("tunnel mode requires -L host:port")
			}
			if _, _, err := net.SplitHostPort(cfg.TunnelAddress); err != nil {
				return fmt.Errorf("tunnel target %q: %v", cfg.TunnelAddress, err)
			}
		}
	case "remote":
		if cfg.Reverse && len(cfg.ServerAddrs()) == 0 {
			return errors.New("reverse mode: at least one address of the local instance is required (-s)")
		}
	}
	return nil
}

func checkPort(what string, p int) error {
	if p < 0 || p > 65535 {
		return fmt.Errorf("%s %d out of range", what, p)
	}
	return nil
}

// Run runs role until ctx is cancelled or a listener fails.
func Run(ctx context.Context, role string, cfg *config.Config, log *slog.Logger) error {
	r, ok := NormalizeRole(role)
	if !ok {
		return fmt.Errorf("unknown role %q", role)
	}
	if err := Validate(r, cfg); err != nil {
		return err
	}
	switch r {
	case "local", "tunnel":
		return runLocal(ctx, r, cfg, log)
	case "remote":
		return runRemote(ctx, cfg, log)
	default:
		return runRelay(ctx, cfg, log)
	}
}

func options(cfg *config.Config, log *slog.Logger) *mp.Options {
	if cfg.Method != "" {
		log.Debug("encryption method is ignored; QUIC (TLS 1.3) encrypts all traffic", "method", cfg.Method)
	}
	return &mp.Options{
		Password:      cfg.EffectivePassword(),
		Window:        int64(cfg.Window),
		ChunkSize:     int(cfg.ChunkSize),
		Congestion:    cfg.Congestion,
		IdleTimeout:   time.Duration(cfg.Timeout) * time.Second,
		StatsInterval: time.Duration(cfg.StatsInterval) * time.Second,
		NoDup:         cfg.NoDup,
		MTU:           cfg.MTU,
		CertFile:      cfg.Cert,
		KeyFile:       cfg.Key,
		SNI:           cfg.SNI,
		Logger:        log,
	}
}

// LocalListenAddr returns the SOCKS5/tunnel listen address of a local config.
func LocalListenAddr(cfg *config.Config) string {
	laddr := cfg.LocalAddress
	if laddr == "" {
		laddr = "127.0.0.1"
	}
	lport := cfg.LocalPort
	if lport == 0 {
		lport = 1080
	}
	return net.JoinHostPort(laddr, strconv.Itoa(lport))
}

func runLocal(ctx context.Context, mode string, cfg *config.Config, log *slog.Logger) error {
	opt := options(cfg, log)
	var udp *proxy.UDPLocal
	if cfg.UDPEnabled() && mode != "tunnel" {
		udp = proxy.NewUDPLocal(nil, log)
		opt.DatagramHandler = udp.HandleDatagram
	}
	errc := make(chan error, 1+len(cfg.Server))
	var opener proxy.Opener
	var shutdown func()
	var pathDesc string
	if cfg.Reverse {
		// Reverse mode: we are the QUIC listener; the remote dials us
		// (directly and through relays that forward to our address).
		opt.SingleSession = true
		srv, err := mp.NewServer(opt, nil)
		if err != nil {
			return err
		}
		binds := cfg.ListenAddrs()
		for _, a := range binds {
			go func() {
				if err := srv.ListenAndServe(a); err != nil {
					errc <- fmt.Errorf("quic listener %s: %w", a, err)
				}
			}()
		}
		opener, shutdown, pathDesc = srv, srv.Close, "reverse, listening on "+strings.Join(binds, ",")
	} else {
		servers := cfg.ServerAddrs()
		client, err := mp.NewClient(servers, opt)
		if err != nil {
			return err
		}
		opener, shutdown, pathDesc = client, client.Close, strings.Join(servers, ",")
	}
	if udp != nil {
		udp.Client = opener
	}
	var ln net.Listener
	what := "off"
	if !cfg.NoSOCKS {
		var err error
		ln, err = net.Listen("tcp", LocalListenAddr(cfg))
		if err != nil {
			shutdown()
			return err
		}
		l := &proxy.Local{Client: opener, Log: log, UDP: udp}
		what = "socks5 on " + ln.Addr().String()
		if mode == "tunnel" {
			l.Tunnel = cfg.TunnelAddress
			what = "tunnel " + ln.Addr().String() + " to " + l.Tunnel
		}
		go func() {
			if err := l.Serve(ln); err != nil && !errors.Is(err, net.ErrClosed) {
				errc <- fmt.Errorf("listener: %w", err)
			}
		}()
	}
	var wgs *wg.Server
	if cfg.WireGuard != nil {
		o := wgOptions(cfg)
		o.Opener, o.UDP, o.Log = opener, udp, log
		var err error
		wgs, err = wg.Start(o)
		if err != nil {
			if ln != nil {
				ln.Close()
			}
			shutdown()
			return err
		}
		log.Info("wireguard listener started", "listen", o.Listen, "port", wgs.Port(), "public_key", wgs.PublicKey(), "peers", len(o.Peers), "udp_and_ping", udp != nil)
		if udp == nil {
			log.Warn("UDP relaying is off: WireGuard peers get no UDP (including DNS) and no ping; enable it with -u (on both ends)")
		}
	}
	log.Info("mpquic local started", "version", Version, "listen", what, "wireguard", wgs != nil, "udp", udp != nil, "paths", pathDesc)
	var err error
	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	if ln != nil {
		ln.Close()
	}
	if wgs != nil {
		wgs.Close()
	}
	shutdown()
	return err
}

// wgOptions converts the WireGuard config (cfg.WireGuard must be set).
func wgOptions(cfg *config.Config) wg.Options {
	w := cfg.WireGuard
	o := wg.Options{Listen: w.WireGuardListen(), PrivateKey: w.PrivateKey, MTU: w.MTU}
	for _, p := range w.Peers {
		o.Peers = append(o.Peers, wg.Peer{PublicKey: p.PublicKey, PresharedKey: p.PresharedKey, AllowedIPs: p.AllowedIPs})
	}
	return o
}

func runRemote(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	opt := options(cfg, log)
	if cfg.UDPEnabled() {
		opt.DatagramHandler = proxy.NewUDPRemote(log).HandleDatagram
	}
	opt.StreamHandler = proxy.RemoteHandler(log, proxy.RemoteOptions{DialTimeout: 10 * time.Second, RaceConnect: cfg.RaceConnect})
	if cfg.Reverse {
		// Reverse mode: dial the local instance over every configured path.
		servers := cfg.ServerAddrs()
		client, err := mp.NewClient(servers, opt)
		if err != nil {
			return err
		}
		log.Info("mpquic remote started", "version", Version, "mode", "reverse", "paths", strings.Join(servers, ","), "udp", cfg.UDPEnabled(), "race_connect", cfg.RaceConnect)
		<-ctx.Done()
		client.Close()
		return nil
	}
	srv, err := mp.NewServer(opt, nil)
	if err != nil {
		return err
	}
	binds := cfg.ListenAddrs()
	errc := make(chan error, len(binds))
	for _, a := range binds {
		go func() { errc <- srv.ListenAndServe(a) }()
	}
	log.Info("mpquic remote started", "version", Version, "listen", strings.Join(binds, ","), "udp", cfg.UDPEnabled(), "race_connect", cfg.RaceConnect)
	select {
	case err = <-errc:
	case <-ctx.Done():
	}
	srv.Close()
	return err
}

func runRelay(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	var relays []*linkem.Relay
	closeAll := func() {
		for _, r := range relays {
			r.Close()
		}
	}
	errc := make(chan error, len(cfg.Relays))
	for _, rc := range cfg.Relays {
		fs, rs := rc.Fwd, rc.Rev
		if fs == "" {
			fs = rc.Link
		}
		if rs == "" {
			rs = rc.Link
		}
		fspec, err := linkem.ParseSpec(fs)
		if err != nil {
			closeAll()
			return err
		}
		rspec, err := linkem.ParseSpec(rs)
		if err != nil {
			closeAll()
			return err
		}
		r, err := linkem.NewRelay(rc.Listen, rc.Target, fspec, rspec)
		if err != nil {
			closeAll()
			return err
		}
		r.Log = log
		relays = append(relays, r)
		log.Info("relay", "listen", r.Addr().String(), "target", rc.Target, "fwd", fspec.String(), "rev", rspec.String())
		go func() { errc <- r.Serve() }()
	}
	if cfg.StatsInterval > 0 {
		go func() {
			t := time.NewTicker(time.Duration(cfg.StatsInterval) * time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
				for _, r := range relays {
					log.Info("relay stats", "listen", r.Addr().String(), "fwd_pass", r.Fwd.Passed, "fwd_drop", r.Fwd.Dropped, "rev_pass", r.Rev.Passed, "rev_drop", r.Rev.Dropped)
				}
			}
		}()
	}
	var err error
	select {
	case err = <-errc:
	case <-ctx.Done():
	}
	closeAll()
	return err
}
