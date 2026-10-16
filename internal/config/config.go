// Package config loads shadowsocks-compatible JSON configuration, extended
// for multiple server paths.
package config

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

// Config mirrors shadowsocks-libev / shadowsocks-rust JSON keys, plus a few
// mpquic-specific ones.
type Config struct {
	Server        StringList    `json:"server"`
	ServerPort    int           `json:"server_port"`
	Servers       []ServerEntry `json:"servers"`
	LocalAddress  string        `json:"local_address"`
	LocalPort     int           `json:"local_port"`
	Password      string        `json:"password"`
	Method        string        `json:"method"`
	Timeout       int           `json:"timeout"`
	TunnelAddress string        `json:"tunnel_address"`
	Mode          string        `json:"mode"`
	FastOpen      bool          `json:"fast_open"`
	NoDelay       bool          `json:"no_delay"`
	ReusePort     bool          `json:"reuse_port"`
	Plugin        string        `json:"plugin"`

	// mpquic extensions
	Congestion    string        `json:"congestion"`
	Window        Size          `json:"window"`
	ChunkSize     Size          `json:"chunk_size"`
	StatsInterval int           `json:"stats_interval"`
	Verbose       bool          `json:"verbose"`
	LogLevel      string        `json:"log_level"`
	Cert          string        `json:"cert"`
	Key           string        `json:"key"`
	SNI           string        `json:"sni"`
	NoDup         bool          `json:"no_dup"`
	Relays        []RelayConfig `json:"relays"`
}

// RelayConfig configures one UDP relay (relay mode).
type RelayConfig struct {
	Listen string `json:"listen"`
	Target string `json:"target"`
	Link   string `json:"link"` // applied to both directions
	Fwd    string `json:"fwd"`  // client -> target (overrides link)
	Rev    string `json:"rev"`  // target -> client (overrides link)
}

// ServerEntry is either "host", "host:port", or a shadowsocks-rust style
// object {"server": ..., "server_port": ..., "password": ..., "method": ...}.
type ServerEntry struct {
	Server     string `json:"server"`
	ServerPort int    `json:"server_port"`
	Password   string `json:"password"`
	Method     string `json:"method"`
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
