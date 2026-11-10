// Package config loads shadowsocks-compatible JSON configuration, extended
// for multiple server paths.
package config

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config mirrors shadowsocks-libev / shadowsocks-rust JSON keys, plus a few
// mpquic-specific ones.
type Config struct {
	Server        StringList    `json:"server,omitempty"`
	ServerPort    int           `json:"server_port,omitempty"`
	Servers       []ServerEntry `json:"servers,omitempty"`
	LocalAddress  string        `json:"local_address,omitempty"`
	LocalPort     int           `json:"local_port,omitempty"`
	Password      string        `json:"password,omitempty"`
	Method        string        `json:"method,omitempty"`
	Timeout       int           `json:"timeout,omitempty"`
	TunnelAddress string        `json:"tunnel_address,omitempty"`
	Mode          string        `json:"mode,omitempty"`
	FastOpen      bool          `json:"fast_open,omitempty"`
	NoDelay       bool          `json:"no_delay,omitempty"`
	ReusePort     bool          `json:"reuse_port,omitempty"`
	Plugin        string        `json:"plugin,omitempty"`

	// mpquic extensions
	// Role selects what a service runs: local, tunnel, remote or relay. The
	// command line ignores it (the mode is the first argument there).
	Role string `json:"role,omitempty"`
	// Reverse swaps the QUIC roles: the local side listens (at "server" /
	// "server_port", i.e. its own public address) and the remote dials the
	// local through the "server" list (the local's address and relays).
	Reverse       bool   `json:"reverse,omitempty"`
	Congestion    string `json:"congestion,omitempty"`
	Window        Size   `json:"window,omitempty"`
	ChunkSize     Size   `json:"chunk_size,omitempty"`
	StatsInterval int    `json:"stats_interval,omitempty"`
	Verbose       bool   `json:"verbose,omitempty"`
	LogLevel      string `json:"log_level,omitempty"`
	Cert          string `json:"cert,omitempty"`
	Key           string `json:"key,omitempty"`
	SNI           string `json:"sni,omitempty"`
	NoDup         bool   `json:"no_dup,omitempty"`
	NoCoupling    bool   `json:"no_coupling,omitempty"` // paths are not rate-controlled together
	// Up / Down: the rate of this endpoint's uplink / downlink ("20mbit";
	// a number is bit/s), if known. The session then sends no more than Up
	// over all paths together, and tells the peer to send no more than
	// Down. Without them the rates are measured.
	Up   Rate `json:"up,omitempty"`
	Down Rate `json:"down,omitempty"`
	MTU  int  `json:"mtu,omitempty"` // path MTU (IP packet size) of every path; 0 discovers it
	// RaceConnect (remote role): race staggered TCP connection attempts to
	// each target and keep the first that connects.
	RaceConnect bool `json:"race_connect,omitempty"`
	// ReliableUDP (local and tunnel roles): relay UDP over streams
	// (retransmitted, congestion-controlled, no size limit) instead of
	// unreliable QUIC DATAGRAMs.
	ReliableUDP bool          `json:"reliable_udp,omitempty"`
	Relays      []RelayConfig `json:"relays,omitempty"`
	// NoSOCKS (local role) disables the SOCKS5 listener, e.g. to serve
	// only WireGuard.
	NoSOCKS bool `json:"no_socks,omitempty"`
	// WireGuard (local role), if set, runs a userspace WireGuard server
	// whose peers' traffic is proxied transparently.
	WireGuard *WireGuardConfig `json:"wireguard,omitempty"`
}

// DefaultWireGuardPort is the default WireGuard listen port.
const DefaultWireGuardPort = 51820

// WireGuardConfig configures the WireGuard listener of the local role.
type WireGuardConfig struct {
	// Listen is the UDP listen address: "[host]:port" or "port"
	// (default: port 51820 on all interfaces).
	Listen     string          `json:"listen,omitempty"`
	PrivateKey string          `json:"private_key,omitempty"`
	MTU        int             `json:"mtu,omitempty"` // interface MTU (default 1420)
	Peers      []WireGuardPeer `json:"peers,omitempty"`
}

// WireGuardPeer is a client of the WireGuard listener.
type WireGuardPeer struct {
	PublicKey    string `json:"public_key"`
	PresharedKey string `json:"preshared_key,omitempty"`
	// AllowedIPs are the peer's tunnel addresses (default: any; required
	// with several peers).
	AllowedIPs StringList `json:"allowed_ips,omitempty"`
}

// WireGuardListen returns the WireGuard listen address with defaults.
func (w *WireGuardConfig) WireGuardListen() string {
	l := strings.TrimSpace(w.Listen)
	if l == "" {
		return ":" + strconv.Itoa(DefaultWireGuardPort)
	}
	if _, err := strconv.Atoi(l); err == nil {
		return ":" + l
	}
	return l
}

// RelayConfig configures one UDP relay (relay mode).
type RelayConfig struct {
	Listen string `json:"listen"`
	Target string `json:"target"`
	Link   string `json:"link,omitempty"` // applied to both directions
	Fwd    string `json:"fwd,omitempty"`  // client -> target (overrides link)
	Rev    string `json:"rev,omitempty"`  // target -> client (overrides link)
}

// ServerEntry is either "host", "host:port", or a shadowsocks-rust style
// object {"server": ..., "server_port": ..., "password": ..., "method": ...}.
type ServerEntry struct {
	Server     string `json:"server"`
	ServerPort int    `json:"server_port,omitempty"`
	Password   string `json:"password,omitempty"`
	Method     string `json:"method,omitempty"`
}

func (e *ServerEntry) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		e.Server = s
		return nil
	}
	type raw ServerEntry
	return json.Unmarshal(b, (*raw)(e))
}

// StringList accepts a JSON string or array of strings.
type StringList []string

func (l *StringList) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*l = []string{s}
		return nil
	}
	var a []string
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*l = a
	return nil
}

// Size accepts a number of bytes or a string like "16MB", "512KB".
type Size int64

func (s *Size) UnmarshalJSON(b []byte) error {
	var n int64
	if err := json.Unmarshal(b, &n); err == nil {
		*s = Size(n)
		return nil
	}
	var str string
	if err := json.Unmarshal(b, &str); err != nil {
		return err
	}
	v, err := ParseSize(str)
	*s = Size(v)
	return err
}

// MarshalJSON writes round sizes as strings ("64MB") and others as numbers.
func (s Size) MarshalJSON() ([]byte, error) {
	if str := FormatSize(int64(s)); strings.HasSuffix(str, "B") {
		return json.Marshal(str)
	}
	return json.Marshal(int64(s))
}

// FormatSize is the inverse of ParseSize: 64<<20 -> "64MB", 1000 -> "1000".
func FormatSize(n int64) string {
	for _, u := range []struct {
		shift  uint
		suffix string
	}{{30, "GB"}, {20, "MB"}, {10, "KB"}} {
		if n != 0 && n%(1<<u.shift) == 0 {
			return strconv.FormatInt(n>>u.shift, 10) + u.suffix
		}
	}
	return strconv.FormatInt(n, 10)
}

// Rate is a bit rate in bit/s. It accepts a number or a string like
// "20mbit", "100 mbps", "1.5g" (decimal units, always bits).
type Rate uint64

func (r *Rate) UnmarshalJSON(b []byte) error {
	var n uint64
	if err := json.Unmarshal(b, &n); err == nil {
		*r = Rate(n)
		return nil
	}
	var str string
	if err := json.Unmarshal(b, &str); err != nil {
		return fmt.Errorf("bad rate %s", b)
	}
	v, err := ParseRate(str)
	*r = v
	return err
}

// MarshalJSON writes round rates as strings ("20mbit") and others as numbers.
func (r Rate) MarshalJSON() ([]byte, error) {
	for _, u := range []struct {
		mult   uint64
		suffix string
	}{{1e9, "gbit"}, {1e6, "mbit"}, {1e3, "kbit"}} {
		if r != 0 && uint64(r)%u.mult == 0 {
			return json.Marshal(strconv.FormatUint(uint64(r)/u.mult, 10) + u.suffix)
		}
	}
	return json.Marshal(uint64(r))
}

// BytesPerSecond converts the rate for mp.Options.
func (r Rate) BytesPerSecond() uint64 { return uint64(r) / 8 }

// ParseRate parses "20mbit", "100 Mbps", "1.5g", "500k", "8000000" (bit/s).
func ParseRate(str string) (Rate, error) {
	l := strings.ToLower(strings.TrimSpace(str))
	l = strings.TrimSuffix(l, "/s")
	for _, suf := range []string{"bits", "bit", "bps"} {
		if t := strings.TrimSuffix(l, suf); t != l {
			l = t
			break
		}
	}
	l = strings.TrimSpace(l)
	mult := 1.0
	switch {
	case strings.HasSuffix(l, "k"):
		mult, l = 1e3, strings.TrimSuffix(l, "k")
	case strings.HasSuffix(l, "m"):
		mult, l = 1e6, strings.TrimSuffix(l, "m")
	case strings.HasSuffix(l, "g"):
		mult, l = 1e9, strings.TrimSuffix(l, "g")
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(l), 64)
	if err != nil || f < 0 || f*mult > 1e15 {
		return 0, fmt.Errorf("bad rate %q (want e.g. 20mbit)", str)
	}
	return Rate(f * mult), nil
}

// ParseSize parses "16MB", "512k", "1048576".
func ParseSize(str string) (int64, error) {
	l := strings.ToLower(strings.TrimSpace(str))
	l = strings.TrimSuffix(l, "b")
	l = strings.TrimSuffix(l, "i")
	mult := int64(1)
	switch {
	case strings.HasSuffix(l, "k"):
		mult, l = 1<<10, strings.TrimSuffix(l, "k")
	case strings.HasSuffix(l, "m"):
		mult, l = 1<<20, strings.TrimSuffix(l, "m")
	case strings.HasSuffix(l, "g"):
		mult, l = 1<<30, strings.TrimSuffix(l, "g")
	}
	f, err := strconv.ParseFloat(l, 64)
	if err != nil {
		return 0, fmt.Errorf("bad size %q", str)
	}
	return int64(f * float64(mult)), nil
}

// Load reads a JSON config file.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := &Config{}
	if err := json.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	return c, nil
}

// Save writes the config as indented JSON. The file is replaced atomically
// (written to a temporary file next to it, then renamed).
func (c *Config) Save(path string) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".mpquic-*.json")
	if err != nil {
		return err
	}
	_, err = tmp.Write(b)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

// Clone returns a deep copy of the config.
func (c *Config) Clone() *Config {
	n := *c
	n.Server = append(StringList(nil), c.Server...)
	n.Servers = append([]ServerEntry(nil), c.Servers...)
	n.Relays = append([]RelayConfig(nil), c.Relays...)
	if c.WireGuard != nil {
		w := *c.WireGuard
		w.Peers = nil
		for _, p := range c.WireGuard.Peers {
			p.AllowedIPs = append(StringList(nil), p.AllowedIPs...)
			w.Peers = append(w.Peers, p)
		}
		n.WireGuard = &w
	}
	return &n
}

// JoinHostPort combines a "host" or "host:port" entry with a default port.
func JoinHostPort(entry string, defPort int) string {
	entry = strings.TrimSpace(entry)
	if _, _, err := net.SplitHostPort(entry); err == nil {
		return entry
	}
	h := strings.TrimSuffix(strings.TrimPrefix(entry, "["), "]")
	return net.JoinHostPort(h, strconv.Itoa(defPort))
}

// ServerAddrs returns all server addresses (host:port) in order.
func (c *Config) ServerAddrs() []string {
	port := c.ServerPort
	if port == 0 {
		port = 8388
	}
	var out []string
	for _, s := range c.Server {
		out = append(out, JoinHostPort(s, port))
	}
	for _, e := range c.Servers {
		p := e.ServerPort
		if p == 0 {
			p = port
		}
		out = append(out, JoinHostPort(e.Server, p))
	}
	return out
}

// EffectivePassword returns the top-level password or the first server's.
func (c *Config) EffectivePassword() string {
	if c.Password != "" {
		return c.Password
	}
	for _, e := range c.Servers {
		if e.Password != "" {
			return e.Password
		}
	}
	return ""
}

// UDPEnabled reports whether UDP relaying is enabled (ss "mode").
func (c *Config) UDPEnabled() bool {
	switch c.Mode {
	case "tcp_and_udp", "udp_only":
		return true
	}
	return false
}

// ListenAddrs returns the UDP addresses a QUIC listener binds: every "server"
// entry combined with server_port (default: all interfaces).
func (c *Config) ListenAddrs() []string {
	port := c.ServerPort
	if port == 0 {
		port = 8388
	}
	if len(c.Server) == 0 {
		return []string{":" + strconv.Itoa(port)}
	}
	var out []string
	for _, b := range c.Server {
		if b == "" {
			out = append(out, ":"+strconv.Itoa(port))
		} else {
			out = append(out, JoinHostPort(b, port))
		}
	}
	return out
}
