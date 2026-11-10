package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestShadowsocksCompat(t *testing.T) {
	var c Config
	err := json.Unmarshal([]byte(`{
		"server": ["1.2.3.4", "relay.example.com:9000", "::1"],
		"server_port": 8388,
		"servers": ["5.6.7.8:7000", {"server": "9.9.9.9", "server_port": 1234, "password": "p2", "method": "aes-256-gcm"}],
		"local_address": "127.0.0.1", "local_port": 1080,
		"password": "", "method": "chacha20-ietf-poly1305", "timeout": 60,
		"mode": "tcp_and_udp", "window": "8MB", "chunk_size": 8192
	}`), &c)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"1.2.3.4:8388", "relay.example.com:9000", "[::1]:8388", "5.6.7.8:7000", "9.9.9.9:1234"}
	if got := c.ServerAddrs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("servers = %v", got)
	}
	if c.EffectivePassword() != "p2" || !c.UDPEnabled() || c.Window != 8<<20 || c.ChunkSize != 8192 {
		t.Fatalf("config %+v", c)
	}
	var single Config
	json.Unmarshal([]byte(`{"server":"10.0.0.1","server_port":443,"password":"x"}`), &single)
	if got := single.ServerAddrs(); len(got) != 1 || got[0] != "10.0.0.1:443" {
		t.Fatalf("single = %v", got)
	}
}

func TestReverseAndListen(t *testing.T) {
	var c Config
	json.Unmarshal([]byte(`{"reverse": true, "server": ["0.0.0.0", "[::1]:9000", "::"], "server_port": 8388}`), &c)
	if !c.Reverse {
		t.Fatal("reverse not parsed")
	}
	if got := c.ListenAddrs(); !reflect.DeepEqual(got, []string{"0.0.0.0:8388", "[::1]:9000", "[::]:8388"}) {
		t.Fatalf("listen = %v", got)
	}
	var d Config
	if got := d.ListenAddrs(); !reflect.DeepEqual(got, []string{":8388"}) {
		t.Fatalf("default listen = %v", got)
	}
}

func TestSaveAndSizes(t *testing.T) {
	for n, want := range map[int64]string{64 << 20: "64MB", 16 << 10: "16KB", 2 << 30: "2GB", 1000: "1000", 0: "0"} {
		if got := FormatSize(n); got != want {
			t.Errorf("FormatSize(%d) = %q, want %q", n, got, want)
		}
	}
	c := &Config{Role: "local", Server: StringList{"a", "b:1"}, Password: "p", Window: 32 << 20, ChunkSize: 1000,
		Relays: []RelayConfig{{Listen: ":9000", Target: "h:1"}}}
	p := filepath.Join(t.TempDir(), "c.json")
	if err := c.Save(p); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	for _, s := range []string{`"window": "32MB"`, `"chunk_size": 1000`, `"role": "local"`} {
		if !strings.Contains(string(b), s) {
			t.Errorf("saved config lacks %s:\n%s", s, b)
		}
	}
	for _, s := range []string{"server_port", "fast_open", "reverse", `"link"`} {
		if strings.Contains(string(b), s) {
			t.Errorf("saved config has unset key %s:\n%s", s, b)
		}
	}
	back, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, c) {
		t.Fatalf("round trip: %+v != %+v", back, c)
	}
	cl := c.Clone()
	cl.Server[0] = "z"
	if c.Server[0] != "a" {
		t.Fatal("Clone shares the server list")
	}
}

func TestWireGuardConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.json")
	os.WriteFile(path, []byte(`{"server":"h","password":"p","no_socks":true,
		"wireguard":{"private_key":"k","peers":[{"public_key":"a","allowed_ips":"10.0.0.2/32"}]}}`), 0o600)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !c.NoSOCKS || c.WireGuard == nil || c.WireGuard.WireGuardListen() != ":51820" ||
		len(c.WireGuard.Peers) != 1 || c.WireGuard.Peers[0].AllowedIPs[0] != "10.0.0.2/32" {
		t.Fatalf("%+v", c.WireGuard)
	}
	n := c.Clone()
	n.WireGuard.Peers[0].AllowedIPs[0] = "x"
	n.WireGuard.Listen = "1"
	if c.WireGuard.Peers[0].AllowedIPs[0] != "10.0.0.2/32" || c.WireGuard.Listen != "" {
		t.Fatal("Clone shares the WireGuard config")
	}
}

func TestRates(t *testing.T) {
	for in, want := range map[string]Rate{
		"20mbit": 20e6, "100 Mbps": 100e6, "1.5g": 1.5e9, "500k": 500e3, "8000000": 8e6, "2.5mbit/s": 2.5e6, "64kbps": 64e3, "0": 0,
	} {
		if got, err := ParseRate(in); err != nil || got != want {
			t.Errorf("ParseRate(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "fast", "-1mbit", "20mb/h"} {
		if _, err := ParseRate(in); err == nil {
			t.Errorf("ParseRate(%q): no error", in)
		}
	}
	var c Config
	if err := json.Unmarshal([]byte(`{"up": "20mbit", "down": 100000000}`), &c); err != nil || c.Up != 20e6 || c.Down != 100e6 {
		t.Fatalf("up/down = %d/%d, %v", c.Up, c.Down, err)
	}
	if c.Up.BytesPerSecond() != 2500000 {
		t.Fatalf("bytes/s = %d", c.Up.BytesPerSecond())
	}
	b, _ := json.Marshal(struct{ A, B, C Rate }{20e6, 1500, 2e9})
	if string(b) != `{"A":"20mbit","B":1500,"C":"2gbit"}` {
		t.Fatalf("marshal: %s", b)
	}
	if err := json.Unmarshal([]byte(`{"up": "lots"}`), &c); err == nil {
		t.Fatal("bad rate accepted")
	}
}
