// Command mpquic is a multipath QUIC proxy, shadowsocks-style.
//
//	mpquic local  -s HOST[:PORT] [-s ...] -p PORT -l LOCAL_PORT -k PASSWORD   (ss-local)
//	mpquic remote -s BIND_ADDR -p PORT -k PASSWORD                           (ss-server)
//	mpquic tunnel -s ... -L TARGET:PORT ...                                  (ss-tunnel)
//	mpquic relay  --listen :9000 --target HOST:PORT [--link SPEC]            (socat UDP + link emulation)
//
// Reverse mode (--reverse on both ends) swaps the QUIC roles: the local
// listens and the remote connects back to it through the relays.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"mpquic/internal/config"
	"mpquic/internal/linkem"
	"mpquic/internal/mp"
	"mpquic/internal/proxy"
)

var version = "dev"

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	mode, args := detectMode(os.Args)
	var err error
	switch mode {
	case "local", "tunnel":
		err = runLocal(mode, args)
	case "remote", "server":
		err = runRemote(args)
	case "relay":
		err = runRelay(args)
	case "version", "-V", "--version":
		fmt.Println("mpquic", version)
	case "help", "-h", "--help", "":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown mode %q\n\n", mode)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "mpquic:", err)
		os.Exit(1)
	}
}

func detectMode(argv []string) (string, []string) {
	switch strings.TrimSuffix(filepath.Base(argv[0]), ".exe") {
	case "ss-local", "sslocal", "mpquic-local":
		return "local", argv[1:]
	case "ss-server", "ssserver", "ss-remote", "mpquic-remote", "mpquic-server":
		return "remote", argv[1:]
	case "ss-tunnel", "mpquic-tunnel":
		return "tunnel", argv[1:]
	case "mpquic-relay":
		return "relay", argv[1:]
	}
	if len(argv) < 2 {
		return "", nil
	}
	return argv[1], argv[2:]
}

func usage() {
	fmt.Fprint(os.Stderr, `mpquic - multipath QUIC proxy (shadowsocks-style)

Usage:
  mpquic local  [options]   SOCKS5 server forwarding over all paths (like ss-local)
  mpquic tunnel [options]   port forwarder to a fixed target (-L), like ss-tunnel
  mpquic remote [options]   server that forwards to targets (like ss-server)
  mpquic relay  [options]   UDP relay with optional link emulation (like socat)
  mpquic version

Common options (shadowsocks-compatible):
  -c FILE           JSON config file (shadowsocks format; "server" may be a list)
  -s HOST[:PORT]    server address; repeat for every path (local) / bind address (remote)
  -p PORT           default server port (8388)
  -b ADDR           local bind address (local mode, default 127.0.0.1)
  -l PORT           local port (local mode, default 1080)
  -k PASSWORD       pre-shared password
  -m METHOD         accepted for compatibility; QUIC/TLS 1.3 always encrypts
  -t SECONDS        idle timeout of proxied connections (default 300)
  -L HOST:PORT      tunnel target (tunnel mode)
  -u                enable UDP relay (SOCKS5 UDP ASSOCIATE; set on both ends)
  -v                verbose logging
  --reverse         reverse mode (set on both ends): the local listens for QUIC
                    (-s/-p = its bind address) and the remote dials it
                    (-s = the local's address and relays forwarding to it)

mpquic options:
  --congestion CC   bbr (default), bbr-aggressive, bbr-conservative
  --window SIZE     per-stream window / reorder buffer (default 16MB)
  --chunk SIZE      scheduling chunk size (default 16KB)
  --stats SECONDS   log path statistics periodically
  --no-dup          disable opportunistic duplication on idle faster paths
  --cert/--key FILE TLS certificate for the server (optional)
  --sni NAME        TLS server name sent by the client

Relay options:
  --listen ADDR --target HOST:PORT [--link SPEC] [--fwd SPEC] [--rev SPEC]
  SPEC: delay=40ms,jitter=5ms,loss=1%,burst=3,rate=20mbit,queue=80ms,
        police=10mbit,reorder=1%,reorder_delay=10ms,dup=0.1%,outage=30s/2s,
        rate2=5mbit,flap=10s
`)
}

type commonFlags struct {
	fs                         *flag.FlagSet
	cfgFile                    string
	servers                    multi
	port, localPort, timeout   int
	localAddr, password        string
	method, tunnel, congestion string
	window, chunk              string
	stats                      int
	verbose, noDup             bool
	cert, key, sni, logLevel   string
	plugin                     string
	udp, udpOnly, reverse      bool
}

func parseCommon(name string, args []string) (*commonFlags, error) {
	f := &commonFlags{fs: flag.NewFlagSet(name, flag.ContinueOnError)}
	fs := f.fs
	fs.Usage = usage
	fs.StringVar(&f.cfgFile, "c", "", "config file")
	fs.Var(&f.servers, "s", "server address (repeatable)")
	fs.Var(&f.servers, "server", "server address (repeatable)")
	fs.IntVar(&f.port, "p", 0, "server port")
	fs.StringVar(&f.localAddr, "b", "", "local address")
	fs.IntVar(&f.localPort, "l", 0, "local port")
	fs.StringVar(&f.password, "k", "", "password")
	fs.StringVar(&f.password, "password", "", "password")
	fs.StringVar(&f.method, "m", "", "encryption method (ignored)")
	fs.IntVar(&f.timeout, "t", 0, "timeout seconds")
	fs.StringVar(&f.tunnel, "L", "", "tunnel target")
	fs.BoolVar(&f.verbose, "v", false, "verbose")
	fs.StringVar(&f.congestion, "congestion", "", "congestion control")
	fs.StringVar(&f.window, "window", "", "stream window")
	fs.StringVar(&f.chunk, "chunk", "", "chunk size")
	fs.IntVar(&f.stats, "stats", 0, "stats interval seconds")
	fs.BoolVar(&f.noDup, "no-dup", false, "disable opportunistic duplication")
	fs.StringVar(&f.cert, "cert", "", "TLS certificate")
	fs.StringVar(&f.key, "key", "", "TLS key")
	fs.StringVar(&f.sni, "sni", "", "TLS SNI")
	fs.StringVar(&f.logLevel, "log-level", "", "debug|info|warn|error")
	fs.StringVar(&f.plugin, "plugin", "", "SIP003 plugin (unsupported)")
	// Accepted for shadowsocks compatibility, ignored.
	var ign string
	var ignb bool
	var igni int
	for _, n := range []string{"a", "f", "plugin-opts", "acl", "mtu", "manager-address", "executable", "d", "protocol"} {
		fs.StringVar(&ign, n, "", "ignored")
	}
	fs.BoolVar(&f.reverse, "reverse", false, "reverse mode")
	fs.BoolVar(&f.udp, "u", false, "enable UDP relay")
	fs.BoolVar(&f.udpOnly, "U", false, "enable UDP relay (UDP only is treated as TCP+UDP)")
	for _, n := range []string{"6", "fast-open", "reuse-port", "no-delay", "mptcp", "use-syslog"} {
		fs.BoolVar(&ignb, n, false, "ignored")
	}
	fs.IntVar(&igni, "n", 0, "ignored")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	return f, nil
}

func (f *commonFlags) load() (*config.Config, error) {
	cfg := &config.Config{}
	if f.cfgFile != "" {
		c, err := config.Load(f.cfgFile)
		if err != nil {
			return nil, err
		}
		cfg = c
	}
	if len(f.servers) > 0 {
		cfg.Server = config.StringList(f.servers)
		cfg.Servers = nil
	}
	if f.port != 0 {
		cfg.ServerPort = f.port
	}
	if f.localAddr != "" {
		cfg.LocalAddress = f.localAddr
	}
	if f.localPort != 0 {
		cfg.LocalPort = f.localPort
	}
	if f.password != "" {
		cfg.Password = f.password
	}
	if f.method != "" {
		cfg.Method = f.method
	}
	if f.timeout != 0 {
		cfg.Timeout = f.timeout
	}
	if f.tunnel != "" {
		cfg.TunnelAddress = f.tunnel
	}
	if f.congestion != "" {
		cfg.Congestion = f.congestion
	}
	if f.window != "" {
		v, err := config.ParseSize(f.window)
		if err != nil {
			return nil, err
		}
		cfg.Window = config.Size(v)
	}
	if f.chunk != "" {
		v, err := config.ParseSize(f.chunk)
		if err != nil {
			return nil, err
		}
		cfg.ChunkSize = config.Size(v)
	}
	if f.stats != 0 {
		cfg.StatsInterval = f.stats
	}
	if f.verbose {
		cfg.Verbose = true
	}
	if f.noDup {
		cfg.NoDup = true
	}
	if f.cert != "" {
		cfg.Cert, cfg.Key = f.cert, f.key
	}
	if f.sni != "" {
		cfg.SNI = f.sni
	}
	if f.logLevel != "" {
		cfg.LogLevel = f.logLevel
	}
	if f.reverse {
		cfg.Reverse = true
	}
	if f.udp || f.udpOnly {
		cfg.Mode = "tcp_and_udp"
	}
	if f.plugin != "" || cfg.Plugin != "" {
		return nil, errors.New("SIP003 plugins are not supported")
	}
	return cfg, nil
}

func newLogger(cfg *config.Config) *slog.Logger {
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
	l := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
	slog.SetDefault(l)
	return l
}

func options(cfg *config.Config, log *slog.Logger) (*mp.Options, error) {
	if cfg.EffectivePassword() == "" {
		return nil, errors.New("password is required (-k or \"password\")")
	}
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
		CertFile:      cfg.Cert,
		KeyFile:       cfg.Key,
		SNI:           cfg.SNI,
		Logger:        log,
	}, nil
}

func waitSignal() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	<-ch
}

func runLocal(mode string, args []string) error {
	f, err := parseCommon(mode, args)
	if err != nil {
		return err
	}
	cfg, err := f.load()
	if err != nil {
		return err
	}
	log := newLogger(cfg)
	opt, err := options(cfg, log)
	if err != nil {
		return err
	}
	if mode == "tunnel" && cfg.TunnelAddress == "" {
		return errors.New("tunnel mode requires -L host:port")
	}
	laddr := cfg.LocalAddress
	if laddr == "" {
		laddr = "127.0.0.1"
	}
	lport := cfg.LocalPort
	if lport == 0 {
		lport = 1080
	}
	var udp *proxy.UDPLocal
	if cfg.UDPEnabled() && mode != "tunnel" {
		udp = proxy.NewUDPLocal(nil, log)
		opt.DatagramHandler = udp.HandleDatagram
	}
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
					log.Error("quic listener", "addr", a, "err", err)
				}
			}()
		}
		opener, shutdown, pathDesc = srv, srv.Close, "reverse, listening on "+strings.Join(binds, ",")
	} else {
		servers := cfg.ServerAddrs()
		if len(servers) == 0 {
			return errors.New("at least one server address is required (-s)")
		}
		client, err := mp.NewClient(servers, opt)
		if err != nil {
			return err
		}
		opener, shutdown, pathDesc = client, client.Close, strings.Join(servers, ",")
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(laddr, strconv.Itoa(lport)))
	if err != nil {
		shutdown()
		return err
	}
	l := &proxy.Local{Client: opener, Log: log, UDP: udp}
	if udp != nil {
		udp.Client = opener
	}
	if mode == "tunnel" {
		l.Tunnel = cfg.TunnelAddress
	}
	what := "socks5"
	if l.Tunnel != "" {
		what = "tunnel to " + l.Tunnel
	}
	log.Info("mpquic local started", "version", version, "listen", ln.Addr().String(), "mode", what, "udp", udp != nil, "paths", pathDesc)
	go func() {
		if err := l.Serve(ln); err != nil && !errors.Is(err, net.ErrClosed) {
			log.Error("listener", "err", err)
		}
	}()
	waitSignal()
	ln.Close()
	shutdown()
	return nil
}

func runRemote(args []string) error {
	f, err := parseCommon("remote", args)
	if err != nil {
		return err
	}
	cfg, err := f.load()
	if err != nil {
		return err
	}
	log := newLogger(cfg)
	opt, err := options(cfg, log)
	if err != nil {
		return err
	}
	if cfg.UDPEnabled() {
		opt.DatagramHandler = proxy.NewUDPRemote(log).HandleDatagram
	}
	opt.StreamHandler = proxy.RemoteHandler(log, 10*time.Second)
	if cfg.Reverse {
		// Reverse mode: dial the local instance over every configured path.
		servers := cfg.ServerAddrs()
		if len(servers) == 0 {
			return errors.New("reverse mode: at least one address of the local instance is required (-s)")
		}
		client, err := mp.NewClient(servers, opt)
		if err != nil {
			return err
		}
		log.Info("mpquic remote started", "version", version, "mode", "reverse", "paths", strings.Join(servers, ","), "udp", cfg.UDPEnabled())
		waitSignal()
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
	log.Info("mpquic remote started", "version", version, "listen", strings.Join(binds, ","), "udp", cfg.UDPEnabled())
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err = <-errc:
	case <-sig:
	}
	srv.Close()
	return err
}

func runRelay(args []string) error {
	fs := flag.NewFlagSet("relay", flag.ContinueOnError)
	fs.Usage = usage
	var cfgFile, link, fwd, rev string
	var listens, targets multi
	var verbose bool
	var statsSec int
	fs.StringVar(&cfgFile, "c", "", "config file with \"relays\"")
	fs.Var(&listens, "listen", "listen address")
	fs.Var(&targets, "target", "target address")
	fs.StringVar(&link, "link", "", "impairment spec for both directions")
	fs.StringVar(&fwd, "fwd", "", "impairment spec client->target")
	fs.StringVar(&rev, "rev", "", "impairment spec target->client")
	fs.BoolVar(&verbose, "v", false, "verbose")
	fs.IntVar(&statsSec, "stats", 0, "log counters every N seconds")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg := &config.Config{Verbose: verbose}
	if cfgFile != "" {
		c, err := config.Load(cfgFile)
		if err != nil {
			return err
		}
		cfg = c
		cfg.Verbose = cfg.Verbose || verbose
	}
	log := newLogger(cfg)
	if len(listens) != len(targets) {
		return errors.New("--listen and --target must be given the same number of times")
	}
	for i := range listens {
		cfg.Relays = append(cfg.Relays, config.RelayConfig{Listen: listens[i], Target: targets[i], Link: link, Fwd: fwd, Rev: rev})
	}
	if len(cfg.Relays) == 0 {
		return errors.New("no relays configured (--listen/--target or \"relays\" in config)")
	}
	var relays []*linkem.Relay
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
			return err
		}
		rspec, err := linkem.ParseSpec(rs)
		if err != nil {
			return err
		}
		r, err := linkem.NewRelay(rc.Listen, rc.Target, fspec, rspec)
		if err != nil {
			return err
		}
		r.Log = log
		relays = append(relays, r)
		log.Info("relay", "listen", r.Addr().String(), "target", rc.Target, "fwd", fspec.String(), "rev", rspec.String())
		go func() { errc <- r.Serve() }()
	}
	if statsSec > 0 {
		go func() {
			for range time.Tick(time.Duration(statsSec) * time.Second) {
				for _, r := range relays {
					log.Info("relay stats", "listen", r.Addr().String(), "fwd_pass", r.Fwd.Passed, "fwd_drop", r.Fwd.Dropped, "rev_pass", r.Rev.Passed, "rev_drop", r.Rev.Dropped)
				}
			}
		}()
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	var err error
	select {
	case err = <-errc:
	case <-sig:
	}
	for _, r := range relays {
		r.Close()
	}
	return err
}
