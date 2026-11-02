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
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"mpquic/internal/app"
	"mpquic/internal/config"
)

var version = "dev"

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	app.Version = version
	mode, args := detectMode(os.Args)
	if mode == "" && launchedFromExplorer() {
		mode = "gui"
	}
	var err error
	switch mode {
	case "local", "tunnel", "relay":
		err = runRole(mode, args)
	case "remote", "server":
		err = runRole("remote", args)
	case "service":
		err = runService(args)
	case "gui":
		err = runGUI(args)
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
  mpquic service COMMAND    Windows service: install, uninstall, start, stop,
                            restart, status (see "mpquic service help")
  mpquic gui                Windows configurator (also opened by double-click)
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
  --congestion CC   bbr (default), bbr-aggressive, bbr-conservative, cubic
  --window SIZE     max per-stream window, auto-tuned from 4MB (default 64MB)
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

// signalContext is cancelled by Ctrl-C / SIGTERM (on Windows, also by
// closing the console window).
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// parseRole builds the config of a role from its command-line arguments
// (exactly as "mpquic ROLE ARGS..." would).
func parseRole(role string, args []string) (*config.Config, error) {
	if role == "relay" {
		return parseRelay(args)
	}
	f, err := parseCommon(role, args)
	if err != nil {
		return nil, err
	}
	return f.load()
}

func runRole(role string, args []string) error {
	cfg, err := parseRole(role, args)
	if err != nil {
		return err
	}
	log := app.NewLogger(cfg, os.Stderr)
	ctx, stop := signalContext()
	defer stop()
	return app.Run(ctx, role, cfg, log)
}

func parseRelay(args []string) (*config.Config, error) {
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
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	cfg := &config.Config{Verbose: verbose}
	if cfgFile != "" {
		c, err := config.Load(cfgFile)
		if err != nil {
			return nil, err
		}
		cfg = c
		cfg.Verbose = cfg.Verbose || verbose
	}
	if statsSec != 0 {
		cfg.StatsInterval = statsSec
	}
	if len(listens) != len(targets) {
		return nil, errors.New("--listen and --target must be given the same number of times")
	}
	for i := range listens {
		cfg.Relays = append(cfg.Relays, config.RelayConfig{Listen: listens[i], Target: targets[i], Link: link, Fwd: fwd, Rev: rev})
	}
	return cfg, nil
}
