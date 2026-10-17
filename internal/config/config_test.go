package config

import (
	"encoding/json"
	"reflect"
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
