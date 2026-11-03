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
	Role       string // local, tunnel, remote, relay
	Reverse    bool
	Paths      string // one address per line
	Port       string
	Password   string
	UDP        bool
	LocalAddr  string
	LocalPort  string
	Tunnel     string
	Relays     string // one relay per line, see FormatRelays
	Congestion string
	Window     string
	Chunk      string
	Timeout    string
	MTU        string
	Stats      string
	NoDup      bool
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
		if e.ServerPort != 0 {
			paths = append(paths, config.JoinHostPort(e.Server, e.ServerPort))
		} else {
			paths = append(paths, e.Server)
		}
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
		NoDup:       c.NoDup,
		RaceConnect: c.RaceConnect,
		Cert:        c.Cert,
		Key:         c.Key,
		SNI:         c.SNI,
		LogLevel:    logLevel,
	}
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
		c.Server = config.StringList(lines(f.Paths))
		c.Servers = nil
		num("Port", f.Port, &c.ServerPort)
		c.Password = f.Password
		switch {
		case f.UDP:
			c.Mode = "tcp_and_udp"
		case c.UDPEnabled():
			c.Mode = ""
		}
		c.LocalAddress = strings.TrimSpace(f.LocalAddr)
		num("Listen port", f.LocalPort, &c.LocalPort)
		c.TunnelAddress = strings.TrimSpace(f.Tunnel)
		c.Congestion = strings.TrimSpace(f.Congestion)
		sz("Window", f.Window, &c.Window)
		sz("Chunk size", f.Chunk, &c.ChunkSize)
		num("Idle timeout", f.Timeout, &c.Timeout)
		num("Path MTU", f.MTU, &c.MTU)
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
