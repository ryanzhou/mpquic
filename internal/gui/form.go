// Package gui is the Windows configurator. This file holds the
// platform-independent form model: the conversion between the form fields
// (all strings and booleans, as edited) and config.Config.
package gui

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"strconv"
	"strings"

	"mpquic/internal/app"
	"mpquic/internal/config"
)

// Form is the editable state of the configurator.
type Form struct {
	Role    string // local, tunnel, remote, relay
	Reverse bool
	// Paths: one address per line. On the dialing side a line may end in
	// "loss=10%", the path's loss tolerance.
	Paths    string
	Port     string
	Password string
	UDP      bool
	// ReliableUDP (local, tunnel): relay UDP over streams.
	ReliableUDP bool
	LocalAddr   string
	LocalPort   string
	Tunnel      string
	Relays      string // one relay per line, see FormatRelays
	Congestion  string
	Window      string
	Chunk       string
	Timeout     string
	MTU         string
	Stats       string
	// Up, Down: the rates of this end's line, if known ("20mbit").
	Up, Down string
	// LossTolerance of every path ("10%"); a path's own is in Paths.
	LossTolerance string
	NoDup         bool
	// RaceConnect (remote): race staggered outbound TCP connection attempts.
	RaceConnect bool
	Cert, Key   string
	SNI         string
	LogLevel    string
}

// DefaultConfig is the config of a new instance.
func DefaultConfig() *config.Config {
	return &config.Config{Role: "local", Password: GeneratePassword(), Mode: "tcp_and_udp"}
}

// GeneratePassword returns a random 32-character password.
func GeneratePassword() string {
	b := make([]byte, 24)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// entryPath is how a "servers" entry shows in the form's list of paths:
// its address, and its loss tolerance if it has one.
func entryPath(e config.ServerEntry) string {
	addr := e.Server
	if e.ServerPort != 0 {
		addr = config.JoinHostPort(e.Server, e.ServerPort)
	}
	if e.LossTolerance > 0 {
		addr += " loss=" + e.LossTolerance.String()
	}
	return addr
}

// parsePath reads a line of the form's list of paths: "host[:port]" and,
// optionally, "loss=10%" (or "loss_tolerance=10%").
func parsePath(line string) (addr string, tolerance config.Fraction, err error) {
	fields := strings.Fields(line)
	addr = fields[0]
	for _, f := range fields[1:] {
		k, v, _ := strings.Cut(f, "=")
		if k != "loss" && k != "loss_tolerance" {
			return "", 0, fmt.Errorf("path %q: unknown option %q (a path is host[:port], optionally followed by loss=10%%)", addr, f)
		}
		if tolerance, err = config.ParseFraction(v); err != nil {
			return "", 0, fmt.Errorf("path %q: loss tolerance: %w", addr, err)
		}
	}
	return addr, tolerance, nil
}

// FormFromConfig fills a form from a config.
func FormFromConfig(c *config.Config) Form {
	role, ok := app.NormalizeRole(c.Role)
	if !ok {
		role = "local"
		if len(c.Relays) > 0 && len(c.Server) == 0 && len(c.Servers) == 0 {
			role = "relay"
		}
	}
	var paths []string
	paths = append(paths, c.Server...)
	for _, e := range c.Servers {
		paths = append(paths, entryPath(e))
	}
	logLevel := strings.ToLower(c.LogLevel)
	if logLevel == "" && c.Verbose {
		logLevel = "debug"
	}
	if logLevel == "" {
		logLevel = "info"
	}
	return Form{
		Role:        role,
		Reverse:     c.Reverse,
		Paths:       strings.Join(paths, "\n"),
		Port:        itoa(c.ServerPort),
		Password:    c.EffectivePassword(),
		UDP:         c.UDPEnabled(),
		ReliableUDP: c.ReliableUDP,
		LocalAddr:   c.LocalAddress,
		LocalPort:   itoa(c.LocalPort),
		Tunnel:      c.TunnelAddress,
		Relays:      FormatRelays(c.Relays),
		Congestion:  c.Congestion,
		Window:      size(c.Window),
		Chunk:       size(c.ChunkSize),
		Timeout:     itoa(c.Timeout),
		MTU:         itoa(c.MTU),
		Stats:       itoa(c.StatsInterval),
		Up:          rate(c.Up),
		Down:        rate(c.Down),
		NoDup:       c.NoDup,
		RaceConnect: c.RaceConnect,
		Cert:        c.Cert,
		Key:         c.Key,
		SNI:         c.SNI,
		LogLevel:    logLevel,

		LossTolerance: fraction(c.LossTolerance),
	}
}

func rate(r config.Rate) string {
	if r == 0 {
		return ""
	}
	return r.String()
}

func fraction(f config.Fraction) string {
	if f == 0 {
		return ""
	}
	return f.String()
}

func itoa(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

func size(s config.Size) string {
	if s == 0 {
		return ""
	}
	return config.FormatSize(int64(s))
}

// Apply returns a copy of base with the form's values. Keys the form does
// not edit (e.g. "method") are preserved. The result is validated.
func (f Form) Apply(base *config.Config) (*config.Config, error) {
	c := base.Clone()
	role, ok := app.NormalizeRole(f.Role)
	if !ok {
		return nil, fmt.Errorf("unknown role %q", f.Role)
	}
	c.Role = role
	if role != "local" {
		// Local-only keys the form does not edit (WireGuard listener).
		c.WireGuard, c.NoSOCKS = nil, false
	}
	var err error
	num := func(what, s string, dst *int) {
		if err != nil {
			return
		}
		s = strings.TrimSpace(s)
		if s == "" {
			*dst = 0
			return
		}
		n, e := strconv.Atoi(s)
		if e != nil || n < 0 {
			err = fmt.Errorf("%s: %q is not a valid number", what, s)
		}
		*dst = n
	}
	sz := func(what, s string, dst *config.Size) {
		if err != nil {
			return
		}
		s = strings.TrimSpace(s)
		if s == "" {
			*dst = 0
			return
		}
		n, e := config.ParseSize(s)
		if e != nil || n < 0 {
			err = fmt.Errorf("%s: %q is not a valid size (e.g. 64MB)", what, s)
		}
		*dst = config.Size(n)
	}
	c.StatsInterval = 0
	num("Stats interval", f.Stats, &c.StatsInterval)
	c.LogLevel = strings.ToLower(strings.TrimSpace(f.LogLevel))
	if c.LogLevel == "info" {
		c.LogLevel = ""
	}
	c.Verbose = false
	if role == "relay" {
		c.Relays, err = ParseRelays(f.Relays)
		if err != nil {
			return nil, err
		}
	} else {
		c.Reverse = f.Reverse
		// Plain addresses are the "server" list. If a path has a loss
		// tolerance, all of them are "servers" entries, which keeps the
		// paths in the order of the form.
		c.Server, c.Servers = nil, nil
		var entries []config.ServerEntry
		own := false
		for _, l := range lines(f.Paths) {
			addr, tol, e := parsePath(l)
			if e != nil && err == nil {
				err = e
			}
			entries = append(entries, config.ServerEntry{Server: addr, LossTolerance: tol})
			own = own || tol > 0
		}
		if own {
			c.Servers = entries
		} else {
			for _, e := range entries {
				c.Server = append(c.Server, e.Server)
			}
		}
		num("Port", f.Port, &c.ServerPort)
		c.Password = f.Password
		switch {
		case f.UDP:
			c.Mode = "tcp_and_udp"
		case c.UDPEnabled():
			c.Mode = ""
		}
		local := role == "local" || role == "tunnel"
		c.ReliableUDP = f.ReliableUDP && f.UDP && local
		c.LocalAddress = strings.TrimSpace(f.LocalAddr)
		num("Listen port", f.LocalPort, &c.LocalPort)
		c.TunnelAddress = strings.TrimSpace(f.Tunnel)
		c.Congestion = strings.TrimSpace(f.Congestion)
		sz("Window", f.Window, &c.Window)
		sz("Chunk size", f.Chunk, &c.ChunkSize)
		num("Idle timeout", f.Timeout, &c.Timeout)
		num("Path MTU", f.MTU, &c.MTU)
		c.Up, c.Down, c.LossTolerance = 0, 0, 0
		if v := strings.TrimSpace(f.Up); v != "" && err == nil {
			if c.Up, err = config.ParseRate(v); err != nil {
				err = fmt.Errorf("Uplink rate: %q is not a valid rate (e.g. 20mbit)", v)
			}
		}
		if v := strings.TrimSpace(f.Down); v != "" && err == nil {
			if c.Down, err = config.ParseRate(v); err != nil {
				err = fmt.Errorf("Downlink rate: %q is not a valid rate (e.g. 100mbit)", v)
			}
		}
		if v := strings.TrimSpace(f.LossTolerance); v != "" && err == nil {
			if c.LossTolerance, err = config.ParseFraction(v); err != nil {
				err = fmt.Errorf("Loss tolerance: %q is not a valid share (e.g. 10%%)", v)
			}
		}
		c.NoDup = f.NoDup
		c.RaceConnect = f.RaceConnect
		c.Cert = strings.TrimSpace(f.Cert)
		c.Key = strings.TrimSpace(f.Key)
		c.SNI = strings.TrimSpace(f.SNI)
	}
	if err != nil {
		return nil, err
	}
	if err := app.Validate(role, c); err != nil {
		return nil, err
	}
	return c, nil
}

func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(strings.ReplaceAll(s, "\r", ""), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out
}

// FormatRelays renders relays one per line: "LISTEN TARGET [SPEC]
// [fwd:SPEC] [rev:SPEC]".
func FormatRelays(rs []config.RelayConfig) string {
	var out []string
	for _, r := range rs {
		f := []string{r.Listen, r.Target}
		if r.Link != "" {
			f = append(f, r.Link)
		}
		if r.Fwd != "" {
			f = append(f, "fwd:"+r.Fwd)
		}
		if r.Rev != "" {
			f = append(f, "rev:"+r.Rev)
		}
		out = append(out, strings.Join(f, " "))
	}
	return strings.Join(out, "\n")
}

// ParseRelays parses the FormatRelays format. A bare port (":9000" or
// "9000") listens on all interfaces.
func ParseRelays(s string) ([]config.RelayConfig, error) {
	var out []config.RelayConfig
	for i, l := range lines(s) {
		f := strings.Fields(l)
		if len(f) < 2 {
			return nil, fmt.Errorf("relay line %d: want \"LISTEN TARGET [LINK-SPEC]\", got %q", i+1, l)
		}
		r := config.RelayConfig{Listen: f[0], Target: f[1]}
		if _, err := strconv.Atoi(r.Listen); err == nil {
			r.Listen = ":" + r.Listen
		}
		if _, _, err := net.SplitHostPort(r.Listen); err != nil {
			return nil, fmt.Errorf("relay line %d: listen address %q: %v", i+1, f[0], err)
		}
		for _, x := range f[2:] {
			switch {
			case strings.HasPrefix(x, "fwd:"):
				r.Fwd = strings.TrimPrefix(x, "fwd:")
			case strings.HasPrefix(x, "rev:"):
				r.Rev = strings.TrimPrefix(x, "rev:")
			case r.Link == "":
				r.Link = x
			default:
				return nil, fmt.Errorf("relay line %d: unexpected %q", i+1, x)
			}
		}
		out = append(out, r)
	}
	return out, nil
}
