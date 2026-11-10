package main

import (
	"reflect"
	"testing"

	"mpquic/internal/config"
)

func TestWireGuardFlags(t *testing.T) {
	const priv = "dwdtCnMYpX08FsFyUbJmRd9ML4frwJkqsXf7pR25LCo="
	const pub1 = "hSDwCYkwp1R0i33ctD73Wg2/Og0mOBr066SpjqqbTmo="
	const pub2 = "3p7bfXt9wbTTW2HC7OQ1Nz+DQ8hbeGdNrfx+FG+IK08="
	cfg, err := parseRole("local", []string{"-s", "h:1", "-k", "pw", "--no-socks",
		"--wg-listen", "51821", "--wg-private-key", priv, "--wg-mtu", "1380",
		"--wg-peer", pub1 + ",10.0.0.2/32, fd00::2", "--wg-peer", pub2 + ",10.0.0.3", "--wg-psk", pub1})
	if err != nil {
		t.Fatal(err)
	}
	want := &config.WireGuardConfig{Listen: "51821", PrivateKey: priv, MTU: 1380, Peers: []config.WireGuardPeer{
		{PublicKey: pub1, PresharedKey: pub1, AllowedIPs: config.StringList{"10.0.0.2/32", "fd00::2"}},
		{PublicKey: pub2, PresharedKey: pub1, AllowedIPs: config.StringList{"10.0.0.3"}},
	}}
	if !cfg.NoSOCKS || !reflect.DeepEqual(cfg.WireGuard, want) {
		t.Fatalf("got no_socks=%v %+v", cfg.NoSOCKS, cfg.WireGuard)
	}
	if got := cfg.WireGuard.WireGuardListen(); got != ":51821" {
		t.Fatalf("listen %q", got)
	}
	cfg, err = parseRole("local", []string{"-s", "h:1", "-k", "pw"})
	if err != nil || cfg.WireGuard != nil || cfg.NoSOCKS {
		t.Fatalf("plain local: %+v %v", cfg, err)
	}
}

func TestTUNFlags(t *testing.T) {
	cfg, err := parseRole("local", []string{"-s", "h:1", "-k", "pw", "--no-socks", "--tun", "mpq0", "--tun-mtu", "1400",
		"--tun-addr", "10.99.0.1/24", "--tun-addr", "fd99::1/64", "--tun-route", "10.0.0.0/8", "--fwmark", "0x4d51"})
	if err != nil {
		t.Fatal(err)
	}
	want := &config.TUNConfig{Name: "mpq0", MTU: 1400, Addresses: config.StringList{"10.99.0.1/24", "fd99::1/64"}, Routes: config.StringList{"10.0.0.0/8"}}
	if !reflect.DeepEqual(cfg.TUN, want) || cfg.FWMark != 0x4d51 {
		t.Fatalf("got %+v fwmark %#x", cfg.TUN, cfg.FWMark)
	}
	if _, err := parseRole("local", []string{"-s", "h:1", "-k", "pw", "--fwmark", "blue"}); err == nil {
		t.Fatal("--fwmark blue accepted")
	}
	cfg, err = parseRole("local", []string{"-s", "h:1", "-k", "pw"})
	if err != nil || cfg.TUN != nil || cfg.FWMark != 0 {
		t.Fatalf("plain local: %+v %v", cfg, err)
	}
}
