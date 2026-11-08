package winsvc

import (
	"reflect"
	"testing"

	"mpquic/internal/config"
)

func TestCheckName(t *testing.T) {
	for _, ok := range []string{"MPQUIC", "mpquic-relay", "a.b_c"} {
		if err := CheckName(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-x", `a\b`, "a b", "x/y", "..", "a:b"} {
		if CheckName(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestFirewallRules(t *testing.T) {
	cases := []struct {
		cfg  config.Config
		want []FirewallRule
	}{
		{config.Config{Role: "remote"}, []FirewallRule{{"UDP", []int{8388}}}},
		{config.Config{Role: "server", Server: config.StringList{"0.0.0.0", "[::]:9000"}, ServerPort: 443}, []FirewallRule{{"UDP", []int{443, 9000}}}},
		{config.Config{Role: "remote", Reverse: true, Server: config.StringList{"a:1"}}, nil},
		{config.Config{Role: "local", Server: config.StringList{"a:1"}}, nil},
		{config.Config{Role: "local", Reverse: true, ServerPort: 8443}, []FirewallRule{{"UDP", []int{8443}}}},
		{config.Config{Role: "local", LocalAddress: "0.0.0.0", LocalPort: 1081, Mode: "tcp_and_udp", Reverse: true},
			[]FirewallRule{{"UDP", nil}, {"TCP", []int{1081}}}},
		{config.Config{Role: "tunnel", LocalAddress: "192.168.1.2", Mode: "tcp_and_udp"}, []FirewallRule{{"TCP", []int{1080}}, {"UDP", []int{1080}}}},
		{config.Config{Role: "tunnel", LocalAddress: "192.168.1.2"}, []FirewallRule{{"TCP", []int{1080}}}},
		{config.Config{Role: "local", LocalAddress: "::1"}, nil},
		{config.Config{Role: "local", WireGuard: &config.WireGuardConfig{}}, []FirewallRule{{"UDP", []int{51820}}}},
		{config.Config{Role: "local", LocalAddress: "0.0.0.0", NoSOCKS: true, WireGuard: &config.WireGuardConfig{Listen: "192.168.1.2:4500"}},
			[]FirewallRule{{"UDP", []int{4500}}}},
		{config.Config{Role: "local", WireGuard: &config.WireGuardConfig{Listen: "127.0.0.1:4500"}}, nil},
		{config.Config{Role: "relay", Relays: []config.RelayConfig{{Listen: ":9001"}, {Listen: "0.0.0.0:9000"}, {Listen: ":9001"}}},
			[]FirewallRule{{"UDP", []int{9000, 9001}}}},
	}
	for i, c := range cases {
		if got := FirewallRules(&c.cfg); !reflect.DeepEqual(got, c.want) {
			t.Errorf("case %d: got %v, want %v", i, got, c.want)
		}
	}
}
